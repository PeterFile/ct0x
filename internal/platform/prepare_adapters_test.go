package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func TestPrepareAdapterDispatchCandidates(t *testing.T) {
	for _, mode := range []string{"pi", "codex-enabled", "codex-default-off", "unknown-version", "unknown-provider", "inactive", "missing-model"} {
		t.Run(mode, func(t *testing.T) {
			f := newRolePrepareFixture(t)
			f.enableRuntime(t)
			candidate := f.opts.Providers[1]
			candidate.ID = "opaque-worker-provider"
			switch mode {
			case "codex-enabled", "codex-default-off":
				candidate.Name, candidate.Version = provider.Codex, "0.166.0"
				f.opts.EnableCodexAppServer = mode == "codex-enabled"
			case "unknown-version":
				candidate.Version = "future"
			case "unknown-provider":
				candidate.Name = "unknown"
			case "inactive":
				f.exec(t, "UPDATE members SET status='left' WHERE id=?", f.worker.ID)
			case "missing-model":
				f.exec(t, "UPDATE members SET model='' WHERE id=?", f.worker.ID)
			}
			f.opts.Providers = append(f.opts.Providers, candidate)
			f.exec(t, "UPDATE members SET provider_id=? WHERE id=?", candidate.ID, f.worker.ID)
			// Even an installed/authenticated legacy capability report is not a
			// selected tool adapter or a server-frozen assignment fact.
			f.opts.Providers[len(f.opts.Providers)-1].Account = provider.Authenticated
			f.opts.Providers[len(f.opts.Providers)-1].Capabilities.Tools = true
			p := f.preparer(t)
			// Sampling must not require worker role provenance before Assignment.
			facts, err := p.sampleDispatchCandidates(context.Background(), f.group.ID, f.coordinator.ID)
			prepareRoleOK(t, err)
			wantReady := mode == "pi" || mode == "codex-enabled"
			if (len(facts) == 1) != wantReady || len(facts) > 1 {
				t.Fatalf("unexpected worker capability facts for %s: %+v", mode, facts)
			}
			if wantReady && facts[0] != (store.DispatchWorkerCapabilityFact{GroupID: f.group.ID, MemberID: f.worker.ID, ProviderID: candidate.ID, Model: f.worker.Model}) {
				t.Fatal("capability fact lost exact opaque provider/model/member binding")
			}
			if mode != "inactive" {
				got, err := p.Prepare(context.Background(), f.pending)
				prepareRoleOK(t, err)
				packet := decodePreparation(t, got.Request.Prompt)
				var metadata struct {
					Candidates *[]roleContextDispatchCandidate `json:"dispatch_candidates"`
					Semantics  string                          `json:"dispatch_candidates_semantics"`
				}
				prepareRoleOK(t, json.Unmarshal([]byte(packet.Responsibility), &metadata))
				if metadata.Candidates == nil || len(*metadata.Candidates) != len(facts) || !strings.Contains(metadata.Semantics, "不是账号或网络实测") || !strings.Contains(metadata.Semantics, "不授予工作") {
					t.Fatal("coordinator context omitted explicit non-authorizing capability samples")
				}
				run := got.Executor.(*memberToolRuntimeRun)
				if !reflect.DeepEqual(run.dispatchCapabilities, facts) {
					t.Fatal("trusted prompt and private runtime binding sampled different candidates")
				}
			}
			// Capability filtering must not hide ordinary visible group members.
			members, err := f.s.ListMembers(context.Background(), f.group.ID, store.ListOptions{Limit: 200})
			prepareRoleOK(t, err)
			if len(members.Items) != 2 {
				t.Fatal("adapter readiness changed member visibility")
			}
			f.assertQueued(t)
		})
	}
}

