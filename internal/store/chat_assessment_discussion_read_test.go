package store

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func chatWorkInvitation(t *testing.T) (chatFixture, domain.ChatDiscussionReceipt, MemberToolScope, string) {
	t.Helper()
	f := chatSetup(t, "user", true)
	discussionAssess(t, f.s, f.scope, f.candidate.ID)
	invite := discussionApply(t, f.s, f.scope, f.candidate.ID, "invite", domain.ChatDiscussionAction{
		Kind: domain.ChatDiscussionInvite, Content: "请补充摘要建议。", RecipientID: f.b})
	discussionEnd(t, f.s, f.scope)
	scope, candidate := discussionArrive(t, f.s, f.groupID, invite.Event.DeliveryID)
	return f, invite, scope, candidate
}

func readDiscussionWork(t *testing.T, s *Store, thread domain.ThreadRef, messageID, candidateID, turnID string) *chatDiscussionWorkAdmission {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := chatDiscussionWorkAdmissionInTx(ctx, tx, thread, messageID, candidateID, turnID)
	if err != nil {
		t.Fatalf("read discussion admission: %v", err)
	}
	if display, err := chatDiscussionWorkAdmissionForDisplayInTx(ctx, tx, thread, messageID, candidateID, turnID); err != nil || !reflect.DeepEqual(display, got) {
		t.Fatalf("display admission = %+v, %v; want strict admission %+v", display, err, got)
	}
	return got
}

func TestChatDiscussionWorkAdmissionPendingAndBoundAssignment(t *testing.T) {
	f, invite, b, cb := chatWorkInvitation(t)
	thread, messageID, turnID := f.policy.ThreadRef, invite.Message.ID, b.Claim.TurnID
	if got := readDiscussionWork(t, f.s, thread, messageID, cb, turnID); got != nil {
		t.Fatalf("unhandled invitation proved work: %+v", got)
	}
	queueEvent(t, f.s, b.Claim, provider.AcceptedEvent, "", "")
	proposal := domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议准备话题摘要。"}
	r, err := f.s.ApplyChatAssessment(ctx, b, cb, proposal, "准备摘要。", "summary")
	if err != nil || r.Outcome != "admitted" {
		t.Fatalf("stored admission: %+v, %v", r, err)
	}
	// Settlement does not erase work proved by an earlier tool transaction.
	queueEvent(t, f.s, b.Claim, provider.TerminalEvent, "", provider.OutcomeFailed)
	if turn, err := f.s.ApplyTurnReceipt(ctx, b.Claim, queueReceipt(provider.OutcomeFailed)); err != nil || turn.State != domain.TurnStateFailed {
		t.Fatalf("settle recipient turn: %+v, %v", turn, err)
	}
	core := chatDiscussionWorkAdmission{TaskID: r.TaskID, MandateID: r.MandateID, DispatchRequestID: r.DispatchRequestID}
	s := queueHandle(t, f.path, false)
	view, err := s.GetDispatchRequest(ctx, thread.GroupID, r.DispatchRequestID)
	if err != nil || view.State.Status != domain.DispatchStatusPending {
		t.Fatalf("fixture dispatch was not pending: %+v, %v", view, err)
	}
	if got := readDiscussionWork(t, s, thread, messageID, cb, turnID); !reflect.DeepEqual(got, &core) {
		t.Fatalf("pending dispatch proved wrong work or fabricated an assignment: %+v", got)
	}

	coord := coordinationDecideBindRequest(t, s, view.Request)
	decision, err := s.DecideDispatch(ctx, coord, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "按策略分配。"}, "assign-summary")
	if err != nil || decision.Assignment == nil {
		t.Fatalf("stored assignment: %+v, %v", decision, err)
	}
	want := core
	want.Assignment = decision.Assignment
	if got := readDiscussionWork(t, s, thread, messageID, cb, turnID); !reflect.DeepEqual(got, &want) {
		t.Fatalf("wrong assignment proof: %+v, want %+v", got, want)
	}

	for _, mismatch := range []struct {
		name                     string
		thread                   domain.ThreadRef
		message, candidate, turn string
	}{
		{"group", domain.ThreadRef{GroupID: f.otherID, TopicID: thread.TopicID}, messageID, cb, turnID},
		{"topic", domain.ThreadRef{GroupID: thread.GroupID, TopicID: "other"}, messageID, cb, turnID},
		{"message", thread, f.candidate.MessageID, cb, turnID},
		{"candidate", thread, messageID, f.candidate.ID, turnID},
		{"turn", thread, messageID, cb, f.candidate.TurnID},
	} {
		t.Run(mismatch.name, func(t *testing.T) {
			if got := readDiscussionWork(t, s, mismatch.thread, mismatch.message, mismatch.candidate, mismatch.turn); got != nil {
				t.Fatalf("mismatched coordinates proved work: %+v", got)
			}
		})
	}

	// Exercise each counterfactual independently on the private fixture DB.
	faultTx := func(t *testing.T, query string, args ...any) *sql.Tx {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tx.Rollback() })
		if _, err := tx.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	for _, fault := range []struct {
		name, query string
		args        []any
		strictErr   error
		displayCore bool
	}{
		{"missing-assignment-binding", `DROP TRIGGER coordination_assignment_dispatch_no_delete;
		 DELETE FROM coordination_assignment_dispatch WHERE assignment_id=?`, []any{decision.Assignment.ID}, sql.ErrNoRows, true},
		{"invalid-assignment-time", `DROP TRIGGER coordination_decisions_no_update; PRAGMA ignore_check_constraints=ON;
		 UPDATE coordination_decisions SET created_at='2026-02-30T00:00:00.000000000Z' WHERE id=?;
		 PRAGMA ignore_check_constraints=OFF`, []any{decision.Decision.ID}, ErrInvalid, true},
		{"invalid-core-source", `DROP TRIGGER chat_assessments_no_update;
		 UPDATE chat_assessments SET receipt_json=json_set(receipt_json,'$.source_id',?) WHERE id=?`,
			[]any{f.candidate.SourceID, r.ID}, ErrInvalid, false},
		{"missing-core-policy-proof", `DROP TRIGGER chat_assessments_no_update;
		 UPDATE chat_assessments SET receipt_json=json_set(receipt_json,'$.authorization_message_id',?) WHERE id=?`,
			[]any{invite.Message.ID, r.ID}, sql.ErrNoRows, false},
		{"ambiguous-recipient-turn", `INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,created_at,updated_at)
		 SELECT 'duplicate-turn',group_id,delivery_id,conversation_id,member_id,'duplicate-claim','unknown',created_at,updated_at
		 FROM turns WHERE id=?`, []any{turnID}, nil, false},
	} {
		t.Run(fault.name, func(t *testing.T) {
			tx := faultTx(t, fault.query, fault.args...)
			if got, err := chatDiscussionWorkAdmissionInTx(ctx, tx, thread, messageID, cb, turnID); got != nil || !errors.Is(err, fault.strictErr) {
				t.Fatalf("strict admission = %+v, %v; want nil, %v", got, err, fault.strictErr)
			}
			var wantDisplay *chatDiscussionWorkAdmission
			displayErr := fault.strictErr
			if fault.displayCore {
				wantDisplay, displayErr = &core, nil
			}
			if got, err := chatDiscussionWorkAdmissionForDisplayInTx(ctx, tx, thread, messageID, cb, turnID); !reflect.DeepEqual(got, wantDisplay) || !errors.Is(err, displayErr) {
				t.Fatalf("display admission = %+v, %v; want %+v, %v", got, err, wantDisplay, displayErr)
			}
		})
	}
	t.Run("assignment-query-error", func(t *testing.T) {
		// Only the optional assignment proof queries coordination_decisions.
		tx := faultTx(t, `ALTER TABLE coordination_decisions RENAME TO damaged_coordination_decisions`)
		const detail = "no such table: coordination_decisions"
		if got, err := chatDiscussionWorkAdmissionInTx(ctx, tx, thread, messageID, cb, turnID); got != nil || err == nil || !strings.Contains(err.Error(), detail) {
			t.Fatalf("strict admission did not propagate query failure: %+v, %v", got, err)
		}
		if got, err := chatDiscussionWorkAdmissionForDisplayInTx(ctx, tx, thread, messageID, cb, turnID); got != nil || err == nil || !strings.Contains(err.Error(), detail) {
			t.Fatalf("display admission did not propagate query failure: %+v, %v", got, err)
		}
	})
}

