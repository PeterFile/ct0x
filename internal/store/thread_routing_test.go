package store

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func enableThreadRoutingFixture(t *testing.T) {
	t.Helper()
	if !slices.Contains(migrations, threadRoutingSchema) {
		original := migrations
		migrations = append(slices.Clone(original), threadRoutingSchema)
		t.Cleanup(func() { migrations = original })
	}
}

func postThreadMessage(t *testing.T, s *Store, r MessageRequest) domain.Message {
	t.Helper()
	m, err := s.PostLocalUserMessage(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestThreadRoutingV30MigrationAndReopenedRetry(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = migrations[:30]
	path := filepath.Join(t.TempDir(), "routing.sqlite")
	old := queueHandle(t, path, false)
	groupID := group(t, old).ID
	owner := member(t, old, groupID).ID
	before := MessageRequest{ActorID: "user", GroupID: groupID, TopicID: "Legacy/Explicit", Content: "exact legacy content", IdempotencyKey: "legacy"}
	legacy := postThreadMessage(t, old, before)
	// The trusted explicit-delivery entry still writes the exact old payload,
	// including the historical equivalence of omitted and empty topic IDs.
	oldMain := MessageRequest{ActorID: "user", GroupID: groupID, Content: "old main", IdempotencyKey: "old-main"}
	legacyMain, err := old.CreateLocalUserMessage(ctx, oldMain, []string{owner})
	if err != nil {
		t.Fatal(err)
	}
	var oldSequence int64
	if err := old.db.QueryRow(`SELECT sequence FROM message_log WHERE message_id=?`, legacy.ID).Scan(&oldSequence); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	migrations = original
	enableThreadRoutingFixture(t)
	current := queueHandle(t, path, false)
	if got := postThreadMessage(t, current, before); !reflect.DeepEqual(got, legacy) {
		t.Fatalf("migration changed legacy receipt: got %+v want %+v", got, legacy)
	}
	for _, explicit := range []bool{false, true} {
		oldMain.TopicIDExplicit = explicit
		if got := postThreadMessage(t, current, oldMain); !reflect.DeepEqual(got, legacyMain) {
			t.Fatalf("migration changed legacy main receipt: got %+v want %+v", got, legacyMain)
		}
	}
	var sequence int64
	var topic string
	var proof []byte
	if err := current.db.QueryRow(`SELECT l.sequence,l.topic_id,p.payload FROM message_log l
	 JOIN message_command_sources p ON p.message_id=l.message_id WHERE l.message_id=?`, legacy.ID).Scan(&sequence, &topic, &proof); err != nil || sequence != oldSequence || topic != before.TopicID || !bytes.Equal(proof, before.payload()) {
		t.Fatalf("migration changed message log/proof: sequence=%d topic=%q err=%v", sequence, topic, err)
	}
	fresh := MessageRequest{ActorID: "user", GroupID: groupID, Content: "New topic: WAL-920 lock timeout", IdempotencyKey: "fresh"}
	race := func(stores ...*Store) domain.Message {
		t.Helper()
		const workers = 6
		type result struct {
			m   domain.Message
			err error
		}
		start := make(chan struct{})
		results := make(chan result, workers)
		for i := range workers {
			go func() {
				<-start
				m, err := stores[i%len(stores)].PostLocalUserMessage(ctx, fresh)
				results <- result{m, err}
			}()
		}
		close(start)
		first := <-results
		for i := 1; i < workers; i++ {
			if got := <-results; got.err != nil || !reflect.DeepEqual(got.m, first.m) {
				t.Errorf("concurrent retry diverged: %+v (%v), first=%+v (%v)", got.m, got.err, first.m, first.err)
			}
		}
		if first.err != nil || first.m.TopicID == "" || first.m.Content != fresh.Content || len(first.m.Deliveries) != 1 || first.m.Deliveries[0].RecipientID != owner {
			t.Fatalf("concurrent send: %+v, %v", first.m, first.err)
		}
		return first.m
	}
	peer := queueHandle(t, path, false)
	first := race(current, peer)
	if err := errors.Join(current.Close(), peer.Close()); err != nil {
		t.Fatal(err)
	}
	reopened, reopenedPeer := queueHandle(t, path, false), queueHandle(t, path, false)
	if got := race(reopened, reopenedPeer); !reflect.DeepEqual(got, first) {
		t.Fatalf("reopen changed receipt: got %+v want %+v", got, first)
	}
	for _, tc := range []struct {
		id, query string
		want      []byte
	}{
		{first.ID, `SELECT payload FROM message_command_sources WHERE message_id=?`, fresh.payload()},
		{first.ID, `SELECT payload FROM mutation_idempotency WHERE record_id=?`, fresh.topicSelectionPayload()},
		{legacyMain.ID, `SELECT payload FROM message_command_sources WHERE message_id=?`, oldMain.payload()},
		{legacyMain.ID, `SELECT payload FROM mutation_idempotency WHERE record_id=?`, oldMain.payload()},
	} {
		if err := reopened.db.QueryRow(tc.query, tc.id).Scan(&proof); err != nil || !bytes.Equal(proof, tc.want) {
			t.Fatalf("stored request fingerprint mismatch: %x (%v)", proof, err)
		}
	}
	if m, found, err := reopened.LookupMessageReceipt(ctx, fresh); err != nil || !found || !reflect.DeepEqual(m, first) {
		t.Fatalf("lookup lost a versioned receipt: %+v, found=%t, err=%v", m, found, err)
	}
	fresh.TopicIDExplicit = true
	if _, _, err := reopened.LookupMessageReceipt(ctx, fresh); !errors.Is(err, ErrConflict) {
		t.Fatalf("lookup accepted a changed main selector: %v", err)
	}
	if err := reopened.db.QueryRow(`SELECT sequence,topic_id FROM message_log WHERE message_id=?`, first.ID).Scan(&sequence, &topic); err != nil || sequence <= oldSequence || topic != first.TopicID {
		t.Fatalf("inferred scope and stable position disagree: %q %d %v", topic, sequence, err)
	}
	threadCounts(t, reopened, map[string]int{"messages": 3, "deliveries": 3, "message_log": 3, "message_command_sources": 3, "thread_routing_topics": 1, "tasks": 0, "mandates": 0})
}

func TestThreadRoutingTaskLinkAndScopedHistory(t *testing.T) {
	enableThreadRoutingFixture(t)
	f := setupMessages(t)
	origin := postThreadMessage(t, f.s, MessageRequest{ActorID: "user", GroupID: f.groupID,
		Content: "新议题：磁盘阵列更换计划 OPS-101", RecipientIDs: []string{}})
	other := postThreadMessage(t, f.s, MessageRequest{ActorID: "user", GroupID: f.groupID,
		Content: "新议题：磁盘阵列更换计划 OPS-102", RecipientIDs: []string{}})
	if other.TopicID == origin.TopicID || other.TopicID == "" || origin.TopicID == "" {
		t.Fatalf("expected distinct topics with the same shortened name: %q %q", origin.TopicID, other.TopicID)
	}
	admission, err := f.s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{TopicID: origin.TopicID, MessageID: origin.ID,
		Mode: domain.RoutingDecisionCreateNew, Reason: "explicit fixture operation", NewTask: &domain.NewTaskRequest{
			Title: "check array", Acceptance: "record compatibility", AssigneeID: f.b,
		}}, "admission")
	if err != nil {
		t.Fatal(err)
	}
	request := MessageRequest{ActorID: "user", GroupID: f.otherID, Content: "跟进那个磁盘阵列更换计划", IdempotencyKey: "link", ThreadLinks: domain.ThreadLinks{RelatedTaskID: admission.Task.ID}}
	if _, err := f.s.PostLocalUserMessage(ctx, request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign task reference accepted: %v", err)
	}
	request.GroupID = f.groupID
	base := time.Now().UTC().Add(-time.Second)
	noise := make([]domain.Message, 61)
	for i := range noise {
		noise[i] = preparationHistoryMessage(fmt.Sprintf("noise-%02d", i), f.groupID, "other-topic", base.Add(time.Duration(i)*time.Nanosecond))
	}
	insertPreparationHistoryMessages(t, f.s, noise...)
	followup := postThreadMessage(t, f.s, request)
	if followup.TopicID != origin.TopicID || followup.RelatedTaskID != admission.Task.ID || len(followup.Deliveries) != 1 || followup.Deliveries[0].RecipientID != f.b {
		t.Fatalf("known task did not route to its verified scope and assignee: %+v", followup)
	}
	history, err := f.s.ListPreparationHistory(ctx, f.groupID, followup.ID, 0)
	if err != nil || len(history) != 1 || history[0].ID != origin.ID {
		t.Fatalf("legacy scoped context was not isolated: %+v, %v", history, err)
	}
	ref := domain.ThreadRef{GroupID: f.groupID, TopicID: origin.TopicID}
	attr, err := f.s.MessageThreadAttribution(ctx, f.groupID, followup.ID)
	if err != nil || attr.Storage != ref || attr.Effective != ref {
		t.Fatalf("wrong context scope: %+v, %v", attr, err)
	}
	if _, err := f.s.MessageThreadAttribution(ctx, f.otherID, followup.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-group scope read: %v", err)
	}
	conflict := request
	conflict.IdempotencyKey, conflict.Content, conflict.RecipientIDs = "contradiction", "跟进那个磁盘阵列更换计划，OPS-102", []string{}
	if m := postThreadMessage(t, f.s, conflict); m.TopicID != "" || m.RelatedTaskID != admission.Task.ID || len(m.Deliveries) != 0 {
		t.Fatalf("contradictory issue and task must remain on main without delivery: %+v", m)
	}
	threadCounts(t, f.s, map[string]int{"thread_routing_topics": 2, "thread_routing_links": 2, "deliveries": 1, "tasks": 1, "mandates": 1})
}

func TestThreadRoutingLegacyFallbackAndDamagedSchema(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = migrations[:30]
	legacy, _ := openTestStore(t)
	groupID := group(t, legacy).ID
	owner := member(t, legacy, groupID).ID
	r := MessageRequest{GroupID: groupID, ActorID: "user", Content: "新议题：QA-404 索引异常"}
	m := postThreadMessage(t, legacy, r)
	if m.TopicID != "" || len(m.Deliveries) != 1 || m.Deliveries[0].RecipientID != owner {
		t.Fatalf("unregistered build changed legacy routing: %+v", m)
	}
	r.ReplyToMessageID = m.ID
	if _, err := legacy.PostLocalUserMessage(ctx, r); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unavailable explicit optional capability accepted: %v", err)
	}
	threadCounts(t, legacy, map[string]int{"messages": 1, "deliveries": 1})
	t.Run("unregistered_orphan_schema", func(t *testing.T) {
		if _, err := legacy.db.Exec(`CREATE TABLE thread_routing_topics (orphan TEXT)`); err != nil {
			t.Fatal(err)
		}
		r.ReplyToMessageID = ""
		if _, err := legacy.PostLocalUserMessage(ctx, r); err == nil {
			t.Error("orphan schema allowed the legacy write path")
		}
		if _, err := legacy.GetMessage(ctx, groupID, m.ID); err == nil {
			t.Error("orphan schema allowed the local timeline detail path")
		}
	})

	migrations = original
	enableThreadRoutingFixture(t)
	version := slices.Index(migrations, threadRoutingSchema) + 1
	if version == 0 {
		t.Fatal("thread routing migration is not registered")
	}
	for _, damage := range []struct {
		name, statement string
		args            []any
	}{
		{"missing_topics_table", `DROP TABLE thread_routing_topics`, nil},
		{"missing_links_table", `DROP TABLE thread_routing_links`, nil},
		{"missing_routing_marker", `DELETE FROM schema_migrations WHERE version=?`, []any{version}},
		{"missing_issue_index", `DROP INDEX thread_routing_issue`, nil},
		{"nonunique_issue_index", `DROP INDEX thread_routing_issue; CREATE INDEX thread_routing_issue ON thread_routing_topics(group_id, issue_id)`, nil},
		{"missing_replace_guard", `DROP TRIGGER thread_routing_links_insert`, nil},
		{"missing_update_guard", `DROP TRIGGER thread_routing_links_update`, nil},
		{"missing_delete_guard", `DROP TRIGGER thread_routing_links_delete`, nil},
	} {
		t.Run(damage.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			groupID := group(t, s).ID
			request := MessageRequest{GroupID: groupID, ActorID: "user", Content: "新议题：QA-409 核对索引计划", IdempotencyKey: "origin"}
			origin := postThreadMessage(t, s, request)
			other := group(t, s).ID
			if _, err := s.db.Exec(damage.statement, damage.args...); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PostLocalUserMessage(ctx, request); err == nil {
				t.Error("damaged install allowed a normal POST replay")
			}
			for i, content := range []string{"跟进 QA-409", "当前讨论 QA-409 核对索引计划"} {
				for _, g := range []string{groupID, other} {
					if m, err := s.PostLocalUserMessage(ctx, MessageRequest{GroupID: g, ActorID: "user", Content: content, IdempotencyKey: fmt.Sprintf("damaged-%d", i)}); err == nil || m.ID != "" {
						t.Errorf("damaged install accepted %q in group %s: %+v (%v)", content, g, m, err)
					}
				}
			}
			if _, err := s.GetMessage(ctx, groupID, origin.ID); err == nil {
				t.Error("damaged install allowed the local timeline detail path")
			}
			threadCounts(t, s, map[string]int{"messages": 1, "deliveries": 0, "message_log": 1})
		})
	}
}

func threadCounts(t *testing.T, s *Store, tables map[string]int) {
	t.Helper()
	for table, want := range tables {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count = %d (%v), want %d", table, count, err, want)
		}
	}
}
