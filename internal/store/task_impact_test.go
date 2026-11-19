package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func impactInput(t *testing.T, f chatFixture, quote, constraint string) (domain.ChatAssessmentReceipt, domain.ChatCandidate) {
	t.Helper()
	a, c := newWorkInputCandidate(t, f, quote)
	apply := f.s.ApplyChatAssessment
	if !f.s.taskWorkExecution.Load() {
		apply = f.s.ApplyChatAssessmentDormant
	}
	out, err := apply(ctx, a, c.ID, domain.ChatAssessment{
		Disposition: domain.ChatDispositionContinueWork, Reason: "原任务的补充要求", Followup: &domain.ChatFollowupAssessment{Constraint: constraint},
	}, "记录原要求。", "impact-"+c.ID)
	if err != nil || out.WorkInputID == "" {
		t.Fatalf("trusted followup: %+v %v", out, err)
	}
	taskWorkTestReceipt(t, f.s, a, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	return out, c
}

func impacts(t *testing.T, s *Store, groupID, taskID string) []domain.TaskImpact {
	t.Helper()
	page, err := s.ListTaskImpacts(ctx, groupID, taskID, ListOptions{})
	if err != nil || page.NextCursor != nil {
		t.Fatalf("task impacts: %+v %v", page, err)
	}
	return page.Items
}

func requireImpact(t *testing.T, items []domain.TaskImpact, kind domain.TaskImpactKind, evidenceID string) domain.TaskImpact {
	t.Helper()
	for _, item := range items {
		if item.Kind == kind && item.EvidenceID == evidenceID {
			return item
		}
	}
	t.Fatalf("missing impact %s %q: %+v", kind, evidenceID, items)
	return domain.TaskImpact{}
}

func TestConstraintImpactReplansWithExistingOwnerAndExactReports(t *testing.T) {
	f, owner, child, _ := workReportOwner(t, 3)
	s, p := f.s, owner.Provenance
	delegate := func(scope MemberToolScope, question, key string) domain.TaskWorkDelegation {
		t.Helper()
		d, err := s.DelegateWork(ctx, scope, domain.DelegateWorkInput{MemberID: child.ID, Question: question}, key)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := delegate(owner.Member, "核对当前格式。", "old-question")
	wake, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, d.DeliveryID)
	if err != nil || wake.Blocked != "" {
		t.Fatalf("delegation readiness: %+v %v", wake, err)
	}
	worker := taskWorkTestClaim(t, s, wake, "child-impact-resource")
	queueEvent(t, s, worker.Claim, provider.AcceptedEvent, "", "")
	report, err := s.ReportWork(ctx, worker, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "按当前格式核对完毕。"}, "old-report")
	if err != nil {
		t.Fatal(err)
	}
	terminal := queueReceipt(provider.OutcomeSucceeded)
	taskWorkTestReceipt(t, s, worker, terminal, domain.TurnStateSucceeded)
	queued := delegate(owner.Member, "也核对当前校验字段。", "old-queued")
	const text = "保持旧格式兼容"
	r, candidate := impactInput(t, f, text, text)
	if len(impacts(t, s, p.GroupID, p.TaskID)) != 0 || r.WorkDeliveryID == "" {
		t.Fatal("an unconfirmed proposal applied or had no real continuation")
	}
	if _, err := s.AcknowledgeWorkInputWithKey(ctx, worker, r.WorkInputID, "unauthorized"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("delegate could ACK the owner's constraint: %v", err)
	}
	taskWorkTestReceipt(t, s, owner.Member, terminal, domain.TurnStateSucceeded)
	followup, err := s.ResolveTaskWorkDelivery(ctx, f.groupID, r.WorkDeliveryID)
	if err != nil || followup.Blocked != "" {
		t.Fatalf("followup readiness: %+v %v", followup, err)
	}
	scope := taskWorkTestClaim(t, s, followup)
	attempt, err := s.BindTaskWorkContinuation(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	queueEvent(t, s, scope.Claim, provider.AcceptedEvent, "", "")

	// Even an ignored secondary effect row must roll back the ACK and notice.
	before := messageSequenceSnapshot(t, s)
	coordinationDecideExec(t, s, `CREATE TEMP TRIGGER ignore_impact BEFORE INSERT ON task_constraint_impacts
	 WHEN NEW.kind='stale_work_report' BEGIN SELECT RAISE(IGNORE); END`)
	if got, err := s.AcknowledgeWorkInputWithKey(ctx, scope, r.WorkInputID, "impact-ack"); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("partial effect was treated as confirmed: %+v %v", got, err)
	}
	coordinationDecideExec(t, s, `DROP TRIGGER ignore_impact`)
	messageSequencePreserved(t, s, before)
	if got, err := s.GetWorkInputForMember(ctx, scope, r.WorkInputID); err != nil || got.Consumption != nil || len(impacts(t, s, p.GroupID, p.TaskID)) != 0 {
		t.Fatalf("rollback left a confirmation or impact: %+v %v", got, err)
	}

	ack, err := s.AcknowledgeWorkInputWithKey(ctx, scope, r.WorkInputID, "impact-ack")
	if err != nil || ack.Consumption == nil {
		t.Fatalf("ACK: %+v %v", ack, err)
	}
	items := impacts(t, s, p.GroupID, p.TaskID)
	applied := requireImpact(t, items, domain.TaskImpactConstraintConfirmed, "")
	if len(items) != 4 || applied.Source.MessageID != candidate.MessageID || applied.Source.SourceID != candidate.SourceID ||
		applied.Source.AssignmentID != p.AssignmentID || applied.Source.MandateID != ack.MandateID || applied.Source.AuthorityGeneration != followup.AuthorityGeneration ||
		applied.Source.OwnerMemberID != scope.MemberID || applied.TaskRevision != attempt.TaskRevision ||
		applied.Acknowledgement == nil || *applied.Acknowledgement != *ack.Consumption || applied.Acknowledgement.AttemptID != attempt.ID ||
		applied.Plan == nil || applied.Plan.BeforeConstraintRevision != 0 || applied.Plan.AfterConstraintRevision != 1 ||
		applied.Plan.AfterConstraint != text || applied.Plan.Acceptance == "" || applied.Delivery == nil ||
		applied.Delivery.AcceptedAt == nil || applied.Delivery.DeliveryID != r.WorkDeliveryID || applied.Delivery.MemberID != scope.MemberID {
		t.Fatalf("impact lost source, plan, native reception or exact ACK: %+v", applied)
	}
	notice, err := s.GetMessage(ctx, p.GroupID, applied.Plan.NoticeMessageID)
	if err != nil || notice.SourceKind != "platform" || notice.TopicID != p.TopicID || len(notice.Deliveries) != 0 {
		t.Fatalf("confirmation notice escaped its scope or woke a model: %+v %v", notice, err)
	}
	requireImpact(t, items, domain.TaskImpactStaleWorkReport, report.ID)
	requireImpact(t, items, domain.TaskImpactStaleDelegation, d.ID)
	requireImpact(t, items, domain.TaskImpactStaleDelegation, queued.ID)
	if saved, err := s.GetTaskWorkReport(ctx, p.GroupID, report.ID); err != nil || !reflect.DeepEqual(saved, report) {
		t.Fatalf("staleness rewrote a historical report: %+v %v", saved, err)
	}
	if _, err := s.GetWorkReportForMember(ctx, scope, report.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale report was still offered as current proof: %v", err)
	}
	for _, delivery := range []string{queued.DeliveryID, report.DeliveryID} {
		if got, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, delivery); err != nil || got.Blocked != "constraint_impact_stale" {
			t.Fatalf("obsolete wake escaped: %+v %v", got, err)
		}
		if turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: p.GroupID, DeliveryID: delivery, MaxActive: 8}); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
			t.Fatalf("obsolete wake was started: %+v %v", turn, err)
		}
	}
	after := messageSequenceSnapshot(t, s)
	if repeated, err := s.AcknowledgeWorkInputWithKey(ctx, scope, r.WorkInputID, "impact-ack"); err != nil || !reflect.DeepEqual(repeated, ack) {
		t.Fatalf("same-key ACK replay: %+v %v", repeated, err)
	}
	messageSequencePreserved(t, s, after)
	cold := queueHandle(t, f.path, false)
	if saved := impacts(t, cold, p.GroupID, p.TaskID); !reflect.DeepEqual(saved, items) {
		t.Fatalf("impact did not survive reopen: %+v", saved)
	}

	fresh := delegate(scope, "按新要求重新核对旧格式。", "fresh-question")
	freshWake, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, fresh.DeliveryID)
	if err != nil || freshWake.Blocked != "" {
		t.Fatalf("new delegated check under existing scope did not start: %+v %v", freshWake, err)
	}
	verifier := taskWorkTestClaim(t, s, freshWake)
	queueEvent(t, s, verifier.Claim, provider.AcceptedEvent, "", "")
	result := AssignmentResultInput{Summary: "按旧格式兼容要求重新核对后提交。"}
	if got, err := s.SubmitTaskWorkResult(ctx, scope, result, "new-result"); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("submitted while a fresh check was running: %+v %v", got, err)
	}
	freshReport, err := s.ReportWork(ctx, verifier, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "已重新核对旧格式样本。"}, "fresh-report")
	if err != nil {
		t.Fatal(err)
	}
	taskWorkTestReceipt(t, s, verifier, terminal, domain.TurnStateSucceeded)
	if current, err := s.ListWorkReportsForMember(ctx, scope, ListOptions{}); err != nil || len(current.Items) != 1 || current.Items[0].ID != freshReport.ID {
		t.Fatalf("current evidence was not separated from old reports: %+v %v", current, err)
	}
	sub, err := s.SubmitTaskWorkResult(ctx, scope, result, "new-result")
	if err != nil || sub.AttemptID != attempt.ID {
		t.Fatalf("legitimate in-scope continuation did not submit: %+v %v", sub, err)
	}
	if got, err := s.GetTask(ctx, p.GroupID, p.TaskID); err != nil || got.Status != domain.TaskStatusSubmitted || got.CurrentSubmissionID != sub.ID {
		t.Fatalf("result should be submitted, not accepted: %+v %v", got, err)
	}
	if slices.ContainsFunc(impacts(t, s, p.GroupID, p.TaskID), func(x domain.TaskImpact) bool { return x.EvidenceID == freshReport.ID || x.EvidenceID == sub.ID }) {
		t.Fatal("the old impact retroactively marked new verified work stale")
	}
	if _, err := s.ListTaskImpacts(ctx, f.otherID, p.TaskID, ListOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign projection: %v", err)
	}
	dispatchAssertFK(t, s)
}

