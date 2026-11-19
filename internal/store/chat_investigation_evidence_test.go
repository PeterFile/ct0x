package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

const investigationLimitQuote = " 只讨论 & 不开始调查。\n"

func investigationCompetingCandidate(t *testing.T) (chatFixture, domain.Delivery, domain.Delivery) {
	t.Helper()
	f := chatSetup(t, "user", false)
	policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
	f.s.EnableInvestigationExecution()
	discussionEnd(t, f.s, f.scope)
	// Keep the second assessor independent of the original coordinator and worker.
	targets := []string{f.a, member(t, f.s, f.groupID).ID}
	root := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID,
		ActorID: "user", Content: "本次：" + investigationLimitQuote, RecipientIDs: targets}, targets)
	first, second := root.Deliveries[0], root.Deliveries[1]
	if first.RecipientID != f.a {
		first, second = second, first
	}
	investigationBindCandidate(t, &f, first)
	f.input = investigationProposal()
	return f, first, second
}

func investigationRecordLimit(t *testing.T, f chatFixture, delivery domain.Delivery) domain.ChatAssessmentReceipt {
	t.Helper()
	scope, id := discussionArrive(t, f.s, f.groupID, delivery.ID)
	input := domain.ChatAssessment{Disposition: "discuss", Reason: "用户明确限定。", DiscussionOnlyExcerpt: new(investigationLimitQuote)}
	r, err := f.s.ApplyChatAssessment(ctx, scope, id, input, "先讨论。", "competing-limit")
	if err != nil || !r.DiscussionOnly || r.DiscussionOnlyExcerpt != investigationLimitQuote || r.MessageID != f.candidate.MessageID || r.SourceID != f.candidate.SourceID {
		t.Fatalf("expected exact same-source limit: %+v, %v", r, err)
	}
	discussionEnd(t, f.s, scope)
	return r
}

func TestChatInvestigationIgnoresTextOnlyLimitMarkers(t *testing.T) {
	f := chatSetup(t, "user", false)
	s := f.s
	policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
	s.EnableInvestigationExecution()
	discussionEnd(t, s, f.scope)

	const markerRecipients = 9
	targets := []string{f.a, f.b}
	for len(targets) < markerRecipients+1 {
		targets = append(targets, member(t, s, f.groupID).ID)
	}
	root := chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID,
		ActorID: "user", Content: "有哪些问题待核对？", RecipientIDs: targets}, targets)
	if len(root.Deliveries) != len(targets) {
		t.Fatalf("got %d root deliveries, want %d", len(root.Deliveries), len(targets))
	}
	input := domain.ChatAssessment{Disposition: "discuss", Reason: "讨论待核对的问题。"}
	for _, delivery := range root.Deliveries[:markerRecipients] {
		scope, id := discussionArrive(t, s, f.groupID, delivery.ID)
		r, err := s.ApplyChatAssessment(ctx, scope, id, input, `说明中提到 "discussion_only_excerpt"，并不构成限制。`, "text-only-marker")
		if err != nil || r.Outcome != "discuss" || r.DiscussionOnly || r.SourceID != root.SourceID {
			t.Fatalf("expected a non-restricting root assessment: %+v, %v", r, err)
		}
		discussionEnd(t, s, scope)
	}
	scope, id := discussionArrive(t, s, f.groupID, root.Deliveries[markerRecipients].ID)
	r, err := s.ApplyChatAssessment(ctx, scope, id, investigationProposal(), "建议调查。", "admit")
	if err != nil || r.Outcome != "admitted" || r.TaskID == "" || r.SourceID != root.SourceID {
		t.Fatalf("text-only markers prevented investigation admission: %+v, %v", r, err)
	}
}

func TestChatInvestigationDamagedSourceEvidence(t *testing.T) {
	for _, name := range []string{"missing-limit-fields", "admitted-backlink", "admitted-duplicate-turn"} {
		t.Run(name, func(t *testing.T) {
			f, delivery, competitor := investigationCompetingCandidate(t)
			// The marker in the notice text is not a typed limitation.
			admission, err := f.s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, `说明中提到 "discussion_only_excerpt": 并不构成限制。`, "admit")
			if err != nil || admission.Outcome != "admitted" || admission.TaskID == "" {
				t.Fatalf("expected live investigation admission: %+v, %v", admission, err)
			}
			if name == "admitted-backlink" {
				if n, err := f.s.RecoverInFlight(ctx); err != nil || n != 1 {
					t.Fatalf("recover original intake: %d, %v", n, err)
				}
				if turn := queueTurn(t, f.s, f.scope.Claim); turn.ClaimToken == f.scope.Claim.Token {
					t.Fatal("recovery did not rotate the historical claim")
				}
			} else {
				discussionEnd(t, f.s, f.scope)
			}
			ready := func() error { return f.s.CheckTaskInvestigationReady(ctx, f.groupID, admission.TaskID) }
			if err := ready(); err != nil {
				t.Fatalf("intact historical intake should remain ready: %v", err)
			}
			switch name {
			case "missing-limit-fields":
				limit := investigationRecordLimit(t, f, competitor)
				if err := ready(); !errors.Is(err, ErrTaskInvestigationNotReady) {
					t.Fatalf("intact limit did not fence readiness: %v", err)
				}
				coordinationDecideExec(t, f.s, `DROP TRIGGER chat_assessments_no_update`)
				coordinationDecideExec(t, f.s, `UPDATE chat_assessments SET receipt_json=json_remove(receipt_json,
				 '$.discussion_only','$.discussion_only_excerpt') WHERE id=?`, limit.ID)
			case "admitted-backlink":
				coordinationDecideExec(t, f.s, `UPDATE deliveries SET turn_id='' WHERE id=?`, delivery.ID)
			case "admitted-duplicate-turn":
				coordinationDecideExec(t, f.s, `INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,created_at,updated_at)
				 SELECT 'duplicate-intake-turn',group_id,delivery_id,conversation_id,member_id,'unused','not_started',created_at,updated_at
				 FROM turns WHERE id=?`, f.scope.Claim.TurnID)
			}
			if err := ready(); !errors.Is(err, ErrTaskInvestigationNotReady) {
				t.Errorf("damaged stored evidence allowed readiness: %v", err)
			}
			if saved, err := f.s.GetTaskInvestigationScope(ctx, f.groupID, admission.TaskID); err != nil || saved.TaskID != admission.TaskID {
				t.Errorf("damage hid historical investigation scope: %+v, %v", saved, err)
			}
		})
	}
}

