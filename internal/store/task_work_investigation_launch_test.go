package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

const investigationLaunchResource = "private-cwd:investigation-fixture"

type investigationLaunchFixture struct {
	chatFixture
	owner    CoordinationToolScope
	child    domain.Member
	artifact domain.ArtifactVersionRef
	policy   domain.ChatPolicy
}

func investigationLaunchMember(t *testing.T, f chatFixture, name, workspace string) domain.Member {
	t.Helper()
	m, err := f.s.CreateMember(ctx, CreateMemberInput{GroupID: f.groupID, Name: name, Role: "research", ActorID: "user",
		ProviderID: "pi", Model: "fixture-model", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newInvestigationLaunchFixture(t *testing.T, childWorkspace string) investigationLaunchFixture {
	t.Helper()
	f := chatSetup(t, "user", false)
	f.b = investigationLaunchMember(t, f, "调查负责人", "").ID
	child := investigationLaunchMember(t, f, "调查协作者", childWorkspace)
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b, child.ID}, 1, "launch-workers"); err != nil {
		t.Fatal(err)
	}
	v := mustPublishArtifact(t, f.s, artifactPublishInput(f.groupID))
	ref := domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ID}
	f, owner, policy := workReportInvestigationAssignment(t, f, domain.ChatInvestigationPolicy{
		Objective: investigationObjective, MaxDelegations: 1, AllowTopicMessages: true, ArtifactVersions: []domain.ArtifactVersionRef{ref},
	})
	co := taskWorkTestCoordinator(t, f.s, f.groupID, owner.Provenance.AssignmentID)
	taskWorkTestReceipt(t, f.s, co, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	queueEvent(t, f.s, owner.Member.Claim, provider.AcceptedEvent, "", "")
	return investigationLaunchFixture{f, owner, child, ref, policy}
}

func investigationLaunchPlan(t *testing.T, s *Store, d domain.TaskWorkDelivery, request string, keys ...string) InvestigationTaskWorkLaunch {
	t.Helper()
	scope := taskWorkTestClaim(t, s, d, keys...)
	if got, err := s.GetTaskWorkExecution(ctx, scope); err != nil || d.Blocked != "" || got != d {
		t.Fatalf("postclaim delivery isn't the exact ready wake: %+v, %v; want %+v", got, err, d)
	}
	m, err := s.GetMember(ctx, d.GroupID, d.MemberID)
	if err != nil {
		t.Fatal(err)
	}
	return InvestigationTaskWorkLaunch{Scope: scope, Delivery: d, RequestID: request, ProviderID: m.ProviderID, Model: m.Model,
		ResourceKey: investigationLaunchResource}
}

func investigationLaunchFollowup(t *testing.T, f chatFixture, owner CoordinationToolScope) InvestigationTaskWorkLaunch {
	t.Helper()
	_, wake := taskWorkTestFollowup(t, f, "继续原任务并注明具体出处。")
	taskWorkTestReceipt(t, f.s, owner.Member, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
	return investigationLaunchPlan(t, f.s, wake, owner.Provenance.RequestID, investigationLaunchResource)
}

func (f investigationLaunchFixture) delegation(t *testing.T, keys ...string) (InvestigationTaskWorkLaunch, domain.TaskWorkDelegation) {
	t.Helper()
	delegation, err := f.s.DelegateWork(ctx, f.owner.Member, domain.DelegateWorkInput{MemberID: f.child.ID,
		Question: "只核对批准版本。", ArtifactVersions: []domain.ArtifactVersionRef{f.artifact}}, "delegate-for-launch")
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.s.ResolveTaskWorkDelivery(ctx, f.groupID, delegation.DeliveryID)
	if err != nil || d.SourceKind != domain.TaskWorkSourceDelegation || d.SourceID != delegation.ID || d.Blocked != "" {
		t.Fatalf("delegation wake not ready beside its exact running owner: %+v, %v", d, err)
	}
	return investigationLaunchPlan(t, f.s, d, f.owner.Provenance.RequestID, keys...), delegation
}

func bindInvestigationLaunchOwner(t *testing.T, s *Store, launch InvestigationTaskWorkLaunch) InvestigationTaskWorkLaunch {
	t.Helper()
	a, err := s.BindTaskWorkContinuation(ctx, launch.Scope)
	if err != nil || a.ID == "" || a.TurnID != launch.Scope.Claim.TurnID {
		t.Fatalf("no exact continuation Attempt: %+v, %v", a, err)
	}
	launch.AttemptID = a.ID
	return launch
}

func denyInvestigationLaunch(t *testing.T, s *Store, launch InvestigationTaskWorkLaunch) {
	t.Helper()
	calls := 0
	err := s.CommitInvestigationTaskWorkLaunch(ctx, launch, func() bool { calls++; return true })
	if !errors.Is(err, ErrMemberToolDenied) || calls != 0 {
		t.Fatalf("want denial without admission; got %v, callback calls=%d", err, calls)
	}
}

func investigationLaunchResourceHeld(t *testing.T, s *Store, launch InvestigationTaskWorkLaunch) {
	t.Helper()
	var owner string
	if err := s.db.QueryRow(`SELECT turn_id FROM resource_claims WHERE resource_key=?`, launch.ResourceKey).Scan(&owner); err != nil || owner != launch.Scope.Claim.TurnID {
		t.Fatalf("launch changed the resource owner to %q: %v", owner, err)
	}
}

func admitInvestigationLaunch(t *testing.T, s *Store, launch InvestigationTaskWorkLaunch) {
	t.Helper()
	turn := queueTurn(t, s, launch.Scope.Claim)
	wakes := workReportWakeCounts(t, s)
	calls := 0
	err := s.CommitInvestigationTaskWorkLaunch(ctx, launch, func() bool { calls++; return calls == 1 })
	if err != nil || calls != 1 {
		t.Fatalf("healthy %s start: %v, callback calls=%d; want success and one admission", launch.Delivery.SourceKind, err, calls)
	}
	if got := queueTurn(t, s, launch.Scope.Claim); !reflect.DeepEqual(got, turn) || queueReceiptJSON(t, s, launch.Scope.Claim) != "" {
		t.Fatal("admission wrote execution or settlement evidence")
	}
	queueDeliveryState(t, s, turn, domain.DeliveryStateQueued)
	investigationLaunchResourceHeld(t, s, launch)
	if got := workReportWakeCounts(t, s); got != wakes {
		t.Fatalf("admission published work: %v, want %v", got, wakes)
	}
}

func TestInvestigationTaskWorkLaunchReadyRoles(t *testing.T) {
	t.Run("followup", func(t *testing.T) {
		f := newInvestigationLaunchFixture(t, "")
		launch := investigationLaunchFollowup(t, f.chatFixture, f.owner)
		// An active Attempt not bound to this Turn isn't launch authority.
		launch.AttemptID = f.owner.AttemptID
		denyInvestigationLaunch(t, f.s, launch)
		launch = bindInvestigationLaunchOwner(t, f.s, launch)
		admitInvestigationLaunch(t, f.s, launch)
	})
	t.Run("report", func(t *testing.T) {
		f := newInvestigationLaunchFixture(t, "")
		child, delegation := f.delegation(t, "child-resource")
		queueEvent(t, f.s, child.Scope.Claim, provider.AcceptedEvent, "", "")
		progress := recordWorkReportProgress(t, f.s, child.Scope, delegation, domain.ReportWorkInput{
			Kind: domain.TaskWorkReportProgress, Summary: "已核对一部分。",
		}, "progress")
		report, err := f.s.ReportWork(ctx, child.Scope, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "已完成核对。"}, "report")
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range []MemberToolScope{child.Scope, f.owner.Member} {
			taskWorkTestReceipt(t, f.s, scope, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
		}
		d, err := f.s.ResolveTaskWorkDelivery(ctx, f.groupID, report.DeliveryID)
		if err != nil || d.SourceKind != domain.TaskWorkSourceReport || d.SourceID != report.ID {
			t.Fatalf("wrong report wake: %+v, %v", d, err)
		}
		launch := investigationLaunchPlan(t, f.s, d, f.owner.Provenance.RequestID, investigationLaunchResource)
		launch = bindInvestigationLaunchOwner(t, f.s, launch)
		wrong := launch
		wrong.Delivery.SourceID = progress.ID // Real same-Task report, but not this wake's report.
		denyInvestigationLaunch(t, f.s, wrong)
		admitInvestigationLaunch(t, f.s, launch)
	})
	t.Run("delegation-beside-running-author", func(t *testing.T) {
		f := newInvestigationLaunchFixture(t, "")
		launch, _ := f.delegation(t, investigationLaunchResource)
		wrong := launch
		wrong.AttemptID = f.owner.AttemptID
		denyInvestigationLaunch(t, f.s, wrong)
		admitInvestigationLaunch(t, f.s, launch)
	})
}

func TestInvestigationTaskWorkLaunchMatchesFrozenPlan(t *testing.T) {
	f := newInvestigationLaunchFixture(t, "")
	launch := bindInvestigationLaunchOwner(t, f.s, investigationLaunchFollowup(t, f.chatFixture, f.owner))
	for name, change := range map[string]func(*InvestigationTaskWorkLaunch){
		"claim-token":   func(l *InvestigationTaskWorkLaunch) { l.Scope.Claim.Token = f.owner.Member.Claim.Token },
		"other-group":   func(l *InvestigationTaskWorkLaunch) { l.Scope.Claim.GroupID = f.otherID },
		"task-revision": func(l *InvestigationTaskWorkLaunch) { l.Delivery.TaskRevision++ },
		"blocked-plan":  func(l *InvestigationTaskWorkLaunch) { l.Delivery.Blocked = "already_claimed" },
		"request":       func(l *InvestigationTaskWorkLaunch) { l.RequestID += "-other" },
		"prior-attempt": func(l *InvestigationTaskWorkLaunch) { l.AttemptID = f.owner.AttemptID },
		"no-attempt":    func(l *InvestigationTaskWorkLaunch) { l.AttemptID = "" },
		"provider":      func(l *InvestigationTaskWorkLaunch) { l.ProviderID += "-other" },
		"model":         func(l *InvestigationTaskWorkLaunch) { l.Model += "-other" },
		"resource":      func(l *InvestigationTaskWorkLaunch) { l.ResourceKey += "-other" },
	} {
		t.Run(name, func(t *testing.T) {
			wrong := launch
			change(&wrong)
			denyInvestigationLaunch(t, f.s, wrong)
		})
	}
}

func TestInvestigationTaskWorkLaunchRejectsNonExactResourceSetOrWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name, workspace string
		keys            []string
	}{
		{name: "no-resource"},
		{name: "extra-resource", keys: []string{investigationLaunchResource, "second-resource"}},
		{name: "configured-workspace", workspace: "/configured-workspace", keys: []string{investigationLaunchResource}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInvestigationLaunchFixture(t, tc.workspace)
			launch, _ := f.delegation(t, tc.keys...)
			denyInvestigationLaunch(t, f.s, launch)
		})
	}
}

