package platform_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

type roleNative func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt

func (f roleNative) Execute(ctx context.Context, req provider.ExecuteRequest, event func(provider.Event) error) provider.ExecutionReceipt {
	return f(ctx, req, event)
}
func roleOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type roleRun struct {
	binding    platform.MemberToolRunBinding
	provenance store.CoordinationRoleProvenance
	claim      store.TurnClaim
	request    provider.ExecuteRequest
}
type roleRuntimeFixture struct {
	db                       *store.Store
	raw                      *sql.DB
	runtime                  *platform.MemberToolRuntime
	local                    *platform.LocalUser
	options                  platform.MemberToolRuntimeOptions
	root, path, group, other string
	lead, worker             domain.Member
	handler                  http.Handler
}

func newRoleRuntime(t *testing.T) *roleRuntimeFixture {
	t.Helper()
	base := t.TempDir()
	f := &roleRuntimeFixture{path: filepath.Join(base, "workspace.db"), root: filepath.Join(base, "runtime")}
	var err error
	f.db, err = store.Open(f.path)
	roleOK(t, err)
	t.Cleanup(func() { f.db.Close() })
	f.raw, err = sql.Open("sqlite", f.path)
	roleOK(t, err)
	t.Cleanup(func() { f.raw.Close() })
	f.local = platform.NewLocalUser(f.db)
	g, err := f.local.CreateGroup(context.Background(), platform.GroupInput{Name: "roles"}, "")
	roleOK(t, err)
	f.group = g.ID
	g, err = f.local.CreateGroup(context.Background(), platform.GroupInput{Name: "other"}, "")
	roleOK(t, err)
	f.other = g.ID
	f.lead, err = f.local.CreateMember(context.Background(), f.group, platform.MemberInput{Name: "lead", Role: "untrusted", ProviderID: "pi", Model: "selected"}, "")
	roleOK(t, err)
	f.worker, err = f.local.CreateMember(context.Background(), f.group, platform.MemberInput{Name: "worker", Role: "untrusted", ProviderID: "pi", Model: "selected"}, "")
	roleOK(t, err)
	_, err = f.db.SetCoordinator(context.Background(), f.group, f.lead.ID, 0, "binding")
	roleOK(t, err)
	_, err = f.db.SetDispatchPolicy(context.Background(), f.group, []string{f.worker.ID}, 0, "policy")
	roleOK(t, err)
	config := filepath.Join(base, "config")
	roleOK(t, os.Mkdir(config, 0700))
	roleOK(t, os.Mkdir(f.root, 0700))
	f.options = platform.MemberToolRuntimeOptions{ConfigRoot: config, Endpoint: "http://127.0.0.1:7310/api/v1/member-tools", Operations: []string{"list_members", "read_channel", "send_message", "create_member"}, RoleMaxActive: 1, RoleExecutionRoot: f.root}
	f.runtime, err = platform.NewMemberToolRuntime(f.db, f.options)
	roleOK(t, err)
	routes := append(httpapi.MemberToolRoutes(f.runtime.Tools()), httpapi.MemberHistoryToolRoutes(f.runtime.Tools())...)
	routes = append(routes, httpapi.MemberRoleToolRoutes(f.runtime.Tools())...)
	server, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
	roleOK(t, err)
	f.handler = server.Handler
	return f
}
func (f *roleRuntimeFixture) coordinator(t *testing.T) roleRun {
	t.Helper()
	ctx := context.Background()
	source, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: "role-topic", Content: "研究并交付文本", RecipientIDs: []string{}}, "")
	roleOK(t, err)
	admission, err := f.db.RequestWork(ctx, f.group, domain.RequestWorkRequest{TopicID: source.TopicID, MessageID: source.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "explicit", NewTask: &domain.NewTaskRequest{Title: "研究", Acceptance: "有依据"}}, "admission-"+source.ID)
	roleOK(t, err)
	coordination, err := f.db.GetTaskCoordination(ctx, f.group, admission.Task.ID)
	roleOK(t, err)
	request, err := f.db.RequestDispatch(ctx, f.group, admission.Task.ID, admission.Task.RevisionAtAdmission, coordination.Coordination.AuthorityGeneration, "dispatch", "dispatch-"+source.ID)
	roleOK(t, err)
	materialized, err := f.db.MaterializeDispatchRequest(ctx, request.ID, 1, time.Now().UTC())
	roleOK(t, err)
	if materialized.Delivery == nil {
		t.Fatal("coordinator delivery missing")
	}
	return f.claim(t, materialized.Delivery.DeliveryID)
}
func (f *roleRuntimeFixture) claim(t *testing.T, delivery string) roleRun {
	t.Helper()
	ctx := context.Background()
	state, err := f.db.ResolveCoordinationDelivery(ctx, f.group, delivery)
	roleOK(t, err)
	if state.Blocked != store.CoordinationReady {
		t.Fatalf("fixture not ready: %s", state.Blocked)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: delivery, MaxActive: f.options.RoleMaxActive, ResourceKeys: []string{"role/" + delivery}})
	roleOK(t, err)
	return roleRun{binding: platform.MemberToolRunBinding{GroupID: turn.GroupID, MemberID: turn.MemberID, ConversationID: turn.ConversationID, DeliveryID: turn.DeliveryID, ProviderID: state.ProviderID, Model: state.Model, ProviderName: provider.Pi}, provenance: state.Provenance, claim: store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}, request: provider.ExecuteRequest{RunID: turn.ID, RuntimeDir: f.root, CWD: f.root, Model: state.Model, Prompt: "trusted packet"}}
}
func (f *roleRuntimeFixture) http(t *testing.T, token, operation, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/"+operation, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}
func (f *roleRuntimeFixture) wrap(t *testing.T, run roleRun, use func(string) provider.ExecutionReceipt, factoryCalls, nativeCalls *int) provider.Executor {
	t.Helper()
	executor, err := f.runtime.WrapCoordinationRole(run.binding, run.provenance, func(path, id string, tools []string) (provider.Executor, error) {
		*factoryCalls++
		want := []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}
		if run.provenance.Kind == store.CoordinationRoleWorker {
			want = []string{"read_channel", "search_history", "read_history", "submit_result"}
		}
		if id != run.claim.TurnID || !slices.Equal(tools, want) {
			t.Fatal("noncanonical role profile/run identity")
		}
		data, err := os.ReadFile(path)
		roleOK(t, err)
		var config struct{ Endpoint, Token string }
		roleOK(t, json.Unmarshal(data, &config))
		info, err := os.Stat(path)
		roleOK(t, err)
		if info.Mode() != 0600 || config.Token == "" || config.Token == run.claim.Token {
			t.Fatal("unsafe role config")
		}
		return roleNative(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			*nativeCalls++
			return use(config.Token)
		}), nil
	})
	roleOK(t, err)
	return executor
}
func (f *roleRuntimeFixture) claims(t *testing.T, run roleRun, state domain.TurnState, count int) {
	t.Helper()
	turn, err := f.db.GetTurn(context.Background(), f.group, run.claim.TurnID)
	roleOK(t, err)
	var got int
	roleOK(t, f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", run.claim.TurnID).Scan(&got))
	if turn.State != state || turn.ClaimToken != run.claim.Token || got != count {
		t.Fatalf("wrong durable execution state: %s claims=%d", turn.State, got)
	}
}

// Snapshot actual durable rows, including BLOB payload bytes, not only counts
// or the HTTP projection. All tables belong to this test's private database.
func (f *roleRuntimeFixture) decisionSnapshot(t *testing.T) map[string][][]any {
	t.Helper()
	result := make(map[string][][]any)
	for _, table := range []string{"coordination_dispatch_requests", "coordination_dispatch_state", "coordination_decisions", "coordination_assignments", "coordination_assignment_dispatch", "coordination_assignment_attempts", "tasks", "task_attempts", "mutation_idempotency"} {
		order := "1"
		if table == "mutation_idempotency" {
			order = "operation,actor_id,group_id,idempotency_key"
		}
		rows, err := f.raw.Query("SELECT * FROM " + table + " ORDER BY " + order)
		roleOK(t, err)
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result[table] = make([][]any, 0)
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for i, value := range values {
				if data, ok := value.([]byte); ok {
					values[i] = slices.Clone(data)
				}
			}
			result[table] = append(result[table], values)
		}
		err = rows.Err()
		closeErr := rows.Close()
		roleOK(t, err)
		roleOK(t, closeErr)
	}
	return result
}

