package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

func assertInspection(t *testing.T, got RoleReadinessInspection, f *rolePrepareFixture, before, after time.Time) {
	t.Helper()
	p := f.state.Provenance
	if got.ObservedAt.Before(before) || got.ObservedAt.After(after) || got.ObservedAt.Location() != time.UTC ||
		got.Role != p.Kind || got.DeliveryID != p.DeliveryID || got.RequestID != p.RequestID || got.AssignmentID != p.AssignmentID ||
		got.HistoryLimit != 50 || got.MaxContextBytes != 256<<10 {
		t.Fatal("inspection lost sampled UTC time, authoritative identity or the actual immutable role profile")
	}
	if reflect.TypeOf(got).NumField() != 8 {
		t.Fatal("inspection DTO grew beyond the accepted diagnostic whitelist")
	}
	data, err := json.Marshal(got)
	prepareRoleOK(t, err)
	var fields map[string]json.RawMessage
	prepareRoleOK(t, json.Unmarshal(data, &fields))
	for key := range fields {
		if !slices.Contains([]string{"observed_at", "role", "delivery_id", "request_id", "assignment_id", "history_limit", "max_context_bytes", "blocked"}, key) {
			t.Fatalf("unexpected diagnostic field: %s", key)
		}
	}
	for _, private := range []string{f.opts.PrivateWorkspaceRoot, f.opts.PiMemberToolsExtension, f.member.Model, f.trigger.Content} {
		if private != "" && strings.Contains(string(data), private) {
			t.Fatal("inspection exposed a private path, model or prompt")
		}
	}
}

func TestInspectRoleReadinessDoesNotExecute(t *testing.T) {
	for _, kind := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(kind), func(t *testing.T) {
			f := newRolePrepareFixture(t)
			if kind == store.CoordinationRoleWorker {
				f.selectWorker(t) // Trusted setup only; not an Inspect action.
			}
			f.enableRuntime(t)
			before := f.snapshot(t)
			root, err := os.Lstat(f.opts.PrivateWorkspaceRoot)
			prepareRoleOK(t, err)
			p := f.preparer(t)
			for round := range 3 {
				if round == 2 {
					p = f.preparer(t)
				}
				start := time.Now().UTC()
				got, err := p.InspectRoleReadiness(context.Background(), f.pending)
				prepareRoleOK(t, err)
				assertInspection(t, got, f, start, time.Now().UTC())
				if got.Blocked != nil {
					t.Fatalf("legal role was blocked: %v", got.Blocked)
				}
				f.assertUnchanged(t, before, false)
				after, err := os.Lstat(f.opts.PrivateWorkspaceRoot)
				prepareRoleOK(t, err)
				if !os.SameFile(root, after) || !root.ModTime().Equal(after.ModTime()) {
					t.Fatal("inspection transiently changed the missing private workspace parent")
				}
			}
			if len(f.opts.MemberTools.roleFence.records) != 0 {
				t.Fatal("inspection reserved an execution owner")
			}
		})
	}
	t.Run("ordinary is non-role, even without a provider", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		f.opts.Providers = nil
		before := f.snapshot(t)
		got, err := f.preparer(t).InspectRoleReadiness(context.Background(), f.ordinary)
		prepareRoleOK(t, err)
		if got.Role != store.CoordinationRoleOrdinary || got.DeliveryID != f.ordinary.Delivery.ID || got.ObservedAt.IsZero() ||
			got.RequestID != "" || got.AssignmentID != "" || got.HistoryLimit != 0 || got.MaxContextBytes != 0 || got.Blocked != nil {
			t.Fatal("ordinary delivery was turned into a role readiness claim")
		}
		f.assertUnchanged(t, before, false)
	})
}

func assertInspectBlockedMatchesPrepare(t *testing.T, f *rolePrepareFixture, p *Preparer, code string) {
	t.Helper()
	before := f.snapshot(t)
	start := time.Now().UTC()
	got, err := p.InspectRoleReadiness(context.Background(), f.pending)
	prepareRoleOK(t, err)
	assertInspection(t, got, f, start, time.Now().UTC())
	prepared, err := p.Prepare(context.Background(), f.pending)
	blocked := assertRoleBlocked(t, prepared, err, f.state, code)
	if !reflect.DeepEqual(got.Blocked, blocked) || !errors.Is(got.Blocked, scheduler.ErrNotReady) {
		t.Fatal("known blocked is not the same diagnostic with nil transport error")
	}
	got.Blocked.Code = "caller-mutated"
	again, err := p.InspectRoleReadiness(context.Background(), f.pending)
	prepareRoleOK(t, err)
	if !reflect.DeepEqual(again.Blocked, blocked) || again.Blocked == got.Blocked {
		t.Fatal("inspection reused mutable blocked evidence")
	}
	f.assertUnchanged(t, before, false)
}

