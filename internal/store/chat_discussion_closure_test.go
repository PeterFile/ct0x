package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func discussionCheckMessageClosure(t *testing.T, s *Store, groupID, messageID, want string) domain.Message {
	t.Helper()
	message, err := s.GetMessage(ctx, groupID, messageID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Reason json.RawMessage `json:"discussion_closure_reason"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(want)
	if want == "" && wire.Reason != nil || want != "" && string(wire.Reason) != string(expected) {
		t.Fatalf("message %s closure = %s; want %q (omitted if empty)", messageID, wire.Reason, want)
	}
	return message
}

func discussionSerialHandoff(t *testing.T) (messageFixture, domain.Message, domain.ChatDiscussionReceipt, TurnClaim) {
	t.Helper()
	f := setupMessages(t)
	s := f.s
	origin := discussionUser(t, f, "serial", f.a)
	sender, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: origin.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	a, ca := discussionCapture(t, s, TurnClaim{GroupID: f.groupID, TurnID: sender.ID, Token: sender.ClaimToken})
	queueEvent(t, s, a.Claim, provider.AcceptedEvent, "", "")
	discussionAssess(t, s, a, ca)
	input := domain.ChatDiscussionAction{Kind: "invite", Content: "请核对结论。", RecipientID: f.b}
	invite := discussionApply(t, s, a, ca, "invite", input)
	claim := ClaimTurnInput{GroupID: f.groupID, DeliveryID: invite.Event.DeliveryID, MaxActive: 1, ResourceKeys: []string{"discussion-held"}}
	if _, err := s.ClaimTurn(ctx, claim); !errors.Is(err, ErrTurnBusy) {
		t.Fatalf("recipient claimed before the sender settled: %v", err)
	}
	if turn, err := s.ApplyTurnReceipt(ctx, a.Claim, queueReceipt(provider.OutcomeSucceeded)); err != nil || turn.State != domain.TurnStateSucceeded {
		t.Fatalf("sender did not reach trusted terminal: %+v, %v", turn, err)
	}
	if got, err := s.ReadChatDiscussionContext(ctx, a, ca); !errors.Is(err, ErrMemberToolDenied) || got != nil {
		t.Fatalf("stale sender read the path: %+v, %v", got, err)
	}
	if _, err := s.ApplyChatDiscussion(ctx, a, ca, input, "invite"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("stale sender replayed its action: %v", err)
	}
	turn, err := s.ClaimTurn(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	return f, origin, invite, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
}

func TestChatDiscussionTerminalClosureSerial(t *testing.T) {
	for _, tc := range []struct {
		outcome provider.Outcome
		reason  string
	}{
		{provider.OutcomeNotStarted, "接收成员的讨论轮未启动"},
		{provider.OutcomeSucceeded, "讨论已中断：接收成员已完成本轮，但未提交结构化讨论动作"},
		{provider.OutcomeFailed, "讨论已中断：接收成员的轮次异常结束，未提交结构化讨论动作"},
		{provider.OutcomeAborted, "讨论已中断：接收成员的轮次异常结束，未提交结构化讨论动作"},
		{provider.OutcomeTruncated, "讨论已中断：接收成员的轮次异常结束，未提交结构化讨论动作"},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			f, origin, invite, b := discussionSerialHandoff(t)
			s := f.s
			evidence, state := queueReceipt(tc.outcome), domain.DeliveryStateAccepted
			const text = "普通文字回复，不是结构化接力。"
			switch tc.outcome {
			case provider.OutcomeNotStarted:
				evidence, state = provider.ExecutionReceipt{Outcome: tc.outcome, ExitCode: -1}, domain.DeliveryStateFailed
			case provider.OutcomeSucceeded:
				queueCandidate(t, s, b, text, tc.outcome)
			}
			discussionCheckMessageClosure(t, s, f.groupID, invite.Message.ID, "")
			settled, err := s.ApplyTurnReceipt(ctx, b, evidence)
			if err != nil || settled.State != domain.TurnState(tc.outcome) {
				t.Fatalf("recipient did not reach trusted terminal: %+v, %v", settled, err)
			}
			queueCounts(t, s, 2, 2, 0)
			before := messageSequenceSnapshot(t, s)
			m := discussionCheckMessageClosure(t, s, f.groupID, invite.Message.ID, tc.reason)
			if len(m.Deliveries) != 1 || m.Deliveries[0].TurnID != b.TurnID || m.Deliveries[0].State != state {
				t.Fatalf("closure rewrote the delivery: %+v", m.Deliveries)
			}
			discussionCheckMessageClosure(t, s, f.groupID, origin.ID, "")
			if tc.outcome == provider.OutcomeSucceeded {
				reply := discussionCheckMessageClosure(t, s, f.groupID, settled.ReplyMessageID, "")
				if reply.Content != text || len(reply.Deliveries) != 0 {
					t.Fatalf("ordinary reply was not preserved: %+v", reply)
				}
			}
			if got, err := s.GetMessage(ctx, f.otherID, invite.Message.ID); !errors.Is(err, ErrNotFound) || got.ID != "" {
				t.Fatalf("foreign group read the head: %+v, %v", got, err)
			}
			messageSequencePreserved(t, s, before)
		})
	}
}

func TestChatDiscussionClosureLeavesUnsettledClaims(t *testing.T) {
	f, _, invite, b := discussionSerialHandoff(t)
	s := f.s
	other := queueDelivery(t, s, f.otherID, f.foreign, "other")
	check := func(state domain.TurnState) {
		t.Helper()
		discussionCheckMessageClosure(t, s, f.groupID, invite.Message.ID, "")
		if turn := queueTurn(t, s, b); turn.State != state {
			t.Fatalf("read changed the turn: %+v, want %s", turn, state)
		}
		if _, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.otherID, DeliveryID: other.ID, MaxActive: 2, ResourceKeys: []string{"discussion-held"}}); !errors.Is(err, ErrTurnBusy) {
			t.Fatalf("%s lost its resource: %v", state, err)
		}
	}
	check(domain.TurnStateStarting)
	queueEvent(t, s, b, provider.AcceptedEvent, "", "")
	check(domain.TurnStateRunning)
	if _, err := s.RequestTurnCancel(ctx, b.GroupID, b.TurnID); err != nil {
		t.Fatal(err)
	}
	check(domain.TurnStateCancelRequested)
	evidence := queueReceipt(provider.OutcomeSucceeded)
	evidence.OutputComplete = false // Native terminal evidence alone is insufficient.
	if _, err := s.ApplyTurnReceipt(ctx, b, evidence); err != nil {
		t.Fatal(err)
	}
	check(domain.TurnStateUnknown)
}

func TestChatDiscussionClosureRejectsDuplicateTurn(t *testing.T) {
	f, _, invite, b := discussionSerialHandoff(t)
	s := f.s
	queueEvent(t, s, b, provider.AcceptedEvent, "", "")
	if turn, err := s.ApplyTurnReceipt(ctx, b, queueReceipt(provider.OutcomeFailed)); err != nil || turn.State != domain.TurnStateFailed {
		t.Fatalf("recipient did not reach trusted terminal: %+v, %v", turn, err)
	}
	discussionCheckMessageClosure(t, s, f.groupID, invite.Message.ID, chatDiscussionAbnormalExitReason)

	// Add an unsettled Turn for the same delivery without changing its terminal binding.
	const duplicateID = "duplicate-discussion-turn"
	coordinationDecideExec(t, s, `INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,created_at,updated_at)
	 SELECT ?,group_id,delivery_id,conversation_id,member_id,'duplicate-claim','unknown',created_at,updated_at
	 FROM turns WHERE id=?`, duplicateID, b.TurnID)
	coordinationDecideExec(t, s, "INSERT INTO resource_claims(resource_key,turn_id) VALUES('discussion-held',?)", duplicateID)

	discussionCheckMessageClosure(t, s, f.groupID, invite.Message.ID, "")
	var owner string
	if err := s.db.QueryRow("SELECT turn_id FROM resource_claims WHERE resource_key='discussion-held'").Scan(&owner); err != nil || owner != duplicateID {
		t.Fatalf("resource holder = %q, %v; want %q", owner, err, duplicateID)
	}
}

func TestChatDiscussionClosureDoesNotFollowAnAdvancedTurn(t *testing.T) {
	f, origin, invite, b := discussionSerialHandoff(t)
	s := f.s
	queueEvent(t, s, b, provider.AcceptedEvent, "", "")
	peer, cb := discussionCapture(t, s, b)
	discussionAssess(t, s, peer, cb)
	answer := discussionApply(t, s, peer, cb, "reply", domain.ChatDiscussionAction{Kind: "reply", Content: "已核对结论。"})
	if turn, err := s.ApplyTurnReceipt(ctx, b, queueReceipt(provider.OutcomeFailed)); err != nil || turn.State != domain.TurnStateFailed {
		t.Fatalf("recipient did not settle after the structured reply: %+v, %v", turn, err)
	}
	for _, id := range []string{origin.ID, invite.Message.ID, answer.Message.ID} {
		discussionCheckMessageClosure(t, s, f.groupID, id, "")
	}
	a, ca := discussionArrive(t, s, f.groupID, answer.Event.DeliveryID)
	discussionCheckContext(t, s, a, ca, origin, f.a, f.a, "", answer.Message.ID, answer.Message.ID, false, invite, answer)
	discussionAssess(t, s, a, ca)
	resolved := discussionApply(t, s, a, ca, "resolve", domain.ChatDiscussionAction{Kind: "resolve", Content: "结论已确认。"})
	discussionCheckContext(t, s, a, ca, origin, f.a, "", "", answer.Message.ID, resolved.Message.ID, true, invite, answer, resolved)
	discussionEnd(t, s, a)
	discussionCheckMessageClosure(t, s, f.groupID, answer.Message.ID, "")
	discussionCheckMessageClosure(t, s, f.groupID, resolved.Message.ID, "")
}

func TestChatDiscussionClosureRequiresCanonicalBoundTurn(t *testing.T) {
	f := setupMessages(t)
	s := f.s
	origin := discussionUser(t, f, "evidence", f.a)
	a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
	discussionAssess(t, s, a, ca)
	invite := discussionApply(t, s, a, ca, "invite", domain.ChatDiscussionAction{Kind: "invite", Content: "请核对结论。", RecipientID: f.b})
	b, _ := discussionArrive(t, s, f.groupID, invite.Event.DeliveryID)
	evidence := queueReceipt(provider.OutcomeFailed)
	if _, err := s.ApplyTurnReceipt(ctx, b.Claim, evidence); err != nil {
		t.Fatal(err)
	}
	checkOpen := func() {
		t.Helper()
		discussionCheckMessageClosure(t, s, f.groupID, invite.Message.ID, "")
		discussionCheckContext(t, s, a, ca, origin, f.a, f.b, invite.Message.ID, origin.ID, invite.Message.ID, false, invite)
	}
	canonical := queueReceiptJSON(t, s, b.Claim)
	for _, receipt := range []string{"", "{", `{"outcome":"failed","terminal":"failed","started":true,"accepted":true}`,
		strings.Replace(canonical, `"output_complete":true`, `"output_complete":false`, 1),
		strings.Replace(canonical, evidence.NativeSessionID, "unbound-session", 1)} {
		coordinationDecideExec(t, s, "UPDATE turns SET receipt_json=? WHERE id=?", receipt, b.Claim.TurnID)
		checkOpen()
	}
	coordinationDecideExec(t, s, "UPDATE turns SET receipt_json=? WHERE id=?", canonical, b.Claim.TurnID)
	other := queueDelivery(t, s, f.groupID, f.b, origin.TopicID)
	otherTurn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: other.ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTurnReceipt(ctx, TurnClaim{GroupID: f.groupID, TurnID: otherTurn.ID, Token: otherTurn.ClaimToken}, evidence); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, s, "UPDATE deliveries SET turn_id=? WHERE id=?", otherTurn.ID, invite.Event.DeliveryID)
	checkOpen()
	// Clearing the delivery pointer cannot turn a claimed delivery into a departure.
	coordinationDecideExec(t, s, "UPDATE deliveries SET turn_id='',state='failed',error=? WHERE id=?", chatDiscussionRecipientLeftReason, invite.Event.DeliveryID)
	checkOpen()
	coordinationDecideExec(t, s, "UPDATE deliveries SET turn_id=?,state='accepted',error='' WHERE id=?", b.Claim.TurnID, invite.Event.DeliveryID)
	const reason = "讨论已中断：接收成员的轮次异常结束，未提交结构化讨论动作"
	got, err := s.ReadChatDiscussionContext(ctx, a, ca)
	if err != nil || got == nil || !got.Closed || got.ClosureReason != reason || got.HeadMessageID != invite.Message.ID ||
		got.ActiveOwnerID != "" || got.ActiveInvitationMessageID != "" || !reflect.DeepEqual(got.Events, []domain.ChatDiscussionReceipt{invite}) {
		t.Fatalf("active tool did not see the same interrupted head: %+v, %v", got, err)
	}
	discussionCheckMessageClosure(t, s, f.groupID, invite.Message.ID, reason)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	closures, err := chatDiscussionClosuresInTx(ctx, tx, f.groupID, origin.ID, invite.Message.ID, other.MessageID)
	if err != nil || !reflect.DeepEqual(closures, map[string]string{invite.Message.ID: reason}) {
		t.Fatalf("bounded batch did not isolate the interrupted head: %v, %v", closures, err)
	}
}

func TestGetMessageDiscussionClosureRegisteredDamage(t *testing.T) {
	f := setupMessages(t)
	message := discussionUser(t, f, "damage", f.a)
	coordinationDecideExec(t, f.s, "DROP TABLE chat_discussion_events")
	if got, err := f.s.GetMessage(ctx, f.groupID, message.ID); err == nil || errors.Is(err, ErrNotFound) || got.ID != "" {
		t.Fatalf("message read hid registered discussion schema damage: %+v, %v", got, err)
	}
	if _, err := f.s.GetMessage(ctx, f.otherID, message.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign group reached the closure query: %v", err)
	}
}
