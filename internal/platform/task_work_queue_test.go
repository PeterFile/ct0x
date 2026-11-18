package platform_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// B produces both kinds of wake through real admission, assignment and live
// claim APIs. The fixture installs no C Task Work profile and runs no provider.
func queueUnsupportedTaskWork(t *testing.T, kind domain.TaskWorkSourceKind) (*roleRuntimeFixture, domain.TaskWorkDelivery) {
	t.Helper()
	ctx := t.Context()
	f := newRoleRuntime(t)
	f.options.RoleMaxActive = 2 // B permits the delegate to overlap its running owner.
	child, err := f.local.CreateMember(ctx, f.group, platform.MemberInput{Name: "delegate", Role: "research", ProviderID: f.worker.ProviderID, Model: f.worker.Model}, "")
	roleOK(t, err)
	_, err = f.db.SetDispatchPolicy(ctx, f.group, []string{f.worker.ID, child.ID}, 1, "delegate-eligible")
	roleOK(t, err)
	engine := coordination.New(f.db)
	engine.EnableInvestigationExecution()
	engine.EnableTaskWorkExecution()
	thread := domain.ThreadRef{GroupID: f.group, TopicID: "queue-investigation"}
	_, err = engine.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
		Investigation: &domain.ChatInvestigationPolicy{Objective: "Check the open question", AllowTopicMessages: true, MaxDelegations: 1}}, 0, "investigation-policy")
	roleOK(t, err)
	claim := func(delivery string) store.MemberToolScope {
		t.Helper()
		turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: delivery, MaxActive: f.options.RoleMaxActive})
		roleOK(t, err)
		scope, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: f.group, TurnID: turn.ID, Token: turn.ClaimToken})
		roleOK(t, err)
		return scope
	}
	settle := func(c store.TurnClaim, receipt provider.ExecutionReceipt) {
		t.Helper()
		_, err := f.db.ApplyTurnReceipt(ctx, c, receipt)
		roleOK(t, err)
	}
	notStarted := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	source, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: thread.TopicID, Content: "Please investigate the open question.", RecipientIDs: []string{f.lead.ID}}, "investigation-source")
	roleOK(t, err)
	intake := claim(source.Deliveries[0].ID)
	candidate, err := engine.CaptureChatCandidate(ctx, intake)
	roleOK(t, err)
	admission, err := engine.AssessChatIntent(ctx, intake, candidate.ID, domain.ChatAssessment{Disposition: "prepare_work",
		Action: domain.ChatActionReadOnlyInvestigation, Reason: "Investigate within the configured scope"}, "Record the investigation.", "investigation-admit")
	roleOK(t, err)
	settle(intake.Claim, notStarted)
	dispatch, err := f.db.MaterializeDispatchRequest(ctx, admission.DispatchRequestID, 1, time.Now().UTC())
	roleOK(t, err)
	co := f.claim(t, dispatch.Delivery.DeliveryID)
	coScope, err := f.db.BindMemberTool(ctx, co.claim)
	roleOK(t, err)
	decision, err := f.db.DecideDispatchWithCapabilities(ctx, store.CoordinationToolScope{Member: coScope, Provenance: co.provenance},
		store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "Bound investigation"}, "assign-investigation", []store.DispatchWorkerCapabilityFact{{
			GroupID: f.group, TaskID: admission.TaskID, TaskRevision: co.provenance.TaskRevision, MemberID: f.worker.ID,
			ProviderID: f.worker.ProviderID, Model: f.worker.Model, RequiredProfile: store.DispatchInvestigationWorkerProfile,
		}})
	roleOK(t, err)
	settle(co.claim, notStarted)
	assigned, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	roleOK(t, err)
	queued, err := f.db.ResolveCoordinationDelivery(ctx, f.group, assigned.Delivery.DeliveryID)
	roleOK(t, err)
	owner := claim(assigned.Delivery.DeliveryID)
	_, err = f.db.BindAssignmentAttempt(ctx, store.CoordinationToolScope{Member: owner, Provenance: queued.Provenance})
	roleOK(t, err)
	const nativeID = "queue-fixture"
	accept := func(scope store.MemberToolScope) {
		t.Helper()
		roleOK(t, f.db.RecordTurnEvent(ctx, scope.Claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: nativeID}))
	}
	terminal := provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, Started: true, Accepted: true,
		NativeSessionID: nativeID, DirectChildReaped: true, OutputComplete: true}
	accept(owner)
	delegation, err := engine.DelegateWork(ctx, owner, domain.DelegateWorkInput{MemberID: child.ID, Question: "Check the first point."}, "delegate-work")
	roleOK(t, err)
	deliveryID, sourceID := delegation.DeliveryID, delegation.ID
	if kind == domain.TaskWorkSourceReport {
		delegate := claim(deliveryID)
		accept(delegate)
		report, err := engine.ReportWork(ctx, delegate, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "The point was checked."}, "report-work")
		roleOK(t, err)
		settle(delegate.Claim, terminal)
		deliveryID, sourceID = report.DeliveryID, report.ID
	}
	settle(owner.Claim, terminal)
	work, err := f.db.ResolveTaskWorkDelivery(ctx, f.group, deliveryID)
	roleOK(t, err)
	if work.SourceKind != kind || work.SourceID != sourceID || work.Blocked != "" {
		t.Fatalf("expected B's ready %s wake, got kind=%q blocked=%q", kind, work.SourceKind, work.Blocked)
	}
	return f, work
}

