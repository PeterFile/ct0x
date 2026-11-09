package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type rolePrepareFixture struct {
	*prepareFixture
	worker, coordinator domain.Member
	ordinary            store.PendingDelivery
	request             domain.DispatchRequest
	state               store.CoordinationRoleState
	runtimeOptions      MemberToolRuntimeOptions
}

func prepareRoleOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// All role relations come from real admission/materialization receipts. The
// opaque provider ID is deliberately not the CLI's name. No executor is run.
func newRolePrepareFixture(t *testing.T, objective ...string) *rolePrepareFixture {
	t.Helper()
	f := &rolePrepareFixture{prepareFixture: newPrepareFixture(t)}
	f.coordinator, f.ordinary = f.member, f.pending
	title := "研究目标"
	if len(objective) != 0 {
		title = objective[0]
	}
	ctx := context.Background()
	var err error
	f.worker, err = f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: f.group.ID, Name: "worker",
		Role: "Untrusted role description", ProviderID: f.member.ProviderID, Model: f.member.Model})
	prepareRoleOK(t, err)
	_, err = f.s.SetCoordinator(ctx, f.group.ID, f.member.ID, 0, "binding")
	prepareRoleOK(t, err)
	_, err = f.s.SetDispatchPolicy(ctx, f.group.ID, []string{f.worker.ID}, 0, "policy")
	prepareRoleOK(t, err)
	admission, err := f.s.RequestWork(ctx, f.group.ID, domain.RequestWorkRequest{TopicID: f.trigger.TopicID,
		MessageID: f.trigger.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "explicit local request",
		NewTask: &domain.NewTaskRequest{Title: title, Acceptance: "有依据的文本结论"}}, "admission")
	prepareRoleOK(t, err)
	coordination, err := f.s.GetTaskCoordination(ctx, f.group.ID, admission.Task.ID)
	prepareRoleOK(t, err)
	f.request, err = f.s.RequestDispatch(ctx, f.group.ID, admission.Task.ID, admission.Task.RevisionAtAdmission,
		coordination.Coordination.AuthorityGeneration, "dispatch", "dispatch")
	prepareRoleOK(t, err)
	materialized, err := f.s.MaterializeDispatchRequest(ctx, f.request.ID, 1, time.Now().UTC())
	prepareRoleOK(t, err)
	if materialized.Delivery == nil {
		t.Fatal("coordinator fixture did not materialize")
	}
	f.selectDelivery(t, materialized.Delivery.MessageID)
	return f
}

func (f *rolePrepareFixture) selectDelivery(t *testing.T, messageID string) {
	t.Helper()
	ctx := context.Background()
	var err error
	f.trigger, err = f.s.GetMessage(ctx, f.group.ID, messageID)
	prepareRoleOK(t, err)
	if len(f.trigger.Deliveries) != 1 {
		t.Fatal("role fixture requires one delivery")
	}
	f.pending = store.PendingDelivery{GroupID: f.group.ID, SenderID: f.trigger.SenderID, Delivery: f.trigger.Deliveries[0]}
	f.member, err = f.s.GetMember(ctx, f.group.ID, f.pending.Delivery.RecipientID)
	prepareRoleOK(t, err)
	f.state, err = f.s.ResolveCoordinationDelivery(ctx, f.group.ID, f.pending.Delivery.ID)
	prepareRoleOK(t, err)
	if f.state.Blocked != store.CoordinationReady || f.state.Provenance.Kind == store.CoordinationRoleOrdinary {
		t.Fatal("real role fixture is not ready")
	}
}

func TestPrepareRolesRequireExplicitRuntime(t *testing.T) {
	f := newRolePrepareFixture(t)
	before := f.snapshot(t)
	got, err := f.preparer(t).Prepare(context.Background(), f.pending)
	assertRoleBlocked(t, got, err, f.state, "role_disabled")
	configurePrepareMemberTools(t, f.prepareFixture)
	selectPrepareMemberToolsPi(f.prepareFixture)
	got, err = f.preparer(t).Prepare(context.Background(), f.pending)
	assertRoleBlocked(t, got, err, f.state, "role_disabled")
	f.assertUnchanged(t, before, false)
}

