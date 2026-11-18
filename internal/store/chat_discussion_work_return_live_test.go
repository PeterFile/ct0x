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

func checkWorkReturnIntakeResource(t *testing.T, f workReturnFixture, held bool) {
	t.Helper()
	var turn string
	err := f.s.db.QueryRow(`SELECT turn_id FROM resource_claims WHERE resource_key=?`, workReturnIntakeResource).Scan(&turn)
	if (held && (err != nil || turn != f.intake.Claim.TurnID)) || (!held && !errors.Is(err, sql.ErrNoRows)) {
		t.Fatalf("intake resource holder=%s, err=%v; want held=%t by %s", turn, err, held, f.intake.Claim.TurnID)
	}
}

func checkWorkReturnWake(t *testing.T, f workReturnFixture) domain.TaskWorkHandback {
	t.Helper()
	s := f.s
	counts := workReportWakeCounts(t, s)
	h := workReturnHandback(t, s, f.owner)
	r := readWorkReturn(t, s, h)
	if h.AuthorTurnID != f.owner.Claim.TurnID || h.AuthorAttemptID != f.ownerAttempt || h.TaskID != f.admission.TaskID || h.MandateID != f.admission.MandateID ||
		r == nil || r.ThreadRef != f.admission.ThreadRef || r.OriginMessageID != f.candidate.MessageID || r.InvitationMessageID != f.invite.Message.ID ||
		r.RecipientID != f.a || r.RecipientID == f.intake.MemberID || r.DeliveryID == "" ||
		workReportWakeCounts(t, s) != ([3]int{counts[0] + 2, counts[1] + 1, counts[2]}) {
		t.Fatalf("expected exact B handback, A publication and one wake: %+v / %+v", h, r)
	}
	m, err := s.GetMessage(ctx, f.groupID, r.MessageID)
	if err != nil || m.TopicID != h.TopicID || len(m.Deliveries) != 1 || m.Deliveries[0].ID != r.DeliveryID ||
		m.Deliveries[0].RecipientID != f.a || m.Deliveries[0].State != domain.DeliveryStateQueued || m.Deliveries[0].TurnID != "" {
		t.Fatalf("return did not queue just the original root: %+v %v", m, err)
	}
	notice, err := s.GetMessage(ctx, f.groupID, h.NoticeMessageID)
	if err != nil || notice.ID == m.ID || notice.SourceID != "work:handback:"+h.ID || len(notice.Deliveries) != 0 {
		t.Fatalf("B notice must remain separate and non-waking: %+v %v", notice, err)
	}
	return h
}

func TestChatDiscussionWorkReturnWhileIntakeActive(t *testing.T) {
	requireWorkReturnSchema(t)
	f := setupWorkReturnAdmission(t)
	f.startWorker(t) // Real dispatch and Worker attempt while the invite is still running.
	s := f.s
	queueEvent(t, s, f.intake.Claim, provider.ResponseEvent, "正在核对。", "")
	intake := queueTurn(t, s, f.intake.Claim)
	if intake.State != domain.TurnStateRunning || intake.AcceptedAt == nil || intake.CandidateTerminal != "" {
		t.Fatalf("intake must be known active: %+v", intake)
	}
	// The same active intake cannot consume the path via an ordinary directed step.
	if r, err := s.ApplyChatDiscussion(ctx, f.intake, f.admission.CandidateID,
		domain.ChatDiscussionAction{Kind: domain.ChatDiscussionReply, Content: "并发下尝试沿原路径回复。"}, "late-reply"); !errors.Is(err, ErrMemberToolDenied) || r.Message.ID != "" {
		t.Fatalf("A5 did not seal the admitted head: %+v %v", r, err)
	}
	checkWorkReturnIntakeResource(t, f, true)
	h := checkWorkReturnWake(t, f)
	if after := queueTurn(t, s, f.intake.Claim); !reflect.DeepEqual(after, intake) {
		t.Fatalf("return changed active intake: %+v; was %+v", after, intake)
	}
	checkWorkReturnIntakeResource(t, f, true)
	work := map[string]string{"task_id": h.TaskID, "mandate_id": h.MandateID, "dispatch_request_id": f.admission.DispatchRequestID,
		"assignment_id": h.AssignmentID, "worker_member_id": h.OwnerMemberID}
	checkProjection := func() {
		t.Helper()
		m := checkDiscussionWorkMessage(t, s, f.groupID, f.invite.Message.ID, work, "") // GetMessage and ListMessages.
		if d := m.Deliveries; m.Discussion == nil || m.Discussion.Kind != domain.ChatDiscussionInvite || len(d) != 1 ||
			d[0].ID != f.invite.Event.DeliveryID || d[0].TurnID != intake.ID || d[0].State != domain.DeliveryStateAccepted {
			t.Fatalf("return changed invitation or acceptance projection: %+v", m)
		}
	}
	checkProjection()
	if c, err := s.ReadChatDiscussionContext(ctx, f.intake, f.admission.CandidateID); err != nil || c == nil ||
		!c.WorkAdmitted || !c.Closed || c.ClosureReason != "" || c.ActiveOwnerID != "" {
		t.Fatalf("admitted discussion did not stay closed to directed moves: %+v %v", c, err)
	}
	f.settleIntake(t)
	checkWorkReturnIntakeResource(t, f, false)
	checkProjection() // The trusted terminal still does not make this an interruption.
}