func TestInvestigationTaskWorkLaunchRechecksFollowupAuthority(t *testing.T) {
	for _, reason := range []string{"handled-input", "revoked-policy", "native-session", "cancelled-turn"} {
		t.Run(reason, func(t *testing.T) {
			f := newInvestigationLaunchFixture(t, "")
			launch := bindInvestigationLaunchOwner(t, f.s, investigationLaunchFollowup(t, f.chatFixture, f.owner))
			s := queueHandle(t, f.path, true)
			s.EnableTaskWorkExecution()
			s.EnableInvestigationExecution()
			switch reason {
			case "handled-input":
				if ack, err := f.s.AcknowledgeWorkInputWithKey(ctx, launch.Scope, launch.Delivery.SourceID, "handled-before-launch"); err != nil || ack.Consumption == nil {
					t.Fatalf("expected real input consumption: %+v, %v", ack, err)
				}
			case "revoked-policy":
				investigationDisablePolicy(t, f.s, f.policy)
			case "native-session":
				queueEvent(t, f.s, launch.Scope.Claim, provider.SessionEvent, "", "")
			case "cancelled-turn":
				if _, err := f.s.RequestTurnCancel(ctx, f.groupID, launch.Scope.Claim.TurnID); err != nil {
					t.Fatal(err)
				}
			}
			denyInvestigationLaunch(t, s, launch)
			investigationLaunchResourceHeld(t, s, launch)
		})
	}
}