func (f *rolePrepareFixture) enableRuntime(t *testing.T) {
	t.Helper()
	selectPrepareMemberToolsPi(f.prepareFixture)
	f.runtimeOptions = configurePrepareMemberTools(t, f.prepareFixture)
	f.runtimeOptions.Operations = slices.Clone(historyProfileNames)
	f.runtimeOptions.RoleMaxActive = 1
	f.runtimeOptions.RoleExecutionRoot = filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "runs")
	prepareRoleOK(t, os.Mkdir(f.runtimeOptions.RoleExecutionRoot, 0700))
	var err error
	f.opts.MemberTools, err = NewMemberToolRuntime(f.s, f.runtimeOptions)
	prepareRoleOK(t, err)
}

func (f *rolePrepareFixture) selectWorker(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID,
		MaxActive: 1, ResourceKeys: []string{"fixture-decision"}})
	prepareRoleOK(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	member, err := f.s.BindMemberTool(ctx, claim)
	prepareRoleOK(t, err)
	decision, err := f.s.DecideDispatch(ctx, store.CoordinationToolScope{Member: member, Provenance: f.state.Provenance},
		store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "eligible worker"}, "assign")
	prepareRoleOK(t, err)
	if decision.Assignment == nil {
		t.Fatal("real assign decision is missing")
	}
	// Fixture-only trusted Store setup: no model/native was invoked. Settle the
	// unused coordinator claim before inspecting the queued worker preparation.
	_, err = f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	prepareRoleOK(t, err)
	materialized, err := f.s.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	prepareRoleOK(t, err)
	if materialized.Delivery == nil {
		t.Fatal("worker fixture did not materialize")
	}
	f.selectDelivery(t, materialized.Delivery.MessageID)
}

func (f *rolePrepareFixture) snapshot(t *testing.T) map[string][][]any {
	t.Helper()
	return memberRoleSnapshot(t, localMessageFixture{db: f.s, raw: f.db})
}

func (f *rolePrepareFixture) assertUnchanged(t *testing.T, before map[string][][]any, workspace bool) {
	t.Helper()
	if !reflect.DeepEqual(before, f.snapshot(t)) {
		t.Fatal("Prepare changed durable messages, deliveries, claims, attempts or role receipts")
	}
	trigger, err := f.s.GetMessage(context.Background(), f.group.ID, f.trigger.ID)
	prepareRoleOK(t, err)
	if !reflect.DeepEqual(trigger, f.trigger) || trigger.Deliveries[0].State != domain.DeliveryStateQueued || trigger.Deliveries[0].TurnID != "" {
		t.Fatal("Prepare modified the complete queued role trigger")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "invoked")); !os.IsNotExist(err) {
		t.Fatalf("sentinel CLI was invoked: %v", err)
	}
	if r := f.opts.MemberTools; r != nil {
		r.tools.mu.RLock()
		count := len(r.tools.capabilities)
		r.tools.mu.RUnlock()
		if count != 0 {
			t.Fatal("Prepare issued a capability")
		}
		memberToolRuntimeEmpty(t, r.configRoot)
		if r.roleFence != nil {
			memberToolRuntimeEmpty(t, r.roleFence.executionRoot)
		}
	}
	if !workspace {
		memberToolRuntimeEmpty(t, f.opts.PrivateWorkspaceRoot)
	}
}

func assertRoleBlocked(t *testing.T, got scheduler.PreparedExecution, err error, state store.CoordinationRoleState, code string) *RolePreparationBlocked {
	t.Helper()
	var blocked *RolePreparationBlocked
	if !errors.Is(err, scheduler.ErrNotReady) || !errors.As(err, &blocked) || !reflect.ValueOf(got).IsZero() {
		t.Fatalf("expected typed %s with empty execution, got executor=%T, error=%v", code, got.Executor, err)
	}
	p := state.Provenance
	if blocked.Code != code || blocked.Role != p.Kind || blocked.DeliveryID != p.DeliveryID ||
		blocked.RequestID != p.RequestID || blocked.AssignmentID != p.AssignmentID {
		t.Fatalf("blocked diagnostic lost role provenance: %+v", blocked)
	}
	if code != "context_budget" {
		data, err := json.Marshal(blocked)
		prepareRoleOK(t, err)
		if blocked.Required != 0 || blocked.Limit != 0 || strings.Contains(string(data), "required") || strings.Contains(string(data), "limit") {
			t.Fatal("non-budget readiness fabricated byte metrics")
		}
	}
	return blocked
}