func TestInspectRoleReadinessMatchesPrepare(t *testing.T) {
	t.Run("disabled and unsupported", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		assertInspectBlockedMatchesPrepare(t, f, f.preparer(t), "role_disabled")
		f.enableRuntime(t)
		f.opts.Providers = nil
		assertInspectBlockedMatchesPrepare(t, f, f.preparer(t), "unsupported_provider")
	})
	t.Run("worker dependency", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		f.selectWorker(t)
		f.enableRuntime(t)
		parent, err := f.s.CreateTask(context.Background(), store.CreateTaskInput{GroupID: f.group.ID, ActorID: "user", Title: "dependency", Acceptance: "accepted only"})
		prepareRoleOK(t, err)
		f.exec(t, "INSERT INTO task_dependencies(group_id,task_id,dependency_id) VALUES(?,?,?)", f.group.ID, f.request.TaskID, parent.ID)
		assertInspectBlockedMatchesPrepare(t, f, f.preparer(t), "dependency_not_accepted")
	})
}

func TestInspectRoleReadinessBudget(t *testing.T) {
	f := newRolePrepareFixture(t, strings.Repeat("目标🙂<&>\\\x00\x01", 1990))
	f.enableRuntime(t)
	minimum := minimumRolePacket(t, f.contextPlan(t, f.preparer(t)))
	f.opts.RoleMaxContextBytes = minimum.Required - 1
	p := f.preparer(t)
	before := f.snapshot(t)
	got, err := p.InspectRoleReadiness(context.Background(), f.pending)
	prepareRoleOK(t, err)
	prepared, err := p.Prepare(context.Background(), f.pending)
	blocked := assertRoleBlocked(t, prepared, err, f.state, "context_budget")
	if !reflect.DeepEqual(got.Blocked, blocked) || blocked.Required != minimum.Required || blocked.Limit != minimum.Required-1 || got.MaxContextBytes != minimum.Required-1 {
		t.Fatal("Inspect did not preserve actual shared Build budget metrics")
	}
	f.assertUnchanged(t, before, false)
	f.opts.RoleMaxContextBytes++
	p = f.preparer(t)
	got, err = p.InspectRoleReadiness(context.Background(), f.pending)
	prepareRoleOK(t, err)
	if got.Blocked != nil || got.MaxContextBytes != minimum.Required {
		t.Fatal("exact minimum budget was not accepted")
	}
	f.assertUnchanged(t, before, false)
	prepared, err = p.Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	if len(prepared.Request.Prompt) != minimum.Required {
		t.Fatal("Inspect/Prepare exact boundary uses different packets")
	}
	f.assertUnchanged(t, before, true)
}

func assertInspectionFailure(t *testing.T, p *Preparer, ctx context.Context, pending store.PendingDelivery, want error) error {
	t.Helper()
	got, err := p.InspectRoleReadiness(ctx, pending)
	var blocked *RolePreparationBlocked
	if !reflect.ValueOf(got).IsZero() || !errors.Is(err, want) || errors.Is(err, scheduler.ErrNotReady) || errors.As(err, &blocked) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("invalid/system inspection did not return zero evidence and a sanitized real error: %v", err)
	}
	return err
}

