package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// v30 cannot produce an artifact-bearing chat text Submission or reopen an
// accepted Task for its old Assignment. Seed only that unsupported predecessor
// shape. All pin, Review, Assignment, WorkInput, ACK and queue operations below
// use the normal Store commands; there is no fixture impact/ACK or pin write.
func impactFormerArtifactSubmission(t *testing.T, s *Store, role CoordinationToolScope) (domain.ArtifactVersion, domain.Submission, domain.ReviewDecision) {
	t.Helper()
	p := role.Provenance
	publish := artifactPublishInput(p.GroupID)
	publish.SourceTaskID = p.TaskID
	version := mustPublishArtifact(t, s, publish)
	const subID = "historical-artifact-submission"
	stamp := time.Now().UTC().Format(timestampLayout)
	coordinationDecideExec(t, s, `INSERT INTO task_submissions(id,group_id,task_id,task_revision,attempt_id,summary,submitted_by,created_at)
	 VALUES(?,?,?,?,?,'旧格式交付',?,?)`, subID, p.GroupID, p.TaskID, p.TaskRevision, role.AttemptID, p.MemberID, stamp)
	coordinationDecideExec(t, s, `INSERT INTO task_submission_artifacts(submission_id,group_id,task_id,task_revision,artifact_id,version_id)
	 VALUES(?,?,?,?,?,?)`, subID, p.GroupID, p.TaskID, p.TaskRevision, version.ArtifactID, version.ID)
	coordinationDecideExec(t, s, `UPDATE tasks SET status='submitted',current_submission_id=? WHERE group_id=? AND id=?`, subID, p.GroupID, p.TaskID)
	sub, err := s.GetTaskSubmission(ctx, p.GroupID, p.TaskID, subID)
	if err != nil {
		t.Fatal(err)
	}
	review, err := s.ReviewTask(ctx, impactReviewInput(sub))
	if err != nil {
		t.Fatal(err)
	}
	return version, sub, review
}

func impactReviewInput(submission domain.Submission) ReviewTaskInput {
	in := taskReviewInput(submission)
	in.IdempotencyKey = "review-" + submission.ID
	return in
}