func TestConstraintImpactContextOnlyAndLateOlderACK(t *testing.T) {
	f := chatSetup(t, "user", false)
	owner := chatAuthorityWorker(t, f)
	for _, in := range []struct{ body, quote string }{{"保持旧格式兼容", "保持旧格式兼容"}, {"保持旧版 CSV 兼容", "保持旧版 CSV 兼容"}, {"供参考：有旧格式样本。", ""}} {
		impactInput(t, f, in.body, in.quote)
	}
	inbox, err := f.s.ListWorkInputsForMember(ctx, owner.Member)
	if err != nil || len(inbox) != 3 {
		t.Fatalf("inbox: %+v %v", inbox, err)
	}
	for _, n := range []int{1, 0, 2} {
		if _, err := f.s.AcknowledgeWorkInputWithKey(ctx, owner.Member, inbox[n].ID, inbox[n].ID); err != nil {
			t.Fatal(err)
		}
	}
	got := impacts(t, f.s, f.groupID, owner.Provenance.TaskID)
	if len(got) != 1 || got[0].Plan == nil || got[0].Plan.AfterConstraintRevision != 2 || got[0].Source.InputID != inbox[1].ID {
		t.Fatalf("late or context-only confirmations falsely changed effective constraint: %+v", got)
	}
	restated, _ := impactInput(t, f, "保持旧版 CSV 兼容", "保持旧版 CSV 兼容")
	if _, err := f.s.AcknowledgeWorkInputWithKey(ctx, owner.Member, restated.WorkInputID, "restated"); err != nil {
		t.Fatal(err)
	}
	if unchanged := impacts(t, f.s, f.groupID, owner.Provenance.TaskID); !reflect.DeepEqual(unchanged, got) {
		t.Fatalf("same effective quote marked current work stale again: %+v", unchanged)
	}
}