func TestPrepareAutomaticTextUsesVerifiedPiRoleFacts(t *testing.T) {
	for _, mode := range []string{"verified-pi", "unverified-pi", "codex"} {
		t.Run(mode, func(t *testing.T) {
			f := newRolePrepareFixture(t)
			f.enableRuntime(t)
			selected := f.opts.Providers[1]
			selected.ID = "automatic-worker-provider"
			switch mode {
			case "unverified-pi":
				selected.Version = "unknown"
			case "codex":
				selected.Name, selected.Version = provider.Codex, "0.166.0"
				f.opts.EnableCodexAppServer = true
			}
			f.opts.Providers = append(f.opts.Providers, selected)
			f.exec(t, "UPDATE members SET provider_id=? WHERE id=?", selected.ID, f.worker.ID)
			p := f.preparer(t)
			ctx := t.Context()
			intake, err := f.s.GetMember(ctx, f.group.ID, f.coordinator.ID)
			prepareRoleOK(t, err)
			facts := p.sampleAutomaticTextRoles(ctx, intake)
			if facts.Coordinator.MemberID != intake.ID || facts.Coordinator.ProviderID != intake.ProviderID || facts.Coordinator.Model != intake.Model {
				t.Fatalf("lost the selected intake actor: %+v", facts.Coordinator)
			}
			if want := mode == "verified-pi"; (len(facts.Workers) == 1) != want || want && facts.Workers[0].ProviderID != selected.ID {
				t.Fatalf("wrong automatic worker facts: %+v", facts.Workers)
			}
			intake.ProviderID = selected.ID
			if mode != "verified-pi" && p.sampleAutomaticTextRoles(ctx, intake).Coordinator.MemberID != "" {
				t.Fatal("unqualified coordinator provider received a role fact")
			}
		})
	}
}

func TestPrepareAdapterUnavailableCandidateCannotDispatch(t *testing.T) {
	for _, mode := range []string{"default-off", "unknown-version"} {
		t.Run(mode, func(t *testing.T) {
			f := newRolePrepareFixture(t)
			f.enableRuntime(t)
			workerProvider := f.opts.Providers[1]
			workerProvider.ID, workerProvider.Name, workerProvider.Version = "opaque-codex-worker", provider.Codex, "0.166.0"
			if mode == "unknown-version" {
				workerProvider.Version, f.opts.EnableCodexAppServer = "future", true
			}
			f.opts.Providers = append(f.opts.Providers, workerProvider)
			f.exec(t, "UPDATE members SET provider_id=? WHERE id=?", workerProvider.ID, f.worker.ID)
			prepared, err := f.preparer(t).Prepare(context.Background(), f.pending)
			prepareRoleOK(t, err)
			executePreparedAdapterFixture(t, f, prepared, false, func(token string) {
				got, err := f.opts.MemberTools.tools.DecideDispatch(context.Background(), token, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "可见性不是能力或许可"}, "unavailable")
				if !errors.Is(err, store.ErrConflict) || !reflect.ValueOf(got).IsZero() {
					t.Fatalf("unselected/unknown native adapter created an assignment: %+v, %v", got, err)
				}
			})
			var assignments int
			prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM coordination_assignments").Scan(&assignments))
			if assignments != 0 {
				t.Fatal("unavailable candidate acquired a persisted assignment")
			}
		})
	}
}

func TestPrepareAdapterCodexOptInAndReports(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	f.opts.Providers[1].Name, f.opts.Providers[1].Version = provider.Codex, "0.166.0"
	f.opts.RoleThinking = "max"
	p := f.preparer(t)
	got, err := p.Prepare(context.Background(), f.pending)
	assertRoleBlocked(t, got, err, f.state, "unsupported_provider")
	baseline := p.AdapterReports()[f.member.ProviderID]
	if baseline.ID != provider.CodexOneShotAdapterID || !baseline.Enabled || baseline.Capabilities == nil || baseline.Capabilities.Supports(provider.CapabilityPlatformTools) {
		t.Fatal("default-off Codex report does not describe the actual one-shot selection")
	}
	f.opts.EnableCodexAppServer = true
	f.opts.PiMemberToolsExtension = "" // Explicit Codex-only setup.
	p = f.preparer(t)
	got, err = p.Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	if got.Request.Thinking != "" {
		t.Fatal("Pi role thinking escaped into Codex request")
	}
	permissions := strings.Join(decodePreparation(t, got.Request.Prompt).Permissions, "\n")
	for _, text := range []string{"Codex AppServer", "受限只读", "apply_patch", "view_image", "读取不承诺隔离", "Artifact", "不提供 resources"} {
		if !strings.Contains(permissions, text) {
			t.Fatalf("native text restriction missing %q", text)
		}
	}
	if strings.Contains(permissions, "已验证 Pi 1.4.0") {
		t.Fatal("Codex role inherited a Pi-specific capability claim")
	}
	report := p.AdapterReports()[f.member.ProviderID]
	if report.ID != provider.CodexAppServerAdapterID || report.Protocol != provider.CodexAppServerProtocol || !report.Enabled || report.Capabilities == nil || !memberToolCapabilitiesSupported(*report.Capabilities) {
		t.Fatal("selected AppServer report is not sourced from the shared capability predicate")
	}
	report.Capabilities.PlatformTools = provider.SupportUnknown
	if p.AdapterReports()[f.member.ProviderID].Capabilities.PlatformTools != provider.SupportSupported {
		t.Fatal("adapter reports retain mutable caller capability pointers")
	}
	f.opts.Providers[1].Version = "unknown"
	p = f.preparer(t)
	if report := p.AdapterReports()[f.member.ProviderID]; !report.Enabled || report.Capabilities != nil {
		t.Fatal("unknown implementation support was reported as known readiness")
	}
	got, err = p.Prepare(context.Background(), f.pending)
	assertRoleBlocked(t, got, err, f.state, "unsupported_provider")
	f.assertQueued(t)
}