func roleNotStarted() provider.ExecutionReceipt {
	return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1, Reason: "PRIVATE-native-before-mkdir"}
}

func TestRoleRuntimeDiscoveredProviderIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "inert-pi")
	roleOK(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0700))
	// Discover only inspects supplied paths. The factory below is entirely
	// synthetic; this private executable is never run, not even for --version.
	catalog := provider.Discover(provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: path}}})
	if len(catalog) != 1 || catalog[0].ID == "" || catalog[0].ID == provider.Pi || catalog[0].Name != provider.Pi ||
		catalog[0].Path != path || catalog[0].Installation != provider.Installed || len(catalog[0].Evidence) != 1 || catalog[0].Evidence[0].Type != provider.DiscoveryProbe {
		t.Fatal("real path-only discovery did not produce an opaque Pi installation ID")
	}
	selected := catalog[0]
	f := newRoleRuntime(t)
	member, err := f.local.CreateMember(ctx, f.group, platform.MemberInput{Name: "discovered lead", Role: "untrusted", ProviderID: selected.ID, Model: "selected"}, "")
	roleOK(t, err)
	previous, err := f.db.GetCoordinator(ctx, f.group)
	roleOK(t, err)
	_, err = f.db.SetCoordinator(ctx, f.group, member.ID, previous.Revision, "discovered-binding")
	roleOK(t, err)
	run := f.coordinator(t)
	run.binding.ProviderName = selected.Name
	scope, err := f.db.BindMemberTool(ctx, run.claim)
	roleOK(t, err)
	state, err := f.db.ResolveCoordinationExecution(ctx, scope, run.provenance)
	roleOK(t, err)
	if member.ProviderID != selected.ID || run.binding.ProviderID != selected.ID || state.ProviderID != selected.ID || state.Blocked != store.CoordinationReady || state.Provenance != run.provenance {
		t.Fatal("Store member, claim and authoritative role selection disagree with discovery")
	}
	f.claims(t, run, domain.TurnStateStarting, 1)
	t.Log("real path-only Discover ID persisted and resolved exactly with ready coordinator claim")
	factories, natives := 0, 0
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, ExitCode: 17, Reason: "PRIVATE-discovered-original"}
	executor := f.wrap(t, run, func(token string) provider.ExecutionReceipt {
		first := f.http(t, token, "decide_dispatch", `{"kind":"decline","reason":"no eligible work"}`, "discovered-decline")
		replay := f.http(t, token, "decide_dispatch", `{"kind":"decline","reason":"no eligible work"}`, "discovered-decline")
		if first.Code != 201 || replay.Code != 201 || first.Body.String() != replay.Body.String() {
			t.Fatalf("discovered-provider real role HTTP failed: %d/%d", first.Code, replay.Code)
		}
		return raw
	}, &factories, &natives)
	if factories != 0 || natives != 0 {
		t.Fatal("Wrap performed execution work")
	}
	// Wrapping freezes trusted kind metadata along with the exact selected ID.
	run.binding.ProviderName = provider.Codex
	got := executor.Execute(ctx, run.request, nil)
	if got != raw || factories != 1 || natives != 1 {
		t.Fatal("discovered Pi did not reach exactly one synthetic native invocation")
	}
	diagnostic, err := f.runtime.InspectCoordinationRoleReceipt(ctx, run.claim.GroupID, run.claim.TurnID)
	roleOK(t, err)
	if diagnostic.Capture == nil || diagnostic.CaptureStatus != "captured" || diagnostic.Capture.Raw != raw || diagnostic.Capture.Aggregate != got || diagnostic.Capture.Source.ProviderID != selected.ID || !diagnostic.CaptureClaimMatchesCurrent {
		t.Fatal("diagnostic lost the original receipt or exact opaque discovered ID")
	}
	for _, bad := range []struct{ name, id, kind string }{
		{"wrong trusted kind", selected.ID, provider.Codex},
		{"prefix without trusted kind", selected.ID, ""},
		{"forged prefix without trusted kind", "pi_forged", ""},
		{"empty ID", "", provider.Pi},
	} {
		t.Run(bad.name, func(t *testing.T) {
			binding := run.binding
			binding.ProviderID, binding.ProviderName = bad.id, bad.kind
			if _, err := f.runtime.WrapCoordinationRole(binding, run.provenance, func(string, string, []string) (provider.Executor, error) { factories++; return nil, nil }); err == nil {
				t.Fatal("unsupported or missing trusted provider kind accepted")
			}
		})
	}
	if factories != 1 || natives != 1 {
		t.Fatal("rejected kind caused factory/native activity")
	}
	f.claims(t, run, domain.TurnStateStarting, 1)
}