func TestConstraintImpactUpgradeDoesNotBackfillHandling(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = original[:taskImpactSchemaVersion-1]
	f := chatSetup(t, "user", false)
	owner := chatAuthorityWorker(t, f)
	initial, _ := impactInput(t, f, "注明来源", "注明来源")
	ack, err := f.s.AcknowledgeWorkInputWithKey(ctx, owner.Member, initial.WorkInputID, "pre-impact")
	if err != nil {
		t.Fatal(err)
	}
	migrations = original
	s := queueHandle(t, f.path, false)
	if same, err := s.AcknowledgeWorkInputWithKey(ctx, owner.Member, initial.WorkInputID, "pre-impact"); err != nil || !reflect.DeepEqual(same, ack) {
		t.Fatalf("upgrade lost the exact migrated ACK: %+v %v", same, err)
	}
	if old := impacts(t, s, f.groupID, ack.TaskID); len(old) != 0 {
		t.Fatalf("upgrade/replay synthesized a retroactive effect: %+v", old)
	}
	f.s = s
	confirmed, _ := impactInput(t, f, "保持旧格式兼容", "保持旧格式兼容")
	if _, err := s.AcknowledgeWorkInputWithKey(ctx, owner.Member, confirmed.WorkInputID, "post-impact"); err != nil {
		t.Fatal(err)
	}
	got := impacts(t, s, f.groupID, ack.TaskID)
	if len(got) != 1 || got[0].Plan == nil || got[0].Plan.BeforeConstraintRevision != 1 || got[0].Plan.AfterConstraintRevision != 2 {
		t.Fatalf("upgrade lost proposal lineage: %+v", got)
	}
	dispatchAssertFK(t, s)
}