func TestPrepareRoleOptions(t *testing.T) {
	f := newPrepareFixture(t)
	p := f.preparer(t)
	if p.historyLimit != 50 || p.maxContextBytes != 16<<10 || p.roleHistoryLimit != 50 || p.roleMaxContextBytes != 256<<10 {
		t.Fatal("ordinary and role defaults were coupled or changed")
	}
	for _, pair := range [][2]int{{1, 1}, {200, 1 << 20}, {7, 8192}} {
		opts := f.opts
		opts.HistoryLimit, opts.MaxContextBytes = 3, 4096
		opts.RoleHistoryLimit, opts.RoleMaxContextBytes = pair[0], pair[1]
		p, err := NewPreparer(f.s, opts)
		prepareRoleOK(t, err)
		opts.RoleHistoryLimit, opts.RoleMaxContextBytes = 0, 0
		if p.historyLimit != 3 || p.maxContextBytes != 4096 || p.roleHistoryLimit != pair[0] || p.roleMaxContextBytes != pair[1] {
			t.Fatal("preparer did not retain independent immutable profile options")
		}
	}
	for _, pair := range [][2]int{{-1, 0}, {201, 0}, {0, -1}, {0, 1<<20 + 1}} {
		opts := f.opts
		opts.RoleHistoryLimit, opts.RoleMaxContextBytes = pair[0], pair[1]
		if p, err := NewPreparer(f.s, opts); p != nil || !errors.Is(err, errPrepareConfig) {
			t.Fatalf("invalid role options accepted: %v", err)
		}
	}
}

func TestPrepareRolesAuthoritativeWrapper(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	p := f.preparer(t)
	before := f.snapshot(t)
	first, err := p.Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	ordinary, err := p.Prepare(context.Background(), f.ordinary)
	prepareRoleOK(t, err)
	again, err := p.Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	run, ok := first.Executor.(*memberToolRuntimeRun)
	if !ok || run.role == nil || *run.role != f.state.Provenance || run.runtime != f.opts.MemberTools || run.used.Load() || run.factory == nil ||
		run.binding.ProviderName != provider.Pi || run.binding.ProviderID != f.member.ProviderID || run.binding.ProviderID == provider.Pi {
		t.Fatal("Prepare did not return the real wrapper with exact opaque provider and role provenance")
	}
	if first.Request != again.Request || first.Request.Model != f.member.Model || first.Request.RunID != "" || first.Request.RuntimeDir != "" || first.Request.Thinking != "" ||
		first.Request.CWD != ordinary.Request.CWD || !slices.Equal(first.ResourceKeys, ordinary.ResourceKeys) {
		t.Fatal("role preparation changed request binding or the existing workspace identity algorithm")
	}
	keys, err := scheduler.WorkspaceResourceKeys([]string{first.Request.CWD})
	prepareRoleOK(t, err)
	if !slices.Equal(first.ResourceKeys, keys) || filepath.Dir(first.Request.CWD) != f.opts.PrivateWorkspaceRoot {
		t.Fatal("role ResourceKeys do not match the actual separately locked private scope")
	}
	f.assertUnchanged(t, before, true)
	if len(f.opts.MemberTools.roleFence.records) != 0 || len(f.opts.MemberTools.roleFence.slots) != 0 {
		t.Fatal("capacity observation reserved a role execution")
	}
}

