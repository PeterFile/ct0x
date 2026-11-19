package store

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"modernc.org/sqlite"
)

func checkWorkReturnIntakeResource(t *testing.T, f workReturnFixture, held bool) {
	t.Helper()
	var turn string
	err := f.s.db.QueryRow(`SELECT turn_id FROM resource_claims WHERE resource_key=?`, workReturnIntakeResource).Scan(&turn)
	if (held && (err != nil || turn != f.intake.Claim.TurnID)) || (!held && !errors.Is(err, sql.ErrNoRows)) {
		t.Fatalf("intake resource holder=%s, err=%v; want held=%t by %s", turn, err, held, f.intake.Claim.TurnID)
	}
}

func checkWorkReturnWake(t *testing.T, f workReturnFixture) (domain.TaskWorkHandback, domain.ChatDiscussionWorkReturnReceipt) {
	t.Helper()
	s := f.s
	counts := workReportWakeCounts(t, s)
	h := workReturnHandback(t, s, f.owner)
	r := readWorkReturn(t, s, h)
	if h.AuthorTurnID != f.owner.Claim.TurnID || h.AuthorAttemptID != f.ownerAttempt || h.TaskID != f.admission.TaskID || h.MandateID != f.admission.MandateID ||
		r == nil || r.ThreadRef != f.admission.ThreadRef || r.OriginMessageID != f.candidate.MessageID || r.InvitationMessageID != f.invite.Message.ID ||
		r.RecipientID != f.a || r.RecipientID == f.intake.MemberID || r.ConversationID != f.scope.ConversationID || r.DeliveryID == "" ||
		workReportWakeCounts(t, s) != ([3]int{counts[0] + 2, counts[1] + 1, counts[2]}) {
		t.Fatalf("expected exact B handback, A publication and one wake: %+v / %+v", h, r)
	}
	checkWorkReturnDelivery(t, s, *r)
	notice, err := s.GetMessage(ctx, f.groupID, h.NoticeMessageID)
	if err != nil || notice.ID == r.MessageID || notice.SourceID != "work:handback:"+h.ID || len(notice.Deliveries) != 0 {
		t.Fatalf("B notice must remain separate and non-waking: %+v %v", notice, err)
	}
	return h, *r
}

func checkWorkReturnDelivery(t *testing.T, s *Store, r domain.ChatDiscussionWorkReturnReceipt) {
	t.Helper()
	if got, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, r.GroupID, r.DeliveryID); err != nil || got != r {
		t.Fatalf("wrong typed return route: %+v %v; want %+v", got, err, r)
	}
	m, err := s.GetMessage(ctx, r.GroupID, r.MessageID)
	if err != nil || m.TopicID != r.TopicID || len(m.Deliveries) != 1 || m.Deliveries[0].ID != r.DeliveryID ||
		m.Deliveries[0].RecipientID != r.RecipientID || m.Deliveries[0].ConversationID != r.ConversationID ||
		m.Deliveries[0].State != domain.DeliveryStateQueued || m.Deliveries[0].TurnID != "" {
		t.Fatalf("return did not queue just the original root: %+v %v", m, err)
	}
}