func TestInspectRoleReadinessIdentityAndFailures(t *testing.T) {
	t.Run("identity and cancellation", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		f.enableRuntime(t)
		p := f.preparer(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		before := f.snapshot(t)
		assertInspectionFailure(t, p, ctx, f.pending, context.Canceled)
		foreign := f.pending
		foreign.GroupID = "PRIVATE-foreign"
		assertInspectionFailure(t, p, context.Background(), foreign, errPrepareBinding)
		conversation, err := f.s.GetOrCreateConversation(context.Background(), f.group.ID, f.member.ID, "other-topic")
		prepareRoleOK(t, err)
		wrongTopic := f.pending
		wrongTopic.Delivery.ConversationID = conversation.ID
		before = f.snapshot(t)
		assertInspectionFailure(t, p, context.Background(), wrongTopic, errPrepareBinding)
		f.assertUnchanged(t, before, false)
		// The original queued input is now stale after a legal setup claim.
		_, err = f.s.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, MaxActive: 1})
		prepareRoleOK(t, err)
		before = f.snapshot(t)
		assertInspectionFailure(t, p, context.Background(), f.pending, errPrepareBinding)
		if !reflect.DeepEqual(before, f.snapshot(t)) {
			t.Fatal("stale inspection changed the real claim")
		}
		memberToolRuntimeEmpty(t, f.opts.PrivateWorkspaceRoot)
	})
	t.Run("real filesystem and encoding failures", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		f.enableRuntime(t)
		loop := filepath.Join(t.TempDir(), "PRIVATE-loop")
		prepareRoleOK(t, os.Symlink(loop, loop))
		selected := f.opts.Providers[1]
		f.opts.Providers[1].Path = loop
		before := f.snapshot(t)
		err := assertInspectionFailure(t, f.preparer(t), context.Background(), f.pending, syscall.ELOOP)
		var pathError *os.PathError
		if !errors.As(err, &pathError) || pathError.Path != loop || !errors.Is(pathError.Err, syscall.ELOOP) {
			t.Fatal("Inspect lost the actual path error chain")
		}
		f.assertUnchanged(t, before, false)
		f.opts.Providers[1] = selected
		f.opts.RoleMaxContextBytes = 1
		f.exec(t, "UPDATE groups SET description=? WHERE id=?", "PRIVATE-bad\xff", f.group.ID)
		before = f.snapshot(t)
		assertInspectionFailure(t, f.preparer(t), context.Background(), f.pending, errPrepareBinding)
		f.assertUnchanged(t, before, false)
	})
	t.Run("real resolver database failure", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		p := f.preparer(t)
		f.exec(t, "DROP TABLE coordination_assignment_dispatch")
		err := assertInspectionFailure(t, p, context.Background(), f.pending, errPrepareRead)
		var databaseError interface{ Code() int }
		if !errors.As(err, &databaseError) || databaseError.Code() == 0 || strings.Contains(err.Error(), "SQL") {
			t.Fatal("Inspect lost its real database cause or exposed SQL")
		}
		memberToolRuntimeEmpty(t, f.opts.PrivateWorkspaceRoot)
	})
}