func TestChatDiscussionWorkReturnRejectsUnprovedLiveIntake(t *testing.T) {
	for _, name := range []string{"cancelling", "candidate-terminal", "extra-turn", "invalid-session", "rotated-live-claim", "cancel-timestamp", "missing-acceptance", "receipt-on-running"} {
		t.Run(name, func(t *testing.T) {
			requireWorkReturnSchema(t)
			f := setupWorkReturnAdmission(t)
			f.startWorker(t)
			s, claim := f.s, f.intake.Claim
			switch name {
			case "cancelling":
				if turn, err := s.RequestTurnCancel(ctx, f.groupID, claim.TurnID); err != nil || turn.State != domain.TurnStateCancelRequested {
					t.Fatalf("request cancellation: %+v %v", turn, err)
				}
			case "candidate-terminal":
				queueEvent(t, s, claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
			case "extra-turn":
				coordinationDecideExec(t, s, `INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,created_at,updated_at)
				 SELECT 'duplicate',group_id,delivery_id,conversation_id,member_id,'duplicate-claim','failed',created_at,updated_at FROM turns WHERE id=?`, claim.TurnID)
			case "invalid-session":
				coordinationDecideExec(t, s, `UPDATE turns SET native_session_id=' not-a-canonical-session ' WHERE id=?`, claim.TurnID)
			case "rotated-live-claim":
				coordinationDecideExec(t, s, `UPDATE turns SET claim_token=? WHERE id=?`, f.scope.Claim.Token, claim.TurnID)
			case "cancel-timestamp":
				coordinationDecideExec(t, s, `UPDATE turns SET cancel_requested_at=accepted_at WHERE id=?`, claim.TurnID)
			case "missing-acceptance":
				coordinationDecideExec(t, s, `UPDATE turns SET accepted_at=NULL WHERE id=?`, claim.TurnID)
			case "receipt-on-running":
				coordinationDecideExec(t, s, `UPDATE turns SET (receipt_json,reason)=(SELECT receipt_json,reason FROM turns WHERE id=?) WHERE id=?`,
					f.scope.Claim.TurnID, claim.TurnID)
			}
			assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied)
			checkWorkReturnIntakeResource(t, f, true)
			if name == "cancelling" {
				taskWorkTestReceipt(t, s, f.intake, queueReceipt(provider.OutcomeAborted), domain.TurnStateAborted)
				checkWorkReturnIntakeResource(t, f, false)
				checkWorkReturnWake(t, f)
			}
		})
	}
}

func TestChatDiscussionWorkReturnAfterIntakeReconciliation(t *testing.T) {
	requireWorkReturnSchema(t)
	f := setupWorkReturnAdmission(t)
	s, old := f.s, f.intake.Claim
	if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("recover the receiving Turn before dispatch: %d %v", n, err)
	}
	recovered := queueTurn(t, s, old)
	if recovered.ID != old.TurnID || recovered.State != domain.TurnStateUnknown || recovered.ClaimToken == old.Token || recovered.AcceptedAt == nil {
		t.Fatalf("recovery did not preserve and fence the accepted intake: %+v", recovered)
	}
	checkWorkReturnIntakeResource(t, f, true)
	f.startWorker(t) // A separate, currently authorized Worker can run during reconciliation.
	owner := queueTurn(t, s, f.owner.Claim)
	assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied)

	receipt := queueReceipt(provider.OutcomeSucceeded)
	if got, err := s.ApplyTurnReceipt(ctx, old, receipt); !errors.Is(err, ErrStaleClaim) || got.ID != "" {
		t.Fatalf("stale intake claim settled unknown: %+v %v", got, err)
	}
	checkWorkReturnIntakeResource(t, f, true)
	fresh := TurnClaim{GroupID: old.GroupID, TurnID: old.TurnID, Token: recovered.ClaimToken}
	incomplete := receipt
	incomplete.OutputComplete = false
	if got, err := s.ApplyTurnReceipt(ctx, fresh, incomplete); err != nil || got.State != domain.TurnStateUnknown {
		t.Fatalf("candidate terminal evidence settled unknown: %+v %v", got, err)
	}
	checkWorkReturnIntakeResource(t, f, true)
	assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied)
	if got, err := s.ApplyTurnReceipt(ctx, fresh, receipt); err != nil || got.State != domain.TurnStateSucceeded || got.ClaimToken != fresh.Token {
		t.Fatalf("trusted receipt failed to reconcile the original Turn: %+v %v", got, err)
	}
	checkWorkReturnIntakeResource(t, f, false)
	if after := queueTurn(t, s, f.owner.Claim); !reflect.DeepEqual(owner, after) {
		t.Fatalf("reconciliation changed the independent Worker claim: %+v", after)
	}
	checkWorkReturnWake(t, f) // The historical admission must survive the intake's rotation.

	if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("recover the Worker after a valid return: %d %v", n, err)
	}
	assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied) // A stored result is not current Worker authority.
	f.owner.Claim.Token = queueTurn(t, s, f.owner.Claim).ClaimToken
	assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied) // A current token on an unknown Worker is not authority either.
}