func TestRoleRuntimeHTTPAssignSubmitAndDecline(t *testing.T) {
	f := newRoleRuntime(t)
	run := f.coordinator(t)
	factories, natives := 0, 0
	var token string
	executor := f.wrap(t, run, func(capability string) provider.ExecutionReceipt {
		token = capability
		body := fmt.Sprintf(`{"kind":"assign","worker_member_id":%q,"reason":"eligible"}`, f.worker.ID)
		first := f.http(t, token, "decide_dispatch", body, "Exact/Key:1")
		afterAssign := f.decisionSnapshot(t)
		replay := f.http(t, token, "decide_dispatch", body, "Exact/Key:1")
		if first.Code != 201 || replay.Code != 201 || first.Body.String() != replay.Body.String() {
			t.Fatalf("legal assign/replay: %d %s / %d %s", first.Code, first.Body, replay.Code, replay.Body)
		}
		beforeConflict := f.decisionSnapshot(t)
		if !reflect.DeepEqual(afterAssign, beforeConflict) {
			t.Fatal("same-key replay changed durable decision/receipt/assignment/mutation rows")
		}
		conflict := f.http(t, token, "decide_dispatch", `{"kind":"decline","reason":"different"}`, "Exact/Key:1")
		// Decide deliberately exposes no rollback marker: even a payload conflict
		// is conservatively unconfirmed at HTTP, not a new decision or safe retry.
		if conflict.Code != 500 || !strings.Contains(conflict.Body.String(), "未确认") {
			t.Fatalf("payload conflict = %d %s", conflict.Code, conflict.Body)
		}
		if !reflect.DeepEqual(beforeConflict, f.decisionSnapshot(t)) {
			t.Fatal("payload conflict changed durable decision/receipt/assignment/mutation rows")
		}
		return roleNotStarted()
	}, &factories, &natives)
	if factories != 0 || natives != 0 {
		t.Fatal("Wrap performed work")
	}
	receipt := executor.Execute(context.Background(), run.request, nil)
	if receipt != roleNotStarted() || factories != 1 || natives != 1 {
		t.Fatalf("role invocation/receipt mismatch: %s %d/%d", receipt.Outcome, factories, natives)
	}
	f.claims(t, run, domain.TurnStateStarting, 1)
	if f.http(t, token, "decide_dispatch", `{"kind":"decline","reason":"revoked"}`, "new").Code != 403 {
		t.Fatal("capability survived Revoke")
	}
	decision, err := f.db.GetDispatchDecision(context.Background(), f.group, run.provenance.RequestID)
	roleOK(t, err)
	duplicate := f.wrap(t, run, func(string) provider.ExecutionReceipt { t.Fatal("duplicate invoked"); return roleNotStarted() }, &factories, &natives)
	if duplicate.Execute(context.Background(), run.request, nil).Outcome != provider.OutcomeUnknown || factories != 1 {
		t.Fatal("Revoke-to-Apply window reopened")
	}
	if err := f.runtime.CheckCoordinationRoleCapacity(context.Background()); err == nil {
		t.Fatal("local receipt freed slot")
	}
	_, err = f.db.ApplyTurnReceipt(context.Background(), run.claim, receipt)
	roleOK(t, err)
	f.claims(t, run, domain.TurnStateNotStarted, 0)
	roleOK(t, f.runtime.CheckCoordinationRoleCapacity(context.Background()))
	assignment, err := f.db.MaterializeAssignment(context.Background(), decision.Assignment.ID, 1, time.Now().UTC())
	roleOK(t, err)
	worker := f.claim(t, assignment.Delivery.DeliveryID)
	var submissionID string
	executor = f.wrap(t, worker, func(capability string) provider.ExecutionReceipt {
		view, err := f.db.GetAssignment(context.Background(), f.group, decision.Assignment.ID)
		roleOK(t, err)
		if view.Binding == nil || view.Binding.TurnID != worker.claim.TurnID || view.Binding.AttemptID == "" {
			t.Fatal("native preceded real Attempt binding")
		}
		first := f.http(t, capability, "submit_result", `{"summary":"text evidence","artifact_versions":[]}`, "Submit/Exact:1")
		replay := f.http(t, capability, "submit_result", `{"summary":"text evidence","artifact_versions":[]}`, "Submit/Exact:1")
		if first.Code != 201 || replay.Code != 201 || first.Body.String() != replay.Body.String() {
			t.Fatalf("legal submit/replay: %d %s / %d %s", first.Code, first.Body, replay.Code, replay.Body)
		}
		var small struct {
			ID string `json:"submission_id"`
		}
		roleOK(t, json.Unmarshal(first.Body.Bytes(), &small))
		submissionID = small.ID
		if f.http(t, capability, "decide_dispatch", `{"kind":"decline","reason":"forged"}`, "wrong-role").Code != 403 {
			t.Fatal("worker gained coordinator write")
		}
		task, err := f.db.GetTask(context.Background(), f.group, run.provenance.TaskID)
		roleOK(t, err)
		if task.Status != domain.TaskStatusSubmitted {
			t.Fatal("Submit became acceptance")
		}
		_, err = f.db.ReviewTask(context.Background(), store.ReviewTaskInput{GroupID: f.group, TaskID: task.ID, SubmissionID: submissionID, ExpectedRevision: task.Revision, ActorID: f.worker.ID, Outcome: domain.ReviewOutcomeAccepted})
		if !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatal("worker gained Review authority")
		}
		return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Reason: "PRIVATE-tool-write-unconfirmed", ExitCode: 0}
	}, &factories, &natives)
	receipt = executor.Execute(context.Background(), worker.request, nil)
	_, err = f.db.ApplyTurnReceipt(context.Background(), worker.claim, receipt)
	roleOK(t, err)
	f.claims(t, worker, domain.TurnStateUnknown, 1)
	if f.runtime.CheckCoordinationRoleCapacity(context.Background()) == nil {
		t.Fatal("Task submitted released execution")
	}
	// Real reconciliation evidence is separate from task submission/Review.
	terminal := provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, Started: true, Accepted: true, NativeSessionID: "synthetic-session", DirectChildReaped: true, OutputComplete: true}
	_, err = f.db.ApplyTurnReceipt(context.Background(), worker.claim, terminal)
	roleOK(t, err)
	roleOK(t, f.runtime.CheckCoordinationRoleCapacity(context.Background()))
	task, err := f.db.GetTask(context.Background(), f.group, run.provenance.TaskID)
	roleOK(t, err)
	_, err = f.db.ReviewTask(context.Background(), store.ReviewTaskInput{GroupID: f.group, TaskID: task.ID, SubmissionID: submissionID, ExpectedRevision: task.Revision, ActorID: "user", Outcome: domain.ReviewOutcomeAccepted, Reason: "reviewed", IdempotencyKey: "review"})
	roleOK(t, err)
	declined := f.coordinator(t)
	executor = f.wrap(t, declined, func(capability string) provider.ExecutionReceipt {
		first := f.http(t, capability, "decide_dispatch", `{"kind":"decline","reason":"no more work"}`, "decline")
		replay := f.http(t, capability, "decide_dispatch", `{"kind":"decline","reason":"no more work"}`, "decline")
		if first.Code != 201 || !reflect.DeepEqual(first.Body.Bytes(), replay.Body.Bytes()) || replay.Code != 201 {
			t.Fatal("legal decline/replay failed")
		}
		return roleNotStarted()
	}, &factories, &natives)
	if executor.Execute(context.Background(), declined.request, nil) != roleNotStarted() || factories != 3 || natives != 3 {
		t.Fatal("decline invocation failed")
	}
}

