package store

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
)

func enableEOnlyQuoteFixture(t *testing.T) {
	t.Helper()
	registry := migrations
	t.Cleanup(func() { migrations = registry })
	if i := slices.Index(registry, messageQuoteSchema); i >= 0 {
		migrations = registry[:i]
	}
	enableThreadRoutingFixture(t)
}

func TestThreadRoutingEOnlyQuoteSources(t *testing.T) {
	enableEOnlyQuoteFixture(t)
	f := chatSetup(t, "user", false)
	admitted, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "生成文本交付", "admission")
	if err != nil {
		t.Fatal(err)
	}
	notice, err := f.s.GetMessage(ctx, f.groupID, admitted.NoticeMessageID)
	if err != nil || notice.SourceKind != "platform" || notice.SenderID != f.a {
		t.Fatalf("fixture needs platform provenance despite an active member ID: %+v, %v", notice, err)
	}
	for _, tc := range []struct{ name, id string }{{"user", f.candidate.MessageID}, {"platform", notice.ID}} {
		t.Run(tc.name, func(t *testing.T) {
			r := MessageRequest{GroupID: f.groupID, ActorID: "user", Content: "再核对一次", IdempotencyKey: tc.name,
				ThreadLinks: domain.ThreadLinks{ReplyToMessageID: tc.id}}
			m := postThreadMessage(t, f.s, r)
			if m.TopicID != f.scope.TopicID || m.ReplyToMessageID != tc.id || m.ReplyTo != nil || len(m.Deliveries) != 0 {
				t.Fatalf("E-only quote should persist the ID and stay public: %+v", m)
			}
			if stored, err := f.s.GetMessage(ctx, f.groupID, m.ID); err != nil || !reflect.DeepEqual(stored, m) {
				t.Fatalf("E-only detail lost its reference: %+v, %v", stored, err)
			}
			if replay := postThreadMessage(t, f.s, r); !reflect.DeepEqual(replay, m) {
				t.Fatalf("E-only replay lost its reference: %+v", replay)
			}
		})
	}
}

func TestThreadRoutingQuoteUpgradeAndTaskLink(t *testing.T) {
	enableEOnlyQuoteFixture(t)
	f := setupMessages(t)
	root := postThreadMessage(t, f.s, MessageRequest{GroupID: f.groupID, ActorID: "user",
		Content: "New topic: QA-416 Catalog rollback", RecipientIDs: []string{}})
	original, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: root.TopicID, ActorID: f.b, Content: "Previous result"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := MessageRequest{GroupID: f.groupID, ActorID: "user", Content: "Check that again", IdempotencyKey: "legacy-E",
		ThreadLinks: domain.ThreadLinks{ReplyToMessageID: original.ID}}
	legacy := postThreadMessage(t, f.s, r)
	if root.TopicID == "" || legacy.TopicID != root.TopicID || legacy.ReplyToMessageID != original.ID || legacy.ReplyTo != nil ||
		len(legacy.Deliveries) != 1 || legacy.Deliveries[0].RecipientID != f.b {
		t.Fatalf("E-only reply lost its raw reference or author routing: %+v", legacy)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}

	migrations = append(slices.Clone(migrations), messageQuoteSchema)
	s := queueHandle(t, f.path, false)
	if replay := postThreadMessage(t, s, r); !reflect.DeepEqual(replay, legacy) {
		t.Fatalf("installing A changed the E-only receipt: %+v, want %+v", replay, legacy)
	}
	if q, err := s.ReadDeliveryQuote(ctx, f.groupID, legacy.Deliveries[0].ID); err != nil || q != nil {
		t.Fatalf("the legacy E edge is not an A delivery quote: %+v, %v", q, err)
	}
	work, err := s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{
		TopicID: root.TopicID, MessageID: root.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "existing work fixture",
		NewTask: &domain.NewTaskRequest{Title: "Check rollback", Acceptance: "Record the result", AssigneeID: f.b},
	}, "admission")
	if err != nil {
		t.Fatal(err)
	}
	r.IdempotencyKey, r.RelatedTaskID, r.ReplyToMessageID = "A-and-E", work.Task.ID, root.ID
	m := postThreadMessage(t, s, r)
	if m.TopicID != root.TopicID || m.ThreadLinks != r.ThreadLinks || m.ReplyTo == nil || m.ReplyTo.MessageID != root.ID {
		t.Fatalf("quote and independent Task link did not compose: %+v", m)
	}
	if len(m.Deliveries) != 0 {
		t.Errorf("a direct quote of user speech must not wake the Task owner: %+v", m.Deliveries)
	}
	if replay := postThreadMessage(t, s, r); !reflect.DeepEqual(replay, m) {
		t.Fatalf("quote and Task link changed on replay: %+v", replay)
	}
	var quoteID, taskID string
	var taskOnly bool
	if err := s.db.QueryRow(`SELECT q.quoted_message_id,l.task_id,l.reply_to_message_id IS NULL AND l.reply_to_topic_id IS NULL
	 FROM message_quotes q JOIN thread_routing_links l ON (l.message_id,l.group_id,l.topic_id)=(q.message_id,q.group_id,q.topic_id)
	 WHERE q.group_id=? AND q.message_id=?`, f.groupID, m.ID).Scan(&quoteID, &taskID, &taskOnly); err != nil ||
		quoteID != root.ID || taskID != work.Task.ID || !taskOnly {
		t.Fatalf("second reply edge or missing Task link: quote=%s task=%s task_only=%t err=%v", quoteID, taskID, taskOnly, err)
	}
	for _, tc := range []struct {
		name, content, wantRecipient string
		recipients                   []string
	}{
		{"comparison", "引用仅作对照。", f.b, nil},
		{"explicit_recipient", r.Content, f.a, []string{f.a}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := r
			request.IdempotencyKey, request.Content, request.RecipientIDs = tc.name, tc.content, tc.recipients
			got := postThreadMessage(t, s, request)
			if got.TopicID != root.TopicID || got.ThreadLinks != r.ThreadLinks || len(got.Deliveries) != 1 || got.Deliveries[0].RecipientID != tc.wantRecipient {
				t.Fatalf("quote and Task link lost authorized routing to %q: %+v", tc.wantRecipient, got)
			}
		})
	}
	threadCounts(t, s, map[string]int{"message_quotes": 3, "thread_routing_links": 4, "tasks": 1, "mandates": 1})
}

