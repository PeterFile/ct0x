package store

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// F supplies the append only; use B's registration if it has landed.
func agentRunExplanationTestMigrations() []string {
	if slices.Contains(migrations, agentRunExplanationSchema) {
		return migrations
	}
	return append(slices.Clone(migrations), agentRunExplanationSchema)
}

func agentRunExplanationFixture(t *testing.T) (messageFixture, MemberToolScope) {
	t.Helper()
	original := migrations
	migrations = agentRunExplanationTestMigrations()
	t.Cleanup(func() { migrations = original })
	return memberOperationFixture(t)
}

func TestAgentRunPublicExplanationExactFact(t *testing.T) {
	f, scope := agentRunExplanationFixture(t)
	s, claim := f.s, scope.Claim
	// An ordinary index must not disable publication or timeline readback.
	coordinationDecideExec(t, s, `CREATE INDEX main.agent_run_explanations_created_at ON agent_run_explanations(created_at)`)
	const candidate = "PRIVATE unconfirmed response, not an explanation"
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
	queueEvent(t, s, claim, provider.ResponseEvent, candidate, "")
	original, err := s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, ListOptions{})
	if err != nil || original.Receipt != nil || len(original.Timeline.Items) != 1 || original.Timeline.Items[0].Kind != "trigger_message" {
		t.Fatalf("a native response was projected as public or as a receipt: %+v, %v", original, err)
	}
	before := messageSequenceSnapshot(t, s)
	delete(before, "agent_run_explanations")
	const key = "private-original-key"
	const maxBytes = AgentRunExplanationMaxBytes
	text := " \n" + strings.Repeat("说", (maxBytes-2)/3) + strings.Repeat("!", (maxBytes-2)%3)
	for _, forge := range []func(*MemberToolScope){
		func(b *MemberToolScope) { *b = MemberToolScope{} },
		func(b *MemberToolScope) { b.Claim.Token = "foreign-token" },
		func(b *MemberToolScope) { b.Claim.TurnID = "other-turn" },
		func(b *MemberToolScope) { b.Claim.GroupID = f.otherID },
		func(b *MemberToolScope) { b.MemberID = f.b },
		func(b *MemberToolScope) { b.ConversationID = "other-conversation" },
		func(b *MemberToolScope) { b.TopicID = "another-topic" },
	} {
		bad := scope
		forge(&bad)
		got, err := s.PublishAgentRunExplanation(ctx, bad, text, key)
		requestDispatchZero(t, got, err, ErrMemberToolDenied)
	}
	for _, bad := range []string{" \n\t", "\xff", "bad\x00text", text + "!"} {
		got, err := s.PublishAgentRunExplanation(ctx, scope, bad, key)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	for _, bad := range []string{"", "a,b", strings.Repeat("k", 129)} {
		got, err := s.PublishAgentRunExplanation(ctx, scope, text, bad)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	published, err := s.PublishAgentRunExplanation(ctx, scope, text, key)
	if err != nil || published.Kind != "public_explanation" || published.ID != "public_explanation:"+claim.TurnID ||
		published.PublicExplanation == nil || published.PublicExplanation.Text != text || published.CreatedAt.IsZero() {
		t.Fatalf("publication did not preserve the explicit text and stable identity: %+v, %v", published, err)
	}
	// Reopen a second handle with deferred transactions, not an in-process cache.
	observer := queueHandle(t, f.path, true)
	replay, err := observer.PublishAgentRunExplanation(ctx, scope, text, key)
	if err != nil || !reflect.DeepEqual(replay, published) {
		t.Fatalf("original-key replay returned a different fact: %+v, %v", replay, err)
	}
	got, err := observer.PublishAgentRunExplanation(ctx, scope, text, "new-key")
	requestDispatchZero(t, got, err, ErrConflict)
	got, err = observer.PublishAgentRunExplanation(ctx, scope, "不同说明", key)
	requestDispatchZero(t, got, err, ErrConflict)
	messageSequencePreserved(t, observer, before) // Includes chat, generic keys, Turn and resource claims.

	digest := sha256.Sum256([]byte(claim.Token))
	var rows, exact int
	if err := s.db.QueryRow(`SELECT count(*),count(CASE WHEN (id,group_id,member_id,conversation_id,topic_id,claim_digest,idempotency_key,text)=(?,?,?,?,?,?,?,?) THEN 1 END)
	 FROM agent_run_explanations`, claim.TurnID, claim.GroupID, scope.MemberID, scope.ConversationID, scope.TopicID, digest[:], key, text).Scan(&rows, &exact); err != nil || rows != 1 || exact != 1 {
		t.Fatalf("expected exactly the bound source and its one key: rows=%d exact=%d err=%v", rows, exact, err)
	}
	for _, change := range []string{
		"UPDATE agent_run_explanations SET text='changed'",
		"DELETE FROM agent_run_explanations",
		"INSERT OR REPLACE INTO agent_run_explanations SELECT * FROM agent_run_explanations",
	} {
		if _, err := s.db.Exec(change); err == nil {
			t.Fatalf("immutable explanation allowed: %s", change)
		}
	}

	// Force a cross-kind tie so the cursor must use the stable entry ID as well as time.
	trigger := original.Timeline.Items[0]
	coordinationDecideExec(t, s, "UPDATE messages SET created_at=? WHERE id=?", published.CreatedAt.Format(timestampLayout), trigger.Message.MessageID)
	trigger.CreatedAt = published.CreatedAt
	queueEvent(t, s, claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
	receipt := queueReceipt(provider.OutcomeSucceeded)
	receipt.OutputComplete = false
	if _, err := s.ApplyTurnReceipt(ctx, claim, receipt); err != nil {
		t.Fatal(err)
	}
	got, err = observer.PublishAgentRunExplanation(ctx, scope, text, key)
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
	if _, err := s.GetAgentRun(ctx, f.otherID, claim.TurnID, ListOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-group Run read: %v", err)
	}
	page, err := s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, ListOptions{Limit: 1})
	if err != nil || page.Run.State != domain.TurnStateUnknown || page.Receipt == nil || page.Receipt.Outcome != domain.TurnStateUnknown ||
		page.Receipt.ObservedTerminal != domain.TurnStateSucceeded || page.Receipt.OutputComplete || len(page.Timeline.Items) != 1 ||
		!reflect.DeepEqual(page.Timeline.Items[0], published) || page.Timeline.NextCursor == nil ||
		*page.Timeline.NextCursor != (Cursor{CreatedAt: published.CreatedAt, ID: published.ID}) {
		t.Fatalf("public explanation and unconfirmed receipt must coexist: %+v, %v", page, err)
	}
	data, err := json.Marshal(page.Timeline.Items[0])
	var fields map[string]json.RawMessage
	var statement map[string]string
	if err != nil || json.Unmarshal(data, &fields) != nil || len(fields) != 4 ||
		json.Unmarshal(fields["public_explanation"], &statement) != nil || len(statement) != 1 || statement["text"] != text ||
		strings.Contains(string(data), key) || strings.Contains(string(data), claim.Token) || strings.Contains(string(data), candidate) {
		t.Fatalf("public DTO exposed more than the exact statement and event metadata: %s, %v", data, err)
	}
	tail, err := s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, ListOptions{Limit: 1, Before: page.Timeline.NextCursor})
	if err != nil || len(tail.Timeline.Items) != 1 || !reflect.DeepEqual(tail.Timeline.Items[0], trigger) || tail.Timeline.NextCursor != nil {
		t.Fatalf("explanation cursor lost or duplicated the tied trigger: %+v, %v", tail, err)
	}
}