func TestInvestigationTaskWorkLaunchDoesNotWidenTextContinuation(t *testing.T) {
	f := chatSetup(t, "user", false)
	f.b = investigationLaunchMember(t, f, "文本负责人", "").ID
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 1, "text-launch-worker"); err != nil {
		t.Fatal(err)
	}
	f.s.EnableTaskWorkExecution()
	f.s.EnableInvestigationExecution()
	chatNextCandidate(t, &f, true)
	owner := chatAuthorityWorker(t, f)
	co := taskWorkTestCoordinator(t, f.s, f.groupID, owner.Provenance.AssignmentID)
	taskWorkTestReceipt(t, f.s, co, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	queueEvent(t, f.s, owner.Member.Claim, provider.AcceptedEvent, "", "")
	launch := bindInvestigationLaunchOwner(t, f.s, investigationLaunchFollowup(t, f, owner))
	denyInvestigationLaunch(t, f.s, launch)
}

func TestInvestigationTaskWorkLaunchRechecksRelatedOwnerSettlement(t *testing.T) {
	f := newInvestigationLaunchFixture(t, "")
	launch, _ := f.delegation(t, investigationLaunchResource)
	owner := f.owner.Member.Claim
	t.Run("cancel-requested", func(t *testing.T) {
		if _, err := f.s.RequestTurnCancel(ctx, owner.GroupID, owner.TurnID); err != nil {
			t.Fatal(err)
		}
		denyInvestigationLaunch(t, f.s, launch)
	})
	t.Run("unknown", func(t *testing.T) {
		taskWorkTestReceipt(t, f.s, f.owner.Member, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}, domain.TurnStateUnknown)
		denyInvestigationLaunch(t, f.s, launch)
	})
	investigationLaunchResourceHeld(t, f.s, launch)
	investigationLaunchResourceHeld(t, f.s, InvestigationTaskWorkLaunch{Scope: f.owner.Member, ResourceKey: "worker-resource"})
}

