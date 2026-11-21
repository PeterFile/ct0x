package store

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func enableThreadCorrectionFixture(t *testing.T) {
	t.Helper()
	enableThreadRoutingFixture(t)
	if !slices.Contains(migrations, threadCorrectionSchema) {
		original := migrations
		migrations = append(slices.Clone(original), threadCorrectionSchema)
		t.Cleanup(func() { migrations = original })
	}
}

func correctionPublicPost(t *testing.T, s *Store, group, key, content string) domain.Message {
	t.Helper()
	return postThreadMessage(t, s, MessageRequest{ActorID: "user", GroupID: group, IdempotencyKey: key, Content: content, RecipientIDs: []string{}})
}

func TestThreadCorrectionBoundHistoryAndUnchangedActiveSource(t *testing.T) {
	enableThreadCorrectionFixture(t)
	f := setupMessages(t)
	target := correctionPublicPost(t, f.s, f.groupID, "target", "新议题：SQL-74 缓存淘汰告警")
	original := postThreadMessage(t, f.s, MessageRequest{ActorID: "user", GroupID: f.groupID, IdempotencyKey: "original",
		Content: "unique-marker 原文", RecipientIDs: []string{f.a}})
	bind := func(d domain.Delivery, group, topic string) MemberToolScope {
		t.Helper()
		turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: group, DeliveryID: d.ID, MaxActive: 4})
		if err != nil {
			t.Fatal(err)
		}
		scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: group, TurnID: turn.ID, Token: turn.ClaimToken})
		if err != nil || scope.TopicID != topic {
			t.Fatalf("wrong claim binding: %+v, %v", scope, err)
		}
		return scope
	}
	active := bind(original.Deliveries[0], f.groupID, "")
	before, err := f.s.CaptureChatCandidate(ctx, active)
	if err != nil || before.SourceID != original.SourceID || before.SourceKind != "local_user_chat" {
		t.Fatalf("original source not bound: %+v, %v", before, err)
	}
	turn := queueTurn(t, f.s, active.Claim)
	command := postThreadMessage(t, f.s, MessageRequest{ActorID: "user", GroupID: f.groupID, IdempotencyKey: "correct",
		Content: "这句话指的是 SQL-74"})
	if len(command.Deliveries) != 0 {
		t.Fatalf("correction issued a delivery: %+v", command.Deliveries)
	}
	if got, err := f.s.CaptureChatCandidate(ctx, active); err != nil || !reflect.DeepEqual(got, before) || !reflect.DeepEqual(queueTurn(t, f.s, active.Claim), turn) {
		t.Fatalf("correction rebound the active Turn: %+v, %v", got, err)
	}
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stale, staleErr := chatSourceSupersededInTx(ctx, tx, before.ThreadRef, before.Generation)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if staleErr != nil || stale {
		t.Fatalf("metadata-only correction was taken as a new work command: %t, %v", stale, staleErr)
	}
	moved, err := f.s.MessageThreadAttribution(ctx, f.groupID, original.ID)
	if err != nil || moved.Storage.TopicID != "" || moved.Effective.TopicID != target.TopicID {
		t.Fatalf("original attribution was not changed independently: %+v, %v", moved, err)
	}
	newScope := bind(queueDelivery(t, f.s, f.groupID, f.b, target.TopicID), f.groupID, target.TopicID)
	foreign := bind(queueDelivery(t, f.s, f.otherID, member(t, f.s, f.otherID).ID, target.TopicID), f.otherID, target.TopicID)
	for _, tc := range []struct {
		name  string
		scope MemberToolScope
		allow bool
	}{{"old topic", active, false}, {"effective topic", newScope, true}, {"foreign group with same topic id", foreign, false}} {
		t.Run(tc.name, func(t *testing.T) {
			read, err := f.s.ReadHistoryForTool(ctx, tc.scope, MemberHistoryReadInput{MessageID: original.ID})
			if tc.allow && (err != nil || read.Content != original.Content) || !tc.allow && !errors.Is(err, ErrNotFound) {
				t.Fatalf("tool read scope: %+v, %v", read, err)
			}
			search, err := f.s.SearchHistoryForTool(ctx, tc.scope, MemberHistorySearchInput{Query: "unique-marker"})
			if err != nil || (len(search.Items) != 0) != tc.allow || tc.allow && (len(search.Items) != 1 || search.Items[0].MessageID != original.ID) {
				t.Fatalf("tool search scope: %+v, %v", search, err)
			}
			page, err := f.s.ReadChannelForTool(ctx, tc.scope, ListOptions{})
			if err != nil || slices.ContainsFunc(page.Items, func(m domain.Message) bool { return m.ID == original.ID }) != tc.allow {
				t.Fatalf("tool channel scope: %+v, %v", page, err)
			}
			group, topic := tc.scope.Claim.GroupID, tc.scope.TopicID
			detail, err := f.s.ReadHistory(ctx, group, &topic, original.ID)
			if tc.allow && (err != nil || detail.SourceID != original.SourceID || detail.TopicID != "") || !tc.allow && !errors.Is(err, ErrNotFound) {
				t.Fatalf("local scoped read must filter by effective topic without changing raw proof: %+v, %v", detail, err)
			}
			local, err := f.s.SearchHistory(ctx, group, HistoryQuery{Query: "unique-marker", TopicID: &topic})
			if err != nil || (len(local.Items) != 0) != tc.allow || tc.allow && len(local.Items) != 1 {
				t.Fatalf("local search scope: %+v, %v", local, err)
			}
		})
	}
	threadCounts(t, f.s, map[string]int{"deliveries": 3, "mandates": 0, "tasks": 0, "message_command_sources": 2, "message_log": 5})
}