func TestAgentRunPublicExplanationKeyCannotReplayAcrossRuns(t *testing.T) {
	f, scope := agentRunExplanationFixture(t)
	s, claim := f.s, scope.Claim
	const text, key = "同一段公开说明。", "one-run-key"
	if _, err := s.PublishAgentRunExplanation(ctx, scope, text, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); err != nil {
		t.Fatal(err)
	}
	d := queueDelivery(t, s, claim.GroupID, scope.MemberID, scope.TopicID)
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: claim.GroupID, DeliveryID: d.ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.BindMemberTool(ctx, TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.PublishAgentRunExplanation(ctx, other, text, key)
	requestDispatchZero(t, got, err, ErrConflict)
	view, err := s.GetAgentRun(ctx, turn.GroupID, turn.ID, ListOptions{})
	if err != nil || len(view.Timeline.Items) != 1 || view.Timeline.Items[0].Kind != "trigger_message" {
		t.Fatalf("other Turn borrowed an explanation: %+v, %v", view, err)
	}
	if got, err := s.PublishAgentRunExplanation(ctx, other, text, "its-own-key"); err != nil || got.ID != "public_explanation:"+turn.ID {
		t.Fatalf("new Turn could not publish independently: %+v, %v", got, err)
	}
}

func TestAgentRunPublicExplanationCommitUnconfirmed(t *testing.T) {
	f, scope := agentRunExplanationFixture(t)
	wrapped, observed := coordinationDecideWrapped(t, f.path, "ack-error", errors.New("PRIVATE commit acknowledgement"))
	const text, key = "保存结果需要对账。", "keep-original-key"
	got, err := wrapped.PublishAgentRunExplanation(ctx, scope, text, key)
	requestDispatchZero(t, got, err, ErrAgentRunExplanationUnknown)
	if len(observed.begins) != 1 || len(observed.queries) == 0 || !strings.HasPrefix(observed.queries[0], "UPDATE turns SET claim_token=claim_token") {
		t.Fatal("publication did not begin its single transaction with the shared live-scope fence")
	}
	if strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("commit error exposed diagnostics: %v", err)
	}
	claim := scope.Claim
	view, err := f.s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, ListOptions{Limit: 1})
	if err != nil || len(view.Timeline.Items) != 1 || view.Timeline.Items[0].Kind != "public_explanation" {
		t.Fatalf("the real commit was not visible for reconciliation: %+v, %v", view, err)
	}
	got, err = f.s.PublishAgentRunExplanation(ctx, scope, text, key)
	if err != nil || !reflect.DeepEqual(got, view.Timeline.Items[0]) {
		t.Fatalf("original key did not reconcile the committed statement: %+v, %v", got, err)
	}
}