func TestPrepareRolesReadinessAndSystemErrors(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	ctx := context.Background()
	before := f.snapshot(t)
	selected := f.opts.Providers[1]
	for _, tc := range []struct {
		name string
		edit func(*provider.Provider)
	}{
		{"non Pi", func(p *provider.Provider) { p.Name, p.Version = provider.Codex, "0.162.1" }},
		{"unsupported version", func(p *provider.Provider) { p.Version = "1.4.1" }},
		{"uninstalled", func(p *provider.Provider) { p.Installation = "" }},
		{"relative path", func(p *provider.Provider) { p.Path = "private-relative" }},
		{"missing executable", func(p *provider.Provider) { p.Path += ".missing" }},
		{"non executable", func(p *provider.Provider) { p.Path = f.opts.PiMemberToolsExtension }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := f.opts
			value := selected
			tc.edit(&value)
			opts.Providers = []provider.Provider{value}
			p, err := NewPreparer(f.s, opts)
			prepareRoleOK(t, err)
			got, err := p.Prepare(ctx, f.pending)
			assertRoleBlocked(t, got, err, f.state, "unsupported_provider")
		})
	}
	opts := f.opts
	opts.Providers = nil
	p, err := NewPreparer(f.s, opts)
	prepareRoleOK(t, err)
	got, err := p.Prepare(ctx, f.pending)
	assertRoleBlocked(t, got, err, f.state, "unsupported_provider")
	loop := filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "PRIVATE-loop")
	prepareRoleOK(t, os.Symlink(loop, loop))
	opts.Providers = []provider.Provider{selected}
	opts.Providers[0].Path = loop
	p, err = NewPreparer(f.s, opts)
	prepareRoleOK(t, err)
	got, err = p.Prepare(ctx, f.pending)
	if !reflect.ValueOf(got).IsZero() || errors.Is(err, scheduler.ErrNotReady) || !errors.Is(err, syscall.ELOOP) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("unexpected provider stat failure was hidden as readiness or leaked: %v", err)
	}
	p = f.preparer(t)
	f.exec(t, "UPDATE members SET model=? WHERE id=?", "PRIVATE\ninvalid-model", f.member.ID)
	got, err = p.Prepare(ctx, f.pending)
	assertRoleBlocked(t, got, err, f.state, "unsupported_provider")
	f.exec(t, "UPDATE members SET model=? WHERE id=?", f.member.Model, f.member.ID)
	prepareRoleOK(t, os.Chmod(f.runtimeOptions.RoleExecutionRoot, 0755))
	got, err = p.Prepare(ctx, f.pending)
	if !reflect.ValueOf(got).IsZero() || errors.Is(err, scheduler.ErrNotReady) || !errors.Is(err, errMemberToolRoleFenceConfig) {
		t.Fatalf("fence root failure was not preserved as a real error: %v", err)
	}
	prepareRoleOK(t, os.Chmod(f.runtimeOptions.RoleExecutionRoot, 0700))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	got, err = p.Prepare(canceled, f.pending)
	if !reflect.ValueOf(got).IsZero() || !errors.Is(err, context.Canceled) || errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("cancellation became a readiness result: %v", err)
	}
	f.assertUnchanged(t, before, false)
	// A real current-readiness change remains role-shaped even with a catalog
	// that would otherwise permit an ordinary Codex executor.
	_, err = f.s.SetDispatchPolicy(ctx, f.group.ID, []string{f.worker.ID}, 1, "new-policy")
	prepareRoleOK(t, err)
	before = f.snapshot(t)
	got, err = p.Prepare(ctx, f.pending)
	assertRoleBlocked(t, got, err, f.state, string(store.CoordinationPolicyStale))
	f.assertUnchanged(t, before, false)
	prepareRoleOK(t, f.s.Close())
	got, err = p.Prepare(ctx, f.pending)
	if err == nil || !reflect.ValueOf(got).IsZero() || errors.Is(err, scheduler.ErrNotReady) || strings.Contains(err.Error(), "sql") {
		t.Fatalf("closed Store became readiness or leaked SQL: %v", err)
	}
}