func queuePreparer(t *testing.T, f *roleRuntimeFixture) (*platform.Preparer, string) {
	t.Helper()
	private := filepath.Join(t.TempDir(), "private")
	roleOK(t, os.Mkdir(private, 0700))
	cli := filepath.Join(t.TempDir(), "metadata-only")
	roleOK(t, os.WriteFile(cli, nil, 0700))
	p, err := platform.NewPreparer(f.db, platform.PrepareOptions{PrivateWorkspaceRoot: private, Providers: []provider.Provider{{
		ID: f.lead.ProviderID, Name: provider.Pi, Path: cli, Installation: provider.Installed, Version: "1.4.0",
	}}})
	roleOK(t, err)
	return p, private
}

func TestUnsupportedV30TaskWorkDoesNotStopScheduler(t *testing.T) {
	for _, kind := range []domain.TaskWorkSourceKind{domain.TaskWorkSourceDelegation, domain.TaskWorkSourceReport} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := t.Context()
			f, work := queueUnsupportedTaskWork(t, kind)
			p, private := queuePreparer(t, f)
			msg, err := f.db.GetMessage(ctx, f.group, work.MessageID)
			roleOK(t, err)
			wake := store.PendingDelivery{GroupID: f.group, SenderID: msg.SenderID, Delivery: msg.Deliveries[0]}
			prepared, err := p.Prepare(ctx, wake)
			var blocked *platform.RolePreparationBlocked
			if !reflect.ValueOf(prepared).IsZero() || !errors.Is(err, scheduler.ErrNotReady) || !errors.As(err, &blocked) || blocked.Code != "typed_profile_unavailable" {
				t.Errorf("valid %s should wait without a prepared execution, got %T, %v (Store denied=%v)", kind, prepared.Executor, err, errors.Is(err, store.ErrMemberToolDenied))
			}
			if inspect, err := p.InspectRoleReadiness(ctx, wake); err != nil || inspect.Blocked == nil || inspect.Blocked.Code != "typed_profile_unavailable" || inspect.HistoryLimit != 0 {
				t.Errorf("valid %s should inspect as unavailable, not as ordinary work: %v", kind, err)
			}
			entries, err := os.ReadDir(private)
			roleOK(t, err)
			if len(entries) != 0 {
				t.Fatal("unsupported work provisioned a workspace")
			}

			// Identical text, but no typed marker. It follows the wake in the same
			// work lane and must be scheduled normally for a different member.
			ordinary, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.group, TopicID: "unrelated", ActorID: msg.SenderID, Content: msg.Content}, []string{f.lead.ID})
			roleOK(t, err)
			ordinaryID := ordinary.Deliveries[0].ID
			page, err := f.db.ListPendingDeliveries(ctx, store.PendingOptions{Lane: "work"})
			roleOK(t, err)
			if len(page.Items) != 2 || page.Items[0].Delivery.ID != work.DeliveryID || page.Items[1].Delivery.ID != ordinaryID {
				t.Fatal("fixture did not queue the two deliveries in the same FIFO scan")
			}
			runCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			defer stop()
			err = scheduler.Run(runCtx, f.db, scheduler.Options{Recovered: true, RuntimeDir: f.root, MaxActive: 1},
				func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
					got, err := p.Prepare(ctx, pending)
					if pending.Delivery.ID == ordinaryID && err == nil && got.Executor != nil {
						got.Executor = roleNative(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
							stop()
							return roleNotStarted()
						})
						return got, nil
					}
					// Never launch the real executor, even on an unexpected prepare.
					return scheduler.PreparedExecution{}, err
				})
			if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
				t.Errorf("scheduler stopped before independent work could run: %v", err)
			}
			msg, err = f.db.GetMessage(ctx, f.group, work.MessageID)
			roleOK(t, err)
			if d := msg.Deliveries[0]; d.State != domain.DeliveryStateQueued || d.TurnID != "" {
				t.Fatal("unsupported wake was claimed or moved out of the queue")
			}
			ordinary, err = f.db.GetMessage(ctx, f.group, ordinary.ID)
			roleOK(t, err)
			if ordinary.Deliveries[0].TurnID == "" {
				t.Fatal("independent work was never claimed")
			}
			turn, err := f.db.GetTurn(ctx, f.group, ordinary.Deliveries[0].TurnID)
			roleOK(t, err)
			if turn.State != domain.TurnStateNotStarted {
				t.Fatalf("independent synthetic executor receipt was not settled: %s", turn.State)
			}
		})
	}
}

func TestUnsupportedV30TaskWorkRequiresPersistedSource(t *testing.T) {
	ctx := t.Context()
	f, work := queueUnsupportedTaskWork(t, domain.TaskWorkSourceDelegation)
	// Keep the physical wake and typed label, but orphan the marker's source.
	// Only a private fixture DB can bypass the immutable marker trigger.
	_, err := f.raw.Exec(`DROP TRIGGER task_work_deliveries_no_update`)
	roleOK(t, err)
	_, err = f.raw.Exec(`UPDATE task_work_deliveries SET source_id='missing-source' WHERE delivery_id=?`, work.DeliveryID)
	roleOK(t, err)
	p, private := queuePreparer(t, f)
	msg, err := f.db.GetMessage(ctx, f.group, work.MessageID)
	roleOK(t, err)
	wake := store.PendingDelivery{GroupID: f.group, SenderID: msg.SenderID, Delivery: msg.Deliveries[0]}
	if got, err := p.Prepare(ctx, wake); !reflect.ValueOf(got).IsZero() || !errors.Is(err, store.ErrMemberToolDenied) || errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("damaged typed marker became ordinary or unavailable work: %T, %v", got.Executor, err)
	}
	entries, err := os.ReadDir(private)
	roleOK(t, err)
	if len(entries) != 0 {
		t.Fatal("damaged typed marker provisioned a workspace")
	}
}
