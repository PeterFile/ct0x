package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskWorkFollowupInvestigation(t *testing.T) {
	for _, name := range []string{"pull-in-live-attempt", "next-protected-owner", "stale-after-confirmation"} {
		t.Run(name, func(t *testing.T) {
			consumeLive := name == "pull-in-live-attempt"
			f := chatSetup(t, "user", true)
			artifact := mustPublishArtifact(t, f.s, artifactPublishInput(f.groupID))
			ref := domain.ArtifactVersionRef{ArtifactID: artifact.ArtifactID, VersionID: artifact.ID}
			policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, ArtifactVersions: []domain.ArtifactVersionRef{ref}}, 1)
			f.s.EnableInvestigationExecution()
			f.s.EnableTaskWorkExecution()
			chatNextCandidate(t, &f, true)
			f.input = investigationProposal()
			old := investigationBindWorker(t, f)
			p := old.Provenance
			co := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
			taskWorkTestReceipt(t, f.s, co, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
			queueEvent(t, f.s, old.Member.Claim, provider.AcceptedEvent, "", "")

			var confirmed domain.TaskWorkInput
			const confirmKey = "confirm-base"
			if !consumeLive {
				s, c := newWorkInputCandidate(t, f, "注明基准格式")
				r, err := f.s.ApplyChatAssessmentDormant(ctx, s, c.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
					Reason: "调查结果保持可核对", Followup: &domain.ChatFollowupAssessment{Constraint: "注明基准格式"}}, "记录基准。", "base")
				if err != nil || !r.WorkInputPending || r.WorkDeliveryID != "" {
					t.Fatalf("explicit attempt-only input: %+v %v", r, err)
				}
				confirmed, err = f.s.AcknowledgeWorkInputWithKey(ctx, old.Member, r.WorkInputID, confirmKey)
				if err != nil || confirmed.Consumption == nil {
					t.Fatalf("prior confirmation: %+v %v", confirmed, err)
				}
				taskWorkTestReceipt(t, f.s, s, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
			}
			const text = "保持旧格式兼容"
			assessor := member(t, f.s, f.groupID).ID
			otherAssessor := member(t, f.s, f.groupID).ID
			m := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: p.TopicID, ActorID: "user", Content: text, RecipientIDs: []string{assessor, otherAssessor}}, []string{assessor, otherAssessor})
			sc, c := workInputCandidate(t, f, m, 0)
			cmd := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "补充原调查的交付约束", Followup: &domain.ChatFollowupAssessment{Constraint: text}}
			r, err := f.s.ApplyChatAssessment(ctx, sc, c.ID, cmd, "记录要求。", "continue")
			if err != nil || r.Outcome != "continued" || r.TaskID != p.TaskID || r.Action != domain.ChatActionReadOnlyInvestigation || r.WorkInputID == "" || r.WorkDeliveryID == "" || r.DispatchRequestID != "" {
				t.Fatalf("followup did not route at the exhausted admission budget: %+v %v", r, err)
			}
			other, alias := workInputCandidate(t, f, m, 1)
			again, err := f.s.ApplyChatAssessment(ctx, other, alias.ID, cmd, "记录要求。", "alias")
			if err != nil || !reflect.DeepEqual(r, again) {
				t.Fatalf("replay changed input/wake identity: %+v %v", again, err)
			}
			var count int
			if err := f.s.db.QueryRow(`SELECT count(*) FROM task_work_deliveries WHERE source_kind='followup' AND assignment_id=?`, p.AssignmentID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("physical followup deliveries = %d: %v", count, err)
			}
			wake, err := f.s.GetMessage(ctx, f.groupID, r.WorkWakeMessageID)
			if err != nil || wake.SourceKind != "platform" || wake.TopicID != p.TopicID || len(wake.Deliveries) != 1 || wake.Deliveries[0].RecipientID != p.MemberID {
				t.Fatalf("wake escaped the real assignment and topic: %+v %v", wake, err)
			}
			pre, err := f.s.ResolveCoordinationDelivery(ctx, f.groupID, r.WorkDeliveryID)
			if err != nil || pre.Provenance.SourceKind != domain.TaskWorkSourceFollowup || pre.Provenance.SourceID != r.WorkInputID || pre.Blocked != "prior_turn_unsettled" {
				t.Fatalf("known-active owner was reentered or lost typed identity: %+v %v", pre, err)
			}
			if _, err := f.s.ResolveCoordinationDelivery(ctx, f.otherID, r.WorkDeliveryID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign wake lookup: %v", err)
			}
			items, err := f.s.ListWorkInputsForMember(ctx, old.Member)
			wantItems := 2
			if consumeLive {
				wantItems = 1
			}
			if err != nil || len(items) != wantItems || items[0].ID != r.WorkInputID || items[0].Content != text || items[0].ReplyMessageID != m.ID || items[0].Consumption != nil {
				t.Fatalf("owner inbox did not expose only the official source: %+v %v", items, err)
			}
			if _, err := f.s.ReadInvestigationChannel(ctx, old, ListOptions{}); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("followup widened an artifact-only scope: %v", err)
			}
			result := AssignmentResultInput{Summary: "调查结论保留旧格式。"}
			if _, err := f.s.SubmitAssignmentResult(ctx, old, result, "submit"); !errors.Is(err, ErrConflict) {
				t.Fatalf("fresh submit ignored the pending constraint: %v", err)
			}
			claim := ClaimTurnInput{GroupID: f.groupID, DeliveryID: r.WorkDeliveryID, MaxActive: 8, ResourceKeys: []string{"worker-resource"}}
			if consumeLive {
				ack, err := f.s.AcknowledgeWorkInput(ctx, old.Member, r.WorkInputID)
				if err != nil || ack.Consumption == nil || ack.Consumption.AttemptID != old.AttemptID || ack.Consumption.TurnID != old.Member.Claim.TurnID {
					t.Fatalf("same Attempt did not confirm followup: %+v %v", ack, err)
				}
				if proof, err := f.s.GetWorkInputForMember(ctx, old.Member, r.WorkInputID); err != nil || !reflect.DeepEqual(proof, ack) {
					t.Fatalf("live owner could not reread the handling proof: %+v %v", proof, err)
				}
				if d, err := f.s.ResolveTaskWorkDelivery(ctx, f.groupID, r.WorkDeliveryID); err != nil || d.Blocked != "followup_handled" {
					t.Fatalf("handled wake was not suppressed before submission: %+v %v", d, err)
				}
				if s, err := f.s.SubmitAssignmentResult(ctx, old, result, "submit"); err != nil || s.AttemptID != old.AttemptID {
					t.Fatalf("live owner could not formally submit: %+v %v", s, err)
				}
				taskWorkTestReceipt(t, f.s, old.Member, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
				if turn, err := f.s.ClaimTurn(ctx, claim); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
					t.Fatalf("a redundant wake was claimable: %+v %v", turn, err)
				}
				if d, err := f.s.ResolveTaskWorkDelivery(ctx, f.groupID, r.WorkDeliveryID); err != nil || d.Blocked != "followup_handled" {
					t.Fatalf("redundant unclaimed wake lost its durable suppression evidence: %+v %v", d, err)
				}
				if page, err := f.s.ListPendingDeliveries(ctx, PendingOptions{Lane: "work"}); err != nil || len(page.Items) != 0 {
					t.Fatalf("redundant wake still in scheduler queue: %+v %v", page, err)
				}
				return
			}

			if _, err := f.s.RequestTurnCancel(ctx, f.groupID, old.Member.Claim.TurnID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.ClaimTurn(ctx, claim); !errors.Is(err, ErrTurnBusy) {
				t.Fatalf("cancel_requested owner replaced: %v", err)
			}
			unknown := queueReceipt(provider.OutcomeUnknown)
			unknown.DirectChildReaped = false
			taskWorkTestReceipt(t, f.s, old.Member, unknown, domain.TurnStateUnknown)
			if _, err := f.s.ClaimTurn(ctx, claim); !errors.Is(err, ErrTurnBusy) {
				t.Fatalf("unknown owner replaced: %v", err)
			}
			foreign := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.otherID, ActorID: "user", Content: "另一组的独立任务"}, []string{f.foreign})
			if _, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.otherID, DeliveryID: foreign.Deliveries[0].ID, MaxActive: 8, ResourceKeys: claim.ResourceKeys}); !errors.Is(err, ErrTurnBusy) {
				t.Fatalf("unknown owner did not retain the global resource claim: %v", err)
			}
			taskWorkTestReceipt(t, f.s, old.Member, queueReceipt(provider.OutcomeAborted), domain.TurnStateAborted)
			// An unopened runtime cannot accidentally dispatch the same typed wake as chat.
			cold := queueHandle(t, f.path, true)
			if q, err := cold.ResolveCoordinationDelivery(ctx, f.groupID, r.WorkDeliveryID); err != nil || q.Provenance != pre.Provenance || q.Blocked != "task_work_execution_disabled" {
				t.Fatalf("no process opt-in lost typed provenance: %+v %v", q, err)
			}
			if turn, err := cold.ClaimTurn(ctx, claim); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
				t.Fatalf("unwired store claimed a native continuation: %+v %v", turn, err)
			}
			page, err := f.s.ListPendingDeliveries(ctx, PendingOptions{Lane: "work"})
			if err != nil || len(page.Items) != 1 || page.Items[0].Delivery.ID != r.WorkDeliveryID {
				t.Fatalf("native scheduler did not retain the typed wake: %+v %v", page, err)
			}
			if ready, err := f.s.ResolveCoordinationDelivery(ctx, f.groupID, r.WorkDeliveryID); err != nil || ready.Blocked != CoordinationReady || ready.Provenance != pre.Provenance {
				t.Fatalf("trustworthy terminal did not make the wake ready: %+v %v", ready, err)
			}
			turn, err := f.s.ClaimTurn(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.CaptureChatCandidate(ctx, scope); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("typed followup fell through to ordinary intake: %v", err)
			}
			wrong := pre.Provenance
			wrong.SourceID = confirmed.ID
			if _, err := f.s.ResolveInvestigationExecution(ctx, scope, wrong); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("claim accepted a different genuine input as its carrier: %v", err)
			}
			if state, err := f.s.ResolveCoordinationExecution(ctx, scope, pre.Provenance); !errors.Is(err, ErrMemberToolDenied) || state != (CoordinationRoleState{}) {
				t.Fatalf("typed investigation fell back to generic profile: %+v %v", state, err)
			}
			state, err := f.s.ResolveInvestigationExecution(ctx, scope, pre.Provenance)
			if err != nil || state.Blocked != CoordinationReady || state.AttemptID != "" || state.Provenance != pre.Provenance {
				t.Fatalf("postclaim identity before binding: %+v %v", state, err)
			}
			attempt, err := f.s.BindTaskWorkContinuation(ctx, scope)
			if err != nil || attempt.ID == "" || attempt.ID == old.AttemptID || attempt.TurnID != turn.ID || attempt.TaskID != p.TaskID || attempt.TaskRevision != p.TaskRevision {
				t.Fatalf("wrong formal continuation Attempt: %+v %v", attempt, err)
			}
			state, err = f.s.ResolveInvestigationExecution(ctx, scope, pre.Provenance)
			if err != nil || state.AttemptID != attempt.ID || state.Provenance.AssignmentID != p.AssignmentID {
				t.Fatalf("exact continuation was not recovered: %+v %v", state, err)
			}
			role := CoordinationToolScope{Member: scope, Provenance: state.Provenance, AttemptID: state.AttemptID}
			items, err = f.s.ListWorkInputsForMember(ctx, scope)
			if err != nil || len(items) != 2 || items[0].ID != r.WorkInputID || items[1].ID != confirmed.ID || items[1].Consumption == nil || items[1].Consumption.AttemptID != old.AttemptID {
				t.Fatalf("continuation cannot see the prior confirmed work state: %+v %v", items, err)
			}
			if prior, err := f.s.AcknowledgeWorkInputWithKey(ctx, scope, confirmed.ID, confirmKey); err != nil || !reflect.DeepEqual(prior, confirmed) {
				t.Fatalf("same-Assignment continuation could not replay the prior confirmation: %+v %v", prior, err)
			}
			if _, v, err := f.s.ReadInvestigationArtifact(ctx, role, ref.ArtifactID, ref.VersionID); err != nil || v.ID != ref.VersionID {
				t.Fatalf("exact scoped artifact action failed: %+v %v", v, err)
			}
			if _, err := f.s.ReadInvestigationChannel(ctx, role, ListOptions{}); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("continuation granted topic history: %v", err)
			}
			if _, err := f.s.SubmitTaskWorkResult(ctx, scope, result, "submit"); !errors.Is(err, ErrConflict) {
				t.Fatalf("fresh continuation bypassed the same constraint fence: %v", err)
			}
			ack, err := f.s.AcknowledgeWorkInput(ctx, scope, r.WorkInputID)
			if err != nil || ack.Content != text || ack.Consumption == nil || ack.Consumption.TurnID != turn.ID || ack.Consumption.AttemptID != attempt.ID {
				t.Fatalf("wrong continuation consumption: %+v %v", ack, err)
			}
			if name == "stale-after-confirmation" {
				investigationDisablePolicy(t, f.s, policy)
				if _, err := f.s.SubmitTaskWorkResult(ctx, scope, result, "submit"); !errors.Is(err, ErrConflict) {
					t.Fatalf("confirmed input overrode invalidated investigation policy: %v", err)
				}
				if _, err := f.s.SubmitTask(ctx, investigationTaskSubmitInput(role, result, "direct")); !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("common submission gate did not recheck typed investigation authority: %v", err)
				}
				return
			}
			before := messageSequenceSnapshot(t, f.s)
			coordinationDecideExec(t, f.s, `CREATE TEMP TRIGGER followup_notice_drift AFTER INSERT ON chat_submission_notices
			 BEGIN UPDATE chat_policies SET revision=revision-1
			 WHERE (group_id,topic_id)=(SELECT group_id,topic_id FROM messages WHERE id=NEW.message_id); END`)
			if got, err := f.s.SubmitTaskWorkResult(ctx, scope, result, "submit"); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
				t.Fatalf("typed submission ignored policy drift during the notice write: %+v %v", got, err)
			}
			coordinationDecideExec(t, f.s, `DROP TRIGGER followup_notice_drift`)
			messageSequencePreserved(t, f.s, before)
			sub, err := f.s.SubmitTaskWorkResult(ctx, scope, result, "submit")
			if err != nil || sub.AttemptID != attempt.ID || sub.TaskID != p.TaskID {
				t.Fatalf("same assignment could not formally submit: %+v %v", sub, err)
			}
			investigationDisablePolicy(t, f.s, policy)
			replay, err := f.s.SubmitTaskWorkResult(ctx, scope, result, "submit")
			if err != nil || !reflect.DeepEqual(replay, sub) {
				t.Fatalf("historical key required freshly granted investigation authority: %+v %v", replay, err)
			}
			dispatchAssertFK(t, f.s)
		})
	}
}