func TestThreadCorrectionConcurrentAppendAndReplay(t *testing.T) {
	enableThreadCorrectionFixture(t)
	f := setupMessages(t)
	a := correctionPublicPost(t, f.s, f.groupID, "a", "新话题：OPS-81 备份告警")
	b := correctionPublicPost(t, f.s, f.groupID, "b", "新话题：OPS-82 索引告警")
	original := correctionPublicPost(t, f.s, f.groupID, "original", "marker")
	peer := queueHandle(t, f.path, false)
	requests := []MessageRequest{
		{ActorID: "user", GroupID: f.groupID, Content: "这句话指的是 OPS-81", IdempotencyKey: "to-a", ThreadLinks: domain.ThreadLinks{ReplyToMessageID: original.ID}},
		{ActorID: "user", GroupID: f.groupID, Content: "这句话指的是 OPS-82", IdempotencyKey: "to-b", ThreadLinks: domain.ThreadLinks{ReplyToMessageID: original.ID}},
	}
	type result struct {
		i   int
		m   domain.Message
		err error
	}
	start, results := make(chan struct{}), make(chan result, 6)
	for i := range 6 {
		go func() {
			<-start
			index := (i / 2) % 2
			m, err := []*Store{f.s, peer}[i%2].PostLocalUserMessage(ctx, requests[index])
			results <- result{index, m, err}
		}()
	}
	close(start)
	var receipts [2]domain.Message
	for range 6 {
		got := <-results
		if got.err != nil || len(got.m.Deliveries) != 0 {
			t.Errorf("concurrent correction returned work or failed: %+v, %v", got.m, got.err)
		}
		if prev := receipts[got.i]; prev.ID != "" && !reflect.DeepEqual(prev, got.m) {
			t.Errorf("same key returned different receipts: %+v / %+v", prev, got.m)
		}
		receipts[got.i] = got.m
	}
	if t.Failed() {
		return
	}
	var last string
	rows, err := f.s.db.Query(`SELECT command_message_id,original_topic_id,previous_topic_id,effective_topic_id
	 FROM thread_corrections WHERE group_id=? AND target_message_id=? ORDER BY sequence`, f.groupID, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	expected := map[string]string{receipts[0].ID: a.TopicID, receipts[1].ID: b.TopicID}
	for rows.Next() {
		var command, raw, previous, next string
		if err := rows.Scan(&command, &raw, &previous, &next); err != nil || raw != "" || previous != last || next == "" || next != expected[command] {
			t.Fatalf("not a serialized correction chain: command=%q raw=%q previous=%q next=%q (%v)", command, raw, previous, next, err)
		}
		delete(expected, command)
		last = next
	}
	if err := rows.Err(); err != nil || len(expected) != 0 {
		t.Fatalf("missing/duplicate correction rows: %v, %v", expected, err)
	}
	later := correctionPublicPost(t, f.s, f.groupID, "later", "new distinct source")
	for i, r := range requests {
		if got := postThreadMessage(t, peer, r); !reflect.DeepEqual(got, receipts[i]) {
			t.Fatalf("explicit replay changed durable receipt: %+v", got)
		}
	}
	attr, err := f.s.MessageThreadAttribution(ctx, f.groupID, original.ID)
	if err != nil || attr.Storage.TopicID != "" || attr.Effective.TopicID != last {
		t.Fatalf("replay reordered the chain: %+v, %v", attr, err)
	}
	if attr, err := f.s.MessageThreadAttribution(ctx, f.groupID, later.ID); err != nil || attr.Effective.TopicID != "" {
		t.Fatalf("replay updated a different message: %+v, %v", attr, err)
	}
	threadCounts(t, f.s, map[string]int{"messages": 6, "thread_corrections": 2, "message_command_sources": 4, "deliveries": 0, "mandates": 0, "tasks": 0})
}

func TestThreadCorrectionLegacyAttemptLocksGenericHistoryToStorage(t *testing.T) {
	enableThreadCorrectionFixture(t)
	f, scope, _, attempt := taskAttemptAgent(t)
	anchor := postThreadMessage(t, f.s, MessageRequest{GroupID: f.groupID, ActorID: "user", TopicID: scope.TopicID,
		Content: "legacy work topic", RecipientIDs: []string{}})
	if _, err := f.s.db.Exec(`INSERT INTO thread_routing_topics(group_id,topic_id,title,title_key,issue_id,origin_message_id)
	 VALUES(?,?,'版本交付','版本交付','REL-56',?)`, f.groupID, anchor.TopicID, anchor.ID); err != nil {
		t.Fatal(err)
	}
	source := correctionPublicPost(t, f.s, f.groupID, "source", "需要记录的跨话题原文")
	correctionPublicPost(t, f.s, f.groupID, "correct", "这句话指的是 REL-56")
	input := MemberHistoryReadInput{MessageID: source.ID}
	if got, err := f.s.ReadHistoryForTool(ctx, scope, input); err != nil || got.Content != source.Content {
		t.Fatalf("ordinary claim must see corrected input before it is bound to work: %+v, %v", got, err)
	}
	if _, err := f.s.ActivateTaskAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.ReadHistoryForTool(ctx, scope, input); !errors.Is(err, ErrNotFound) || got.MessageID != "" {
		t.Fatalf("same claim retained effective reads after durable attempt binding: %+v, %v", got, err)
	}
	if got, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: anchor.ID}); err != nil || got.Content != anchor.Content {
		t.Fatalf("attempt lost its storage topic history: %+v, %v", got, err)
	}
}

