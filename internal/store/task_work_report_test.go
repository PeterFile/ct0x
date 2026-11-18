package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func workReportOwner(t *testing.T, budget int) (chatFixture, CoordinationToolScope, domain.Member, domain.ChatPolicy) {
	t.Helper()
	return workReportOwnerWithPolicy(t, chatSetup(t, "user", false), domain.ChatInvestigationPolicy{
		Objective: investigationObjective, AllowTopicMessages: true, MaxDelegations: budget,
	})
}

func workReportOwnerWithPolicy(t *testing.T, f chatFixture, cfg domain.ChatInvestigationPolicy, alsoEligible ...string) (chatFixture, CoordinationToolScope, domain.Member, domain.ChatPolicy) {
	t.Helper()
	child, err := f.s.CreateMember(ctx, CreateMemberInput{GroupID: f.groupID, Name: "调查协作者", Role: "research", ActorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, append([]string{f.b, child.ID}, alsoEligible...), 1, "delegation-receivers"); err != nil {
		t.Fatal(err)
	}
	f, owner, policy := workReportInvestigationAssignment(t, f, cfg)
	co := taskWorkTestCoordinator(t, f.s, f.groupID, owner.Provenance.AssignmentID)
	taskWorkTestReceipt(t, f.s, co, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	queueEvent(t, f.s, owner.Member.Claim, provider.AcceptedEvent, "", "")
	return f, owner, child, policy
}

func workReportInvestigationAssignment(t *testing.T, f chatFixture, cfg domain.ChatInvestigationPolicy) (chatFixture, CoordinationToolScope, domain.ChatPolicy) {
	t.Helper()
	policy := investigationPolicy(t, f, cfg, 1)
	f.s.EnableInvestigationExecution()
	f.s.EnableTaskWorkExecution()
	chatNextCandidate(t, &f, true)
	f.input = investigationProposal()
	return f, investigationBindWorker(t, f), policy
}

func workReportWakeCounts(t *testing.T, s *Store) [3]int {
	t.Helper()
	var counts [3]int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM messages), (SELECT count(*) FROM deliveries),
 (SELECT count(*) FROM task_work_deliveries)`).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func recordWorkReportProgress(t *testing.T, s *Store, worker MemberToolScope, d domain.TaskWorkDelegation, input domain.ReportWorkInput, key string) domain.TaskWorkReport {
	t.Helper()
	r, err := s.ReportWork(ctx, worker, input, key)
	if err != nil || r.ID == "" || r.DeliveryID != "" || r.MessageID != "" || r.TaskWorkParent != d.TaskWorkParent ||
		r.DelegationID != d.ID || r.SourceTurnID != worker.Claim.TurnID || r.ActorID != worker.MemberID ||
		r.SourceDeliveryID != d.DeliveryID || r.SourceMessageID != d.MessageID || r.ReportWorkInput != input {
		t.Fatalf("progress: %+v %v", r, err)
	}
	if saved, err := s.GetTaskWorkReport(ctx, d.GroupID, r.ID); err != nil || saved != r {
		t.Fatalf("persisted progress: %+v %v; want %+v", saved, err, r)
	}
	var id string
	var payload []byte
	err = s.db.QueryRow(`SELECT record_id,payload FROM mutation_idempotency
 WHERE operation='report_work' AND actor_id=? AND group_id=? AND idempotency_key=?`, worker.MemberID, d.GroupID, key).Scan(&id, &payload)
	if err != nil || id != r.ID || !reflect.DeepEqual(payload, mutationPayload(d.ID, worker.Claim.TurnID, string(input.Kind), input.Summary, input.AnswerTo)) {
		t.Fatalf("progress idempotency for %q: record=%q, want %q, err=%v", key, id, r.ID, err)
	}
	return r
}

func TestB3WorkSubmissionWaitsForDelegations(t *testing.T) {
	for _, name := range []string{"initial", "continuation"} {
		t.Run(name, func(t *testing.T) {
			f, owner, child, _ := workReportOwner(t, 2)
			s, p := f.s, owner.Provenance
			scope, attemptID := owner.Member, owner.AttemptID
			terminal := queueReceipt(provider.OutcomeSucceeded)
			if name == "continuation" {
				d, err := s.DelegateWork(ctx, scope, domain.DelegateWorkInput{MemberID: child.ID, Question: "先核对第一条。"}, "first-child")
				if err != nil {
					t.Fatal(err)
				}
				wake, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, d.DeliveryID)
				if err != nil || wake.Blocked != "" {
					t.Fatalf("first child wake: %+v %v", wake, err)
				}
				worker := taskWorkTestClaim(t, s, wake)
				queueEvent(t, s, worker.Claim, provider.AcceptedEvent, "", "")
				r, err := s.ReportWork(ctx, worker, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "已核对第一条。"}, "first-report")
				if err != nil {
					t.Fatal(err)
				}
				taskWorkTestReceipt(t, s, worker, terminal, domain.TurnStateSucceeded)
				taskWorkTestReceipt(t, s, scope, terminal, domain.TurnStateSucceeded)
				next, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, r.DeliveryID)
				if err != nil || next.Blocked != "" {
					t.Fatalf("owner continuation wake: %+v %v", next, err)
				}
				scope = taskWorkTestClaim(t, s, next)
				a, err := s.BindTaskWorkContinuation(ctx, scope)
				if err != nil {
					t.Fatal(err)
				}
				attemptID = a.ID
				queueEvent(t, s, scope.Claim, provider.AcceptedEvent, "", "")
			}

			d, err := s.DelegateWork(ctx, scope, domain.DelegateWorkInput{MemberID: child.ID, Question: "核对剩余意见。"}, "pending-child")
			if err != nil {
				t.Fatal(err)
			}
			input := AssignmentResultInput{Summary: "已核对所有意见。"}
			submit := func() (domain.Submission, error) {
				if name == "initial" {
					return s.SubmitAssignmentResult(ctx, owner, input, "result-after-child")
				}
				return s.SubmitTaskWorkResult(ctx, scope, input, "result-after-child")
			}
			assertBlocked := func(why string) {
				t.Helper()
				before := messageSequenceSnapshot(t, s)
				if r, err := submit(); !errors.Is(err, ErrConflict) || r.ID != "" {
					t.Fatalf("owner could submit with %s child: %+v %v", why, r, err)
				}
				messageSequencePreserved(t, s, before)
				if task, err := s.GetTask(ctx, p.GroupID, p.TaskID); err != nil || task.Status != domain.TaskStatusOpen || task.ActiveAttemptID != attemptID || task.CurrentSubmissionID != "" {
					t.Fatalf("denial changed current task: %+v %v", task, err)
				}
			}
			assertBlocked("queued")
			wake, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, d.DeliveryID)
			if err != nil || wake.Blocked != "" {
				t.Fatalf("intentional owner/child overlap lost: %+v %v", wake, err)
			}
			worker := taskWorkTestClaim(t, s, wake, "b3-pending-child-resource")
			assertBlocked("starting")
			queueEvent(t, s, worker.Claim, provider.AcceptedEvent, "", "")
			assertBlocked("running")
			if _, err := s.RequestTurnCancel(ctx, p.GroupID, worker.Claim.TurnID); err != nil {
				t.Fatal(err)
			}
			assertBlocked("cancel-requested")
			taskWorkTestReceipt(t, s, worker, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}, domain.TurnStateUnknown)
			assertBlocked("unknown")
			var heldBy string
			if err := s.db.QueryRow(`SELECT turn_id FROM resource_claims WHERE resource_key='b3-pending-child-resource'`).Scan(&heldBy); err != nil || heldBy != worker.Claim.TurnID {
				t.Fatalf("denial released the child claim: %q %v", heldBy, err)
			}
			taskWorkTestReceipt(t, s, worker, terminal, domain.TurnStateSucceeded)
			receipt := queueReceiptJSON(t, s, worker.Claim)
			coordinationDecideExec(t, s, `UPDATE turns SET receipt_json='' WHERE id=?`, worker.Claim.TurnID)
			assertBlocked("terminal-looking without a receipt")
			coordinationDecideExec(t, s, `UPDATE turns SET receipt_json=? WHERE id=?`, receipt, worker.Claim.TurnID)
			coordinationDecideExec(t, s, `UPDATE deliveries SET state='failed' WHERE id=?`, d.DeliveryID)
			assertBlocked("broken delivery association")
			coordinationDecideExec(t, s, `UPDATE deliveries SET state='accepted' WHERE id=?`, d.DeliveryID)
			coordinationDecideExec(t, s, `CREATE TEMP TRIGGER invalidate_child_on_notice AFTER INSERT ON chat_submission_notices
			 BEGIN UPDATE turns SET receipt_json='' WHERE delivery_id IN (SELECT d.delivery_id FROM task_work_delegations d
			 JOIN task_submissions sub ON (sub.group_id,sub.task_id)=(d.group_id,d.task_id) WHERE sub.id=NEW.submission_id); END`)
			assertBlocked("proof lost during the notice write")
			coordinationDecideExec(t, s, `DROP TRIGGER invalidate_child_on_notice`)
			result, err := submit()
			if err != nil || result.AttemptID != attemptID || result.TaskID != p.TaskID || result.SubmittedBy != p.MemberID {
				t.Fatalf("settled child prevented current result: %+v %v", result, err)
			}
			before := messageSequenceSnapshot(t, s)
			if again, err := submit(); err != nil || !reflect.DeepEqual(again, result) {
				t.Fatalf("submission replay: %+v %v", again, err)
			}
			messageSequencePreserved(t, s, before)
			reviewInput := taskReviewInput(result)
			reviewInput.Outcome, reviewInput.Reason = domain.ReviewOutcomeRejected, "请另行确认调查范围。"
			if review, err := s.ReviewTask(ctx, reviewInput); err != nil || review.SubmissionID != result.ID || review.ReviewedBy != "user" || review.Outcome != domain.ReviewOutcomeRejected {
				t.Fatalf("local Review failed: %+v %v", review, err)
			} else if notice := chatReviewNotice(t, s, review); len(notice.Deliveries) != 0 {
				t.Fatalf("investigation rejection silently woke repair: %+v", notice)
			}
			before = messageSequenceSnapshot(t, s)
			if again, err := submit(); err != nil || !reflect.DeepEqual(again, result) {
				t.Fatalf("review invalidated exact historical submission replay: %+v %v", again, err)
			}
			messageSequencePreserved(t, s, before)
		})
	}
}

func TestB3WorkReportRoundTrip(t *testing.T) {
	f := chatSetup(t, "user", false)
	pub := artifactPublishInput(f.groupID)
	base := mustPublishArtifact(t, f.s, pub)
	pub.ArtifactID, pub.ExpectedVersionID, pub.Name = base.ArtifactID, base.ID, ""
	head := mustPublishArtifact(t, f.s, pub)
	// The conflicting version has an approved base but an unapproved head.
	conflict := mustPublishArtifact(t, f.s, pub)
	baseRef := domain.ArtifactVersionRef{ArtifactID: base.ArtifactID, VersionID: base.ID}
	conflictRef := domain.ArtifactVersionRef{ArtifactID: conflict.ArtifactID, VersionID: conflict.ID}
	f, origin, child, _ := workReportOwnerWithPolicy(t, f, domain.ChatInvestigationPolicy{
		Objective: investigationObjective, AllowTopicMessages: true, MaxDelegations: 2, ArtifactVersions: []domain.ArtifactVersionRef{baseRef, conflictRef},
	})
	s, p := f.s, origin.Provenance
	const supplement = "注明具体出处。"
	assessor, candidate := newWorkInputCandidate(t, f, supplement)
	followup, err := s.ApplyChatAssessment(ctx, assessor, candidate.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
		Reason: "补充原调查的交付要求", Followup: &domain.ChatFollowupAssessment{Constraint: supplement}}, "记录原要求。", "before-report")
	if err != nil || followup.TaskID != p.TaskID || followup.WorkInputID == "" || followup.WorkDeliveryID == "" {
		t.Fatalf("expected a real B1 input and wake: %+v %v", followup, err)
	}
	taskWorkTestReceipt(t, s, assessor, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	input := domain.DelegateWorkInput{MemberID: child.ID, Question: "只核对尚未回应的意见。", AllowTopicMessages: true, ArtifactVersions: []domain.ArtifactVersionRef{conflictRef}}
	delegation, err := s.DelegateWork(ctx, origin.Member, input, "delegate")
	if err != nil || delegation.TaskID != p.TaskID || delegation.AssignmentID != p.AssignmentID || delegation.OwnerMemberID != p.MemberID ||
		delegation.MemberID != child.ID || delegation.AuthorTurnID != origin.Member.Claim.TurnID || delegation.AuthorAttemptID != origin.AttemptID {
		t.Fatalf("no actual owner delegation: %+v %v", delegation, err)
	}
	if got, err := s.GetTaskWorkDelegation(ctx, f.groupID, delegation.ID); err != nil || !reflect.DeepEqual(got, delegation) {
		t.Fatalf("startup preview: %+v %v", got, err)
	}
	delivery, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, delegation.DeliveryID)
	if err != nil || delivery.SourceKind != domain.TaskWorkSourceDelegation || delivery.SourceID != delegation.ID || delivery.Blocked != "" {
		t.Fatalf("delegation wake: %+v %v", delivery, err)
	}
	terminal := queueReceipt(provider.OutcomeSucceeded)
	uncertain := terminal
	uncertain.Outcome, uncertain.DirectChildReaped = provider.OutcomeUnknown, false
	taskWorkTestReceipt(t, s, origin.Member, uncertain, domain.TurnStateUnknown)
	taskWorkTestPeek(t, s, delivery, "prior_turn_unsettled")
	taskWorkTestReceipt(t, s, origin.Member, terminal, domain.TurnStateSucceeded)
	taskWorkTestPeek(t, s, delivery, "")
	worker := taskWorkTestClaim(t, s, delivery, "b3-child-resource")
	queueEvent(t, s, worker.Claim, provider.AcceptedEvent, "", "")
	if a, v, err := s.ReadTaskWorkArtifact(ctx, worker, conflict.ArtifactID, conflict.ID); err != nil || a.CurrentVersionID != "" || v.ID != conflict.ID || v.ExpectedVersionID != "" || v.ObservedVersionID != "" {
		t.Fatalf("child saw an ungranted version link: %+v %+v %v", a, v, err)
	}
	if _, _, err := s.ReadTaskWorkArtifact(ctx, worker, base.ArtifactID, base.ID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("child inherited the owner's base grant: %v", err)
	}
	if _, err := s.CaptureChatCandidate(ctx, worker); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("delegation wake was captured as intake: %v", err)
	}
	if page, err := s.ReadTaskWorkChannel(ctx, worker, ListOptions{}); err != nil || len(page.Items) == 0 {
		t.Fatalf("child lost its explicit topic grant: %+v %v", page, err)
	} else {
		for _, m := range page.Items {
			if m.TopicID != p.TopicID || m.GroupID != f.groupID {
				t.Fatalf("child read out-of-scope material: %+v", m)
			}
		}
	}
	if _, err := s.ListWorkInputsForMember(ctx, worker); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("child inherited the owner's input scope: %v", err)
	}
	if _, err := s.DelegateWork(ctx, worker, input, "recursive"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("child could delegate: %v", err)
	}
	if _, err := s.SubmitTaskWorkResult(ctx, worker, AssignmentResultInput{Summary: "不能代提"}, "not-submit"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("child could submit: %v", err)
	}
	wakeCounts := workReportWakeCounts(t, s)
	progressInput := domain.ReportWorkInput{Kind: domain.TaskWorkReportProgress, Summary: "已核对第一条。"}
	progress := recordWorkReportProgress(t, s, worker, delegation, progressInput, "progress")
	if got := workReportWakeCounts(t, s); got != wakeCounts {
		t.Fatalf("progress changed message/delivery/wake counts: %v, want %v", got, wakeCounts)
	}
	before := messageSequenceSnapshot(t, s)
	if again, err := s.ReportWork(ctx, worker, progressInput, "progress"); err != nil || again != progress {
		t.Fatalf("progress replay: %+v %v", again, err)
	}
	messageSequencePreserved(t, s, before)
	finalInput := domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "已核对全部意见：一条仍未回应。"}
	if _, err := s.ReportWork(ctx, worker, domain.ReportWorkInput{Kind: finalInput.Kind, Summary: finalInput.Summary, AnswerTo: followup.WorkInputID}, "deliver"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unverified answer_to accepted: %v", err)
	}
	messageSequencePreserved(t, s, before)
	report, err := s.ReportWork(ctx, worker, finalInput, "deliver")
	if err != nil || report.ID == "" || report.DeliveryID == "" || report.DelegationID != delegation.ID ||
		report.ActorID != worker.MemberID || report.OwnerMemberID != origin.Member.MemberID || report.SourceDeliveryID != delivery.DeliveryID || report.SourceMessageID != delivery.MessageID {
		t.Fatalf("not an exact child report: %+v %v", report, err)
	}
	wake, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, report.DeliveryID)
	if err != nil || wake.SourceKind != domain.TaskWorkSourceReport || wake.SourceID != report.ID || wake.MemberID != origin.Member.MemberID || wake.Blocked != "prior_turn_unsettled" {
		t.Fatalf("owner wake did not wait for child terminal evidence: %+v %v", wake, err)
	}
	before = messageSequenceSnapshot(t, s)
	if again, err := s.ReportWork(ctx, worker, finalInput, "deliver"); err != nil || again != report {
		t.Fatalf("delivery replay: %+v %v", again, err)
	}
	if _, err := s.ReportWork(ctx, worker, progressInput, "deliver"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed report replay payload: %v", err)
	}
	if _, err := s.ReportWork(ctx, worker, finalInput, "duplicate"); !errors.Is(err, ErrConflict) {
		t.Fatalf("child could repeatedly deliver: %v", err)
	}
	messageSequencePreserved(t, s, before)
	if task, err := s.GetTask(ctx, f.groupID, p.TaskID); err != nil || task.Status != domain.TaskStatusOpen || task.ActiveAttemptID != origin.AttemptID {
		t.Fatalf("child altered formal task lifecycle: %+v %v", task, err)
	}
	if _, err := s.RequestTurnCancel(ctx, f.groupID, worker.Claim.TurnID); err != nil {
		t.Fatal(err)
	}
	assertWaiting := func() {
		t.Helper()
		taskWorkTestPeek(t, s, wake, "prior_turn_unsettled")
		if turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: wake.DeliveryID, MaxActive: 8}); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
			t.Fatalf("claim overtook unsettled source: %+v %v", turn, err)
		}
		var held string
		if err := s.db.QueryRow(`SELECT turn_id FROM resource_claims WHERE resource_key='b3-child-resource'`).Scan(&held); err != nil || held != worker.Claim.TurnID {
			t.Fatalf("wait released child resource: %q %v", held, err)
		}
		if _, err := s.ReportWork(ctx, worker, finalInput, "deliver"); !errors.Is(err, ErrMemberToolDenied) {
			t.Fatalf("unsettled non-live claim could replay: %v", err)
		}
	}
	assertWaiting()
	taskWorkTestReceipt(t, s, worker, uncertain, domain.TurnStateUnknown)
	queueDeliveryState(t, s, queueTurn(t, s, worker.Claim), domain.DeliveryStateAccepted)
	assertWaiting()
	taskWorkTestReceipt(t, s, worker, terminal, domain.TurnStateSucceeded)
	taskWorkTestPeek(t, s, wake, "")
	queued, err := s.ResolveCoordinationDelivery(ctx, p.GroupID, wake.DeliveryID)
	if err != nil || queued.Provenance.SourceKind != domain.TaskWorkSourceReport || queued.Provenance.SourceID != report.ID {
		t.Fatalf("wrong report projection: %+v %v", queued, err)
	}
	next := taskWorkTestClaim(t, s, wake, "b3-owner-resource")
	attempt, err := s.BindTaskWorkContinuation(ctx, next)
	if err != nil || attempt.TaskID != p.TaskID || attempt.ID == origin.AttemptID {
		t.Fatalf("owner continuation attempt: %+v %v", attempt, err)
	}
	wrong := queued.Provenance
	wrong.SourceID = progress.ID
	if _, err := s.ResolveInvestigationExecution(ctx, next, wrong); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("report continuation accepted a different source: %v", err)
	}
	if state, err := s.ResolveInvestigationExecution(ctx, next, queued.Provenance); err != nil || state.Blocked != CoordinationReady || state.AttemptID != attempt.ID {
		t.Fatalf("lost read-only role on the report continuation: %+v %v", state, err)
	}
	if a, v, err := s.ReadTaskWorkArtifact(ctx, next, conflict.ArtifactID, conflict.ID); err != nil || a.CurrentVersionID != "" || v.ExpectedVersionID != base.ID || v.ObservedVersionID != "" || conflict.ObservedVersionID != head.ID {
		t.Fatalf("owner's version links were not independently fenced: %+v %+v %v", a, v, err)
	}
	if saved, err := s.GetWorkReportForMember(ctx, next, wake.SourceID); err != nil || saved != report {
		t.Fatalf("owner did not read exact whole report: %+v %v", saved, err)
	}
	if page, err := s.ListWorkReportsForMember(ctx, next, ListOptions{Limit: 1}); err != nil || len(page.Items) != 1 || page.Items[0] != report || page.NextCursor == nil {
		t.Fatalf("owner report list: %+v %v", page, err)
	} else if second, err := s.ListWorkReportsForMember(ctx, next, ListOptions{Limit: 1, Before: page.NextCursor}); err != nil || len(second.Items) != 1 || second.Items[0] != progress || second.NextCursor != nil {
		t.Fatalf("owner report pagination: %+v %v", second, err)
	}
	formal := AssignmentResultInput{Summary: report.Summary}
	before = messageSequenceSnapshot(t, s)
	if denied, err := s.SubmitTaskWorkResult(ctx, next, formal, "formal-submit"); !errors.Is(err, ErrConflict) || denied.ID != "" {
		t.Fatalf("report substituted for input acknowledgement: %+v %v", denied, err)
	}
	messageSequencePreserved(t, s, before)
	if ack, err := s.AcknowledgeWorkInputWithKey(ctx, next, followup.WorkInputID, "b3-followup-ack"); err != nil || ack.Consumption == nil || ack.Consumption.AttemptID != attempt.ID || ack.Content != supplement {
		t.Fatalf("report owner lost B1 input ownership: %+v %v", ack, err)
	}
	result, err := s.SubmitTaskWorkResult(ctx, next, formal, "formal-submit")
	if err != nil || result.AttemptID != attempt.ID || result.TaskID != p.TaskID || result.SubmittedBy != origin.Member.MemberID {
		t.Fatalf("owner could not formally submit: %+v %v", result, err)
	}
	if task, err := s.GetTask(ctx, f.groupID, p.TaskID); err != nil || task.Status != domain.TaskStatusSubmitted {
		t.Fatalf("formal local-user review was bypassed: %+v %v", task, err)
	}
	if _, err := s.ReviewTask(ctx, taskReviewInput(result)); err != nil {
		t.Fatal(err)
	}
	if page, err := s.ListTaskWorkReports(ctx, f.groupID, p.TaskID, ListOptions{}); err != nil || len(page.Items) != 2 || page.Items[0] != report {
		t.Fatalf("lost historical reports after formal review: %+v %v", page, err)
	}
	if _, err := s.ListTaskWorkReports(ctx, f.otherID, p.TaskID, ListOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-group report disclosure: %v", err)
	}
	dispatchAssertFK(t, s)
}

func TestB3WorkReportRedeliveryAndSerialDelegation(t *testing.T) {
	f := chatSetup(t, "user", false)
	s := f.s
	sibling := member(t, s, f.groupID)
	f, owner, child, _ := workReportOwnerWithPolicy(t, f, domain.ChatInvestigationPolicy{
		Objective: investigationObjective, AllowTopicMessages: true, MaxDelegations: 2,
	}, sibling.ID)
	p := owner.Provenance
	first, err := s.DelegateWork(ctx, owner.Member, domain.DelegateWorkInput{MemberID: child.ID, Question: "先核对第一项"}, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.DelegateWork(ctx, owner.Member, domain.DelegateWorkInput{MemberID: sibling.ID, Question: "再核对第二项"}, "second")
	if err != nil || second.Ordinal != 2 {
		t.Fatalf("second delegation: %+v %v", second, err)
	}
	firstWake, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, first.DeliveryID)
	if err != nil || firstWake.Blocked != "" {
		t.Fatalf("first delegation: %+v %v", firstWake, err)
	}
	worker := taskWorkTestClaim(t, s, firstWake)
	terminal := queueReceipt(provider.OutcomeSucceeded)
	queueEvent(t, s, worker.Claim, provider.AcceptedEvent, "", "")
	block := domain.ReportWorkInput{Kind: domain.TaskWorkReportBlocked, Summary: "需要核对第二项。"}
	r1, err := s.ReportWork(ctx, worker, block, "blocked")
	if err != nil {
		t.Fatal(err)
	}
	followed, err := s.ReportWork(ctx, worker, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "已交付第一项。"}, "delivered")
	if err != nil || followed.ID == r1.ID || followed.DeliveryID == r1.DeliveryID {
		t.Fatalf("distinct report sources coalesced: %+v %v", followed, err)
	}
	siblingWake, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, second.DeliveryID)
	if err != nil || siblingWake.Blocked != "prior_turn_unsettled" {
		t.Fatalf("a different member was allowed to run beside the first child: %+v %v", siblingWake, err)
	}
	if turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: p.GroupID, DeliveryID: second.DeliveryID, MaxActive: 8}); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
		t.Fatalf("sibling claim bypassed readiness: %+v %v", turn, err)
	}
	taskWorkTestReceipt(t, s, worker, terminal, domain.TurnStateSucceeded)
	worker2 := taskWorkTestClaim(t, s, siblingWake)
	// The wrong live Turn cannot impersonate the first worker at the report seam.
	forged := worker2
	forged.MemberID = worker.MemberID
	if _, err := s.ReportWork(ctx, forged, block, "blocked"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("accepted another member with sibling claim: %v", err)
	}
	taskWorkTestReceipt(t, s, worker2, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	taskWorkTestReceipt(t, s, owner.Member, terminal, domain.TurnStateSucceeded)

	// An unopened process sees the same directed wake, but cannot claim it.
	s = queueHandle(t, f.path, false)
	wake, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, r1.DeliveryID)
	if err != nil || wake.SourceID != r1.ID || wake.MemberID != p.MemberID || wake.AssignmentID != p.AssignmentID || wake.Blocked != "task_work_execution_disabled" {
		t.Fatalf("reopen lost the typed owner wake: %+v %v", wake, err)
	}
	if turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: p.GroupID, DeliveryID: wake.DeliveryID, MaxActive: 8}); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
		t.Fatalf("closed process claimed typed report: %+v %v", turn, err)
	}
	s.EnableTaskWorkExecution()
	taskWorkTestPeek(t, s, wake, "investigation_scope_stale")
	s.EnableInvestigationExecution()
	next := taskWorkTestClaim(t, s, wake)
	attempt, err := s.BindTaskWorkContinuation(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetWorkReportForMember(ctx, next, r1.ID); err != nil || got != r1 {
		t.Fatalf("wrong report on redelivery: %+v %v", got, err)
	}
	// B3 does not choose the owner's next action. The owner can return the issue
	// without automatically submitting or starting the coordinator.
	if _, err := s.HandbackWork(ctx, next, domain.HandbackWorkInput{Summary: "请查看阻塞问题。"}, "return"); err != nil {
		t.Fatal(err)
	}
	if task, err := s.GetTask(ctx, p.GroupID, p.TaskID); err != nil || task.Status != domain.TaskStatusOpen || task.CurrentSubmissionID != "" || task.ActiveAttemptID != attempt.ID {
		t.Fatalf("handback decided the task: %+v %v", task, err)
	}
	later, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, followed.DeliveryID)
	if err != nil || later.SourceID != followed.ID || later.MemberID != p.MemberID || later.Blocked != "prior_turn_unsettled" {
		t.Fatalf("later source escaped the bound running owner: %+v %v", later, err)
	}
	taskWorkTestReceipt(t, s, next, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	taskWorkTestPeek(t, s, wake, "already_claimed")
	if turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: p.GroupID, DeliveryID: wake.DeliveryID, MaxActive: 8}); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
		t.Fatalf("the same physical report was reclaimed: %+v %v", turn, err)
	}
	last := taskWorkTestClaim(t, s, later)
	if a, err := s.BindTaskWorkContinuation(ctx, last); err != nil || a.ID == attempt.ID || a.TaskID != p.TaskID || last.MemberID != p.MemberID {
		t.Fatalf("second actionable report changed ownership: %+v %v", a, err)
	}
	if got, err := s.GetWorkReportForMember(ctx, last, followed.ID); err != nil || got != followed {
		t.Fatalf("second actionable report lost source: %+v %v", got, err)
	}
}

func TestB3WorkDelegationReplayAndAuthority(t *testing.T) {
	f, owner, child, policy := workReportOwner(t, 1)
	s, p := f.s, owner.Provenance
	view, err := s.GetAssignment(ctx, f.groupID, p.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	for name, member := range map[string]string{"self": p.MemberID, "coordinator": view.Decision.CoordinatorMemberID, "outside-group": f.foreign} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.DelegateWork(ctx, owner.Member, domain.DelegateWorkInput{MemberID: member, Question: "不可选接收者"}, "same-key"); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("unauthorized target accepted: %v", err)
			}
		})
	}
	before := messageSequenceSnapshot(t, s)
	tx, err := s.beginMemberToolTx(ctx, owner.Member)
	if err != nil {
		t.Fatal(err)
	}
	ownerFacts, _, err := taskWorkOwnerInTx(ctx, tx, owner.Member)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	_, err = taskWorkB3WakeInTx(ctx, tx, owner.Member, ownerFacts.v, domain.TaskWorkSourceDelegation, "forged-source", child.ID, false, nil)
	tx.Rollback()
	if err == nil {
		t.Fatal("typed marker could be inserted without its logical delegation")
	}
	input := domain.DelegateWorkInput{MemberID: child.ID, Question: "不授予资料"}
	forged := owner.Member
	forged.Claim.Token += "bad"
	if _, err := s.DelegateWork(ctx, forged, input, "same-key"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("disconnected scope delegated: %v", err)
	}
	if _, err := s.DelegateWork(ctx, owner.Member, domain.DelegateWorkInput{MemberID: child.ID, Question: input.Question,
		ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: "not-allowed", VersionID: "not-allowed"}}}, "same-key"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("unapproved artifact granted: %v", err)
	}
	messageSequencePreserved(t, s, before)
	d, err := s.DelegateWork(ctx, owner.Member, input, "same-key")
	if err != nil || d.AllowTopicMessages || len(d.ArtifactVersions) != 0 || d.Ordinal != 1 || d.MaxDelegations != 1 {
		t.Fatalf("bad default grant: %+v %v", d, err)
	}
	before = messageSequenceSnapshot(t, s)
	if _, err := s.DelegateWork(ctx, owner.Member, input, "next-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("exhausted budget granted another child: %v", err)
	}
	changed := input
	changed.Question += "？"
	if _, err := s.DelegateWork(ctx, owner.Member, changed, "same-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay succeeded: %v", err)
	}
	messageSequencePreserved(t, s, before)
	wake, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, d.DeliveryID)
	if err != nil || wake.Blocked != "" {
		t.Fatalf("known active origin unexpectedly blocked: %+v %v", wake, err)
	}
	worker := taskWorkTestClaim(t, s, wake)
	if _, err := s.ReadTaskWorkChannel(ctx, worker, ListOptions{}); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("omitted grant inherited parent's history: %v", err)
	}
	progress := domain.ReportWorkInput{Kind: domain.TaskWorkReportProgress, Summary: "进度。"}
	r, err := s.ReportWork(ctx, worker, progress, "progress")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetWorkReportForMember(ctx, owner.Member, r.ID); err != nil || got != r {
		t.Fatalf("live owner could not see progress during child overlap: %+v %v", got, err)
	}
	if _, err := s.ReportWork(ctx, owner.Member, progress, "progress"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("parent claimed the child report: %v", err)
	}
	if _, err := s.GetWorkReportForMember(ctx, worker, r.ID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("child inherited owner's read: %v", err)
	}
	policy.Enabled = false
	if _, err := s.SetChatPolicy(ctx, policy.ThreadRef, policy.ChatPolicyRequest, policy.Revision, "stop"); err != nil {
		t.Fatal(err)
	}
	before = messageSequenceSnapshot(t, s)
	if got, err := s.GetTaskWorkExecution(ctx, worker); err != nil || got.Blocked != "investigation_scope_unavailable" {
		t.Fatalf("live child kept a stale policy: %+v %v", got, err)
	}
	if _, err := s.ReportWork(ctx, worker, progress, "another-progress"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale policy allowed a new report: %v", err)
	}
	if got, err := s.ReportWork(ctx, worker, progress, "progress"); err != nil || got != r {
		t.Fatalf("lost report replay under live scope: %+v %v", got, err)
	}
	if got, err := s.DelegateWork(ctx, owner.Member, input, "same-key"); err != nil || !reflect.DeepEqual(got, d) {
		t.Fatalf("lost delegation replay under live scope: %+v %v", got, err)
	}
	messageSequencePreserved(t, s, before)
	taskWorkTestReceipt(t, s, worker, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}, domain.TurnStateUnknown)
	if _, err := s.ReportWork(ctx, worker, progress, "progress"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("unknown claim could replay: %v", err)
	}
}

func TestB3WorkReportAtomicWritesAndHandback(t *testing.T) {
	f, owner, child, _ := workReportOwner(t, 1)
	s := f.s
	input := domain.DelegateWorkInput{MemberID: child.ID, Question: "核对后报告。"}
	before := messageSequenceSnapshot(t, s)
	if _, err := s.db.Exec(`CREATE TRIGGER b3_ignore_delegation BEFORE INSERT ON task_work_delegations BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if d, err := s.DelegateWork(ctx, owner.Member, input, "delegate"); !errors.Is(err, ErrConflict) || d.ID != "" {
		t.Fatalf("ignored delegation acknowledged: %+v %v", d, err)
	}
	messageSequencePreserved(t, s, before)
	if _, err := s.db.Exec(`DROP TRIGGER b3_ignore_delegation`); err != nil {
		t.Fatal(err)
	}
	d, err := s.DelegateWork(ctx, owner.Member, input, "delegate")
	if err != nil {
		t.Fatal(err)
	}
	handback := domain.HandbackWorkInput{Summary: "如有争议请复核。"}
	h, err := s.HandbackWork(ctx, owner.Member, handback, "handback")
	if err != nil || h.TaskID != d.TaskID || h.CoordinatorID == "" || h.CoordinatorID == owner.Member.MemberID {
		t.Fatalf("failed bound handback: %+v %v", h, err)
	}
	m, err := s.GetMessage(ctx, f.groupID, h.NoticeMessageID)
	if err != nil || m.TopicID != owner.Member.TopicID || len(m.Deliveries) != 0 || m.SourceKind != "platform" || m.SourceID != "work:handback:"+h.ID {
		t.Fatalf("handback attribution or wake: %+v %v", m, err)
	}
	before = messageSequenceSnapshot(t, s)
	if again, err := s.HandbackWork(ctx, owner.Member, handback, "handback"); err != nil || again != h {
		t.Fatalf("handback replay changed notice: %+v %v", again, err)
	}
	messageSequencePreserved(t, s, before)
	wake, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, d.DeliveryID)
	if err != nil || wake.Blocked != "" {
		t.Fatalf("no child wake: %+v %v", wake, err)
	}
	worker := taskWorkTestClaim(t, s, wake)
	// A wrong Turn or a logical report without its wake must not take a slot.
	// The second insert reaches the deferred foreign key at commit.
	const insert = `INSERT INTO task_work_reports(id,delegation_id,group_id,task_id,worker_member_id,source_turn_id,kind,summary,ordinal,delivery_id,created_at)
	 VALUES('uncommitted-report',?,?,?,?,?,'question','核对',1,'missing-report-wake',?)`
	before = messageSequenceSnapshot(t, s)
	if _, err := s.db.Exec(insert, d.ID, d.GroupID, d.TaskID, worker.MemberID, owner.Member.Claim.TurnID, d.CreatedAt.Format(timestampLayout)); err == nil {
		t.Fatal("report schema accepted the owner's Turn in place of the child")
	}
	if _, err := s.db.Exec(insert, d.ID, d.GroupID, d.TaskID, worker.MemberID, worker.Claim.TurnID, d.CreatedAt.Format(timestampLayout)); err == nil {
		t.Fatal("logical actionable report committed without its typed wake")
	} else if varErr := (*sqlite.Error)(nil); !errors.As(err, &varErr) || varErr.Code() != sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
		t.Fatalf("expected the deferred report FK, got %v", err)
	}
	messageSequencePreserved(t, s, before)
	wakeCounts := workReportWakeCounts(t, s)
	progress := domain.ReportWorkInput{Kind: domain.TaskWorkReportProgress, Summary: "进度记录。"}
	for i := 0; i < domain.MaxWorkReportsPerDelegation-1; i++ {
		recordWorkReportProgress(t, s, worker, d, progress, fmt.Sprintf("progress-%d", i))
	}
	if got := workReportWakeCounts(t, s); got != wakeCounts {
		t.Fatalf("progress changed message/delivery/wake counts: %v, want %v", got, wakeCounts)
	}
	before = messageSequenceSnapshot(t, s)
	if _, err := s.ReportWork(ctx, worker, progress, "last-slot"); !errors.Is(err, ErrConflict) {
		t.Fatalf("progress consumed the escalation slot: %v", err)
	}
	messageSequencePreserved(t, s, before)
	if _, err := s.db.Exec(`CREATE TRIGGER b3_ignore_report_wake BEFORE INSERT ON task_work_deliveries
 WHEN NEW.source_kind='report' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	in := domain.ReportWorkInput{Kind: domain.TaskWorkReportQuestion, Summary: "需要确认什么？"}
	if r, err := s.ReportWork(ctx, worker, in, "last-slot"); !errors.Is(err, ErrConflict) || r.ID != "" {
		t.Fatalf("ignored wake acknowledged: %+v %v", r, err)
	}
	messageSequencePreserved(t, s, before)
	page, err := s.ListTaskWorkReports(ctx, d.GroupID, d.TaskID, ListOptions{})
	if err != nil || len(page.Items) != domain.MaxWorkReportsPerDelegation-1 {
		t.Fatalf("logical report survived failed wake: %+v %v", page, err)
	}
	coordinationDecideExec(t, s, `DROP TRIGGER b3_ignore_report_wake`)
	coordinationDecideExec(t, s, `CREATE TRIGGER b3_ignore_report BEFORE INSERT ON task_work_reports WHEN NEW.kind<>'progress' BEGIN SELECT RAISE(IGNORE); END`)
	if r, err := s.ReportWork(ctx, worker, in, "last-slot"); !errors.Is(err, ErrConflict) || r.ID != "" {
		t.Fatalf("ignored report source acknowledged: %+v %v", r, err)
	}
	messageSequencePreserved(t, s, before)
	coordinationDecideExec(t, s, `DROP TRIGGER b3_ignore_report`)
	r, err := s.ReportWork(ctx, worker, in, "last-slot")
	if err != nil || r.DeliveryID == "" {
		t.Fatalf("rollback consumed the last slot for a question: %+v %v", r, err)
	}
	if m, err := s.GetMessage(ctx, f.groupID, r.MessageID); err != nil || m.SourceKind != "platform" || m.SourceID != "work:report:"+r.ID {
		t.Fatalf("report wake lost typed source: %+v %v", m, err)
	}
	if _, err := s.HandbackWork(ctx, worker, domain.HandbackWorkInput{Summary: "不可转派"}, "not-handback"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("child could hand back: %v", err)
	}
	// Corrupt the source only in this disposable fixture, leaving the marker.
	coordinationDecideExec(t, s, `DROP TRIGGER task_work_reports_no_delete`)
	coordinationDecideExec(t, s, `DELETE FROM task_work_reports WHERE id=?`, r.ID)
	before = messageSequenceSnapshot(t, s)
	if _, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, r.DeliveryID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("unproven report resolved: %v", err)
	}
	if _, err := s.ReportWork(ctx, worker, in, "last-slot"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("unproven report replayed: %v", err)
	}
	messageSequencePreserved(t, s, before)
}