func TestChatInvestigationInvalidLimitProof(t *testing.T) {
	for _, name := range []string{"limit-backlink", "different-quote", "trailing-payload-byte"} {
		t.Run(name, func(t *testing.T) {
			f, _, competitor := investigationCompetingCandidate(t)
			limit := investigationRecordLimit(t, f, competitor)
			switch name {
			case "limit-backlink":
				coordinationDecideExec(t, f.s, `UPDATE deliveries SET turn_id='' WHERE id=?`, competitor.ID)
			case "different-quote":
				limit.DiscussionOnlyExcerpt = strings.TrimSpace(limit.DiscussionOnlyExcerpt)
				data, err := json.Marshal(limit)
				if err != nil {
					t.Fatal(err)
				}
				coordinationDecideExec(t, f.s, `DROP TRIGGER chat_assessments_no_update`)
				coordinationDecideExec(t, f.s, `UPDATE chat_assessments SET receipt_json=? WHERE id=?`, string(data), limit.ID)
			case "trailing-payload-byte":
				coordinationDecideExec(t, f.s, `DROP TRIGGER chat_assessments_no_update`)
				coordinationDecideExec(t, f.s, `UPDATE chat_assessments SET payload=CAST(payload || x'00' AS BLOB) WHERE id=?`, limit.ID)
			}
			// Admission must not turn an invalid proof into a trusted quoted decline or ignore it.
			got, err := f.s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "建议调查。", "admit")
			requestDispatchZero(t, got, err, ErrMemberToolDenied)
		})
	}
}

func TestChatInvestigationLateLimitRejectsBoundWorkerSubmission(t *testing.T) {
	f, _, competitor := investigationCompetingCandidate(t)
	s := f.s
	r, coordinator := investigationCoordinator(t, f)
	assign := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "按限制执行调查。"}
	decision, err := s.DecideDispatchWithCapabilities(ctx, coordinator, assign, "prior-assignment", []DispatchWorkerCapabilityFact{investigationCapabilityFact(t, f, r)})
	if err != nil || decision.Assignment == nil {
		t.Fatalf("expected prior assignment: %+v, %v", decision, err)
	}
	worker := coordinationDecideWorker(t, requestDispatchFixture{s: s, worker: f.b}, r, decision, true)
	if state, err := s.ResolveInvestigationExecution(ctx, worker.Member, worker.Provenance); err != nil || state.AttemptID != worker.AttemptID || state.Blocked != CoordinationReady {
		t.Fatalf("expected live bound worker before limit: %+v, %v", state, err)
	}
	investigationRecordLimit(t, f, competitor)
	var resource string
	if err := s.db.QueryRow(`SELECT resource_key FROM resource_claims WHERE turn_id=?`, worker.Member.Claim.TurnID).Scan(&resource); err != nil || resource == "" {
		t.Fatalf("fixture did not retain the bound worker's resource claim: %v", err)
	}
	before := coordinationDecideSnapshot(t, s)
	input := AssignmentResultInput{Summary: "受限调查结论。"}
	result, err := s.SubmitAssignmentResult(ctx, worker, input, "after-limit-assignment")
	requestDispatchZero(t, result, err, ErrMemberToolDenied)
	result, err = s.SubmitTask(ctx, investigationTaskSubmitInput(worker, input, "after-limit-direct"))
	requestDispatchZero(t, result, err, ErrMemberToolDenied)
	if replay, err := s.DecideDispatchWithCapabilities(ctx, coordinator, assign, "prior-assignment", nil); err != nil || !reflect.DeepEqual(replay, decision) {
		t.Errorf("late limit changed prior decision replay: %+v, %v", replay, err)
	}
	coordinationDecidePreserved(t, s, before)
}