// Invoke the actual config-bound adapter factory after claim, but replace only
// native Execute with a synthetic fixture. This is wiring evidence, not a CLI,
// model, account, protocol or sandbox acceptance test.
func executePreparedAdapterFixture(t *testing.T, f *rolePrepareFixture, prepared scheduler.PreparedExecution, cancelUnknown bool, use func(string)) (store.TurnClaim, provider.ExecutionReceipt) {
	t.Helper()
	ctx := context.Background()
	run := prepared.Executor.(*memberToolRuntimeRun)
	var issued string
	replaceExecute := func(path string, names []string, actual provider.Executor, err error) (provider.Executor, error) {
		if err != nil || actual == nil {
			t.Fatalf("actual claimed adapter factory failed: %v", err)
		}
		var endpoint, token string
		if run.projectExperienceMCP {
			endpoint, token = memberToolRuntimeProjectExperienceConfig(t, path, names)
		} else {
			endpoint, token = memberToolRuntimeConfigWithDiscussion(t, path, run.intakeSchema.piDiscussionExcerpt)
		}
		if endpoint != f.runtimeOptions.Endpoint {
			t.Fatal("factory changed the original endpoint/config contract")
		}
		issued = token
		return memberToolRuntimeExecutorFunc(func(ctx context.Context, req provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
			session := "synthetic-" + req.RunID
			for _, kind := range []provider.EventKind{provider.SessionEvent, provider.AcceptedEvent} {
				prepareRoleOK(t, onEvent(provider.Event{Kind: kind, NativeSessionID: session}))
			}
			use(token)
			if cancelUnknown {
				_, err := f.s.RequestTurnCancel(ctx, f.group.ID, req.RunID)
				prepareRoleOK(t, err)
				return provider.ExecutionReceipt{Started: true, Accepted: true, NativeSessionID: session, Outcome: provider.OutcomeUnknown, DirectChildReaped: true, ExitCode: 0}
			}
			prepareRoleOK(t, onEvent(provider.Event{Kind: provider.ResponseEvent, NativeSessionID: session, Text: "合成接线回执"}))
			prepareRoleOK(t, onEvent(provider.Event{Kind: provider.TerminalEvent, NativeSessionID: session, Terminal: provider.OutcomeSucceeded}))
			return provider.ExecutionReceipt{Started: true, Accepted: true, NativeSessionID: session, Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, OutputComplete: true, DirectChildReaped: true, ExitCode: 0}
		}), nil
	}
	switch {
	case run.contextFactory != nil:
		factory := run.contextFactory
		run.contextFactory = func(ctx context.Context, path, runID string, names []string) (provider.Executor, error) {
			actual, err := factory(ctx, path, runID, names)
			return replaceExecute(path, names, actual, err)
		}
	case run.intakeBuilder != nil:
		build := run.intakeBuilder
		run.intakeBuilder = func(config provider.NativeMemberToolConfig) (provider.Executor, error) {
			actual, err := build(config)
			return replaceExecute(config.Path, nil, actual, err)
		}
	default:
		factory := run.factory
		run.factory = func(path, id string, names []string) (provider.Executor, error) {
			actual, err := factory(path, id, names)
			return replaceExecute(path, names, actual, err)
		}
	}
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: run.binding.DeliveryID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
	prepareRoleOK(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	request := prepared.Request
	request.RunID, request.RuntimeDir = turn.ID, f.runtimeOptions.RoleExecutionRoot
	receipt := run.Execute(ctx, request, func(event provider.Event) error { return f.s.RecordTurnEvent(ctx, claim, event) })
	if issued == "" || len(f.opts.MemberTools.tools.capabilities) != 0 {
		t.Fatal("claim did not issue then drain/revoke exactly its own tool token")
	}
	memberToolRuntimeEmpty(t, f.runtimeOptions.ConfigRoot)
	_, err = f.opts.MemberTools.tools.TransportOperations(ctx, issued)
	if !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("completed adapter kept a live token")
	}
	settled, err := f.s.ApplyTurnReceipt(ctx, claim, receipt)
	prepareRoleOK(t, err)
	wantState, wantClaims := domain.TurnStateSucceeded, 0
	if cancelUnknown {
		wantState, wantClaims = domain.TurnStateUnknown, 1
	}
	var claims int
	prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", turn.ID).Scan(&claims))
	if settled.State != wantState || claims != wantClaims {
		t.Fatalf("adapter changed common receipt/resource semantics: state=%s claims=%d", settled.State, claims)
	}
	if run.role != nil {
		diagnostic, err := f.opts.MemberTools.InspectCoordinationRoleReceipt(ctx, f.group.ID, turn.ID)
		prepareRoleOK(t, err)
		if diagnostic.Capture == nil || diagnostic.Capture.Raw != receipt || diagnostic.Capture.Aggregate != receipt || diagnostic.Capture.Source.ProviderID != run.binding.ProviderID {
			t.Fatal("selected adapter did not share original role receipt capture")
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "invoked")); !os.IsNotExist(err) {
		t.Fatal("wiring fixture invoked the sentinel CLI")
	}
	return claim, receipt
}