func TestRoleRuntimeCancellationAndStartupGuards(t *testing.T) {
	f := newRoleRuntime(t)
	run := f.coordinator(t)
	factories, natives := 0, 0
	// Trusted Wrap captures values only; no new files, capability or factory.
	original := run
	executor := f.wrap(t, run, func(string) provider.ExecutionReceipt {
		_, err := f.db.RequestTurnCancel(context.Background(), run.claim.GroupID, run.claim.TurnID)
		roleOK(t, err)
		return roleNotStarted()
	}, &factories, &natives)
	run.provenance.RequestID = "mutated-after-wrap"
	run.binding.Model = "mutated-after-wrap"
	entries, err := os.ReadDir(f.root)
	roleOK(t, err)
	if len(entries) != 0 || factories != 0 {
		t.Fatal("Wrap was not side-effect free")
	}
	if executor.Execute(context.Background(), original.request, nil) != roleNotStarted() {
		t.Fatal("cancel_requested denied already-produced not_started evidence")
	}
	f.claims(t, original, domain.TurnStateCancelRequested, 1)
	if f.runtime.CheckCoordinationRoleCapacity(context.Background()) == nil {
		t.Fatal("cancel released fence")
	}
	_, err = f.db.ApplyTurnReceipt(context.Background(), original.claim, roleNotStarted())
	roleOK(t, err)
	roleOK(t, f.runtime.CheckCoordinationRoleCapacity(context.Background()))
	next := f.coordinator(t)
	// Stale provenance, cross-group, unsupported provider and wrong execution root
	// must never reach factory/native or use an ordinary profile.
	bad := next
	bad.provenance.RequestID = "foreign"
	denied := f.wrap(t, bad, func(string) provider.ExecutionReceipt { t.Fatal("stale role invoked"); return roleNotStarted() }, &factories, &natives)
	if denied.Execute(context.Background(), bad.request, nil).Outcome != provider.OutcomeUnknown || factories != 1 {
		t.Fatal("stale role reached factory")
	}
	bad = next
	bad.binding.GroupID = f.other
	bad.provenance.GroupID = f.other
	denied = f.wrap(t, bad, func(string) provider.ExecutionReceipt { t.Fatal("cross-group invoked"); return roleNotStarted() }, &factories, &natives)
	if denied.Execute(context.Background(), bad.request, nil).Outcome != provider.OutcomeUnknown {
		t.Fatal("cross-group escaped")
	}
	options := f.options
	options.RoleMaxActive = 0
	options.RoleExecutionRoot = ""
	disabled, err := platform.NewMemberToolRuntime(f.db, options)
	roleOK(t, err)
	if _, err = disabled.WrapCoordinationRole(next.binding, next.provenance, func(string, string, []string) (provider.Executor, error) {
		t.Fatal("disabled factory")
		return nil, nil
	}); err == nil {
		t.Fatal("roles enabled by default")
	}
	bad = next
	bad.binding.ProviderID = "codex"
	bad.binding.ProviderName = provider.Codex
	if _, err = f.runtime.WrapCoordinationRole(bad.binding, bad.provenance, func(string, string, []string) (provider.Executor, error) {
		t.Fatal("unsupported factory")
		return nil, nil
	}); err == nil {
		t.Fatal("unsupported role accepted")
	}
	if factories != 1 || natives != 1 {
		t.Fatal("guard introduced another invocation")
	}
}