func TestInspectRoleReadinessWorkspace(t *testing.T) {
	t.Run("private target evidence", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		f.enableRuntime(t)
		p := f.preparer(t)
		// Prepare is setup for this existing-target test, not an Inspect action.
		ready, err := p.Prepare(context.Background(), f.pending)
		prepareRoleOK(t, err)
		path := ready.Request.CWD
		original, err := os.Lstat(path)
		prepareRoleOK(t, err)
		before := f.snapshot(t)
		got, err := p.InspectRoleReadiness(context.Background(), f.pending)
		prepareRoleOK(t, err)
		after, err := os.Lstat(path)
		prepareRoleOK(t, err)
		if got.Blocked != nil || !os.SameFile(original, after) || original.Mode() != after.Mode() || !original.ModTime().Equal(after.ModTime()) {
			t.Fatal("Inspect changed or rejected the existing private target")
		}
		prepareRoleOK(t, os.Remove(path))
		got, err = p.InspectRoleReadiness(context.Background(), f.pending)
		prepareRoleOK(t, err)
		if got.Blocked != nil {
			t.Fatal("absent authorized private target requires no creation or future readiness promise")
		}
		input, err := p.loadPreparationInput(context.Background(), f.pending)
		prepareRoleOK(t, err)
		cwd, key, err := p.resolvePreparationWorkspace(input.member, input.conversation, false)
		prepareRoleOK(t, err)
		if cwd != "" || key != "" {
			t.Fatal("read-only resolution fabricated a missing target identity")
		}
		for _, kind := range []string{"file", "symlink", "dangling", "mode"} {
			switch kind {
			case "file":
				prepareRoleOK(t, os.WriteFile(path, []byte("PRIVATE-untouched"), 0600))
			case "symlink":
				prepareRoleOK(t, os.Symlink(t.TempDir(), path))
			case "dangling":
				prepareRoleOK(t, os.Symlink(filepath.Join(t.TempDir(), "absent"), path))
			case "mode":
				prepareRoleOK(t, os.Mkdir(path, 0755))
			}
			assertInspectionFailure(t, p, context.Background(), f.pending, errPrepareWorkspace)
			prepareRoleOK(t, os.Remove(path))
		}
		f.assertUnchanged(t, before, false)
		prepareRoleOK(t, os.Rename(f.opts.PrivateWorkspaceRoot, f.opts.PrivateWorkspaceRoot+"-old"))
		prepareRoleOK(t, os.Mkdir(f.opts.PrivateWorkspaceRoot, 0700))
		assertInspectionFailure(t, p, context.Background(), f.pending, errPrepareWorkspace)
	})
	t.Run("exact copied external grant", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		f.enableRuntime(t)
		external := t.TempDir()
		marker := filepath.Join(external, "PRIVATE-unchanged")
		prepareRoleOK(t, os.WriteFile(marker, []byte("original"), 0600))
		f.exec(t, "UPDATE members SET workspace=? WHERE id=?", external, f.member.ID)
		assertInspectBlockedMatchesPrepare(t, f, f.preparer(t), "workspace_not_ready")
		f.opts.WorkspaceGrants = []WorkspaceGrant{{GroupID: f.group.ID, MemberID: f.member.ID, Root: external}}
		p := f.preparer(t)
		f.opts.WorkspaceGrants[0].Root = t.TempDir()
		before := f.snapshot(t)
		got, err := p.InspectRoleReadiness(context.Background(), f.pending)
		prepareRoleOK(t, err)
		if got.Blocked != nil {
			t.Fatal("Inspect ignored the immutable exact workspace grant")
		}
		data, err := os.ReadFile(marker)
		prepareRoleOK(t, err)
		entries, err := os.ReadDir(external)
		prepareRoleOK(t, err)
		if string(data) != "original" || len(entries) != 1 {
			t.Fatal("Inspect wrote to the external workspace")
		}
		f.assertUnchanged(t, before, false)
	})
}

func TestInspectRoleReadinessConcurrentIsolation(t *testing.T) {
	t.Run("immutable profiles", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		f.enableRuntime(t)
		f.opts.HistoryLimit, f.opts.MaxContextBytes = 1, 4096
		f.opts.RoleHistoryLimit, f.opts.RoleMaxContextBytes = 2, 64<<10
		p := f.preparer(t)
		ordinary, err := p.Prepare(context.Background(), f.ordinary)
		prepareRoleOK(t, err)
		before := f.snapshot(t)
		operations := slices.Clone(p.memberTools.operations)
		f.opts.HistoryLimit, f.opts.RoleHistoryLimit, f.opts.RoleMaxContextBytes = 99, 99, 1
		var wg sync.WaitGroup
		failures := make(chan error, 4)
		for worker := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 3 {
					if worker%2 == 0 {
						got, err := p.InspectRoleReadiness(context.Background(), f.pending)
						if err != nil || got.Blocked != nil || got.Role != f.state.Provenance.Kind || got.DeliveryID != f.pending.Delivery.ID || got.HistoryLimit != 2 || got.MaxContextBytes != 64<<10 {
							failures <- fmt.Errorf("concurrent inspection changed role profile: %v", err)
							return
						}
					} else {
						got, err := p.Prepare(context.Background(), f.ordinary)
						if err != nil || got.Request != ordinary.Request || !slices.Equal(got.ResourceKeys, ordinary.ResourceKeys) {
							failures <- fmt.Errorf("concurrent inspection changed ordinary preparation: %v", err)
							return
						}
					}
				}
			}()
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		if p.historyLimit != 1 || p.maxContextBytes != 4096 || p.roleHistoryLimit != 2 || p.roleMaxContextBytes != 64<<10 || !slices.Equal(operations, p.memberTools.operations) {
			t.Fatal("inspection mutated the preparer's immutable profiles")
		}
		f.assertUnchanged(t, before, true)
	})
	t.Run("cancel and unknown owners remain held", func(t *testing.T) {
		f := newRolePrepareFixture(t)
		f.enableRuntime(t)
		ctx := context.Background()
		turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.ordinary.Delivery.ID, MaxActive: 1, ResourceKeys: []string{"fixture-held"}})
		prepareRoleOK(t, err)
		claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
		fence := f.opts.MemberTools.roleFence
		owner, err := fence.reserve(ctx, claim, f.runtimeOptions.RoleExecutionRoot)
		prepareRoleOK(t, err)
		owner.markLocalDone()
		_, err = f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
		prepareRoleOK(t, err)
		p := f.preparer(t)
		for _, state := range []domain.TurnState{domain.TurnStateCancelRequested, domain.TurnStateUnknown} {
			if state == domain.TurnStateUnknown {
				_, err = f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown})
				prepareRoleOK(t, err)
			}
			before := f.snapshot(t)
			got, err := p.InspectRoleReadiness(ctx, f.pending)
			prepareRoleOK(t, err)
			if got.Blocked == nil || got.Blocked.Code != "role_runtime_capacity" || got.Blocked.Required != 0 || got.Blocked.Limit != 0 {
				t.Fatal("Inspect did not preserve occupied capacity as a typed diagnostic")
			}
			current, err := f.s.GetTurn(ctx, claim.GroupID, claim.TurnID)
			prepareRoleOK(t, err)
			var resources int
			prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", claim.TurnID).Scan(&resources))
			if current.State != state || resources != 1 || len(fence.records) != 1 || fence.records[claim.TurnID] != owner {
				t.Fatal("inspection reclaimed an unknown/cancel owner or its actual claim")
			}
			f.assertUnchanged(t, before, false)
		}
	})
}