func memberToolRuntimeProjectExperienceConfig(t *testing.T, path string, names []string) (string, string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode() != 0600 || !filepath.IsAbs(path) {
		t.Fatalf("config must be an absolute regular 0600 file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]json.RawMessage
	if json.Unmarshal(data, &config) != nil || len(config) != 3 {
		t.Fatal("project-experience config contains unapproved fields")
	}
	var endpoint, token string
	var tools []string
	if json.Unmarshal(config["endpoint"], &endpoint) != nil || endpoint == "" ||
		json.Unmarshal(config["token"], &token) != nil || token == "" ||
		json.Unmarshal(config["project_experience_tools"], &tools) != nil {
		t.Fatal("invalid project-experience config")
	}
	if !slices.Equal(tools, names) {
		t.Fatalf("issued project-experience tools changed: got %v, want %v", tools, names)
	}
	return endpoint, token
}

func TestPrepareAdaptersShareOrdinaryRoleSubmissionReviewChain(t *testing.T) {
	for _, name := range []string{provider.Pi, provider.Codex, provider.Agy} {
		t.Run(name, func(t *testing.T) {
			f := newRolePrepareFixture(t)
			f.enableRuntime(t)
			f.opts.RoleThinking = "max"
			if name == provider.Codex {
				f.opts.Providers[1].Name, f.opts.Providers[1].Version = name, "0.166.0"
				f.opts.EnableCodexAppServer, f.opts.PiMemberToolsExtension = true, ""
			} else if name == provider.Agy {
				f.opts.Providers[1].Name, f.opts.Providers[1].Version = name, "1.5.16"
				f.opts.EnableAgyACP, f.opts.PiMemberToolsExtension = true, ""
			}
			p := f.preparer(t)
			ctx := context.Background()
			ordinary, err := p.Prepare(ctx, f.ordinary)
			prepareRoleOK(t, err)
			if name == provider.Agy {
				run := ordinary.Executor.(*memberToolRuntimeRun)
				if !run.projectExperienceMCP || run.contextFactory == nil {
					t.Fatal("ordinary AGY project experience lost its context-aware MCP factory")
				}
			}
			executePreparedAdapterFixture(t, f, ordinary, false, func(token string) {
				members, err := f.opts.MemberTools.tools.ListMembers(ctx, token, store.ListOptions{})
				prepareRoleOK(t, err)
				if len(members.Items) != 2 {
					t.Fatal("ordinary adapter changed list_members visibility")
				}
				_, err = f.opts.MemberTools.tools.DecideDispatch(ctx, token, store.CoordinatorDecisionInput{Kind: "decline", Reason: "普通令牌不得扩权"}, "denied")
				if !errors.Is(err, store.ErrMemberToolDenied) {
					t.Fatal("ordinary native tools acquired coordinator scope")
				}
			})
			coordinator, err := p.Prepare(ctx, f.pending)
			prepareRoleOK(t, err)
			var decision domain.DecisionReceipt
			executePreparedAdapterFixture(t, f, coordinator, false, func(token string) {
				decision, err = f.opts.MemberTools.tools.DecideDispatch(ctx, token, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "共用冻结能力门禁"}, "assign")
				prepareRoleOK(t, err)
				if decision.Assignment == nil {
					t.Fatal("known selected adapter candidate did not create assignment")
				}
			})
			materialized, err := f.s.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
			prepareRoleOK(t, err)
			if materialized.Delivery == nil {
				t.Fatal("assignment did not produce the common worker delivery")
			}
			f.selectDelivery(t, materialized.Delivery.MessageID)
			worker, err := p.Prepare(ctx, f.pending)
			prepareRoleOK(t, err)
			if (name == provider.Codex || name == provider.Agy) && (worker.Request.Thinking != "" || coordinator.Request.Thinking != "") || name == provider.Pi && worker.Request.Thinking != "max" {
				t.Fatal("adapter-specific role thinking selection changed")
			}
			var submission domain.Submission
			executePreparedAdapterFixture(t, f, worker, false, func(token string) {
				submission, err = f.opts.MemberTools.tools.SubmitAssignmentResult(ctx, token, store.AssignmentResultInput{Summary: "共同文本交付"}, "submit")
				prepareRoleOK(t, err)
				task, err := f.s.GetTask(ctx, f.group.ID, f.request.TaskID)
				prepareRoleOK(t, err)
				if task.Status != domain.TaskStatusSubmitted || task.CurrentSubmissionID != submission.ID {
					t.Fatal("submission bypassed the shared task lifecycle or became acceptance")
				}
			})
			_, err = NewLocalUser(f.s).ReviewTask(ctx, f.group.ID, f.request.TaskID, TaskReviewInput{ExpectedRevision: f.request.TaskRevision, SubmissionID: submission.ID, Outcome: domain.ReviewOutcomeAccepted, Reason: "本机用户评审"}, "review")
			prepareRoleOK(t, err)
			task, err := f.s.GetTask(ctx, f.group.ID, f.request.TaskID)
			prepareRoleOK(t, err)
			if task.Status != domain.TaskStatusAccepted {
				t.Fatal("selected adapter did not use the existing explicit Review chain")
			}
		})
	}
}

func TestPrepareAdaptersCancelUnknownKeepsResources(t *testing.T) {
	for _, name := range []string{provider.Pi, provider.Codex, provider.Agy} {
		t.Run(name, func(t *testing.T) {
			f := newRolePrepareFixture(t)
			f.enableRuntime(t)
			if name == provider.Codex {
				f.opts.Providers[1].Name, f.opts.Providers[1].Version = name, "0.166.0"
				f.opts.EnableCodexAppServer = true
			} else if name == provider.Agy {
				f.opts.Providers[1].Name, f.opts.Providers[1].Version = name, "1.5.16"
				f.opts.EnableAgyACP = true
			}
			prepared, err := f.preparer(t).Prepare(context.Background(), f.pending)
			prepareRoleOK(t, err)
			executePreparedAdapterFixture(t, f, prepared, true, func(token string) {
				_, err := f.opts.MemberTools.tools.ListMembers(context.Background(), token, store.ListOptions{})
				prepareRoleOK(t, err)
			})
			if err := f.opts.MemberTools.CheckCoordinationRoleCapacity(context.Background()); !errors.Is(err, errMemberToolRoleFenceCapacity) {
				t.Fatal("native cancellation/unknown prematurely reclaimed the role slot")
			}
		})
	}
}