func TestRoleRuntimeHTTPCommitRejectionKeepsExecutionEvidence(t *testing.T) {
	f := newRoleRuntime(t)
	run := f.coordinator(t)
	factories, natives := 0, 0
	// This is a real deferred-FK Commit rejection, not a lost-ack simulation.
	_, err := f.raw.Exec("CREATE TABLE role_runtime_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)")
	roleOK(t, err)
	_, err = f.raw.Exec("CREATE TRIGGER role_runtime_commit_fault AFTER INSERT ON coordination_decisions BEGIN INSERT INTO role_runtime_commit_guard VALUES('PRIVATE-MISSING-GROUP'); END")
	roleOK(t, err)
	receipt := f.wrap(t, run, func(token string) provider.ExecutionReceipt {
		w := f.http(t, token, "decide_dispatch", `{"kind":"decline","reason":"legal"}`, "commit-rejection")
		if w.Code < 500 || !strings.Contains(w.Body.String(), "未确认") || strings.Contains(w.Body.String(), "PRIVATE") {
			t.Fatalf("Commit rejection lost uncertainty/privacy: %d %s", w.Code, w.Body)
		}
		// A tool Commit error does not turn truthful native evidence into a tool write.
		return roleNotStarted()
	}, &factories, &natives).Execute(context.Background(), run.request, nil)
	if receipt != roleNotStarted() || natives != 1 {
		t.Fatal("tool failure forged native outcome")
	}
	var count int
	roleOK(t, f.raw.QueryRow("SELECT count(*) FROM coordination_decisions").Scan(&count))
	if count != 0 {
		t.Fatal("rejected Commit persisted decision")
	}
	f.claims(t, run, domain.TurnStateStarting, 1)
	// Reject the real execution Apply Commit as well. A completed sidecar and
	// localDone are still insufficient to free the shared fence or resources.
	_, err = f.raw.Exec("CREATE TRIGGER role_runtime_apply_commit_fault AFTER UPDATE OF receipt_json ON turns BEGIN INSERT INTO role_runtime_commit_guard VALUES('PRIVATE-MISSING-GROUP'); END")
	roleOK(t, err)
	if _, err := f.db.ApplyTurnReceipt(context.Background(), run.claim, receipt); err == nil {
		t.Fatal("Apply Commit fault unexpectedly succeeded")
	}
	f.claims(t, run, domain.TurnStateStarting, 1)
	if f.runtime.CheckCoordinationRoleCapacity(context.Background()) == nil {
		t.Fatal("rejected Apply Commit freed the fence")
	}
	diagnostic, err := f.runtime.InspectCoordinationRoleReceipt(context.Background(), run.claim.GroupID, run.claim.TurnID)
	roleOK(t, err)
	if diagnostic.Capture == nil || diagnostic.Capture.Raw != receipt || diagnostic.Current.Receipt != nil {
		t.Fatal("capture was confused with successful Store Commit")
	}
	_, err = f.raw.Exec("DROP TRIGGER role_runtime_apply_commit_fault")
	roleOK(t, err)
	_, err = f.db.ApplyTurnReceipt(context.Background(), run.claim, receipt)
	roleOK(t, err)
	f.claims(t, run, domain.TurnStateNotStarted, 0)
	roleOK(t, f.runtime.CheckCoordinationRoleCapacity(context.Background()))
}