func TestChatDiscussionWorkAdmissionSurvivesClaimRecovery(t *testing.T) {
	f := chatSetup(t, "user", true)
	s, thread := f.s, f.policy.ThreadRef
	discussionAssess(t, s, f.scope, f.candidate.ID)
	invite := discussionApply(t, s, f.scope, f.candidate.ID, "invite", domain.ChatDiscussionAction{
		Kind: domain.ChatDiscussionInvite, Content: "请补充摘要建议。", RecipientID: f.b})
	discussionEnd(t, s, f.scope)
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: invite.Event.DeliveryID,
		MaxActive: 1, ResourceKeys: []string{"discussion-intake"}})
	if err != nil {
		t.Fatal(err)
	}
	claim := TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
	b, err := s.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CaptureChatCandidate(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	queueEvent(t, s, claim, provider.AcceptedEvent, "", "")

	proposal := domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议准备话题摘要。"}
	const content, key = "准备摘要。", "summary"
	if got, err := s.ApplyChatAssessment(ctx, b, f.candidate.ID, proposal, content, key); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
		t.Fatalf("the live recipient claim used the publisher's candidate: %+v, %v", got, err)
	}
	r, err := s.ApplyChatAssessment(ctx, b, c.ID, proposal, content, key)
	if err != nil || r.Outcome != "admitted" {
		t.Fatalf("stored recipient admission: %+v, %v", r, err)
	}
	want := &chatDiscussionWorkAdmission{TaskID: r.TaskID, MandateID: r.MandateID, DispatchRequestID: r.DispatchRequestID}
	checkHistory := func(phase string) {
		t.Run(phase, func(t *testing.T) {
			if got := readDiscussionWork(t, s, thread, invite.Message.ID, c.ID, turn.ID); !reflect.DeepEqual(got, want) {
				t.Fatalf("admission = %+v, want %+v", got, want)
			}
		})
	}
	checkHistory("before recovery")
	if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("recover recipient: %d, %v", n, err)
	}
	recovered := queueTurn(t, s, claim)
	if recovered.State != domain.TurnStateUnknown || recovered.DeliveryID != invite.Event.DeliveryID || recovered.ClaimToken == claim.Token {
		t.Fatalf("recovery did not rotate the same recipient Turn to unknown: %+v", recovered)
	}
	checkHistory("while unknown")
	if got := readDiscussionWork(t, s, thread, invite.Message.ID, c.ID, f.candidate.TurnID); got != nil {
		t.Fatalf("the publisher Turn proved the recovered recipient's admission: %+v", got)
	}
	if got, err := s.ApplyChatAssessment(ctx, b, c.ID, proposal, content, key); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
		t.Fatalf("stale scope replayed a tool write: %+v, %v", got, err)
	}
	receipt := queueReceipt(provider.OutcomeFailed)
	if got, err := s.ApplyTurnReceipt(ctx, claim, receipt); !errors.Is(err, ErrStaleClaim) || got.ID != "" {
		t.Fatalf("old claim settled recovered work: %+v, %v", got, err)
	}
	if got := queueTurn(t, s, claim); !reflect.DeepEqual(got, recovered) {
		t.Fatalf("history read or stale write changed the recovered Turn: %+v", got)
	}
	queueCounts(t, s, 2, 2, 1)

	fresh := TurnClaim{GroupID: claim.GroupID, TurnID: claim.TurnID, Token: recovered.ClaimToken}
	if got, err := s.ApplyTurnReceipt(ctx, fresh, receipt); err != nil || got.ID != turn.ID || got.State != domain.TurnStateFailed {
		t.Fatalf("trusted reconciliation did not settle the recipient: %+v, %v", got, err)
	}
	checkHistory("after trusted settlement")
	queueCounts(t, s, 2, 2, 0)
}