func TestInvestigationTaskWorkLaunchAdmissionAndCommit(t *testing.T) {
	f := newInvestigationLaunchFixture(t, "")
	launch, _ := f.delegation(t, investigationLaunchResource)
	if err := f.s.CommitInvestigationTaskWorkLaunch(ctx, launch, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil callback: %v", err)
	}
	t.Run("cancelled-before-admission", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		calls := 0
		err := f.s.CommitInvestigationTaskWorkLaunch(cancelled, launch, func() bool { calls++; return true })
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("pre-cancelled: %v, callback calls=%d", err, calls)
		}
	})
	t.Run("admit-refuses", func(t *testing.T) {
		calls := 0
		err := f.s.CommitInvestigationTaskWorkLaunch(ctx, launch, func() bool { calls++; return false })
		if !errors.Is(err, ErrMemberToolDenied) || calls != 1 {
			t.Fatalf("refusal: %v, callback calls=%d", err, calls)
		}
	})
	t.Run("context-lost-at-admission", func(t *testing.T) {
		bounded, cancel := context.WithCancel(ctx)
		defer cancel()
		calls := 0
		err := f.s.CommitInvestigationTaskWorkLaunch(bounded, launch, func() bool { calls++; cancel(); return true })
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("context loss: %v, callback calls=%d", err, calls)
		}
	})
	t.Run("commit-failure", func(t *testing.T) {
		// Fail the transaction's commit, not any of its authorization reads.
		coordinationDecideExec(t, f.s, `CREATE TEMP TABLE launch_parent(id INTEGER PRIMARY KEY);
		 CREATE TEMP TABLE launch_fault(id INTEGER REFERENCES launch_parent(id) DEFERRABLE INITIALLY DEFERRED);
		 CREATE TEMP TRIGGER launch_commit_fault AFTER UPDATE OF claim_token ON main.turns
		 BEGIN INSERT INTO launch_fault VALUES(1); END`)
		calls := 0
		err := f.s.CommitInvestigationTaskWorkLaunch(ctx, launch, func() bool { calls++; return true })
		if err == nil || calls != 1 {
			t.Fatalf("commit failure: %v, callback calls=%d", err, calls)
		}
		var count int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM launch_fault`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed commit left effects: %d, %v", count, err)
		}
	})
	investigationLaunchResourceHeld(t, f.s, launch)
}