func TestThreadCorrectionDoesNotGrantFrozenInvestigationReads(t *testing.T) {
	enableThreadCorrectionFixture(t)
	f, policy, role := investigationUserWorker(t)
	if !policy.Investigation.AllowTopicMessages || role.Member.TopicID == "" {
		t.Fatal("fixture requires a typed Turn that may read its original topic")
	}
	origin := postThreadMessage(t, f.s, MessageRequest{ActorID: "user", GroupID: f.groupID, TopicID: role.Member.TopicID,
		Content: "topic origin", RecipientIDs: []string{}})
	if _, err := f.s.db.Exec(`INSERT INTO thread_routing_topics(group_id,topic_id,title,title_key,issue_id,origin_message_id)
	 VALUES(?,?, '调查归档资料','调查归档资料','SEC-41',?)`, f.groupID, role.Member.TopicID, origin.ID); err != nil {
		t.Fatal(err)
	}
	source := correctionPublicPost(t, f.s, f.groupID, "public-source", "正文：不同原始话题")
	correctionPublicPost(t, f.s, f.groupID, "correct", "这句话指的是 SEC-41")
	if attr, err := f.s.MessageThreadAttribution(ctx, f.groupID, source.ID); err != nil || attr.Effective.TopicID != role.Member.TopicID {
		t.Fatalf("expected live correction in same group: %+v %v", attr, err)
	}
	page, err := f.s.ReadInvestigationChannel(ctx, role, ListOptions{})
	if err != nil || !slices.ContainsFunc(page.Items, func(m domain.Message) bool { return m.ID == origin.ID }) ||
		slices.ContainsFunc(page.Items, func(m domain.Message) bool { return m.ID == source.ID }) {
		t.Fatalf("typed scope was widened by a topical overlay: %+v, %v", page, err)
	}
	// Even an internal call to the shared generic reader uses storage scope.
	if got, err := f.s.ReadHistoryForTool(ctx, role.Member, MemberHistoryReadInput{MessageID: source.ID}); !errors.Is(err, ErrNotFound) || got.MessageID != "" {
		t.Fatalf("typed Turn gained ordinary effective history: %+v, %v", got, err)
	}
	artifact := mustPublishArtifact(t, f.s, artifactPublishInput(f.groupID))
	if _, _, err := f.s.ReadInvestigationArtifact(ctx, role, artifact.ArtifactID, artifact.ID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("correction granted formal material access: %v", err)
	}
}