func TestThreadRoutingReplaysLegacyA3Quote(t *testing.T) {
	enableThreadRoutingFixture(t)
	if !slices.Contains(migrations, messageQuoteSchema) {
		original := migrations
		t.Cleanup(func() { migrations = original })
		migrations = append(slices.Clone(migrations), messageQuoteSchema)
	}
	f := setupMessages(t)
	original, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: "old-topic", ActorID: f.b, Content: "旧原文"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := MessageRequest{GroupID: f.groupID, ActorID: "user", Content: "  保留原请求\n", IdempotencyKey: "old-A3",
		ThreadLinks: domain.ThreadLinks{ReplyToMessageID: original.ID}}
	// A3 used a quote-only fingerprint and did not infer an omitted topic.
	proof := mutationPayload(r.GroupID, "", r.Content, "omitted/quote", original.ID)
	tx, id, _, err := f.s.beginMutation(ctx, r.scope(), proof)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	quote, _, err := resolveMessageQuoteInTx(ctx, tx, r, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertRequestedMessageInTx(ctx, tx, r, id, "", []string{f.b}, quote); err != nil {
		t.Fatal(err)
	}
	source, err := messageCommandSourceInTx(ctx, tx, id, "local_user_chat", proof, r.IdempotencyKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	m := postThreadMessage(t, f.s, r)
	if m.ID != id || m.SourceID != source || m.SourceKind != "local_user_chat" || m.TopicID != "" || m.Content != r.Content ||
		m.ReplyToMessageID != original.ID || m.ReplyTo == nil || m.ReplyTo.TopicID != original.TopicID ||
		len(m.Deliveries) != 1 || m.Deliveries[0].RecipientID != f.b {
		t.Fatalf("A3 receipt was rerouted, lost its source or lost its quote: %+v", m)
	}
	if got, found, err := f.s.LookupMessageReceipt(ctx, r); err != nil || !found || !reflect.DeepEqual(got, m) {
		t.Fatalf("lookup lost the A3 receipt: %+v, found=%t, err=%v", got, found, err)
	}
	r.RecipientIDs = []string{} // Omission and public-only are distinct requests.
	if _, err := f.s.PostLocalUserMessage(ctx, r); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed recipients matched legacy quote: %v", err)
	}
	var savedProof, savedKey []byte
	if err := f.s.db.QueryRow(`SELECT s.payload,i.payload FROM message_command_sources s JOIN mutation_idempotency i
	 ON i.record_id=s.message_id WHERE s.id=?`, source).Scan(&savedProof, &savedKey); err != nil ||
		!bytes.Equal(savedProof, proof) || !bytes.Equal(savedKey, proof) {
		t.Fatalf("A3 source or key bytes changed: %v", err)
	}
	threadCounts(t, f.s, map[string]int{"messages": 2, "deliveries": 1, "message_quotes": 1, "thread_routing_links": 0})
}