func TestPrepareRolesCapacityObservation(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	p := f.preparer(t)
	ctx := context.Background()
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.ordinary.Delivery.ID,
		MaxActive: 1, ResourceKeys: []string{"fixture-capacity"}})
	prepareRoleOK(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	fence := f.opts.MemberTools.roleFence
	owner, err := fence.reserve(ctx, claim, f.runtimeOptions.RoleExecutionRoot)
	prepareRoleOK(t, err)
	before := f.snapshot(t)
	got, err := p.Prepare(ctx, f.pending)
	assertRoleBlocked(t, got, err, f.state, "role_runtime_capacity")
	f.assertUnchanged(t, before, false)
	if len(fence.records) != 1 || fence.records[turn.ID] != owner || owner.localDone {
		t.Fatal("Prepare changed or released the existing execution owner")
	}
	owner.markLocalDone()
	// Exercise a genuine fence read failure through the private planning seam;
	// public Prepare would encounter this closed Store at its identity reads.
	prepareRoleOK(t, f.s.Close())
	_, err = p.planCoordinationRole(ctx, f.state, f.group, f.member, domain.Conversation{}, f.trigger)
	if err == nil || errors.Is(err, scheduler.ErrNotReady) || !errors.Is(err, errPrepareRead) {
		t.Fatalf("fence Store read failure was classified as capacity: %v", err)
	}
}

func TestPrepareRolesResolverFailuresNeverFallback(t *testing.T) {
	t.Run("broken role relation", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		f.exec(t, "DROP TRIGGER coordination_dispatch_requests_no_delete")
		f.exec(t, "DELETE FROM coordination_dispatch_requests WHERE id=?", f.request.ID)
		before := f.snapshot(t)
		got, err := f.preparer(t).Prepare(context.Background(), f.pending)
		if err == nil || got.Executor != nil || errors.Is(err, scheduler.ErrNotReady) || !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatalf("broken role parent became ordinary/disabled: executor=%T, error=%v", got.Executor, err)
		}
		f.assertUnchanged(t, before, false)
	})
	t.Run("ordinary resolver SQL failure", func(t *testing.T) {
		f := newPrepareFixture(t)
		f.exec(t, "DROP TABLE coordination_assignment_dispatch")
		got, err := f.preparer(t).Prepare(context.Background(), f.pending)
		if err == nil || got.Executor != nil || errors.Is(err, scheduler.ErrNotReady) || !errors.Is(err, errPrepareRead) || strings.Contains(err.Error(), "SELECT") {
			t.Fatalf("resolver SQL error became ordinary or leaked: %v", err)
		}
		f.assertQueued(t)
		memberToolRuntimeEmpty(t, f.opts.PrivateWorkspaceRoot)
	})
}

func TestPrepareRolesValidateResolvedIdentity(t *testing.T) {
	f := newRolePrepareFixture(t)
	conversation, err := f.s.GetConversation(context.Background(), f.group.ID, f.pending.Delivery.ConversationID)
	prepareRoleOK(t, err)
	prepareRoleOK(t, validatePreparationRole(f.state, f.group, f.member, conversation, f.trigger, f.pending.Delivery))
	for _, edit := range []func(*store.CoordinationRoleState){
		func(s *store.CoordinationRoleState) { s.Provenance.Kind = "" },
		func(s *store.CoordinationRoleState) { s.Provenance.Kind = "unknown" },
		func(s *store.CoordinationRoleState) { s.Provenance.Kind = store.CoordinationRoleOrdinary },
		func(s *store.CoordinationRoleState) { s.Provenance.ConversationID = "foreign" },
		func(s *store.CoordinationRoleState) { s.Provenance.RequestID = "" },
		func(s *store.CoordinationRoleState) { s.Provenance.AssignmentID = "not-coordinator" },
		func(s *store.CoordinationRoleState) { s.ProviderID = "drifted" },
		func(s *store.CoordinationRoleState) { s.Model = "drifted" },
		func(s *store.CoordinationRoleState) { s.Blocked = "unknown" },
		func(s *store.CoordinationRoleState) { s.AttemptID = "premature" },
	} {
		state := f.state
		edit(&state)
		if !errors.Is(validatePreparationRole(state, f.group, f.member, conversation, f.trigger, f.pending.Delivery), errPrepareBinding) {
			t.Fatal("zero/unknown kind or mismatched resolver evidence passed validation")
		}
	}
}