func TestThreadCorrectionNoImplicitJumpOverInterveningInput(t *testing.T) {
	enableThreadCorrectionFixture(t)
	f := setupMessages(t)
	root := correctionPublicPost(t, f.s, f.groupID, "root", "新话题：QOS-41 带宽告警")
	original := correctionPublicPost(t, f.s, f.groupID, "source", "建议每小时统计一次。")
	if _, err := f.s.CreateMessage(ctx, MessageRequest{ActorID: f.a, GroupID: f.groupID, Content: "收到。"}, nil); err != nil {
		t.Fatal(err)
	}
	first := postThreadMessage(t, f.s, MessageRequest{ActorID: "user", GroupID: f.groupID, Content: "这句话指的是 QOS-41", IdempotencyKey: "interleaved"})
	if first.TopicID != "" || len(first.Deliveries) != 0 || first.ThreadCorrection == nil || first.ThreadCorrection.Question == "" {
		t.Fatalf("interleaved implicit correction guessed a route: %+v", first)
	}
	explicit := MessageRequest{ActorID: "user", GroupID: f.groupID, Content: "这句话指的是 QOS-41", ThreadLinks: domain.ThreadLinks{ReplyToMessageID: original.ID}}
	postThreadMessage(t, f.s, explicit)
	attr, err := f.s.MessageThreadAttribution(ctx, f.groupID, original.ID)
	if err != nil || attr.Effective.TopicID != root.TopicID {
		t.Fatalf("verified reply did not recover the intended source: %+v, %v", attr, err)
	}
	// An apparent extra instruction is not a topic name and must stay inert.
	explicit.Content = "这句话指的是 QOS-41，请立刻执行。"
	m := postThreadMessage(t, f.s, explicit)
	if m.TopicID != "" || len(m.Deliveries) != 0 || !strings.Contains(m.Content, "请立刻执行") || m.ThreadCorrection == nil || m.ThreadCorrection.Question == "" {
		t.Fatalf("mixed input became new work or rewrote the user text: %+v", m)
	}
	threadCounts(t, f.s, map[string]int{"deliveries": 0, "tasks": 0, "mandates": 0})
}

func TestThreadCorrectionUnavailableKeepsOrdinaryRouting(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })
	for _, routed := range []bool{false, true} {
		migrations = original[:30]
		if routed {
			migrations = append(slices.Clone(migrations), threadRoutingSchema)
		}
		f := setupMessages(t)
		plain := postThreadMessage(t, f.s, MessageRequest{ActorID: "user", GroupID: f.groupID, IdempotencyKey: "plain", Content: "普通发言"})
		if plain.TopicID != "" || len(plain.Deliveries) != 1 {
			t.Fatalf("uninstalled correction changed ordinary chat: %+v", plain)
		}
		if _, err := f.s.PostLocalUserMessage(ctx, MessageRequest{ActorID: "user", GroupID: f.groupID, Content: "这句话指的是告警", IdempotencyKey: "inert"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unavailable correction must not turn into a work command: %v", err)
		}
		if attr, err := f.s.MessageThreadAttribution(ctx, f.groupID, plain.ID); err != nil || attr.Effective != attr.Storage || attr.CorrectionMessageID != "" {
			t.Fatalf("storage-only attribution fallback changed: %+v, %v", attr, err)
		}
		threadCounts(t, f.s, map[string]int{"messages": 1, "deliveries": 1, "message_command_sources": 1, "message_log": 1})
	}
}

