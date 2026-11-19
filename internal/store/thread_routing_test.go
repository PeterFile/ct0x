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
	for _, query := range []string{
		`SELECT payload FROM message_command_sources WHERE message_id=?`,
		`SELECT payload FROM mutation_idempotency WHERE record_id=?`,
	} {
		if err := reopened.db.QueryRow(query, first.ID).Scan(&proof); err != nil || !bytes.Equal(proof, fresh.payload()) {
			t.Fatalf("inference rewrote the exact local-user request payload: %x (%v)", proof, err)
		}
	}
	if err := reopened.db.QueryRow(`SELECT sequence,topic_id FROM message_log WHERE message_id=?`, first.ID).Scan(&sequence, &topic); err != nil || sequence <= oldSequence || topic != first.TopicID {
		t.Fatalf("inferred scope and stable position disagree: %q %d %v", topic, sequence, err)
	}
	threadCounts(t, reopened, map[string]int{"messages": 2, "deliveries": 2, "message_log": 2, "message_command_sources": 2, "thread_routing_topics": 1, "tasks": 0, "mandates": 0})
}

func TestThreadRoutingTaskLinkAndScopedHistory(t *testing.T) {
	enableThreadRoutingFixture(t)
	f := setupMessages(t)
	origin := postThreadMessage(t, f.s, MessageRequest{ActorID: "user", GroupID: f.groupID,
		Content: "新议题：磁盘阵列更换计划", RecipientIDs: []string{}})
	admission, err := f.s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{TopicID: origin.TopicID, MessageID: origin.ID,
		Mode: domain.RoutingDecisionCreateNew, Reason: "explicit fixture operation", NewTask: &domain.NewTaskRequest{
			Title: "check array", Acceptance: "record compatibility", AssigneeID: f.b,
		}}, "admission")
	if err != nil {
		t.Fatal(err)
	}
	request := MessageRequest{ActorID: "user", GroupID: f.otherID, Content: "followup", IdempotencyKey: "link", ThreadLinks: domain.ThreadLinks{RelatedTaskID: admission.Task.ID}}
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
	threadCounts(t, f.s, map[string]int{"thread_routing_topics": 1, "thread_routing_links": 1, "deliveries": 1, "tasks": 1, "mandates": 1})
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

	migrations = original
	enableThreadRoutingFixture(t)
	for _, statement := range []string{
		`DROP TABLE thread_routing_topics`,
		`DROP TABLE thread_routing_links`,
		`DELETE FROM schema_migrations WHERE version=(SELECT max(version) FROM schema_migrations)`,
	} {
		t.Run(statement, func(t *testing.T) {
			s, _ := openTestStore(t)
			groupID := group(t, s).ID
			if _, err := s.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if m, err := s.PostLocalUserMessage(ctx, MessageRequest{GroupID: groupID, ActorID: "user", TopicID: "legacy", Content: "still explicit", IdempotencyKey: "damaged"}); err == nil || m.ID != "" {
				t.Fatalf("damaged installed E schema fell open: %+v, %v", m, err)
			}
			threadCounts(t, s, map[string]int{"messages": 0, "deliveries": 0, "message_log": 0})
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