func TestChatDiscussionWorkAdmissionNoProof(t *testing.T) {
	t.Run("independent-user-command-and-routed-invitation", func(t *testing.T) {
		f, invite, b, cb := chatWorkInvitation(t)
		thread := f.policy.ThreadRef
		command := chatLocalMessage(t, f.s, MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user",
			Content: "现在按新要求单独生成文本摘要。"}, []string{f.a})
		a, ca := discussionArrive(t, f.s, f.groupID, command.Deliveries[0].ID)
		r, err := f.s.ApplyChatAssessment(ctx, a, ca, f.input, "按新要求交付。", "independent-work")
		if err != nil || r.Outcome != "admitted" {
			t.Fatalf("separate same-topic command did not admit work: %+v, %v", r, err)
		}
		if got := readDiscussionWork(t, f.s, thread, invite.Message.ID, ca, a.Claim.TurnID); got != nil {
			t.Fatalf("used a different Turn's admission: %+v", got)
		}
		discussionAssess(t, f.s, b, cb)
		// A real local command may route the invitation's message without the
		// recipient having admitted it on this Turn. Neither this route nor the
		// model's final reply establishes an admission by that recipient.
		routed, err := f.s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{TopicID: thread.TopicID, MessageID: invite.Message.ID,
			Mode: domain.RoutingDecisionCreateNew, Reason: "用户独立建单。", NewTask: &domain.NewTaskRequest{Title: "独立任务", Acceptance: "文本交付"}}, "local-route")
		if err != nil || routed.Task.ID == "" {
			t.Fatalf("independent route: %+v, %v", routed, err)
		}
		queueEvent(t, f.s, b.Claim, provider.AcceptedEvent, "", "")
		queueEvent(t, f.s, b.Claim, provider.ResponseEvent, "任务已准入："+routed.Task.ID, "")
		queueEvent(t, f.s, b.Claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
		if turn, err := f.s.ApplyTurnReceipt(ctx, b.Claim, queueReceipt(provider.OutcomeSucceeded)); err != nil || turn.State != domain.TurnStateSucceeded {
			t.Fatalf("complete recipient turn: %+v, %v", turn, err)
		}
		if got := readDiscussionWork(t, f.s, thread, invite.Message.ID, cb, b.Claim.TurnID); got != nil {
			t.Fatalf("ordinary discussion or an independent route proved work: %+v", got)
		}
	})
	t.Run("ordinary-member-admission", func(t *testing.T) {
		f := chatSetup(t, "agent", true)
		proposal := domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议准备话题摘要。"}
		r, err := f.s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "准备摘要。", "legacy-summary")
		if err != nil || r.Outcome != "admitted" || r.SourceKind != "member" || r.Authority != "written_preauthorization" {
			t.Fatalf("legacy proposal lost its scoped authority: %+v, %v", r, err)
		}
		if got := readDiscussionWork(t, f.s, r.ThreadRef, r.MessageID, r.CandidateID, r.TurnID); got != nil {
			t.Fatalf("ordinary member chat became a structured invitation: %+v", got)
		}
	})
}