func impactPinnedOwner(t *testing.T, f chatFixture, child domain.Task, memberID, topic string) CoordinationToolScope {
	t.Helper()
	s := f.s
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: topic}
	m := coordinationTriggerMessage(t, s, thread)
	work := requestWorkLink(domain.RequestWorkRequest{TopicID: topic, MessageID: m.ID, Reason: "按固定成果版本处理"}, child)
	if _, err := s.RequestWork(ctx, f.groupID, work, "admit-"+child.ID); err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestDispatch(ctx, f.groupID, child.ID, child.Revision, 1, "安排执行", "dispatch-"+child.ID)
	if err != nil {
		t.Fatal(err)
	}
	co := coordinationDecideBindRequest(t, s, r)
	decision, err := s.DecideDispatch(ctx, co, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: memberID, Reason: "指派该任务负责人"}, "assign-"+child.ID)
	if err != nil || decision.Assignment == nil {
		t.Fatalf("dependent assignment: %+v %v", decision, err)
	}
	taskWorkTestReceipt(t, s, co.Member, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	ready, err := s.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	if err != nil || ready.Delivery == nil {
		t.Fatalf("dependent delivery: %+v %v", ready, err)
	}
	d := ready.Delivery
	state, err := s.ResolveCoordinationDelivery(ctx, f.groupID, d.DeliveryID)
	if err != nil || state.Blocked != "" {
		t.Fatalf("dependent readiness: %+v %v", state, err)
	}
	claimed, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.DeliveryID, MaxActive: 8, ResourceKeys: []string{child.ID}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: claimed.ID, Token: claimed.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	role := CoordinationToolScope{Member: scope, Provenance: state.Provenance}
	binding, err := s.BindAssignmentAttempt(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	role.AttemptID = binding.AttemptID
	queueEvent(t, s, scope.Claim, provider.AcceptedEvent, "", "")
	return role
}

func TestConstraintImpactExactPinsAndCancellationRetainClaims(t *testing.T) {
	withTaskImpactSchema(t)
	f := chatSetup(t, "user", false)
	s := f.s
	second, third := member(t, s, f.groupID), member(t, s, f.groupID)
	if _, err := s.SetDispatchPolicy(ctx, f.groupID, []string{f.b, second.ID, third.ID}, 1, "extra-workers"); err != nil {
		t.Fatal(err)
	}
	chatNextCandidate(t, &f, true)
	owner := chatAuthorityWorker(t, f)
	co := taskWorkTestCoordinator(t, s, f.groupID, owner.Provenance.AssignmentID)
	taskWorkTestReceipt(t, s, co, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	queueEvent(t, s, owner.Member.Claim, provider.AcceptedEvent, "", "")
	v1, sub, review := impactFormerArtifactSubmission(t, s, owner)
	makeChild := func(in CreateTaskInput) domain.Task {
		t.Helper()
		child, err := s.CreateTask(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return child
	}
	pinInput := artifactDependencyCreateInput(sub)
	pinInput.IdempotencyKey = "old-pin"
	runningTask := makeChild(pinInput)
	pinInput.IdempotencyKey = ""
	unknownTask := makeChild(pinInput)
	waitingTask := makeChild(pinInput)
	humanTask := makeChild(pinInput)
	humanAttempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(humanTask))
	if err != nil {
		t.Fatal(err)
	}
	priorResultTask := makeChild(pinInput)
	priorResult := artifactDependencySubmit(t, s, priorResultTask, v1)
	running := impactPinnedOwner(t, f, runningTask, second.ID, "different-topic")
	unknown := impactPinnedOwner(t, f, unknownTask, third.ID, "another-topic")
	uncertain := queueReceipt(provider.OutcomeUnknown)
	uncertain.DirectChildReaped = false
	taskWorkTestReceipt(t, s, unknown.Member, uncertain, domain.TurnStateUnknown)

	other := createTestTask(t, s, f.groupID)
	publish := artifactPublishInput(f.groupID)
	publish.SourceTaskID, publish.ArtifactID, publish.ExpectedVersionID, publish.Name = other.ID, v1.ArtifactID, v1.ID, ""
	v2 := mustPublishArtifact(t, s, publish)
	unrelatedSub := artifactDependencySubmit(t, s, other, v2)
	if _, err := s.ReviewTask(ctx, impactReviewInput(unrelatedSub)); err != nil {
		t.Fatal(err)
	}
	unrelated := makeChild(artifactDependencyCreateInput(unrelatedSub))
	// The deployed lifecycle has no accepted->open command. Do not install
	// one for this fixture or mutate any Submission, Review, version or pin.
	coordinationDecideExec(t, s, `UPDATE tasks SET status='open',current_submission_id=NULL,active_attempt_id=? WHERE id=?`, owner.AttemptID, sub.TaskID)
	r, candidate := impactInput(t, f, "保持旧格式兼容", "保持旧格式兼容")
	otherWake := queueDelivery(t, s, f.otherID, f.foreign, "独立话题")
	otherTurn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.otherID, DeliveryID: otherWake.ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	otherClaim := TurnClaim{GroupID: f.otherID, TurnID: otherTurn.ID, Token: otherTurn.ClaimToken}
	queueEvent(t, s, otherClaim, provider.AcceptedEvent, "", "")
	acked, err := s.AcknowledgeWorkInputWithKey(ctx, owner.Member, r.WorkInputID, "exact-pinned-impact")
	if err != nil || acked.Consumption == nil {
		t.Fatalf("owner ACK: %+v %v", acked, err)
	}
	rootImpacts := impacts(t, s, f.groupID, sub.TaskID)
	proof := requireImpact(t, rootImpacts, domain.TaskImpactStaleSubmission, sub.ID)
	if proof.Submission == nil || proof.Submission.SubmissionID != sub.ID || proof.Submission.TaskRevision != sub.TaskRevision || len(rootImpacts) != 8 {
		t.Fatalf("root view did not include its earlier proof and exact downstream impacts: %+v", rootImpacts)
	}
	for _, task := range []domain.Task{runningTask, unknownTask, waitingTask, humanTask, priorResultTask} {
		got := impacts(t, s, f.groupID, task.ID)
		pin := requireImpact(t, got, domain.TaskImpactStalePin, sub.TaskID)
		if pin.Pin == nil || pin.Pin.ArtifactID != v1.ArtifactID || pin.Pin.VersionID != v1.ID || pin.Pin.ReviewID != review.ID ||
			pin.Pin.ReviewOutcome != domain.ReviewOutcomeAccepted || pin.Pin.SubmissionID != sub.ID || pin.Pin.DependencyRevision != sub.TaskRevision ||
			pin.TaskRevision != task.Revision || pin.Source.MessageID != candidate.MessageID || pin.Decision != nil || pin.Delivery != nil {
			t.Fatalf("wrong exact old pin or fabricated choice: %+v", pin)
		}
		if task.ID == priorResultTask.ID && pin.Pin.AffectedSubmissionID != priorResult.ID {
			t.Fatalf("missing affected result: %+v", pin)
		}
		if task.ID == runningTask.ID {
			cancel := requireImpact(t, got, domain.TaskImpactCancelRequested, running.Member.Claim.TurnID)
			if cancel.Cancellation == nil || cancel.Cancellation.State != domain.TurnStateCancelRequested {
				t.Fatalf("missing actual cancel request: %+v", cancel)
			}
		} else if len(got) != 1 {
			t.Fatalf("cancellation of a non-running owner: %+v", got)
		}
	}
	assertClaim := func(role CoordinationToolScope, state domain.TurnState) {
		t.Helper()
		turn, err := s.GetTurn(ctx, f.groupID, role.Member.Claim.TurnID)
		if err != nil || turn.State != state {
			t.Fatalf("wrong live execution state: %+v %v", turn, err)
		}
		if held, err := s.HasResourceClaim(ctx, role.Provenance.TaskID); err != nil || !held {
			t.Fatalf("resource prematurely released while %s: %v", state, err)
		}
	}
	assertClaim(running, domain.TurnStateCancelRequested)
	assertClaim(unknown, domain.TurnStateUnknown)
	turn, err := s.GetTurn(ctx, f.groupID, unknown.Member.Claim.TurnID)
	if err != nil || turn.CancelRequestedAt != nil {
		t.Fatalf("unknown turn was cancelled: %+v %v", turn, err)
	}
	taskWorkTestReceipt(t, s, running.Member, uncertain, domain.TurnStateUnknown)
	assertClaim(running, domain.TurnStateUnknown)
	taskWorkTestReceipt(t, s, running.Member, queueReceipt(provider.OutcomeAborted), domain.TurnStateAborted)
	if held, err := s.HasResourceClaim(ctx, runningTask.ID); err != nil || held {
		t.Fatalf("terminal receipt did not release the claim: %v", err)
	}
	assertClaim(unknown, domain.TurnStateUnknown)
	if saved, err := s.GetTurn(ctx, f.otherID, otherTurn.ID); err != nil || saved.State != domain.TurnStateRunning || saved.CancelRequestedAt != nil {
		t.Fatalf("unrelated Turn was affected: %+v %v", saved, err)
	}
	if _, err := s.ApplyTurnReceipt(ctx, otherClaim, queueReceipt(provider.OutcomeSucceeded)); err != nil {
		t.Fatal(err)
	}
	foreignProbe := queueDelivery(t, s, f.otherID, f.foreign, "验证资源占用")
	if _, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.otherID, DeliveryID: foreignProbe.ID, MaxActive: 8,
		ResourceKeys: []string{unknownTask.ID}}); !errors.Is(err, ErrTurnBusy) {
		t.Fatalf("unknown resource escaped global mutual exclusion: %v", err)
	}

	// Even a still-accepted head cannot make this exact immutable pin usable.
	coordinationDecideExec(t, s, `UPDATE tasks SET status='accepted',current_submission_id=?,active_attempt_id=NULL WHERE id=?`, sub.ID, sub.TaskID)
	if _, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(waitingTask)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale pin permitted a new start: %v", err)
	}
	if _, err := s.SubmitTask(ctx, taskSubmissionInput(humanAttempt)); !errors.Is(err, ErrConflict) {
		t.Fatalf("an already active dependent committed with its stale pin: %v", err)
	}
	if _, err := s.ReviewTask(ctx, impactReviewInput(priorResult)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale downstream output was accepted as current: %v", err)
	}
	if _, err := s.CreateTask(ctx, pinInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("fresh pin to stale proof was accepted: %v", err)
	}
	pinInput.IdempotencyKey = "old-pin"
	if replay, err := s.CreateTask(ctx, pinInput); err != nil || replay.ID != runningTask.ID {
		t.Fatalf("historical pin replay failed: %+v %v", replay, err)
	}
	otherResult := artifactDependencySubmit(t, s, unrelated, v2)
	if _, err := s.ReviewTask(ctx, impactReviewInput(otherResult)); err != nil || len(impacts(t, s, f.groupID, unrelated.ID)) != 0 {
		t.Fatalf("unrelated exact version was held or did not complete: %v", err)
	}
	if saved, err := s.GetTaskSubmission(ctx, sub.GroupID, sub.TaskID, sub.ID); err != nil || !reflect.DeepEqual(saved, sub) {
		t.Fatalf("old Submission was mutated: %+v %v", saved, err)
	}
	if saved, err := s.GetTaskReview(ctx, sub.GroupID, sub.TaskID, review.ID); err != nil || !reflect.DeepEqual(saved, review) {
		t.Fatalf("old Review was mutated: %+v %v", saved, err)
	}
	if saved, err := s.GetArtifactVersion(ctx, sub.GroupID, v1.ArtifactID, v1.ID); err != nil || saved != v1 {
		t.Fatalf("old ArtifactVersion was mutated: %+v %v", saved, err)
	}
	if pins, err := s.ListTaskArtifactDependencies(ctx, f.groupID, waitingTask.ID, ListOptions{}); err != nil || !reflect.DeepEqual(pins.Items, pinInput.ArtifactDependencies) {
		t.Fatalf("old pin was mutated: %+v %v", pins, err)
	}
	dispatchAssertFK(t, s)
}
