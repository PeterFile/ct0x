package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func withTaskImpactSchema(t *testing.T) {
	t.Helper()
	if slices.Contains(migrations, taskImpactSchema) {
		return
	}
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = append(slices.Clone(original), taskImpactSchema)
}

func impactInput(t *testing.T, f chatFixture, quote, constraint string) (domain.ChatAssessmentReceipt, domain.ChatCandidate) {
	t.Helper()
	a, c := newWorkInputCandidate(t, f, quote)
	return assessImpactInput(t, f, a, c, constraint), c
}

func assessImpactInput(t *testing.T, f chatFixture, a MemberToolScope, c domain.ChatCandidate, constraint string) domain.ChatAssessmentReceipt {
	t.Helper()
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
	return out
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
	withTaskImpactSchema(t)
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

func TestConstraintImpactIndependentACKs(t *testing.T) {
	withTaskImpactSchema(t)
	for _, tc := range []struct {
		name        string
		assessOrder [2]int
	}{
		{"older-source-ACKed-last", [2]int{0, 1}},
		{"newer-source-assessed-first-ACKed-last", [2]int{1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, owner, child, _ := workReportOwner(t, 1)
			s, scope, p := f.s, owner.Member, owner.Provenance
			quotes := [2]string{"保持旧格式兼容", "注明结论来源"}
			var intake [2]MemberToolScope
			var sources [2]domain.ChatCandidate
			var proposals [2]domain.ChatAssessmentReceipt
			for n, quote := range quotes {
				intake[n], sources[n] = newWorkInputCandidate(t, f, quote)
			}
			if sources[0].Generation >= sources[1].Generation {
				t.Fatalf("user messages must commit in A, B order: %d >= %d", sources[0].Generation, sources[1].Generation)
			}
			for _, n := range tc.assessOrder {
				proposals[n] = assessImpactInput(t, f, intake[n], sources[n], quotes[n])
			}
			// Confirm in reverse proposal order, independently of message order.
			late, early := tc.assessOrder[0], tc.assessOrder[1]
			ack := func(n int) domain.TaskWorkInput {
				t.Helper()
				id := proposals[n].WorkInputID
				got, err := s.AcknowledgeWorkInputWithKey(ctx, scope, id, id)
				if err != nil || got.ID != id || got.Consumption == nil {
					t.Fatalf("ACK %q: %+v %v", quotes[n], got, err)
				}
				return got
			}
			first := ack(early)
			if got := impacts(t, s, p.GroupID, p.TaskID); len(got) != 1 || got[0].Source.InputID != first.ID {
				t.Fatalf("only the first ACK should have an impact so far: %+v", got)
			}
			d, err := s.DelegateWork(ctx, scope, domain.DelegateWorkInput{MemberID: child.ID, Question: "核对已确认的要求。"}, "intervening-check")
			if err != nil {
				t.Fatal(err)
			}
			wake, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, d.DeliveryID)
			if err != nil || wake.Blocked != "" {
				t.Fatalf("intervening delegation: %+v %v", wake, err)
			}
			worker := taskWorkTestClaim(t, s, wake)
			queueEvent(t, s, worker.Claim, provider.AcceptedEvent, "", "")
			report, err := s.ReportWork(ctx, worker, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "已核对已确认的要求。"}, "intervening-report")
			if err != nil {
				t.Fatal(err)
			}
			taskWorkTestReceipt(t, s, worker, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
			if got, err := s.GetWorkReportForMember(ctx, scope, report.ID); err != nil || got != report {
				t.Fatalf("report not current between ACKs: %+v %v", got, err)
			}

			second := ack(late)
			items := impacts(t, s, p.GroupID, p.TaskID)
			if len(items) != 4 {
				t.Fatalf("independent ACKs must record two confirmations and invalidate the intervening delegation and report: %+v", items)
			}
			for order, want := range []domain.TaskWorkInput{second, first} {
				n := tc.assessOrder[order]
				var before string
				if order == 1 {
					before = quotes[late] // The first persisted proposal was still pending when assessed.
				}
				idx := slices.IndexFunc(items, func(x domain.TaskImpact) bool {
					return x.Kind == domain.TaskImpactConstraintConfirmed && x.Source.InputID == want.ID
				})
				if idx < 0 {
					t.Fatalf("missing confirmation for %q: %+v", quotes[n], items)
				}
				x, c := items[idx], sources[n]
				if x.Source.MessageID != c.MessageID || x.Source.SourceSequence != c.Generation || x.Source.SourceSequence != want.SourceSequence ||
					x.Source.SourceID != c.SourceID || x.Source.TriggerID != want.TriggerID || x.Acknowledgement == nil ||
					*x.Acknowledgement != *want.Consumption || x.Decision != nil || x.Plan == nil ||
					x.Plan.BeforeConstraintRevision != int64(order) || x.Plan.BeforeConstraint != before ||
					x.Plan.AfterConstraintRevision != int64(order+1) || x.Plan.AfterConstraint != quotes[n] {
					t.Fatalf("lost original proposal snapshot or exact ACK for %q: %+v", quotes[n], x)
				}
			}
			for _, x := range []domain.TaskImpact{
				requireImpact(t, items, domain.TaskImpactStaleDelegation, d.ID),
				requireImpact(t, items, domain.TaskImpactStaleWorkReport, report.ID),
			} {
				if x.Source.InputID != second.ID || x.Source.SourceSequence != second.SourceSequence ||
					x.Acknowledgement == nil || *x.Acknowledgement != *second.Consumption {
					t.Fatalf("wrong ACK invalidated the intervening evidence: %+v", x)
				}
			}
			if _, err := s.GetWorkReportForMember(ctx, scope, report.ID); !errors.Is(err, ErrConflict) {
				t.Fatalf("pre-confirmation report remained current: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = queueHandle(t, f.path, false)
			s.EnableInvestigationExecution()
			s.EnableTaskWorkExecution()
			if got, err := s.GetTaskWorkReport(ctx, p.GroupID, report.ID); err != nil || got != report {
				t.Fatalf("lost immutable report after reopening: %+v %v", got, err)
			}
			if got, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, report.DeliveryID); err != nil || got.Blocked != "constraint_impact_stale" {
				t.Fatalf("intervening report wake was usable after reopen: %+v %v", got, err)
			}
			before := messageSequenceSnapshot(t, s)
			if !reflect.DeepEqual(ack(early), first) || !reflect.DeepEqual(ack(late), second) {
				t.Fatal("identical replay after reopening changed an ACK")
			}
			messageSequencePreserved(t, s, before)
			if saved := impacts(t, s, p.GroupID, p.TaskID); !reflect.DeepEqual(saved, items) {
				t.Fatalf("reopen or replay changed the impacts: %+v", saved)
			}
		})
	}
}

func TestConstraintImpactContextOnlyAndSharedQuote(t *testing.T) {
	withTaskImpactSchema(t)
	f := chatSetup(t, "user", false)
	owner := chatAuthorityWorker(t, f)
	const quote = "注明结论来源"
	for _, tc := range []struct {
		body, excerpt string
		confirmations int
	}{
		{"在正文" + quote, quote, 1},
		{"供参考：有旧格式样本。", "", 1},
		{"在摘要" + quote, quote, 2},
	} {
		r, _ := impactInput(t, f, tc.body, tc.excerpt)
		if _, err := f.s.AcknowledgeWorkInputWithKey(ctx, owner.Member, r.WorkInputID, r.WorkInputID); err != nil {
			t.Fatal(err)
		}
		got := impacts(t, f.s, f.groupID, owner.Provenance.TaskID)
		if len(got) != tc.confirmations || slices.ContainsFunc(got, func(x domain.TaskImpact) bool {
			return x.Kind == domain.TaskImpactConstraintConfirmed && x.Source.InputID == r.WorkInputID
		}) != (tc.excerpt != "") {
			t.Fatalf("context is not a change and shared excerpt does not identify a replay: %+v", got)
		}
	}
}

func TestConstraintImpactUpgradeDoesNotBackfillHandling(t *testing.T) {
	original := migrations
	t.Cleanup(func() { migrations = original })
	idx := slices.Index(original, taskImpactSchema)
	if idx >= 0 {
		migrations = original[:idx]
	}
	f := chatSetup(t, "user", false)
	owner := chatAuthorityWorker(t, f)
	initial, _ := impactInput(t, f, "注明来源", "注明来源")
	ack, err := f.s.AcknowledgeWorkInputWithKey(ctx, owner.Member, initial.WorkInputID, "pre-impact")
	if err != nil {
		t.Fatal(err)
	}
	if idx >= 0 {
		migrations = original
	} else {
		migrations = append(slices.Clone(original), taskImpactSchema)
	}
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
	withTaskImpactSchema(t)
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
