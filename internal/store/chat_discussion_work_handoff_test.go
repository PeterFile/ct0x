package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func checkDiscussionWorkMessage(t *testing.T, s *Store, groupID, messageID string, work map[string]string, closure string) domain.Message {
	t.Helper()
	m, err := s.GetMessage(ctx, groupID, messageID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Work map[string]string `json:"discussion_work"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || !reflect.DeepEqual(wire.Work, work) || m.DiscussionClosureReason != closure {
		t.Fatalf("message %s: work=%v, closure=%q, err=%v; want work=%v, closure=%q", messageID, wire.Work, m.DiscussionClosureReason, err, work, closure)
	}
	page, err := s.ListMessages(ctx, groupID, MessageListOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Items {
		if row.ID == messageID {
			if !reflect.DeepEqual(row, m) {
				t.Fatalf("page/detail mismatch for %s: work=%+v / %+v, reason=%q / %q", messageID,
					row.DiscussionWork, m.DiscussionWork, row.DiscussionClosureReason, m.DiscussionClosureReason)
			}
			return m
		}
	}
	t.Fatalf("timeline lost public message %s", messageID)
	return domain.Message{}
}

func checkDiscussionPendingProof(t *testing.T, s *Store, admitted domain.Message) {
	t.Helper()
	want := admitted
	want.DiscussionWork = nil
	if got := checkDiscussionWorkMessage(t, s, want.GroupID, want.ID, nil, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("pending proof changed the original invitation: %+v; want %+v", got, want)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	states, err := chatDiscussionEvidenceInTx(ctx, tx, want.GroupID, want.ID)
	if err != nil || states[want.ID] != (chatDiscussionEvidence{pendingProof: true}) {
		t.Fatalf("invitation head is not pending proof: %+v, %v", states[want.ID], err)
	}
}

func TestChatDiscussionWorkHandoffRead(t *testing.T) {
	f := chatSetup(t, "user", false)
	s := f.s
	investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
	s.EnableInvestigationExecution() // Enable B's test admission gate; no execution runtime.
	worker := member(t, s, f.groupID)
	if _, err := s.SetDispatchPolicy(ctx, f.groupID, []string{worker.ID}, 1, "other-worker"); err != nil {
		t.Fatal(err)
	}
	discussionAssess(t, s, f.scope, f.candidate.ID)
	input := domain.ChatDiscussionAction{Kind: domain.ChatDiscussionInvite, RecipientID: f.b,
		Content: strings.Repeat("<", 3000), ExpectedReply: "请先列出待核对的问题。"}
	invite := discussionApply(t, s, f.scope, f.candidate.ID, "invite", input)
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: invite.Event.DeliveryID, MaxActive: 2, ResourceKeys: []string{"discussion-intake"}})
	if err != nil {
		t.Fatal(err)
	}
	b, cb := discussionCapture(t, s, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, nil, "")
	queueEvent(t, s, b.Claim, provider.AcceptedEvent, "", "")
	r, err := s.ApplyChatAssessment(ctx, b, cb, investigationProposal(), "建议调查。", "investigate")
	if err != nil || r.Outcome != "admitted" || r.MessageID != invite.Message.ID || r.TurnID != b.Claim.TurnID {
		t.Fatalf("B did not admit the receiving Turn: %+v, %v", r, err)
	}
	work := map[string]string{"task_id": r.TaskID, "mandate_id": r.MandateID, "dispatch_request_id": r.DispatchRequestID}
	dispatch, err := s.GetDispatchRequest(ctx, f.groupID, r.DispatchRequestID)
	if err != nil || dispatch.State.Status != domain.DispatchStatusPending {
		t.Fatalf("fixture must not have dispatched work yet: %+v, %v", dispatch, err)
	}
	if task, err := s.GetTask(ctx, f.groupID, r.TaskID); err != nil || task.Status != domain.TaskStatusOpen || task.ActiveAttemptID != "" {
		t.Fatalf("admission is not execution or success: %+v, %v", task, err)
	}
	checkContext := func(scope MemberToolScope, candidate, incoming string) {
		t.Helper()
		got, err := s.ReadChatDiscussionContext(ctx, scope, candidate)
		if err != nil || got == nil {
			t.Fatalf("read work-owned context: %v", err)
		}
		wantEvent := invite
		wantEvent.Message.Content = ""
		if !got.WorkAdmitted || !got.Closed || got.ClosureReason != "" || got.ActiveOwnerID != "" || got.ActiveInvitationMessageID != "" ||
			got.HeadMessageID != invite.Message.ID || got.IncomingMessageID != incoming || !got.ContentOmitted ||
			!reflect.DeepEqual(got.Events, []domain.ChatDiscussionReceipt{wantEvent}) {
			t.Fatalf("work handoff did not close only the ordinary path or preserve event metadata: closed=%t, closure=%q, owner=%q", got.Closed, got.ClosureReason, got.ActiveOwnerID)
		}
		data, err := json.Marshal(got)
		if err != nil || len(data)+1 > chatDiscussionContextJSONBytes || !strings.Contains(string(data), `"work_admitted":true`) {
			t.Fatalf("work-owned context must signal admission within its JSON budget: %d bytes, %v", len(data)+1, err)
		}
	}
	checkReplay := func() {
		t.Helper()
		if got := discussionApply(t, s, f.scope, f.candidate.ID, "invite", input); !reflect.DeepEqual(got, invite) {
			t.Fatal("admission changed the immutable invitation replay")
		}
		changed := input
		changed.Content = "不同的邀请"
		if _, err := s.ApplyChatDiscussion(ctx, f.scope, f.candidate.ID, changed, "invite"); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed same-key publication did not conflict: %v", err)
		}
	}
	before := messageSequenceSnapshot(t, s)
	checkContext(b, cb, invite.Message.ID)
	checkContext(f.scope, f.candidate.ID, f.candidate.MessageID)
	admitted := checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
	if admitted.Content != invite.Message.Content || admitted.TopicID != f.policy.TopicID || admitted.Discussion == nil ||
		admitted.Discussion.Kind != domain.ChatDiscussionInvite || admitted.Discussion.InvitationMessageID != invite.Message.ID ||
		admitted.Discussion.RootMessageID != f.candidate.MessageID {
		t.Fatal("admission was not attached to the original public invitation in its topic")
	}
	for _, action := range []domain.ChatDiscussionAction{
		{Kind: domain.ChatDiscussionInvite, Content: "再请另一位核对。", RecipientID: worker.ID},
		{Kind: domain.ChatDiscussionReply, Content: "尝试走原讨论返回路径。"},
	} {
		if got, err := s.ApplyChatDiscussion(ctx, b, cb, action, "extend"); !errors.Is(err, ErrMemberToolDenied) || got.Message.ID != "" {
			t.Fatalf("work-owned head advanced the ordinary path: %s, %v", action.Kind, err)
		}
	}
	checkReplay()
	messageSequencePreserved(t, s, before)

	queueEvent(t, s, b.Claim, provider.ResponseEvent, "普通文字回复，不是结构化接力。", "")
	queueEvent(t, s, b.Claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
	if got, err := s.RequestTurnCancel(ctx, f.groupID, b.Claim.TurnID); err != nil || got.State != domain.TurnStateCancelRequested {
		t.Fatalf("fixture cancellation: %s, %v", got.State, err)
	}
	checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
	queueCounts(t, s, 2, 2, 2)
	incomplete := queueReceipt(provider.OutcomeSucceeded)
	incomplete.OutputComplete = false
	if got, err := s.ApplyTurnReceipt(ctx, b.Claim, incomplete); err != nil || got.State != domain.TurnStateUnknown {
		t.Fatalf("fixture uncertainty: %s, %v", got.State, err)
	}
	checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
	queueCounts(t, s, 2, 2, 2)

	settled, err := s.ApplyTurnReceipt(ctx, b.Claim, queueReceipt(provider.OutcomeSucceeded))
	if err != nil || settled.State != domain.TurnStateSucceeded || settled.ReplyMessageID == "" {
		t.Fatalf("recipient did not reach a trusted terminal without a structured return: %+v, %v", settled, err)
	}
	s = queueHandle(t, f.path, false)
	before = messageSequenceSnapshot(t, s)
	checkContext(f.scope, f.candidate.ID, f.candidate.MessageID)
	if got, err := s.ReadChatDiscussionContext(ctx, b, cb); !errors.Is(err, ErrMemberToolDenied) || got != nil {
		t.Fatalf("settled claim regained tool access: %v", err)
	}
	m := checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
	if len(m.Deliveries) != 1 || m.Deliveries[0].TurnID != settled.ID || m.Deliveries[0].State != domain.DeliveryStateAccepted {
		t.Fatalf("work projection changed delivery evidence: %+v", m.Deliveries)
	}
	for _, id := range []string{f.candidate.MessageID, r.NoticeMessageID, settled.ReplyMessageID} {
		checkDiscussionWorkMessage(t, s, f.groupID, id, nil, "")
	}
	if got, err := s.GetMessage(ctx, f.otherID, invite.Message.ID); !errors.Is(err, ErrNotFound) || got.ID != "" {
		t.Fatalf("foreign group read work evidence: %v", err)
	}
	checkReplay()
	messageSequencePreserved(t, s, before)

	discussionEnd(t, s, f.scope)
	// The reopened Store read history without opting in. Only the separate
	// fixture write enables B's gate and supplies the scoped worker fact.
	s.EnableInvestigationExecution()
	request := dispatch.Request
	coordinator := coordinationDecideBindRequest(t, s, request)
	facts := []DispatchWorkerCapabilityFact{{GroupID: worker.GroupID, MemberID: worker.ID, ProviderID: worker.ProviderID, Model: worker.Model,
		RequiredProfile: DispatchInvestigationWorkerProfile, TaskID: request.TaskID, TaskRevision: request.TaskRevision}}
	decision, err := s.DecideDispatchWithCapabilities(ctx, coordinator, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: worker.ID, Reason: "按策略分配。"}, "assign-investigation", facts)
	if err != nil || decision.Assignment == nil {
		t.Fatalf("fixture assignment: %+v, %v; cause=%v", decision, err, errors.Unwrap(err))
	}
	work["assignment_id"], work["worker_member_id"] = decision.Assignment.ID, worker.ID
	before = messageSequenceSnapshot(t, s)
	checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
	messageSequencePreserved(t, s, before)

	// Persist a bad decision timestamp without breaking the assignment's foreign keys.
	coordinationDecideExec(t, s, `DROP TRIGGER coordination_decisions_no_update;
	 PRAGMA ignore_check_constraints=ON;
	 UPDATE coordination_decisions SET created_at='2026-02-30T00:00:00.000000000Z' WHERE id=?;
	 PRAGMA ignore_check_constraints=OFF`, decision.Decision.ID)
	delete(work, "assignment_id")
	delete(work, "worker_member_id")
	checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
}

func admitDiscussionSummary(t *testing.T, s *Store, scope MemberToolScope, candidate string) domain.ChatAssessmentReceipt {
	t.Helper()
	r, err := s.ApplyChatAssessment(ctx, scope, candidate, domain.ChatAssessment{
		Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议准备话题摘要。",
	}, "准备摘要。", "prepare-summary")
	if err != nil || r.Outcome != "admitted" {
		t.Fatalf("fixture admission: %+v, %v", r, err)
	}
	return r
}

func TestChatDiscussionWorkHandoffStaysOnEarlierInvite(t *testing.T) {
	f, invite, b, cb := chatWorkInvitation(t)
	s := f.s
	admitted := admitDiscussionSummary(t, s, b, cb)
	// Reproduce an already-persisted reply from before the A handoff guard.
	// Use the ordinary publication transaction and schema, bypassing only plan.
	tx, err := s.beginMemberToolTx(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	e := domain.ChatDiscussionEvent{Kind: domain.ChatDiscussionReply, OriginMessageID: invite.Event.OriginMessageID,
		SourceMessageID: invite.Message.ID, InvitationMessageID: invite.Message.ID, ActorID: f.b, RecipientID: f.a}
	const content = "先前版本已沿普通路径返回。"
	mutation := mutationScope{operation: "create_message", actorID: f.b, groupID: f.groupID, key: "old-return"}
	payload := mutationPayload("chat_discussion/v1", cb, e.Kind, content, "", "")
	r, err := publishChatDiscussionInTx(ctx, tx, b, cb, content, mutation, payload, 2, e, []string{f.a})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if proof := readDiscussionWork(t, s, f.policy.ThreadRef, invite.Message.ID, cb, b.Claim.TurnID); proof == nil {
		t.Fatal("the superseded invitation must still have genuine B evidence")
	}
	work := map[string]string{"task_id": admitted.TaskID, "mandate_id": admitted.MandateID, "dispatch_request_id": admitted.DispatchRequestID}
	checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
	if reply := checkDiscussionWorkMessage(t, s, f.groupID, r.Message.ID, nil, ""); reply.Discussion == nil ||
		reply.Discussion.Kind != domain.ChatDiscussionReply || reply.Discussion.InvitationMessageID != invite.Message.ID {
		t.Fatalf("the reply lost its original discussion relation: %+v", reply.Discussion)
	}
	origin, err := s.GetMessage(ctx, f.groupID, f.candidate.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	discussionCheckContext(t, s, b, cb, origin, f.a, f.a, "", invite.Message.ID, r.Message.ID, false, invite, r)
	discussionEnd(t, s, b)

	dispatch, err := s.GetDispatchRequest(ctx, f.groupID, admitted.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := coordinationDecideBindRequest(t, s, dispatch.Request)
	decision, err := s.DecideDispatch(ctx, coordinator, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "按策略分配。"}, "assign-summary")
	if err != nil || decision.Assignment == nil {
		t.Fatalf("fixture assignment: %+v, %v", decision, err)
	}
	work["assignment_id"], work["worker_member_id"] = decision.Assignment.ID, f.b
	checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
	checkDiscussionWorkMessage(t, s, f.groupID, r.Message.ID, nil, "")
	discussionEnd(t, s, coordinator.Member)

	// The reply's real receiving Turn can still act on the later head.
	a, ca := discussionArrive(t, s, f.groupID, r.Event.DeliveryID)
	discussionCheckContext(t, s, a, ca, origin, f.a, f.a, "", r.Message.ID, r.Message.ID, false, invite, r)
	discussionAssess(t, s, a, ca)
	resolved := discussionApply(t, s, a, ca, "resolve", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionResolve, Content: "结论已确认。"})
	discussionCheckContext(t, s, a, ca, origin, f.a, "", "", r.Message.ID, resolved.Message.ID, true, invite, r, resolved)
	checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
}

func TestChatDiscussionWorkHandoffDistinguishesMissingAndInvalidProof(t *testing.T) {
	f := chatSetup(t, "user", true)
	s := f.s
	discussionAssess(t, s, f.scope, f.candidate.ID)
	input := domain.ChatDiscussionAction{Kind: domain.ChatDiscussionInvite, Content: "请补充摘要建议。", RecipientID: f.b}
	invite := discussionApply(t, s, f.scope, f.candidate.ID, "invite", input)
	b, cb := discussionArrive(t, s, f.groupID, invite.Event.DeliveryID)
	r := admitDiscussionSummary(t, s, b, cb)
	admitted := checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, map[string]string{
		"task_id": r.TaskID, "mandate_id": r.MandateID, "dispatch_request_id": r.DispatchRequestID,
	}, "")

	// A different delivery's Turn cannot prove this admission.
	coordinationDecideExec(t, s, "UPDATE deliveries SET turn_id=? WHERE id=?", f.candidate.TurnID, invite.Event.DeliveryID)
	checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, nil, "")
	coordinationDecideExec(t, s, "UPDATE deliveries SET turn_id=? WHERE id=?", b.Claim.TurnID, invite.Event.DeliveryID)

	coordinationDecideExec(t, s, "DROP TRIGGER chat_assessments_no_update")
	origin, err := s.GetMessage(ctx, f.groupID, f.candidate.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	reply := domain.ChatDiscussionAction{Kind: domain.ChatDiscussionReply, Content: "尝试走原讨论返回路径。"}
	checkPending := func() {
		t.Helper()
		before := messageSequenceSnapshot(t, s)
		checkDiscussionPendingProof(t, s, admitted)
		discussionCheckContext(t, s, b, cb, origin, f.a, "", "", invite.Message.ID, invite.Message.ID, false, invite)
		if got, err := s.ApplyChatDiscussion(ctx, b, cb, reply, "reply"); !errors.Is(err, ErrMemberToolDenied) || got.Message.ID != "" {
			t.Fatalf("head pending proof advanced the ordinary path: %s, %v", got.Message.ID, err)
		}
		if got := discussionApply(t, s, f.scope, f.candidate.ID, "invite", input); !reflect.DeepEqual(got, invite) {
			t.Fatal("pending proof changed the original invitation replay")
		}
		messageSequencePreserved(t, s, before)
	}

	// Damage only the paired references. The exact receipt still claims admission.
	var decisionID string
	if err := s.db.QueryRowContext(ctx, `SELECT decision_id FROM chat_assessments WHERE id=? AND candidate_id=?`, r.ID, cb).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, s, `UPDATE chat_assessments SET decision_id=NULL,dispatch_request_id=NULL WHERE id=? AND candidate_id=?`, r.ID, cb)
	checkPending()
	coordinationDecideExec(t, s, `UPDATE chat_assessments SET decision_id=?,dispatch_request_id=? WHERE id=? AND candidate_id=?`, decisionID, r.DispatchRequestID, r.ID, cb)

	// B returns nil when the named notice cannot be bound to this recipient.
	coordinationDecideExec(t, s, "UPDATE chat_assessments SET notice_message_id=? WHERE id=?", f.candidate.MessageID, r.ID)
	checkPending()

	foreign := createTestTask(t, s, f.otherID)
	coordinationDecideExec(t, s, `UPDATE chat_assessments SET notice_message_id=?,receipt_json=json_set(receipt_json,'$.task_id',?) WHERE id=?`, r.NoticeMessageID, foreign.ID, r.ID)
	checkPending() // ErrInvalid: the receipt names a Task outside its admission.
	coordinationDecideExec(t, s, `UPDATE chat_assessments SET receipt_json=json_set(receipt_json,'$.task_id',?,'$.authorization_message_id',?) WHERE id=?`, r.TaskID, origin.ID, r.ID)
	checkPending() // sql.ErrNoRows: the user question is not the written policy source.

	// A failure of the proof query itself must still fail reads touching that head.
	coordinationDecideExec(t, s, "ALTER TABLE chat_policy_scopes RENAME TO unavailable_chat_policy_scopes")
	if _, err := s.GetMessage(ctx, f.groupID, invite.Message.ID); err == nil || !strings.Contains(err.Error(), "no such table: chat_policy_scopes") {
		t.Fatalf("exact read hid the proof query error: %v", err)
	}
	if _, err := s.ListMessages(ctx, f.groupID, MessageListOptions{Limit: 50}); err == nil || !strings.Contains(err.Error(), "no such table: chat_policy_scopes") {
		t.Fatalf("timeline hid the proof query error: %v", err)
	}
	// The bounded page ends at the public assessment notice, before the bad head.
	page, err := s.ListMessages(ctx, f.groupID, MessageListOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != r.NoticeMessageID || page.Items[0].DiscussionWork != nil {
		t.Fatalf("out-of-page proof affected the notice: %+v, %v", page, err)
	}

	// A2 must first verify the publisher's capture before attempting any B proof.
	coordinationDecideExec(t, s, "DROP TRIGGER chat_intake_executions_update")
	coordinationDecideExec(t, s, "UPDATE chat_intake_executions SET source_id=? WHERE candidate_id=?", "unbound-publisher-source", f.candidate.ID)
	plain := checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, nil, "")
	if plain.Content != invite.Message.Content || plain.Discussion != nil || len(plain.Deliveries) != 1 || plain.Deliveries[0].TurnID != b.Claim.TurnID {
		t.Fatal("unverified relationship changed the public invitation or its delivery")
	}
}

func TestChatDiscussionWorkHandoffMalformedAssessment(t *testing.T) {
	f, invite, b, cb := chatWorkInvitation(t)
	s := f.s
	peer := member(t, s, f.groupID)
	r := admitDiscussionSummary(t, s, b, cb)
	want := checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, map[string]string{
		"task_id": r.TaskID, "mandate_id": r.MandateID, "dispatch_request_id": r.DispatchRequestID,
	}, "")
	want.DiscussionWork = nil
	var original string
	if err := s.db.QueryRowContext(ctx, `SELECT receipt_json FROM chat_assessments WHERE id=? AND candidate_id=?`, r.ID, cb).Scan(&original); err != nil {
		t.Fatal(err)
	}
	readInvitePage := func(t *testing.T) (Page[domain.Message], error) {
		t.Helper()
		first, err := s.ListMessages(ctx, f.groupID, MessageListOptions{Limit: 1})
		if err != nil || len(first.Items) != 1 || first.Items[0].ID != r.NoticeMessageID || first.NextCursor == nil {
			t.Fatalf("out-of-page evidence affected the notice: %+v, %v", first, err)
		}
		return s.ListMessages(ctx, f.groupID, MessageListOptions{Limit: 2, Before: first.NextCursor})
	}

	// Valid SQLite JSON can fail ordinary field decoding or time.Time's custom decoder.
	coordinationDecideExec(t, s, "DROP TRIGGER chat_assessments_no_update")
	for _, tc := range []struct{ name, path, value, kind string }{
		{"task_id_number", "$.task_id", `42`, "integer"},
		{"created_at_number", "$.created_at", `42`, "integer"},
		{"created_at_invalid_string", "$.created_at", `"not-a-timestamp"`, "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var valid bool
			var kind string
			err := s.db.QueryRowContext(ctx, `UPDATE chat_assessments SET receipt_json=json_set(?, ?, json(?))
			 WHERE id=? AND candidate_id=? RETURNING json_valid(receipt_json),json_type(receipt_json,?)`,
				original, tc.path, tc.value, r.ID, cb, tc.path).Scan(&valid, &kind)
			if err != nil || !valid || kind != tc.kind {
				t.Fatalf("fixture is not valid JSON with %s of type %s: valid=%t type=%q err=%v", tc.path, tc.kind, valid, kind, err)
			}
			before := messageSequenceSnapshot(t, s)
			t.Run("detail", func(t *testing.T) {
				if got, err := s.GetMessage(ctx, f.groupID, want.ID); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("typed receipt corruption changed the public invitation: %+v, %v; want %+v", got, err, want)
				}
			})
			t.Run("page", func(t *testing.T) {
				page, err := readInvitePage(t)
				if err != nil || len(page.Items) != 2 || !reflect.DeepEqual(page.Items[0], want) {
					t.Fatalf("page containing the invitation: %+v, %v; want %+v", page, err, want)
				}
			})
			for _, action := range []domain.ChatDiscussionAction{
				{Kind: domain.ChatDiscussionReply, Content: "尝试走原讨论返回路径。"},
				{Kind: domain.ChatDiscussionInvite, Content: "尝试邀请另一位成员。", RecipientID: peer.ID},
			} {
				t.Run("no_new_"+action.Kind, func(t *testing.T) {
					if got, err := s.ApplyChatDiscussion(ctx, b, cb, action, "new-"+action.Kind); !errors.Is(err, ErrMemberToolDenied) || got.Message.ID != "" {
						t.Fatalf("corrupt assessment must deny new progress without a write error: %s, %v", got.Message.ID, err)
					}
				})
			}
			messageSequencePreserved(t, s, before)
		})
	}
	t.Run("cancellation", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.GetMessage(cancelled, f.groupID, want.ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("exact read hid cancellation: %v", err)
		}
		if _, err := s.ListMessages(cancelled, f.groupID, MessageListOptions{Limit: 2}); !errors.Is(err, context.Canceled) {
			t.Fatalf("timeline hid cancellation: %v", err)
		}
	})
	checkReadError := func(t *testing.T, target any) {
		t.Helper()
		if _, err := s.GetMessage(ctx, f.groupID, want.ID); !errors.As(err, target) {
			t.Errorf("exact read error=%v, want %T", err, target)
		}
		if _, err := readInvitePage(t); !errors.As(err, target) {
			t.Errorf("paged read error=%v, want %T", err, target)
		}
	}
	t.Run("invalid_storage_json", func(t *testing.T) {
		// This violates the table CHECK, unlike the typed cases above.
		coordinationDecideExec(t, s, `PRAGMA ignore_check_constraints=ON`)
		coordinationDecideExec(t, s, `UPDATE chat_assessments SET receipt_json='{' WHERE id=? AND candidate_id=?`, r.ID, cb)
		coordinationDecideExec(t, s, `PRAGMA ignore_check_constraints=OFF`)
		var syntax *json.SyntaxError
		checkReadError(t, &syntax)
	})
	t.Run("unrelated_mandate", func(t *testing.T) {
		coordinationDecideExec(t, s, `UPDATE chat_assessments SET receipt_json=? WHERE id=? AND candidate_id=?`, original, r.ID, cb)
		// Only the receiving candidate's assessment JSON is evidence here.
		coordinationDecideExec(t, s, `UPDATE chat_assessments SET receipt_json=json_set(receipt_json,'$.created_at',42) WHERE candidate_id=?`, f.candidate.ID)
		coordinationDecideExec(t, s, "DROP TRIGGER mandates_no_update")
		coordinationDecideExec(t, s, `UPDATE mandates SET dependency_ids='[42]' WHERE id=?`, r.MandateID)
		var badType *json.UnmarshalTypeError
		checkReadError(t, &badType)

		coordinationDecideExec(t, s, `UPDATE mandates SET dependency_ids='[]',created_at='not-a-timestamp' WHERE id=?`, r.MandateID)
		var badTime *time.ParseError
		checkReadError(t, &badTime)
	})
}

func TestChatDiscussionWorkHandoffMissingProofIsNotInterruption(t *testing.T) {
	f, invite, b, cb := chatWorkInvitation(t)
	s := f.s
	r := admitDiscussionSummary(t, s, b, cb) // Admitted before the receiving CLI accepted.
	if turn, err := s.ApplyTurnReceipt(ctx, b.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil || turn.State != domain.TurnStateNotStarted {
		t.Fatalf("fixture settlement: %s, %v", turn.State, err)
	}
	admitted := checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, map[string]string{
		"task_id": r.TaskID, "mandate_id": r.MandateID, "dispatch_request_id": r.DispatchRequestID,
	}, "")
	if d := admitted.Deliveries[0]; d.State != domain.DeliveryStateFailed || d.TurnID != b.Claim.TurnID || d.Error != "执行未启动" {
		t.Fatalf("admission changed the failed delivery: %+v", d)
	}
	coordinationDecideExec(t, s, "DROP TRIGGER chat_assessments_no_update")
	coordinationDecideExec(t, s, `UPDATE chat_assessments SET decision_id=NULL,dispatch_request_id=NULL WHERE id=? AND candidate_id=?`, r.ID, cb)
	checkDiscussionPendingProof(t, s, admitted)
}

func TestChatDiscussionWorkHandoffReadsRecoveredAdmission(t *testing.T) {
	f := chatSetup(t, "user", true)
	s := f.s
	discussionAssess(t, s, f.scope, f.candidate.ID)
	invite := discussionApply(t, s, f.scope, f.candidate.ID, "invite", domain.ChatDiscussionAction{
		Kind: domain.ChatDiscussionInvite, Content: "请补充摘要建议。", RecipientID: f.b})
	discussionEnd(t, s, f.scope)
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: invite.Event.DeliveryID,
		MaxActive: 1, ResourceKeys: []string{"discussion-intake"}})
	if err != nil {
		t.Fatal(err)
	}
	b, cb := discussionCapture(t, s, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	queueEvent(t, s, b.Claim, provider.AcceptedEvent, "", "")
	r := admitDiscussionSummary(t, s, b, cb)
	work := map[string]string{"task_id": r.TaskID, "mandate_id": r.MandateID, "dispatch_request_id": r.DispatchRequestID}
	admitted := checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, "")
	queueCounts(t, s, 2, 2, 1)
	before := messageSequenceSnapshot(t, s)
	// Keep the original claim live and bound while introducing a second Turn.
	const duplicate = "duplicate-receiver"
	coordinationDecideExec(t, s, `INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,created_at,updated_at)
	 SELECT ?,group_id,delivery_id,conversation_id,member_id,'duplicate-claim','not_started',created_at,updated_at
	 FROM turns WHERE id=?`, duplicate, b.Claim.TurnID)
	ambiguous := messageSequenceSnapshot(t, s)
	reply := domain.ChatDiscussionAction{Kind: domain.ChatDiscussionReply, Content: "尝试走原讨论返回路径。"}
	if got, err := s.ApplyChatDiscussion(ctx, b, cb, reply, "ambiguous-reply"); !errors.Is(err, ErrMemberToolDenied) || got.Message.ID != "" {
		t.Fatalf("ambiguous receiving Turn allowed new progress: %s, %v", got.Message.ID, err)
	}
	checkDiscussionPendingProof(t, s, admitted)
	messageSequencePreserved(t, s, ambiguous)

	if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("fixture recovery: %d, %v", n, err)
	}
	recovered := queueTurn(t, s, b.Claim)
	if recovered.State != domain.TurnStateUnknown || recovered.DeliveryID != invite.Event.DeliveryID || recovered.ClaimToken == b.Claim.Token {
		t.Fatalf("fixture did not rotate the same receiving Turn's claim: %+v", recovered)
	}
	reconciliation := b
	reconciliation.Claim.Token = recovered.ClaimToken
	if got, err := s.ApplyChatDiscussion(ctx, reconciliation, cb, reply, "ambiguous-reply"); !errors.Is(err, ErrMemberToolDenied) || got.Message.ID != "" {
		t.Fatalf("unknown receiving Turn advanced the ambiguous head: %s, %v", got.Message.ID, err)
	}
	checkDiscussionPendingProof(t, s, admitted)
	delete(ambiguous, "turns") // Recovery may rotate the claim, but must preserve its resource.
	messageSequencePreserved(t, s, ambiguous)
	if got := queueTurn(t, s, b.Claim); !reflect.DeepEqual(got, recovered) {
		t.Fatalf("reading or advancing the ambiguous head changed the unknown Turn: %+v", got)
	}
	// Once uniqueness is restored, the durable admission can be read even while the Turn is unknown.
	coordinationDecideExec(t, s, "DELETE FROM turns WHERE id=?", duplicate)
	checkHistory := func(t *testing.T) {
		t.Helper()
		if got := checkDiscussionWorkMessage(t, s, f.groupID, invite.Message.ID, work, ""); !reflect.DeepEqual(got, admitted) {
			t.Fatalf("history changed the original invitation or its delivery: %+v; want %+v", got, admitted)
		}
		for _, scope := range []MemberToolScope{b, reconciliation} {
			if _, err := s.BindMemberTool(ctx, scope.Claim); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("history granted a live member tool scope: %v", err)
			}
			if got, err := s.ApplyChatDiscussion(ctx, scope, cb, reply, "late-reply"); !errors.Is(err, ErrMemberToolDenied) || got.Message.ID != "" {
				t.Fatalf("history advanced the ordinary discussion path: %s, %v", got.Message.ID, err)
			}
		}
	}
	receipt := queueReceipt(provider.OutcomeFailed)
	delete(before, "turns") // Recovery changes the Turn state and token, not its existing claims.
	t.Run("while unknown", func(t *testing.T) {
		checkHistory(t)
		if got, err := s.ApplyTurnReceipt(ctx, b.Claim, receipt); !errors.Is(err, ErrStaleClaim) || got.ID != "" {
			t.Fatalf("the original token settled the recovered Turn: %s, %v", got.ID, err)
		}
		if got := queueTurn(t, s, b.Claim); !reflect.DeepEqual(got, recovered) {
			t.Fatalf("history or stale settlement changed the recovered Turn: %+v", got)
		}
		messageSequencePreserved(t, s, before)
		queueCounts(t, s, 2, 2, 1)
	})
	t.Run("after trusted settlement", func(t *testing.T) {
		settled, err := s.ApplyTurnReceipt(ctx, reconciliation.Claim, receipt)
		if err != nil || settled.ID != turn.ID || settled.State != domain.TurnStateFailed {
			t.Fatalf("trusted settlement did not reach the original Turn: %s, %v", settled.State, err)
		}
		checkHistory(t)
		if got := queueTurn(t, s, b.Claim); !reflect.DeepEqual(got, settled) {
			t.Fatalf("history changed the settled Turn: %+v", got)
		}
		delete(before, "resource_claims")
		delete(before, "native_run_bindings")
		messageSequencePreserved(t, s, before)
		queueCounts(t, s, 2, 2, 0)
	})
}

func TestChatDiscussionWorkHandoffDoesNotUseGenericAdmission(t *testing.T) {
	t.Run("independent route on the invitation", func(t *testing.T) {
		f, invite, b, cb := chatWorkInvitation(t)
		discussionAssess(t, f.s, b, cb)
		// A normal discuss receipt has neither formal reference, but is not pending proof.
		origin, err := f.s.GetMessage(ctx, f.groupID, f.candidate.MessageID)
		if err != nil {
			t.Fatal(err)
		}
		discussionCheckContext(t, f.s, b, cb, origin, f.a, f.b, invite.Message.ID, invite.Message.ID, invite.Message.ID, false, invite)
		r, err := f.s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{TopicID: f.policy.TopicID, MessageID: invite.Message.ID,
			Mode: domain.RoutingDecisionCreateNew, Reason: "用户独立建单。", NewTask: &domain.NewTaskRequest{Title: "独立任务", Acceptance: "文本交付"}}, "route")
		if err != nil || r.Task.ID == "" {
			t.Fatalf("fixture independent admission: %+v, %v", r, err)
		}
		queueEvent(t, f.s, b.Claim, provider.AcceptedEvent, "", "")
		if got, err := f.s.ApplyTurnReceipt(ctx, b.Claim, queueReceipt(provider.OutcomeFailed)); err != nil || got.State != domain.TurnStateFailed {
			t.Fatalf("fixture settlement: %v", err)
		}
		checkDiscussionWorkMessage(t, f.s, f.groupID, invite.Message.ID, nil, chatDiscussionAbnormalExitReason)
	})
	t.Run("plain member message with chat admission", func(t *testing.T) {
		f := chatSetup(t, "agent", true)
		r := admitDiscussionSummary(t, f.s, f.scope, f.candidate.ID)
		checkDiscussionWorkMessage(t, f.s, f.groupID, r.MessageID, nil, "")
		if got, err := f.s.ReadChatDiscussionContext(ctx, f.scope, f.candidate.ID); err != nil || got != nil {
			t.Fatalf("legacy member message acquired discussion context: %v", err)
		}
	})
}