func TestConstraintImpactRetainsExactRepairSubmission(t *testing.T) {
	f, original, oldSub := chatRepairFixSubmitted(t, "请交付文本。", "旧版文本交付。")
	review := chatRepairFixBindReview(t, &f, original, oldSub, "拒收，请按原范围修复后提交。")
	var err error
	f.candidate, err = f.s.CaptureChatCandidate(ctx, f.scope)
	if err != nil {
		t.Fatal(err)
	}
	f.input = chatRepairInput(t, f.input, review.ID, "review_request")
	origin, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "按真实评审请求修复。", "impact-repair-intake")
	if err != nil || origin.RepairLink == nil || origin.RepairLink.ParentSubmissionID != oldSub.ID {
		t.Fatalf("expected a separately authorized repair: %+v %v", origin, err)
	}
	taskWorkTestReceipt(t, f.s, f.scope, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	dispatch, err := f.s.GetDispatchRequest(ctx, f.groupID, origin.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	co := coordinationDecideBindRequest(t, f.s, dispatch.Request)
	decision, err := f.s.DecideDispatch(ctx, co, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "按原范围修复"}, "impact-repair-assign")
	if err != nil || decision.Assignment == nil {
		t.Fatalf("repair dispatch: %+v %v", decision, err)
	}
	repair := coordinationDecideWorker(t, requestDispatchFixture{s: f.s, worker: f.b}, dispatch.Request, decision, true)
	taskWorkTestReceipt(t, f.s, co.Member, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	const quote = "保持旧格式兼容"
	r, _ := impactInput(t, f, quote, quote)
	if r.TaskID != repair.Provenance.TaskID || r.TaskID == oldSub.TaskID {
		t.Fatalf("source did not target the real repair: %+v", r)
	}
	if _, err := f.s.AcknowledgeWorkInput(ctx, repair.Member, r.WorkInputID); err != nil {
		t.Fatal(err)
	}
	x := requireImpact(t, impacts(t, f.s, f.groupID, r.TaskID), domain.TaskImpactStaleSubmission, oldSub.ID)
	if x.Submission == nil || x.Submission.TaskID != oldSub.TaskID || x.Submission.TaskRevision != oldSub.TaskRevision || x.TaskID != r.TaskID {
		t.Fatalf("impact lost the actual predecessor generation: %+v", x)
	}
	if saved, err := f.s.GetTaskReview(ctx, f.groupID, oldSub.TaskID, review.ID); err != nil || !reflect.DeepEqual(saved, review) {
		t.Fatalf("constraint change replaced the historical rejection: %+v %v", saved, err)
	}
}