func TestThreadCorrectionDamagedInstallRejectsOrdinarySend(t *testing.T) {
	enableThreadCorrectionFixture(t)
	version := slices.Index(migrations, threadCorrectionSchema) + 1
	for _, tc := range []struct {
		name, statement string
		args            []any
	}{
		{"missing_table", `DROP TABLE thread_corrections`, nil},
		{"same_columns_without_constraints", `DROP TABLE thread_corrections;
		 CREATE TABLE thread_corrections (sequence INTEGER,group_id TEXT,command_message_id TEXT,target_message_id TEXT,
		 original_topic_id TEXT,previous_topic_id TEXT,effective_topic_id TEXT,question TEXT)`, nil},
		{"missing_marker", `DELETE FROM schema_migrations WHERE version=?`, []any{version}},
		{"missing_index", `DROP INDEX thread_corrections_incoming`, nil},
		{"missing_insert_guard", `DROP TRIGGER thread_corrections_insert`, nil},
		{"missing_delete_guard", `DROP TRIGGER thread_corrections_delete`, nil},
		{"inert_update_guard", `DROP TRIGGER thread_corrections_update;
		 CREATE TRIGGER thread_corrections_update BEFORE UPDATE ON thread_corrections BEGIN SELECT 1; END`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupMessages(t)
			r := MessageRequest{GroupID: f.groupID, ActorID: "user", TopicID: "chosen-topic", TopicIDExplicit: true,
				Content: "普通发言，不带引用", IdempotencyKey: "sent", RecipientIDs: []string{f.a}}
			m := postThreadMessage(t, f.s, r)
			if _, err := f.s.db.Exec(tc.statement, tc.args...); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"sent", "fresh"} {
				r.IdempotencyKey = key
				if got, err := f.s.PostLocalUserMessage(ctx, r); err == nil || errors.Is(err, ErrInvalid) || got.ID != "" {
					t.Errorf("installed but broken correction schema allowed ordinary POST (%q): %+v (%v)", key, got, err)
				}
			}
			if got, err := f.s.MessageThreadAttribution(ctx, f.groupID, m.ID); err == nil {
				t.Errorf("damaged installation produced effective attribution: %+v", got)
			}
			threadCounts(t, f.s, map[string]int{"messages": 1, "message_log": 1, "deliveries": 1, "message_command_sources": 1, "mutation_idempotency": 1})
		})
	}
}

func TestThreadCorrectionReturnToOriginalAndImmutableAudit(t *testing.T) {
	enableThreadCorrectionFixture(t)
	f := setupMessages(t)
	a := correctionPublicPost(t, f.s, f.groupID, "a", "新议题：OPS-31 拉取告警")
	b := correctionPublicPost(t, f.s, f.groupID, "b", "新议题：OPS-32 写入告警")
	source := correctionPublicPost(t, f.s, f.groupID, "source", "OPS-31 marker")
	put := func(content string) domain.Message {
		t.Helper()
		return postThreadMessage(t, f.s, MessageRequest{ActorID: "user", GroupID: f.groupID, Content: content, ThreadLinks: domain.ThreadLinks{ReplyToMessageID: source.ID}})
	}
	out := put("这句话指的是 OPS-32")
	back := put("这句话指的是 OPS-31")
	noop := put("这句话指的是 OPS-31")
	if out.ThreadCorrection == nil || back.ThreadCorrection == nil || noop.ThreadCorrection == nil ||
		out.ThreadCorrection.PreviousTopicID != a.TopicID || out.ThreadCorrection.EffectiveTopicID != b.TopicID ||
		back.ThreadCorrection.PreviousTopicID != b.TopicID || back.ThreadCorrection.EffectiveTopicID != a.TopicID ||
		noop.ThreadCorrection.PreviousTopicID != a.TopicID {
		t.Fatalf("unexpected correction lineage: %+v / %+v / %+v", out.ThreadCorrection, back.ThreadCorrection, noop.ThreadCorrection)
	}
	attr, err := f.s.MessageThreadAttribution(ctx, f.groupID, source.ID)
	if err != nil || attr.Effective != attr.Storage || attr.CorrectionMessageID != back.ID {
		t.Fatalf("return to raw topic must still invalidate an old confirmation snapshot: %+v, %v", attr, err)
	}
	for i, topic := range []string{b.TopicID, a.TopicID} {
		page, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: "marker", TopicID: &topic, Limit: 1})
		if err != nil || page.NextCursor != "" || len(page.Items) != i || i == 1 && page.Items[0].ID != source.ID {
			t.Fatalf("stale incoming move or duplicate visible: %+v, %v", page, err)
		}
	}
	for _, query := range []string{
		`UPDATE thread_corrections SET question='' WHERE command_message_id=?`,
		`DELETE FROM thread_corrections WHERE command_message_id=?`,
		`INSERT OR REPLACE INTO thread_corrections SELECT * FROM thread_corrections WHERE command_message_id=?`,
	} {
		if _, err := f.s.db.Exec(query, out.ID); err == nil || !strings.Contains(err.Error(), "不可") {
			t.Fatalf("mutable correction proof: %q, %v", query, err)
		}
	}
	dispatchAssertFK(t, f.s)
}