// This fixed source guard supplements filesystem/DB evidence: native sentinels
// alone cannot detect value-only executor construction. It is not a general
// call-graph proof; these small shared bodies are also inspected directly.
func TestInspectRoleReadinessCallBoundary(t *testing.T) {
	for _, target := range []struct{ path, name string }{
		{"prepare_inspect.go", "InspectRoleReadiness"},
		{"prepare_inspect.go", "roleInspectionResult"},
		{"prepare.go", "loadPreparationInput"},
		{"prepare_roles.go", "planCoordinationRole"},
		{"prepare_roles.go", "format"},
		{"../provider/execution_factory.go", "ValidateExecutor"},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), target.path, nil, 0)
		prepareRoleOK(t, err)
		found := false
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != target.name {
				continue
			}
			found = true
			var authorityReads []string
			readonlyCalls, stats := 0, 0
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch callee := call.Fun.(type) {
				case *ast.Ident:
					name = callee.Name
				case *ast.SelectorExpr:
					name = callee.Sel.Name
				}
				if name == "Stat" {
					stats++
				}
				if slices.Contains([]string{"GetGroup", "GetMember", "GetConversation", "GetMessage", "ResolveCoordinationDelivery"}, name) {
					authorityReads = append(authorityReads, name)
				}
				if target.name == "InspectRoleReadiness" && name == "resolvePreparationWorkspace" {
					readonlyCalls++
					flag, ok := call.Args[len(call.Args)-1].(*ast.Ident)
					if !ok || flag.Name != "false" {
						t.Error("Inspect workspace mode is not the private literal false")
					}
				}
				if slices.Contains([]string{"Prepare", "workspace", "NewExecutor", "NewPiExecutor", "NewCodexExecutor", "NewPiMemberToolExecutorWithTools", "Wrap", "WrapCoordinationRole", "ClaimTurn", "BindMemberTool", "Issue", "Mkdir", "MkdirAll", "OpenFile"}, name) {
					t.Errorf("%s still calls forbidden Inspect operation %s", target.name, name)
				}
				return true
			})
			if target.name == "InspectRoleReadiness" && readonlyCalls != 1 {
				t.Error("Inspect does not use the single read-only workspace boundary")
			}
			if target.name == "loadPreparationInput" && !slices.Equal(authorityReads, []string{"GetGroup", "GetMember", "GetConversation", "GetMessage", "ResolveCoordinationDelivery"}) {
				t.Error("shared authority load changed the original read order/count")
			}
			if (target.name == "planCoordinationRole" || target.name == "ValidateExecutor") && stats != 1 {
				t.Error("role preflight/provider validation no longer has one original stat each")
			}
		}
		if !found {
			t.Errorf("shared no-construction boundary is missing: %s", target.name)
		}
	}
}