func TestAgentRunPublicExplanationPartialSchema(t *testing.T) {
	for _, tc := range []struct {
		name, change string
	}{
		{"missing_trigger", `DROP TRIGGER agent_run_explanations_no_delete`},
		{"missing_column", `ALTER TABLE agent_run_explanations DROP COLUMN text`},
		{"missing_constraint", `DROP TABLE agent_run_explanations;` +
			strings.Replace(agentRunExplanationSchema, "    UNIQUE(group_id,member_id,idempotency_key),\n", "", 1)},
		{"extra_unique_index", `CREATE UNIQUE INDEX main.agent_run_explanations_extra_unique ON agent_run_explanations(id)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, scope := agentRunExplanationFixture(t)
			s, claim := f.s, scope.Claim
			before, err := s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, ListOptions{})
			if err != nil || len(before.Timeline.Items) != 1 || before.Timeline.Items[0].Kind != "trigger_message" {
				t.Fatalf("read ordinary Run history: %+v, %v", before, err)
			}
			const text, key = "Public explanation", "partial-schema"
			if _, err := s.PublishAgentRunExplanation(ctx, scope, text, key); err != nil {
				t.Fatal(err)
			}
			coordinationDecideExec(t, s, tc.change)
			if supported, err := s.AgentRunExplanationSupported(ctx); err != nil || supported {
				t.Errorf("partial schema advertised explanation support: %t, %v", supported, err)
			}
			if now, err := s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, ListOptions{}); err != nil || !reflect.DeepEqual(now, before) {
				t.Errorf("partial schema changed ordinary Run history: %+v, %v", now, err)
			}
			got, err := s.PublishAgentRunExplanation(ctx, scope, text, key)
			requestDispatchZero(t, got, err, ErrAgentRunExplanationUnavailable)
		})
	}
}

func TestAgentRunPublicExplanationV30Compatibility(t *testing.T) {
	original := migrations
	landing := agentRunExplanationTestMigrations()
	migrations = original[:30]
	defer func() { migrations = original }()
	f, scope := memberOperationFixture(t)
	s, claim := f.s, scope.Claim
	messageSequenceLedger(t, s, 30)
	if supported, err := s.AgentRunExplanationSupported(ctx); err != nil || supported {
		t.Fatalf("v30 advertised an absent table: %t, %v", supported, err)
	}
	before, err := s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, ListOptions{})
	if err != nil || len(before.Timeline.Items) != 1 || before.Timeline.Items[0].Kind != "trigger_message" {
		t.Fatalf("v30 safe history: %+v, %v", before, err)
	}
	oldRows := messageSequenceSnapshot(t, s)
	got, err := s.PublishAgentRunExplanation(ctx, scope, "公开说明", "version-check")
	requestDispatchZero(t, got, err, ErrAgentRunExplanationUnavailable)

	migrations = landing
	if err := migrate(s.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, s, len(landing))
	messageSequencePreserved(t, s, oldRows)
	if supported, err := s.AgentRunExplanationSupported(ctx); err != nil || !supported {
		t.Fatalf("actual registered table not detected: %t, %v", supported, err)
	}
	if now, err := s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, ListOptions{}); err != nil || !reflect.DeepEqual(now, before) {
		t.Fatalf("append changed or backfilled old Run history: %+v, %v", now, err)
	}
	if got, err := s.PublishAgentRunExplanation(ctx, scope, "公开说明", "version-check"); err != nil || got.PublicExplanation == nil {
		t.Fatalf("append did not enable an explicit publication: %+v, %v", got, err)
	}
	dispatchAssertFK(t, s)
	var withoutRowID bool
	if err := s.db.QueryRow(`SELECT wr FROM pragma_table_list('agent_run_explanations')
	 WHERE schema='main' AND type='table'`).Scan(&withoutRowID); err != nil || !withoutRowID {
		t.Fatalf("explanation table is not WITHOUT ROWID: %t, %v", withoutRowID, err)
	}
}