func TestChatDiscussionWorkReturnRejectsReplySource(t *testing.T) {
	f := setupWorkReturn(t)
	s := f.s
	const replyConflict = "讨论工作回传不能作为轮次回复"
	t.Run("before-marker", func(t *testing.T) {
		// Handback adds no delivery of its own. Point its exact author Turn at
		// the new A delivery's message before the return marker is inserted.
		coordinationDecideExec(t, s, `CREATE TRIGGER return_reply_fixture AFTER INSERT ON deliveries
		 BEGIN UPDATE turns SET reply_message_id=NEW.message_id WHERE id=(SELECT author_turn_id FROM task_work_handbacks
		 WHERE (group_id,topic_id)=(NEW.group_id,NEW.topic_id)); END`)
		defer coordinationDecideExec(t, s, `DROP TRIGGER return_reply_fixture`)
		before := messageSequenceSnapshot(t, s)
		// The public tool redacts SQL errors. Unlike a receipt-read denial,
		// the storage conflict must abort the transaction at insertion.
		if got, err := s.HandbackWork(ctx, f.owner, workReturnInput, workReturnKey); err == nil || errors.Is(err, ErrMemberToolDenied) || got != (domain.TaskWorkHandback{}) {
			t.Fatalf("return marker accepted an existing reply or reached readback: %+v %v", got, err)
		}
		messageSequencePreserved(t, s, before)
	})
	h := workReturnHandback(t, s, f.owner)
	r := readWorkReturn(t, s, h)
	if r == nil || r.DeliveryID == "" {
		t.Fatalf("missing valid return: %+v", r)
	}
	for _, tc := range []struct {
		name, query, turnSource string
	}{
		{"update", `UPDATE turns SET reply_message_id=? WHERE id=?`, h.AuthorTurnID},
		{"insert", `INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,reply_message_id,created_at,updated_at)
		 SELECT 'extra-reply',t.group_id,t.delivery_id,t.conversation_id,t.member_id,'extra-reply-claim','succeeded',?,t.created_at,t.updated_at
		 FROM turns t JOIN message_command_sources c ON c.turn_id=t.id WHERE c.message_id=?`, f.invite.Message.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, tc.query, r.MessageID, tc.turnSource); err == nil || !strings.Contains(err.Error(), replyConflict) {
				t.Fatalf("reply link did not hit the schema guard: %v", err)
			}
		})
	}
	t.Run("synthetic-corruption", func(t *testing.T) {
		coordinationDecideExec(t, s, `DROP TRIGGER IF EXISTS chat_discussion_work_returns_no_reply_update`)
		coordinationDecideExec(t, s, `UPDATE turns SET reply_message_id=? WHERE id=?`, r.MessageID, h.AuthorTurnID)
		if got, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, f.groupID, r.DeliveryID); !errors.Is(err, ErrMemberToolDenied) || got != (domain.ChatDiscussionWorkReturnReceipt{}) {
			t.Errorf("resolver fell back or accepted conflicting reply evidence: %+v %v", got, err)
		}
		m, err := s.GetMessage(ctx, f.groupID, r.MessageID)
		if err != nil || m.SourceKind != "member" || m.SourceID != "turn:"+h.AuthorTurnID {
			t.Fatalf("fixture did not inject conflicting generic member source: %+v %v", m, err)
		}
		turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: r.DeliveryID, MaxActive: 6})
		if err != nil {
			t.Fatal(err)
		}
		scope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
		if err != nil {
			t.Fatal(err)
		}
		before := messageSequenceSnapshot(t, s)
		if candidate, err := s.CaptureChatCandidate(ctx, scope); err == nil || candidate.ID != "" {
			t.Errorf("return acquired member intake authority: candidate=%s source=%s error=%v", candidate.ID, candidate.SourceKind, err)
		}
		messageSequencePreserved(t, s, before)
		assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied) // Even the exact B replay must fail closed.
	})
}