func TestChatDiscussionWorkReturnQueueRouting(t *testing.T) {
	f := setupWorkReturn(t)
	s := f.s
	_, r := checkWorkReturnWake(t, f)
	message, err := s.GetMessage(ctx, r.GroupID, r.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	// Matching visible text is not a typed marker, even in the same topic.
	ordinary, err := s.CreateMessage(ctx, MessageRequest{GroupID: r.GroupID, TopicID: r.TopicID,
		ActorID: r.SenderID, Content: message.Content}, []string{r.RecipientID})
	if err != nil {
		t.Fatal(err)
	}
	d := ordinary.Deliveries[0]
	page, err := s.ListPendingDeliveries(ctx, PendingOptions{Lane: "work"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, pending := range page.Items {
		if pending.GroupID == r.GroupID {
			seen[pending.Delivery.ID] = true
		}
	}
	if !seen[r.DeliveryID] || !seen[d.ID] {
		t.Fatal("fixture did not expose both deliveries to production preparation")
	}
	before := coordinationDecideSnapshot(t, s)
	if got, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, r.GroupID, d.ID); got != (domain.ChatDiscussionWorkReturnReceipt{}) || err != ErrNotFound {
		t.Fatalf("ordinary delivery must have exact typed absence: %+v %v", got, err)
	}
	want := CoordinationRoleState{Provenance: CoordinationRoleProvenance{Kind: CoordinationRoleOrdinary, GroupID: r.GroupID,
		DeliveryID: r.DeliveryID, MessageID: r.MessageID, ConversationID: r.ConversationID, MemberID: r.RecipientID, TopicID: r.TopicID},
		ProviderID: "pi", Model: "model"}
	held := want
	held.Blocked = CoordinationDiscussionWorkReturnHandlerRequired
	if got, err := s.ResolveCoordinationDelivery(ctx, r.GroupID, r.DeliveryID); err != nil || got != held {
		t.Fatalf("production resolver did not quietly hold the typed delivery: %+v %v", got, err)
	}
	untyped := want
	untyped.Provenance.DeliveryID, untyped.Provenance.MessageID = d.ID, d.MessageID
	if got, err := s.ResolveCoordinationDelivery(ctx, r.GroupID, d.ID); err != nil || got != untyped {
		t.Fatalf("unrelated ordinary work no longer prepares: %+v %v", got, err)
	}
	probe, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, r.GroupID, r.DeliveryID)
	if err != nil || probe != r {
		t.Fatalf("server probe: %+v %v", probe, err)
	}
	if got, err := s.ResolveCoordinationDiscussionWorkReturnDelivery(ctx, r.GroupID, r.DeliveryID, probe); err != nil || got != want {
		t.Fatalf("exact receipt must return ready ordinary identity without Work provenance: %+v %v", got, err)
	}
	deny := func(t *testing.T, group, delivery string, receipt domain.ChatDiscussionWorkReturnReceipt) {
		t.Helper()
		if got, err := s.ResolveCoordinationDiscussionWorkReturnDelivery(ctx, group, delivery, receipt); got != (CoordinationRoleState{}) ||
			!errors.Is(err, ErrMemberToolDenied) || errors.Is(err, ErrNotFound) {
			t.Fatalf("failed typed reproof must never allow fallback: %+v %v", got, err)
		}
	}
	for _, field := range reflect.VisibleFields(reflect.TypeOf(probe)) {
		if field.Type.Kind() == reflect.String {
			t.Run(field.Name, func(t *testing.T) {
				bad := probe
				v := reflect.ValueOf(&bad).Elem().FieldByIndex(field.Index)
				v.SetString(v.String() + "-mismatch")
				deny(t, r.GroupID, r.DeliveryID, bad)
			})
		}
	}
	deny(t, f.otherID, r.DeliveryID, probe)
	deny(t, r.GroupID, d.ID, probe)
	deny(t, r.GroupID, "missing", probe)
	if got, err := s.ResolveCoordinationDelivery(ctx, r.GroupID, r.DeliveryID); err != nil || got != held {
		t.Fatalf("typed read must not unlock the legacy resolver: %+v %v", got, err)
	}
	coordinationDecidePreserved(t, s, before)

	t.Run("inactive-recipient", func(t *testing.T) {
		coordinationDecideExec(t, s, `UPDATE members SET status='left' WHERE id=?`, r.RecipientID)
		defer coordinationDecideExec(t, s, `UPDATE members SET status='active' WHERE id=?`, r.RecipientID)
		inactive := want
		inactive.Blocked = CoordinationMemberInactive
		if got, err := s.ResolveCoordinationDelivery(ctx, r.GroupID, r.DeliveryID); err != nil || got != inactive {
			t.Fatalf("generic route did not preserve recipient inactivity: %+v %v", got, err)
		}
		if got, err := s.ResolveCoordinationDiscussionWorkReturnDelivery(ctx, r.GroupID, r.DeliveryID, probe); err != nil || got != inactive {
			t.Fatalf("exact receipt did not preserve recipient inactivity: %+v %v", got, err)
		}
	})

	// This is a read seam, not a claim gate. Another claimant after the probe
	// invalidates preparation, without retiring the historical publication.
	if _, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: r.DeliveryID, MaxActive: 6}); err != nil {
		t.Fatal(err)
	}
	before = coordinationDecideSnapshot(t, s)
	deny(t, r.GroupID, r.DeliveryID, probe)
	if got, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, r.GroupID, r.DeliveryID); err != nil || got != probe {
		t.Fatalf("claim changed the historical receipt: %+v %v", got, err)
	}
	coordinationDecidePreserved(t, s, before)
}