func TestPrepareRolesWorkerBlockedKeepsAssignment(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.selectWorker(t)
	f.enableRuntime(t)
	p := f.preparer(t)
	ctx := context.Background()
	parent, err := f.s.CreateTask(ctx, store.CreateTaskInput{GroupID: f.group.ID, ActorID: "user", Title: "dependency", Acceptance: "accepted only"})
	prepareRoleOK(t, err)
	f.exec(t, "INSERT INTO task_dependencies(group_id,task_id,dependency_id) VALUES(?,?,?)", f.group.ID, f.request.TaskID, parent.ID)
	before := f.snapshot(t)
	got, err := p.Prepare(ctx, f.pending)
	assertRoleBlocked(t, got, err, f.state, string(store.CoordinationDependencyNotAccepted))
	f.assertUnchanged(t, before, false)
}

// This combines actual factory stat failures with the classifier called by
// Prepare. It does not force interleaving between the two stats inside Prepare.
func TestPrepareRolesCapturedExecutorStatErrors(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	before := f.snapshot(t)
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"removed", syscall.ENOENT},
		{"self-symlink", syscall.ELOOP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := f.opts.Providers[1]
			selected.Path = filepath.Join(t.TempDir(), "PRIVATE-provider")
			contents := []byte("#!/bin/sh\nprintf invoked > \"$0.invoked\"\nexit 99\n")
			prepareRoleOK(t, os.WriteFile(selected.Path, contents, 0700))
			info, err := os.Stat(selected.Path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				t.Fatalf("executable preflight failed: %v", err)
			}
			prepareRoleOK(t, os.Remove(selected.Path))
			if tc.errno == syscall.ELOOP {
				prepareRoleOK(t, os.Symlink(selected.Path, selected.Path))
			}
			executor, captured := provider.NewExecutor(selected, 0)
			if tc.errno == syscall.ELOOP {
				prepareRoleOK(t, os.Remove(selected.Path))
			}
			// Restore before classification to rule out guessing errno via stat.
			prepareRoleOK(t, os.WriteFile(selected.Path, contents, 0700))
			var original *os.PathError
			if executor != nil || !errors.Is(captured, tc.errno) || !errors.As(captured, &original) ||
				original.Op != "stat" || original.Path != selected.Path || !errors.Is(original.Err, tc.errno) ||
				captured.Error() != "提供方执行配置无效或版本不受支持" {
				t.Fatalf("factory did not preserve its actual stat error: %v", captured)
			}
			err = roleExecutorPreparationError(context.Background(), f.state, captured)
			var blocked *RolePreparationBlocked
			if tc.errno == syscall.ENOENT {
				p := f.state.Provenance
				if !errors.Is(captured, os.ErrNotExist) || !errors.Is(err, scheduler.ErrNotReady) || !errors.As(err, &blocked) ||
					blocked.Code != "unsupported_provider" || blocked.Role != p.Kind || blocked.DeliveryID != p.DeliveryID ||
					blocked.RequestID != p.RequestID || blocked.AssignmentID != p.AssignmentID || blocked.Required != 0 || blocked.Limit != 0 {
					t.Fatalf("actual captured absence lost typed role provenance: %v", err)
				}
				data, marshalErr := json.Marshal(blocked)
				prepareRoleOK(t, marshalErr)
				if strings.Contains(string(data), "required") || strings.Contains(string(data), "limit") {
					t.Fatal("provider absence fabricated byte metrics")
				}
			} else {
				var preserved *os.PathError
				if errors.Is(err, scheduler.ErrNotReady) || errors.As(err, &blocked) || !errors.Is(err, errPrepareRead) ||
					!errors.Is(err, captured) || !errors.Is(err, tc.errno) || !errors.As(err, &preserved) || preserved != original || err.Error() != errPrepareRead.Error() {
					t.Fatalf("actual captured I/O failure became readiness or lost its chain: %v", err)
				}
			}
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("role classifier leaked the private provider path")
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			err = roleExecutorPreparationError(canceled, f.state, captured)
			if !errors.Is(err, context.Canceled) || errors.Is(err, scheduler.ErrNotReady) || errors.As(err, &blocked) {
				t.Fatalf("captured factory failure overrode cancellation: %v", err)
			}
			if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
				t.Fatalf("factory invoked native executable: %v", err)
			}
		})
	}
	f.assertUnchanged(t, before, false)
}

