package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// Build a two-hop discussion with a scoped investigation worker.
// The original inviter, last inviter, assessor and coordinator are distinct.
func a7HandbackOwner(t *testing.T) (chatFixture, domain.ChatDiscussionReceipt, CoordinationToolScope) {
	t.Helper()
	f := chatSetup(t, "user", false)
	s := f.s
	coordinator, assessor := member(t, s, f.groupID).ID, member(t, s, f.groupID).ID
	if _, err := s.SetCoordinator(ctx, f.groupID, coordinator, 1, "a7-coordinator"); err != nil {
		t.Fatal(err)
	}
	investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
	s.EnableInvestigationExecution()
	chatNextCandidate(t, &f, true)
	settle := func(scope MemberToolScope) {
		t.Helper()
		taskWorkTestReceipt(t, s, scope, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
	}
	queueEvent(t, s, f.scope.Claim, provider.AcceptedEvent, "", "")
	discussionAssess(t, s, f.scope, f.candidate.ID)
	first := discussionApply(t, s, f.scope, f.candidate.ID, "a7-start", domain.ChatDiscussionAction{
		Kind: domain.ChatDiscussionInvite, RecipientID: f.b, Content: "请核对这份摘要。"})
	settle(f.scope)
	middle, mid := discussionArrive(t, s, f.groupID, first.Event.DeliveryID)
	queueEvent(t, s, middle.Claim, provider.AcceptedEvent, "", "")
	discussionAssess(t, s, middle, mid)
	head := discussionApply(t, s, middle, mid, "a7-forward", domain.ChatDiscussionAction{
		Kind: domain.ChatDiscussionInvite, RecipientID: assessor, Content: "请评估未决问题。"})
	settle(middle)
	intake, candidate := discussionArrive(t, s, f.groupID, head.Event.DeliveryID)
	queueEvent(t, s, intake.Claim, provider.AcceptedEvent, "", "")
	admitted, err := s.ApplyChatAssessment(ctx, intake, candidate, investigationProposal(), "已核对现有范围。", "a7-admit")
	if err != nil || admitted.Outcome != "admitted" {
		t.Fatalf("structured admission: %+v %v", admitted, err)
	}
	settle(intake)
	dispatch, err := s.GetDispatchRequest(ctx, f.groupID, admitted.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	r := dispatch.Request
	co := coordinationDecideBindRequest(t, s, r)
	decision, err := s.DecideDispatchWithCapabilities(ctx, co, CoordinatorDecisionInput{
		Kind: "assign", WorkerMemberID: f.b, Reason: "分配给受限调查成员",
	}, "a7-assign", []DispatchWorkerCapabilityFact{investigationCapabilityFact(t, f, r)})
	if err != nil || decision.Assignment == nil {
		t.Fatalf("scoped investigation assignment: %+v %v", decision, err)
	}
	owner := coordinationDecideWorker(t, requestDispatchFixture{s: s, worker: f.b}, r, decision, true)
	taskWorkTestReceipt(t, s, co.Member, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	queueEvent(t, s, owner.Member.Claim, provider.AcceptedEvent, "", "")
	s.EnableTaskWorkExecution()
	return f, head, owner
}

const a7HandbackKey = "a7-handback"

func TestBHandbackA7(t *testing.T) {
	const priorVersion = 30
	const a7Version = priorVersion + 1
	if len(migrations) < a7Version || migrations[priorVersion] != chatDiscussionWorkReturnSchema {
		t.Fatal("A7 must follow the published v30 ledger at v31")
	}
	// Fingerprint the ordered, NUL-separated v1–v30 SQL from c38b5f2 (#195).
	const frozenV30 = "7cf235d45d09ac131ae61c34c6c5a8158bababdd44885fa6005bd4e248b05619"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(migrations[:priorVersion], "\x00")))); got != frozenV30 {
		t.Fatalf("published v30 migration prefix changed: %s; want %s", got, frozenV30)
	}
	input := domain.HandbackWorkInput{Summary: "尚需确认第三项约束。"}
	handback := func(t *testing.T, s *Store, owner CoordinationToolScope) domain.TaskWorkHandback {
		t.Helper()
		h, err := s.HandbackWork(ctx, owner.Member, input, a7HandbackKey)
		if err != nil || h.ID == "" || h.AuthorTurnID != owner.Member.Claim.TurnID || h.AuthorAttemptID != owner.AttemptID ||
			h.AssignmentID != owner.Provenance.AssignmentID || h.HandbackWorkInput != input {
			t.Fatalf("bound Handback: %+v %v", h, err)
		}
		notice, err := s.GetMessage(ctx, h.GroupID, h.NoticeMessageID)
		if err != nil || notice.TopicID != h.TopicID || notice.SenderID != h.OwnerMemberID || notice.SourceKind != "platform" ||
			notice.SourceID != "work:handback:"+h.ID || len(notice.Deliveries) != 0 {
			t.Fatalf("formal notice lost attribution or woke a member: %+v %v", notice, err)
		}
		return h
	}
	returnIDs := func(t *testing.T, s *Store, h domain.TaskWorkHandback) (string, string) {
		t.Helper()
		var message, delivery string
		if err := s.db.QueryRow(`SELECT message_id,coalesce(delivery_id,'') FROM chat_discussion_work_returns WHERE handback_id=?`, h.ID).
			Scan(&message, &delivery); err != nil || message == "" || message == h.NoticeMessageID {
			t.Fatalf("missing separate return for %s: %q %q %v", h.ID, message, delivery, err)
		}
		return message, delivery
	}
	replay := func(t *testing.T, s *Store, owner CoordinationToolScope, h domain.TaskWorkHandback) {
		t.Helper()
		before := messageSequenceSnapshot(t, s)
		if got, err := s.HandbackWork(ctx, owner.Member, input, a7HandbackKey); err != nil || got != h {
			t.Fatalf("same-key replay changed Handback: %+v %v; want %+v", got, err, h)
		}
		messageSequencePreserved(t, s, before)
	}

	t.Run("original-inviter", func(t *testing.T) {
		f, invite, owner := a7HandbackOwner(t)
		s := f.s
		chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: f.candidate.TopicID, ActorID: "user", Content: "明天例会几点开始？"}, nil)
		// Keep actual unknown and cancellation locks present while the callback runs.
		for _, name := range []string{"a7-unknown", "a7-cancel"} {
			d := queueDelivery(t, s, f.otherID, member(t, s, f.otherID).ID, name)
			turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.otherID, DeliveryID: d.ID, MaxActive: 8, ResourceKeys: []string{name}})
			if err != nil {
				t.Fatal(err)
			}
			claim := TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
			queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
			if name == "a7-unknown" {
				taskWorkTestReceipt(t, s, MemberToolScope{Claim: claim}, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}, domain.TurnStateUnknown)
			} else if _, err := s.RequestTurnCancel(ctx, f.otherID, turn.ID); err != nil {
				t.Fatal(err)
			}
			if held, err := s.HasResourceClaim(ctx, name); err != nil || !held {
				t.Fatalf("fixture lacks %s lock: %t %v", name, held, err)
			}
		}
		before := messageSequenceSnapshot(t, s)
		counts := workReportWakeCounts(t, s)
		h := handback(t, s, owner)
		if got := workReportWakeCounts(t, s); got != ([3]int{counts[0] + 2, counts[1] + 1, counts[2]}) {
			t.Fatalf("expected a notice, a return, and one ordinary wake; before %v, got %v", counts, got)
		}
		for _, table := range []string{"messages", "message_log", "deliveries", "task_work_handbacks", "chat_discussion_work_returns", "mutation_idempotency"} {
			delete(before, table)
		}
		messageSequencePreserved(t, s, before) // Includes Tasks, Assignments, Attempts, Turns and resource claims.
		message, delivery := returnIDs(t, s, h)
		reopened := queueHandle(t, f.path, false)
		want := domain.ChatDiscussionWorkReturnReceipt{ThreadRef: f.candidate.ThreadRef, OriginMessageID: f.candidate.MessageID,
			InvitationMessageID: invite.Message.ID, HandbackID: h.ID, HandbackNoticeMessageID: h.NoticeMessageID,
			MessageID: message, SenderID: owner.Member.MemberID, RecipientID: f.a, DeliveryID: delivery, ConversationID: f.scope.ConversationID}
		if got, err := reopened.ResolveChatDiscussionWorkReturnDelivery(ctx, f.groupID, delivery); err != nil || got != want {
			t.Fatalf("wrong original discussion route: %+v %v; want %+v", got, err, want)
		}
		if got, err := s.ResolveChatDiscussionWorkReturnDelivery(ctx, f.otherID, delivery); !errors.Is(err, ErrMemberToolDenied) || got.MessageID != "" {
			t.Fatalf("return resolved in another group: %+v %v", got, err)
		}
		m, err := s.GetMessage(ctx, f.groupID, message)
		if err != nil || m.TopicID != want.TopicID || m.SenderID != want.SenderID || m.SourceKind != "unknown" || m.SourceID != "" ||
			!strings.HasSuffix(m.Content, h.Summary) || len(m.Deliveries) != 1 {
			t.Fatalf("wrong return publication: %+v %v", m, err)
		}
		if d := m.Deliveries[0]; d.ID != delivery || d.RecipientID != f.a || d.ConversationID != f.scope.ConversationID ||
			d.State != domain.DeliveryStateQueued || d.TurnID != "" {
			t.Fatalf("wrong queued wake: %+v", d)
		}
		if role, err := s.ResolveCoordinationDelivery(ctx, f.groupID, delivery); err != nil ||
			role.Provenance.Kind != CoordinationRoleOrdinary || role.Blocked != CoordinationDiscussionWorkReturnHandlerRequired {
			t.Fatalf("legacy preparation must not start the discussion return: %+v %v", role, err)
		}
		coordinationDecideExec(t, s, "UPDATE members SET status='left' WHERE id=?", f.a)
		reopened.EnableTaskWorkExecution()
		replay(t, reopened, owner, h)
	})

	t.Run("rollback", func(t *testing.T) {
		f, _, owner := a7HandbackOwner(t)
		s := f.s
		coordinationDecideExec(t, s, `CREATE TRIGGER a7_ignore_return BEFORE INSERT ON chat_discussion_work_returns BEGIN SELECT RAISE(IGNORE); END`)
		before := coordinationDecideSnapshot(t, s)
		if h, err := s.HandbackWork(ctx, owner.Member, input, a7HandbackKey); !errors.Is(err, ErrConflict) || h != (domain.TaskWorkHandback{}) {
			t.Fatalf("partial callback acknowledged: %+v %v", h, err)
		}
		coordinationDecidePreserved(t, s, before) // Also proves rollback of the B notice and key.
		coordinationDecideExec(t, s, `DROP TRIGGER a7_ignore_return`)
		h := handback(t, s, owner)
		if _, delivery := returnIDs(t, s, h); delivery == "" {
			t.Fatal("rollback consumed the key or original wake slot")
		}
	})

	t.Run("departed-inviter", func(t *testing.T) {
		f, _, owner := a7HandbackOwner(t)
		s := f.s
		coordinationDecideExec(t, s, "UPDATE members SET status='left' WHERE id=?", f.a)
		counts := workReportWakeCounts(t, s)
		h := handback(t, s, owner)
		message, delivery := returnIDs(t, s, h)
		m, err := s.GetMessage(ctx, f.groupID, message)
		if err != nil || delivery != "" || len(m.Deliveries) != 0 || m.TopicID != h.TopicID ||
			workReportWakeCounts(t, s) != ([3]int{counts[0] + 2, counts[1], counts[2]}) {
			t.Fatalf("departed inviter should get a visible-only return: %+v %v", m, err)
		}
		replay(t, s, owner, h)
	})

	t.Run("no-lineage", func(t *testing.T) {
		f, owner, child, _ := workReportOwner(t, 1)
		s := f.s
		m := discussionUser(t, f.messageFixture, owner.Member.TopicID, f.a)
		root, c := discussionArrive(t, s, f.groupID, m.Deliveries[0].ID)
		discussionAssess(t, s, root, c)
		discussionApply(t, s, root, c, "a7-unrelated", domain.ChatDiscussionAction{
			Kind: domain.ChatDiscussionInvite, RecipientID: child.ID, Content: "无关讨论。"})
		counts := workReportWakeCounts(t, s)
		h := handback(t, s, owner)
		if workReportWakeCounts(t, s) != ([3]int{counts[0] + 1, counts[1], counts[2]}) ||
			len(messageSequenceTableRows(t, s, "chat_discussion_work_returns")) != 0 {
			t.Fatal("no-lineage Handback did not stay a non-waking notice")
		}
		replay(t, s, owner, h)
	})

	t.Run("v30-to-v31-replay", func(t *testing.T) {
		all := migrations
		defer func() { migrations = all }()
		migrations = all[:priorVersion]
		f, _, owner := a7HandbackOwner(t)
		messageSequenceLedger(t, f.s, priorVersion)
		counts := workReportWakeCounts(t, f.s)
		h := handback(t, f.s, owner)
		if got := workReportWakeCounts(t, f.s); got != ([3]int{counts[0] + 1, counts[1], counts[2]}) {
			t.Fatalf("v30 Handback should publish only its notice; before %v, got %v", counts, got)
		}
		before := messageSequenceSnapshot(t, f.s)
		if err := f.s.Close(); err != nil {
			t.Fatal(err)
		}
		migrations = all[:a7Version]
		s := queueHandle(t, f.path, false)
		messageSequenceLedger(t, s, a7Version)
		messageSequencePreserved(t, s, before)
		if len(messageSequenceTableRows(t, s, "chat_discussion_work_returns")) != 0 {
			t.Fatal("v31 migration backfilled a v30 Handback return")
		}
		s.EnableTaskWorkExecution()
		replay(t, s, owner, h)
	})
}