func TestChatDiscussionWorkReturnDamagedDeliveryReference(t *testing.T) {
	f := setupWorkReturn(t)
	s := f.s
	_, r := checkWorkReturnWake(t, f)
	var ddl string
	if err := s.db.QueryRow(`SELECT sql FROM main.sqlite_schema WHERE name='chat_discussion_work_returns_update'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, s, `DROP TRIGGER chat_discussion_work_returns_update`)
	coordinationDecideExec(t, s, `UPDATE chat_discussion_work_returns SET delivery_id=NULL WHERE handback_id=?`, r.HandbackID)
	coordinationDecideExec(t, s, ddl) // Intact DDL; only the indexed message link still identifies the marker.
	before := coordinationDecideSnapshot(t, s)
	if got, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, r.GroupID, r.DeliveryID); got != (domain.ChatDiscussionWorkReturnReceipt{}) ||
		!errors.Is(err, ErrMemberToolDenied) || errors.Is(err, ErrNotFound) {
		t.Errorf("typed probe hid the damaged marker: %+v %v", got, err)
	}
	if got, err := s.ResolveCoordinationDelivery(ctx, r.GroupID, r.DeliveryID); got != (CoordinationRoleState{}) ||
		!errors.Is(err, ErrMemberToolDenied) || errors.Is(err, ErrNotFound) || errors.Unwrap(err) == ErrMemberToolDenied {
		t.Errorf("legacy route treated damaged evidence as absence or as an overridable delegation denial: %+v %v", got, err)
	}
	if got, err := s.ResolveCoordinationDiscussionWorkReturnDelivery(ctx, r.GroupID, r.DeliveryID, r); got != (CoordinationRoleState{}) ||
		!errors.Is(err, ErrMemberToolDenied) || errors.Is(err, ErrNotFound) {
		t.Errorf("stale exact receipt bypassed damaged persisted evidence: %+v %v", got, err)
	}
	coordinationDecidePreserved(t, s, before)
}

func assertNoWorkReturnDiscussionReply(t *testing.T, f workReturnFixture, scope MemberToolScope) {
	t.Helper()
	if r, err := f.s.ApplyChatDiscussion(ctx, scope, f.admission.CandidateID,
		domain.ChatDiscussionAction{Kind: domain.ChatDiscussionReply, Content: "尝试沿原路径回复。"}, "late-reply"); !errors.Is(err, ErrMemberToolDenied) || r.Message.ID != "" {
		t.Fatalf("admitted intake advanced the structured discussion: %+v %v", r, err)
	}
}

func checkWorkReturnNativeReply(t *testing.T, f workReturnFixture, turn domain.Turn, text string, before [3]int) {
	t.Helper()
	if after := workReportWakeCounts(t, f.s); turn.ReplyMessageID == "" || after != ([3]int{before[0] + 1, before[1], before[2]}) {
		t.Fatalf("trusted settlement must publish one reply without a wake: reply=%q, counts=%v (was %v)", turn.ReplyMessageID, after, before)
	}
	m := checkDiscussionWorkMessage(t, f.s, f.groupID, turn.ReplyMessageID, nil, "") // Public detail and timeline.
	if m.TopicID != f.intake.TopicID || m.SenderID != f.intake.MemberID || m.Content != text ||
		m.SourceKind != "member" || m.SourceID != "turn:"+f.intake.Claim.TurnID || len(m.Deliveries) != 0 || m.Discussion != nil {
		t.Fatalf("native output was not an ordinary public reply: %+v", m)
	}
}

func TestChatDiscussionWorkReturnWhileIntakeActive(t *testing.T) {
	requireWorkReturnSchema(t)
	f := setupWorkReturnAdmission(t)
	f.startWorker(t) // Real dispatch and Worker attempt while the invite is still running.
	s := f.s
	const text = "正在核对。"
	counts := workReportWakeCounts(t, s)
	queueEvent(t, s, f.intake.Claim, provider.ResponseEvent, text, "")
	intake := queueTurn(t, s, f.intake.Claim)
	if intake.State != domain.TurnStateRunning || intake.AcceptedAt == nil || intake.CandidateTerminal != "" ||
		intake.CandidateText != text || intake.ReplyMessageID != "" {
		t.Fatalf("intake must be known active with private candidate output: %+v", intake)
	}
	assertNoWorkReturnDiscussionReply(t, f, f.intake) // A5 seals the head before handback.
	if after := workReportWakeCounts(t, s); after != counts {
		t.Fatalf("active intake published a message or delivery: %v; was %v", after, counts)
	}
	checkWorkReturnIntakeResource(t, f, true)
	h, returned := checkWorkReturnWake(t, f)
	counts = workReportWakeCounts(t, s)
	assertNoWorkReturnDiscussionReply(t, f, f.intake) // A7 must not reopen the still-live claim's path.
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
		!c.WorkAdmitted || !c.Closed || c.ClosureReason != "" || c.ActiveOwnerID != "" ||
		c.HeadMessageID != f.invite.Message.ID || c.ActiveInvitationMessageID != "" {
		t.Fatalf("admitted discussion did not stay closed to directed moves: %+v %v", c, err)
	}
	f.settleIntake(t)
	checkWorkReturnIntakeResource(t, f, false)
	checkWorkReturnNativeReply(t, f, queueTurn(t, s, f.intake.Claim), text, counts)
	checkProjection() // The trusted terminal still does not make this an interruption.
	checkWorkReturnDelivery(t, s, returned)
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
	const text = "已核对讨论材料。"
	counts := workReportWakeCounts(t, s)
	queueEvent(t, s, old, provider.ResponseEvent, text, "")
	if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("recover the receiving Turn before dispatch: %d %v", n, err)
	}
	recovered := queueTurn(t, s, old)
	if recovered.ID != old.TurnID || recovered.State != domain.TurnStateUnknown || recovered.ClaimToken == old.Token || recovered.AcceptedAt == nil ||
		recovered.CandidateText != text || recovered.ReplyMessageID != "" || workReportWakeCounts(t, s) != counts {
		t.Fatalf("recovery did not preserve and fence the accepted intake and its private output: %+v", recovered)
	}
	checkWorkReturnIntakeResource(t, f, true)
	f.startWorker(t) // A separate, currently authorized Worker can run during reconciliation.
	owner := queueTurn(t, s, f.owner.Claim)
	assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied)

	counts = workReportWakeCounts(t, s)
	receipt := queueReceipt(provider.OutcomeSucceeded)
	if got, err := s.ApplyTurnReceipt(ctx, old, receipt); !errors.Is(err, ErrStaleClaim) || got.ID != "" {
		t.Fatalf("stale intake claim settled unknown: %+v %v", got, err)
	}
	assertNoWorkReturnDiscussionReply(t, f, f.intake)
	checkWorkReturnIntakeResource(t, f, true)
	fresh := f.intake
	fresh.Claim.Token = recovered.ClaimToken
	incomplete := receipt
	incomplete.OutputComplete = false
	if got, err := s.ApplyTurnReceipt(ctx, fresh.Claim, incomplete); err != nil || got.State != domain.TurnStateUnknown || got.ReplyMessageID != "" {
		t.Fatalf("incomplete receipt settled unknown or published a reply: %+v %v", got, err)
	}
	assertNoWorkReturnDiscussionReply(t, f, fresh) // Rotation grants no tool authority while unknown.
	checkWorkReturnIntakeResource(t, f, true)
	assertNoWorkReturn(t, s, f.owner, ErrMemberToolDenied)
	if after := workReportWakeCounts(t, s); after != counts {
		t.Fatalf("unknown intake published a message or delivery: %v; was %v", after, counts)
	}
	settled, err := s.ApplyTurnReceipt(ctx, fresh.Claim, receipt)
	if err != nil || settled.ID != old.TurnID || settled.State != domain.TurnStateSucceeded || settled.ClaimToken != fresh.Claim.Token {
		t.Fatalf("trusted receipt failed to reconcile the original Turn: %+v %v", settled, err)
	}
	assertNoWorkReturnDiscussionReply(t, f, fresh)
	checkWorkReturnNativeReply(t, f, settled, text, counts)
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
		const trigger = "chat_discussion_work_returns_no_reply_update"
		var ddl string
		if err := s.db.QueryRow(`SELECT sql FROM main.sqlite_schema WHERE type='trigger' AND name=?`, trigger).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		coordinationDecideExec(t, s, `DROP TRIGGER `+trigger)
		coordinationDecideExec(t, s, `UPDATE turns SET reply_message_id=? WHERE id=?`, r.MessageID, h.AuthorTurnID)
		coordinationDecideExec(t, s, ddl) // Restore the schema so the reader must reject the bad row itself.
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

func TestChatDiscussionWorkReturnSchemaGuard(t *testing.T) {
	const dropMarker = `DELETE FROM main.schema_migrations WHERE version=31`
	const dropTable = `DROP TABLE main.chat_discussion_work_returns`
	damageLiveLedger := func(t *testing.T, s *Store) {
		t.Helper()
		messageSequenceLedger(t, s, 31)
		// Damage only the registration on the already open DB. Reopening via
		// Open would run migrations and exercise a different failure path.
		coordinationDecideExec(t, s, dropMarker)
		messageSequenceLedger(t, s, 30)
	}
	checkHandback := func(t *testing.T, f workReturnFixture) {
		t.Helper()
		if h, err := f.s.HandbackWork(ctx, f.owner, workReturnInput, workReturnKey); h != (domain.TaskWorkHandback{}) ||
			!errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			t.Errorf("schema damage acknowledged a Handback or returned the wrong error: handback=%q notice=%q err=%v", h.ID, h.NoticeMessageID, err)
		}
	}
	checkResolve := func(t *testing.T, s *Store, groupID, deliveryID string, want error) {
		t.Helper()
		if got, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, groupID, deliveryID); got != (domain.ChatDiscussionWorkReturnReceipt{}) ||
			!errors.Is(err, want) || (want != ErrNotFound && errors.Is(err, ErrNotFound)) {
			t.Errorf("wrong return classification: %+v %v; want %v", got, err, want)
		}
	}

	t.Run("new-handback-without-marker", func(t *testing.T) {
		f := setupWorkReturn(t)
		damageLiveLedger(t, f.s)
		before := coordinationDecideSnapshot(t, f.s)
		checkHandback(t, f)
		coordinationDecidePreserved(t, f.s, before) // Includes the B notice/key and any A publication or wake.
	})
	t.Run("committed-return-without-marker", func(t *testing.T) {
		f := setupWorkReturn(t)
		_, r := checkWorkReturnWake(t, f)
		checkResolve(t, f.s, f.groupID, "unknown-delivery", ErrNotFound)
		damageLiveLedger(t, f.s)
		before := coordinationDecideSnapshot(t, f.s)
		checkResolve(t, f.s, r.GroupID, r.DeliveryID, ErrConflict)
		checkHandback(t, f) // The exact replay must not redeliver or acknowledge the damage.
		coordinationDecidePreserved(t, f.s, before)
	})

	t.Run("new-handback-without-protection", func(t *testing.T) {
		f := setupWorkReturn(t)
		coordinationDecideExec(t, f.s, `DROP TRIGGER chat_discussion_work_return_deliveries_identity`)
		messageSequenceLedger(t, f.s, 31)
		before := coordinationDecideSnapshot(t, f.s)
		checkHandback(t, f)
		coordinationDecidePreserved(t, f.s, before)
	})

	for _, name := range []string{"lookalike-table", "missing-composite-foreign-key", "no-op-protection"} {
		t.Run(name, func(t *testing.T) {
			f := setupWorkReturn(t)
			_, r := checkWorkReturnWake(t, f)
			s := f.s
			switch name {
			case "lookalike-table":
				// Keep all columns and lookup keys, but lose the marker, FKs and table triggers.
				coordinationDecideExec(t, s, dropTable+`; CREATE TABLE chat_discussion_work_returns (
				 origin_message_id TEXT PRIMARY KEY NOT NULL,
				 group_id TEXT NOT NULL,
				 topic_id TEXT NOT NULL,
				 invitation_message_id TEXT NOT NULL UNIQUE,
				 handback_id TEXT NOT NULL UNIQUE,
				 message_id TEXT NOT NULL UNIQUE,
				 delivery_id TEXT UNIQUE,
				 CHECK(message_id NOT IN (origin_message_id,invitation_message_id))
				) STRICT, WITHOUT ROWID`)
			case "missing-composite-foreign-key":
				// Keep the other FKs and restore every trigger to isolate the composite FK check.
				defs := messageSequenceRows(t, s.db, `SELECT sql FROM main.sqlite_schema
				 WHERE tbl_name='chat_discussion_work_returns' AND type IN ('table','trigger') ORDER BY type,name`)
				if len(defs) != 4 {
					t.Fatalf("expected the real table and its three attached triggers, got %d definitions", len(defs))
				}
				const fk = ",\n    FOREIGN KEY(origin_message_id,invitation_message_id) REFERENCES chat_discussion_events(origin_message_id,message_id)"
				table := defs[0][0].(string)
				withoutFK := strings.Replace(table, fk, "", 1)
				if withoutFK == table {
					t.Fatal("fixture did not remove the composite foreign key")
				}
				coordinationDecideExec(t, s, dropTable)
				coordinationDecideExec(t, s, withoutFK)
				for _, row := range defs[1:] {
					coordinationDecideExec(t, s, row[0].(string))
				}
			case "no-op-protection":
				coordinationDecideExec(t, s, `DROP TRIGGER chat_discussion_work_returns_delete;
				 CREATE TRIGGER chat_discussion_work_returns_delete BEFORE DELETE ON chat_discussion_work_returns
				 BEGIN SELECT 1; END`)
			}
			messageSequenceLedger(t, s, 31)
			before := coordinationDecideSnapshot(t, s)
			checkResolve(t, s, r.GroupID, r.DeliveryID, ErrConflict)
			checkHandback(t, f)
			coordinationDecidePreserved(t, s, before)
		})
	}

	for _, tc := range []struct{ name, damage string }{
		{"marker-without-table", dropTable},
		{"retained-delivery-triggers", dropTable + "; " + dropMarker + `;
		 DROP TRIGGER chat_discussion_work_returns_seal;
		 DROP TRIGGER chat_discussion_work_returns_no_command;
		 DROP TRIGGER chat_discussion_work_returns_no_reply_insert;
		 DROP TRIGGER chat_discussion_work_returns_no_reply_update;
		 DROP TRIGGER chat_discussion_work_returns_no_authority`},
		{"view-for-table", dropTable + `; CREATE VIEW chat_discussion_work_returns AS
		 SELECT id AS handback_id,group_id,message_id,id AS delivery_id FROM deliveries WHERE 0`},
		{"wrong-table", dropTable + `; CREATE TABLE chat_discussion_work_returns AS
		 SELECT id AS handback_id,group_id,message_id,id AS delivery_id FROM deliveries WHERE 0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupMessages(t)
			coordinationDecideExec(t, f.s, tc.damage)
			checkResolve(t, f.s, f.groupID, "unknown-delivery", ErrConflict)
		})
	}
	for _, tc := range []struct{ name, damage string }{
		{"ledger-gap", `DELETE FROM main.schema_migrations WHERE version=17`},
		{"future-marker", `INSERT INTO main.schema_migrations(version) VALUES(32)`},
		{"replaced-marker", `UPDATE main.schema_migrations SET version=32 WHERE version=17`},
		{"missing-fk-parent", `DROP TABLE main.task_work_handbacks`},
		{"missing-guard-dependency", `DROP TABLE main.chat_candidates`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupMessages(t)
			d := queueDelivery(t, f.s, f.groupID, f.a, "schema-guard")
			// The full v31 DDL remains, including trigger bodies that use the missing tables.
			coordinationDecideExec(t, f.s, tc.damage)
			checkResolve(t, f.s, f.groupID, d.ID, ErrConflict)
			if state, err := f.s.ResolveCoordinationDelivery(ctx, f.groupID, d.ID); state != (CoordinationRoleState{}) ||
				!errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
				t.Errorf("legacy resolver treated damaged schema as compatible: %+v %v", state, err)
			}
		})
	}
	t.Run("all-v31-evidence-erased-after-open", func(t *testing.T) {
		f := setupWorkReturn(t)
		_, r := checkWorkReturnWake(t, f)
		for _, row := range messageSequenceRows(t, f.s.db, `SELECT name FROM main.sqlite_schema
		 WHERE type='trigger' AND name GLOB 'chat_discussion_work_return[s_]*'`) {
			coordinationDecideExec(t, f.s, "DROP TRIGGER main."+row[0].(string))
		}
		coordinationDecideExec(t, f.s, dropTable+"; "+dropMarker)
		messageSequenceLedger(t, f.s, 30)
		before := messageSequenceSnapshot(t, f.s)
		checkResolve(t, f.s, r.GroupID, r.DeliveryID, ErrConflict)
		if state, err := f.s.ResolveCoordinationDelivery(ctx, r.GroupID, r.DeliveryID); state != (CoordinationRoleState{}) ||
			!errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			t.Errorf("erasing all v31 evidence made the existing wake ordinary: %+v %v", state, err)
		}
		checkHandback(t, f)
		messageSequencePreserved(t, f.s, before)
	})
	t.Run("sqlite-execution-error", func(t *testing.T) {
		f := setupMessages(t)
		coordinationDecideExec(t, f.s, `DROP TABLE main.schema_migrations`)
		var cause *sqlite.Error
		if got, err := f.s.ResolveChatDiscussionWorkReturnDelivery(ctx, f.groupID, "unknown-delivery"); got != (domain.ChatDiscussionWorkReturnReceipt{}) ||
			!errors.As(err, &cause) || errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), "no such table") {
			t.Fatalf("SQLite error was lost, exposed or reclassified as a schema mismatch: %+v %v", got, err)
		}
	})
}