func TestPrepareRolesWorkspaceErrorBoundary(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.selectWorker(t)
	f.enableRuntime(t)
	ctx := context.Background()
	workspace := t.TempDir()
	f.exec(t, "UPDATE members SET workspace=? WHERE id IN (?,?)", workspace, f.member.ID, f.coordinator.ID)
	p := f.preparer(t)
	before := f.snapshot(t)
	got, err := p.Prepare(ctx, f.pending)
	assertRoleBlocked(t, got, err, f.state, "workspace_not_ready")
	ordinary, err := p.Prepare(ctx, f.ordinary)
	var blocked *RolePreparationBlocked
	if !reflect.ValueOf(ordinary).IsZero() || err != scheduler.ErrNotReady || errors.As(err, &blocked) {
		t.Fatalf("ordinary ungranted workspace error changed: %v", err)
	}
	f.assertUnchanged(t, before, false)

	// An invalid private scope is a real workspace error, not readiness. The
	// preceding successful Prepare only creates/reuses a directory, never runs.
	f.exec(t, "UPDATE members SET workspace='' WHERE id IN (?,?)", f.member.ID, f.coordinator.ID)
	ready, err := p.Prepare(ctx, f.pending)
	prepareRoleOK(t, err)
	prepareRoleOK(t, os.Chmod(ready.Request.CWD, 0755))
	before = f.snapshot(t)
	got, err = p.Prepare(ctx, f.pending)
	if !reflect.ValueOf(got).IsZero() || !errors.Is(err, errPrepareWorkspace) || errors.Is(err, scheduler.ErrNotReady) || errors.As(err, &blocked) {
		t.Fatalf("private workspace system error became readiness: %v", err)
	}
	f.assertUnchanged(t, before, true)
	for _, table := range []string{"resource_claims", "task_attempts", "coordination_assignment_attempts"} {
		var count int
		prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM "+table).Scan(&count))
		if count != 0 {
			t.Fatalf("workspace preparation created %s", table)
		}
	}
	if len(f.opts.MemberTools.roleFence.records) != 0 {
		t.Fatal("workspace preparation reserved an execution owner")
	}
}

func TestPrepareRolesConcurrentIsolation(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	f.opts.HistoryLimit, f.opts.MaxContextBytes = 1, 4<<10
	f.opts.RoleHistoryLimit = 2
	p := f.preparer(t)
	ctx := context.Background()
	ordinary, err := p.Prepare(ctx, f.ordinary)
	prepareRoleOK(t, err)
	role, err := p.Prepare(ctx, f.pending)
	prepareRoleOK(t, err)
	before := f.snapshot(t)
	operations := slices.Clone(p.memberTools.operations)
	var wg sync.WaitGroup
	failures := make(chan error, 4)
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pending, want := f.pending, role
			if worker%2 == 0 {
				pending, want = f.ordinary, ordinary
			}
			for range 3 {
				got, err := p.Prepare(ctx, pending)
				if err != nil || got.Request != want.Request || !slices.Equal(got.ResourceKeys, want.ResourceKeys) {
					failures <- fmt.Errorf("concurrent profile changed: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if p.historyLimit != 1 || p.maxContextBytes != 4<<10 || p.roleHistoryLimit != 2 || p.roleMaxContextBytes != 256<<10 || !slices.Equal(operations, p.memberTools.operations) {
		t.Fatal("concurrent Prepare mutated shared profile state")
	}
	f.assertUnchanged(t, before, true)
}
