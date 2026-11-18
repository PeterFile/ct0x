package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// These tests use existing cmd entry points, not a proposed composition result
// type. The native fixture proves local wiring only, not real provider acceptance.
func TestCoordinationCompositionFlags(t *testing.T) {
	entry := memberToolsEntry(t)
	base := []string{"--data-dir", filepath.Join(t.TempDir(), "unused")}
	ordinary := append(append([]string{}, base...), "--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", entry)
	roles := append(append([]string{}, ordinary...), "--enable-coordination-roles")
	for _, args := range [][]string{base, ordinary} {
		implicit, err := parseConfig(args, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		explicit, err := parseConfig(append(append([]string{}, args...), "--enable-coordination-roles=false"), io.Discard)
		if err != nil || !reflect.DeepEqual(implicit, explicit) {
			t.Fatalf("role opt-in must default false without changing ordinary config: %v", err)
		}
	}
	if _, err := parseConfig(roles, io.Discard); err != nil {
		t.Fatalf("explicit roles with trusted ordinary dependencies rejected: %v", err)
	}
	for _, value := range []string{"", "off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		t.Run("accepted-thinking="+value, func(t *testing.T) {
			if _, err := parseConfig(append(append([]string{}, roles...), "--role-thinking="+value), io.Discard); err != nil {
				t.Fatalf("exact platform thinking value %q rejected: %v", value, err)
			}
		})
	}
	invalid := [][]string{
		{"--enable-coordination-roles"},
		{"--enable-execution", "--enable-coordination-roles"},
		{"--enable-member-tools", "--pi-member-tools-extension", entry, "--enable-coordination-roles"},
		{"--enable-execution", "--enable-member-tools", "--enable-coordination-roles"},
		{"--role-thinking=max"}, {"--role-thinking="},
		append(append([]string{}, ordinary...), "--role-thinking="),
		append(append([]string{}, ordinary...), "--enable-coordination-roles=false", "--role-thinking=max"),
		append(append([]string{}, ordinary...), "--enable-coordination-roles=false", "--role-thinking="),
		{"--model=fixture-model"}, {"--thinking=max"},
	}
	for _, value := range []string{" max", "max ", "MAX", "Max", "OFF", "Minimal", "LOW", "Medium", "HIGH", "Xhigh", "auto", "max\n", "\t", "--thinking=max", "high,max", "max\x00"} {
		invalid = append(invalid, append(append([]string{}, roles...), "--role-thinking="+value))
	}
	for i, args := range invalid {
		t.Run(fmt.Sprintf("rejected-%d", i), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "must-not-exist")
			args = append(append([]string{}, args...), "--data-dir", dir)
			if _, err := parseConfig(args, io.Discard); err == nil {
				t.Fatal("inconsistent role flags reached startup")
			}
			err := runWithSearch(context.Background(), args, io.Discard, provider.Search{})
			if err == nil || !chinese(err.Error()) {
				t.Fatalf("inconsistent role flags accepted or not Chinese: %v", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("rejected role flags created runtime state: %v", err)
			}
		})
	}
	var help bytes.Buffer
	if err := runWithSearch(context.Background(), []string{"--help"}, &help, provider.Search{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--enable-coordination-roles", "--role-thinking", "--enable-member-tools", "max"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("help omits %q", want)
		}
	}
}

func coordinationCompositionHTTP(t *testing.T, method, endpoint string, input any, key string, status int, result any) []byte {
	t.Helper()
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != status || !json.Valid(data) || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("%s %s: status=%d want=%d body=%s err=%v", method, request.URL.Path, response.StatusCode, status, data, err)
	}
	if status >= 400 && !chinese(string(data)) {
		t.Fatal("composition error is not Chinese JSON")
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func coordinationCompositionReadinessURL(base, group, message, delivery string) string {
	return base + "/api/v1/groups/" + group + "/messages/" + message + "/deliveries/" + delivery + "/coordination-readiness"
}

func TestCoordinationCompositionRoutesOptIn(t *testing.T) {
	for _, mode := range []string{"api-only", "ordinary-tools", "roles"} {
		t.Run(mode, func(t *testing.T) {
			f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
			args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path}
			if mode != "api-only" {
				args = memberToolsArgs(t, dir, f.path)
			}
			if mode == "roles" {
				args = append(args, "--enable-coordination-roles")
			}
			base, stop := startRun(t, args...)
			var group, foreign domain.Group
			executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "角色路由边界"}, &group)
			executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "隔离群"}, &foreign)
			var member domain.Member
			path := base + "/api/v1/groups/" + group.ID
			executionJSON(t, "POST", path+"/members", platform.MemberInput{Name: "未配置执行成员", Role: "Coordinator文字不授予角色"}, &member)
			var message domain.Message
			executionJSON(t, "POST", path+"/messages", platform.MessageInput{Content: "保持排队", RecipientIDs: []string{member.ID}}, &message)
			var policy domain.GroupDispatchPolicy
			coordinationCompositionHTTP(t, "GET", path+"/coordination/dispatch-policy", nil, "", 200, &policy)
			if policy.Revision != 0 || len(policy.AllowedWorkerIDs) != 0 {
				t.Fatal("role opt-in inferred authority from member text")
			}
			var admission domain.AdmissionReceipt
			coordinationCompositionHTTP(t, "POST", path+"/coordination/requests", domain.RequestWorkRequest{
				MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "显式工作授权",
				NewTask: &domain.NewTaskRequest{Title: "路由兼容", Acceptance: "保留已有RequestWork"},
			}, "ordinary-request-work", 201, &admission)
			dispatchStatus, roleStatus, readinessStatus := 404, 404, 404
			if mode == "roles" {
				dispatchStatus, roleStatus, readinessStatus = 400, 403, 200
			}
			coordinationCompositionHTTP(t, "POST", path+"/tasks/"+admission.Task.ID+"/dispatch-requests", map[string]any{}, "", dispatchStatus, nil)
			for _, operation := range []string{"decide_dispatch", "submit_result"} {
				code := "not_found"
				if mode == "roles" {
					code = "forbidden"
				}
				input := map[string]any{"summary": "角色权限检查"}
				if operation == "decide_dispatch" {
					input = map[string]any{"kind": "decline", "reason": "角色权限检查"}
				}
				memberToolsHTTP(t, base+"/api/v1/member-tools", "not-a-capability", operation, input, "route-"+operation, roleStatus, code)
			}
			d := message.Deliveries[0]
			var inspection platform.RoleReadinessInspection
			coordinationCompositionHTTP(t, "GET", coordinationCompositionReadinessURL(base, group.ID, message.ID, d.ID), nil, "", readinessStatus, &inspection)
			if mode == "roles" && (inspection.Role != store.CoordinationRoleOrdinary || inspection.DeliveryID != d.ID || inspection.ObservedAt.IsZero() || inspection.Blocked != nil) {
				t.Fatal("registered readiness did not inspect the actual ordinary queued delivery")
			}
			coordinationCompositionHTTP(t, "GET", coordinationCompositionReadinessURL(base, foreign.ID, message.ID, d.ID), nil, "", 404, nil)
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(f.root, "calls")); !os.IsNotExist(err) {
				t.Fatal("route registration/readiness started a native execution")
			}
		})
	}
}

type coordinationCompositionExpectation struct {
	DataDir, GroupID, CoordinatorID, WorkerID, Extension, Thinking, Mode string
}

type coordinationCompositionReport struct {
	RunID, ConfigPath, AssignmentID, SubmissionID string
}

type coordinationCompositionFixture struct {
	native       executionFixture
	dir          string
	db           *store.Store
	raw          *sql.DB
	group        domain.Group
	lead, worker domain.Member
}

func newCoordinationCompositionFixture(t *testing.T, mode, thinking string) *coordinationCompositionFixture {
	t.Helper()
	f := &coordinationCompositionFixture{native: memberToolsFixture(t), dir: t.TempDir()}
	// Reuse the existing probe/argv/subprocess launcher; only select a different
	// test-binary helper. No second bridge, provider adapter or intent pump.
	body, err := os.ReadFile(f.native.path)
	if err != nil || bytes.Count(body, []byte("^TestMemberToolsNativeHelper$")) != 1 {
		t.Fatalf("existing native launcher contract changed: %v", err)
	}
	body = bytes.Replace(body, []byte("^TestMemberToolsNativeHelper$"), []byte("^TestCoordinationCompositionNativeHelper$"), 1)
	if err := os.WriteFile(f.native.path, body, 0700); err != nil {
		t.Fatal(err)
	}
	base, stop := startRun(t, "--listen", "127.0.0.1:0", "--data-dir", f.dir, "--pi-path", f.native.path)
	selected := executionProvider(t, base)
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "受信角色装配"}, &f.group)
	for _, target := range []*domain.Member{&f.lead, &f.worker} {
		executionJSON(t, "POST", base+"/api/v1/groups/"+f.group.ID+"/members", platform.MemberInput{
			Name: "仅文字角色", Role: "--thinking off; Coordinator/Worker文字不能改选授权", ProviderID: selected.ID, Model: "fixture-model",
		}, target)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	f.db, err = store.Open(filepath.Join(f.dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.raw, err = sql.Open("sqlite", filepath.Join(f.dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.raw.Close() })
	if _, err := f.db.SetCoordinator(context.Background(), f.group.ID, f.lead.ID, 0, "coordinator"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetDispatchPolicy(context.Background(), f.group.ID, []string{f.worker.ID}, 0, "workers"); err != nil {
		t.Fatal(err)
	}
	memberToolsWriteJSON(t, filepath.Join(f.native.root, "expected.json"), coordinationCompositionExpectation{
		DataDir: f.dir, GroupID: f.group.ID, CoordinatorID: f.lead.ID, WorkerID: f.worker.ID,
		Extension: memberToolsEntry(t), Thinking: thinking, Mode: mode,
	})
	return f
}

func (f *coordinationCompositionFixture) args(t *testing.T, thinking string) []string {
	t.Helper()
	args := append(memberToolsArgs(t, f.dir, f.native.path), "--enable-coordination-roles")
	if thinking != "" {
		args = append(args, "--role-thinking", thinking)
	}
	return args
}

func coordinationCompositionAdmission(t *testing.T, db *store.Store, groupID string) domain.AdmissionReceipt {
	t.Helper()
	ctx := t.Context()
	source, err := db.CreateMessage(ctx, store.MessageRequest{GroupID: groupID, ActorID: "user", TopicID: "role-topic", Content: "明确开展文本任务", RecipientIDs: []string{}}, []string{})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := db.RequestWork(ctx, groupID, domain.RequestWorkRequest{TopicID: source.TopicID, MessageID: source.ID,
		Mode: domain.RoutingDecisionCreateNew, Reason: "显式本地授权", NewTask: &domain.NewTaskRequest{Title: "角色组合文本", Acceptance: "保存真实工具提交而非自动接纳"}}, "admit-"+source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

func (f *coordinationCompositionFixture) request(t *testing.T) domain.DispatchRequest {
	t.Helper()
	return coordinationCompositionRequest(t, f.db, f.group.ID)
}

func coordinationCompositionRequest(t *testing.T, db *store.Store, groupID string) domain.DispatchRequest {
	t.Helper()
	admission := coordinationCompositionAdmission(t, db, groupID)
	view, err := db.GetTaskCoordination(t.Context(), groupID, admission.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := db.RequestDispatch(t.Context(), groupID, admission.Task.ID, admission.Task.RevisionAtAdmission, view.Coordination.AuthorityGeneration, "fixture setup", "dispatch-"+admission.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func (f *coordinationCompositionFixture) dispatchHTTP(t *testing.T, base string) domain.DispatchRequest {
	t.Helper()
	admission := coordinationCompositionAdmission(t, f.db, f.group.ID)
	view, err := f.db.GetTaskCoordination(context.Background(), f.group.ID, admission.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := base + "/api/v1/groups/" + f.group.ID + "/tasks/" + admission.Task.ID + "/dispatch-requests"
	input := map[string]any{"expected_task_revision": admission.Task.RevisionAtAdmission, "expected_authority_generation": view.Coordination.AuthorityGeneration, "reason": "显式角色调度"}
	var request domain.DispatchRequest
	first := coordinationCompositionHTTP(t, "POST", endpoint, input, "dispatch-"+admission.Task.ID, 201, &request)
	if replay := coordinationCompositionHTTP(t, "POST", endpoint, input, "dispatch-"+admission.Task.ID, 201, nil); !bytes.Equal(first, replay) {
		t.Fatal("real dispatch route lost its immutable same-key receipt")
	}
	if request.TaskID != admission.Task.ID || request.CoordinatorMemberID != f.lead.ID || request.RequestedBy != "user" {
		t.Fatal("HTTP dispatch did not use the configured Store/Engine authority")
	}
	var saved domain.DispatchView
	coordinationCompositionHTTP(t, "GET", base+"/api/v1/groups/"+f.group.ID+"/coordination/dispatch-requests/"+request.ID, nil, "", 200, &saved)
	if saved.Request != request {
		t.Fatal("registered dispatch read route used different immutable authority")
	}
	return request
}

func coordinationCompositionCount(t *testing.T, raw *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := raw.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *coordinationCompositionFixture) await(t *testing.T, label string, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(filepath.Join(f.native.root, "failed")); err == nil {
			calls, _ := os.ReadFile(filepath.Join(f.native.root, "calls"))
			for _, id := range strings.Fields(string(calls)) {
				data, _ := os.ReadFile(filepath.Join(f.dir, "runtime", id, "stdout.jsonl"))
				if len(data) > 4096 {
					data = data[len(data)-4096:]
				}
				t.Logf("synthetic native assertion tail for %s: %s", id, data)
			}
			t.Fatalf("%s: synthetic native helper failed", label)
		}
		if ready() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			calls, _ := os.ReadFile(filepath.Join(f.native.root, "calls"))
			t.Fatalf("%s: no eventual progress within 30s; synthetic run IDs=%q, turns=%d, resource claims=%d", label, calls,
				coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM turns"), coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM resource_claims"))
		}
	}
}

func (f *coordinationCompositionFixture) report(t *testing.T, kind string) coordinationCompositionReport {
	t.Helper()
	path := filepath.Join(f.native.root, kind+".json")
	f.await(t, kind+" native report", func() bool { _, err := os.Stat(path); return err == nil })
	var report coordinationCompositionReport
	memberToolsReadJSON(t, path, &report)
	return report
}

func (f *coordinationCompositionFixture) turn(t *testing.T, run string, state domain.TurnState) domain.Turn {
	t.Helper()
	var turn domain.Turn
	f.await(t, "persist "+string(state)+" receipt", func() bool {
		var err error
		turn, err = f.db.GetTurn(context.Background(), f.group.ID, run)
		return err == nil && turn.State == state
	})
	return turn
}

func (f *coordinationCompositionFixture) release(t *testing.T, kind string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.native.root, "release-"+kind), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *coordinationCompositionFixture) ordinary(t *testing.T) domain.Message {
	t.Helper()
	// A distinct member gives this delivery an independent workspace resource.
	member, err := f.db.CreateMember(context.Background(), store.CreateMemberInput{GroupID: f.group.ID, Name: "普通执行探针", Role: "不是角色授权", ProviderID: f.lead.ProviderID, Model: "fixture-model"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := f.db.CreateMessage(context.Background(), store.MessageRequest{GroupID: f.group.ID, ActorID: "user", TopicID: "ordinary", Content: "普通六工具", RecipientIDs: []string{member.ID}}, []string{member.ID})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func (f *coordinationCompositionFixture) workerQueuedAtCapacity(t *testing.T, base string, report coordinationCompositionReport) domain.AssignmentView {
	t.Helper()
	var assignment domain.AssignmentView
	f.await(t, "assignment delivery receipt", func() bool {
		var err error
		assignment, err = f.db.GetAssignment(context.Background(), f.group.ID, report.AssignmentID)
		return err == nil && assignment.Dispatch.Delivery != nil
	})
	d := assignment.Dispatch.Delivery
	var inspection platform.RoleReadinessInspection
	coordinationCompositionHTTP(t, "GET", coordinationCompositionReadinessURL(base, f.group.ID, d.MessageID, d.DeliveryID), nil, "", 200, &inspection)
	if inspection.Role != store.CoordinationRoleWorker || inspection.AssignmentID != report.AssignmentID || inspection.RequestID != assignment.Request.ID || inspection.Blocked == nil || inspection.Blocked.Code != "role_runtime_capacity" {
		t.Fatalf("readiness did not observe the execution runtime's occupied M=1 fence: role=%s blocked=%v", inspection.Role, inspection.Blocked)
	}
	if assignment.Binding != nil || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM turns WHERE delivery_id=?", d.DeliveryID) != 0 || coordinationCompositionCount(t, f.raw, "SELECT count(DISTINCT turn_id) FROM resource_claims") != 1 {
		t.Fatal("scheduler MaxActive=1 diverged from the role runtime fence")
	}
	return assignment
}

func (f *coordinationCompositionFixture) receipt(t *testing.T, run string) provider.ExecutionReceipt {
	t.Helper()
	var data string
	if err := f.raw.QueryRow("SELECT receipt_json FROM turns WHERE id=?", run).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var receipt provider.ExecutionReceipt
	if err := json.Unmarshal([]byte(data), &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestCoordinationCompositionDisabledLeavesRoleIntentAndDeliveryQueued(t *testing.T) {
	f := newCoordinationCompositionFixture(t, "roles-disabled", "")
	pending, materialized := f.request(t), f.request(t)
	// One trusted Store setup call supplies a role delivery; the process must
	// neither prepare it as ordinary nor run its own pass while roles are off.
	setup, err := f.db.MaterializeDispatchRequest(context.Background(), materialized.ID, 1, time.Now().UTC())
	if err != nil || setup.Delivery == nil {
		t.Fatalf("role delivery setup failed: %v", err)
	}
	_, stop := startRun(t, memberToolsArgs(t, f.dir, f.native.path)...)
	f.ordinary(t)
	ordinary := f.report(t, "ordinary")
	f.turn(t, ordinary.RunID, domain.TurnStateSucceeded)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	view, err := f.db.GetDispatchRequest(context.Background(), f.group.ID, pending.ID)
	if err != nil || view.State.Status != domain.DispatchStatusPending || view.State.Generation != 1 || view.State.Delivery != nil {
		t.Fatal("disabled composition ran an intent pass")
	}
	message, err := f.db.GetMessage(context.Background(), f.group.ID, setup.Delivery.MessageID)
	if err != nil || message.Deliveries[0].State != domain.DeliveryStateQueued || message.Deliveries[0].TurnID != "" || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM turns") != 1 {
		t.Fatal("disabled role runtime fell back to ordinary execution")
	}
}

func TestCoordinationCompositionFailureDoesNotStartIntentPass(t *testing.T) {
	f := newCoordinationCompositionFixture(t, "normal", "max")
	request := f.request(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	failure := errors.New("composition startup output failure")
	for _, mode := range []string{"routes", "output"} {
		t.Run(mode, func(t *testing.T) {
			args := append(f.args(t, "max"), "--listen", address)
			var output io.Writer = executionFailWriter{failure}
			if mode == "routes" {
				args, output = append(args, "--web-dir", t.TempDir()), io.Discard
			}
			if _, err := parseConfig(args, io.Discard); err != nil {
				t.Fatalf("failure test did not reach valid role composition: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := runWithSearch(ctx, args, output, provider.Search{})
			if err == nil || errors.Is(err, context.DeadlineExceeded) || mode == "output" && !errors.Is(err, failure) {
				t.Fatalf("composition failure was not returned before serve: %v", err)
			}
			view, err := f.db.GetDispatchRequest(context.Background(), f.group.ID, request.ID)
			if err != nil || view.State.Status != domain.DispatchStatusPending || view.State.Generation != 1 || view.State.Delivery != nil || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM turns") != 0 {
				t.Fatal("intent pass or scheduler ran before composition/output succeeded")
			}
			entries, err := os.ReadDir(filepath.Join(f.dir, "member-tools"))
			if err != nil || len(entries) != 0 {
				t.Fatal("failed composition issued a native capability/config")
			}
			rebound, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatalf("failed composition retained its reserved listener: %v", err)
			}
			rebound.Close()
			lock, err := lockDataDir(f.dir)
			if err != nil {
				t.Fatalf("failed composition retained ownership: %v", err)
			}
			lock.Close()
			if _, err := os.Stat(filepath.Join(f.native.root, "calls")); !os.IsNotExist(err) {
				t.Fatal("failed composition invoked the native fixture")
			}
		})
	}
}

func TestCoordinationCompositionUnknownCancelAndShutdownHoldClaims(t *testing.T) {
	for _, mode := range []string{"unknown", "turn-cancel", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoordinationCompositionFixture(t, mode, "max")
			base, stop, _, _ := startCoordinationComposition(t, 10*time.Second, f.args(t, "max")...)
			f.dispatchHTTP(t, base)
			coordinator := f.report(t, "coordinator")
			coordinationCompositionAssignmentPrerequisite(t, f, coordinator)
			assignment := f.workerQueuedAtCapacity(t, base, coordinator)
			var before domain.Turn
			f.await(t, "durable coordinator acceptance", func() bool {
				var err error
				before, err = f.db.GetTurn(context.Background(), f.group.ID, coordinator.RunID)
				return err == nil && before.AcceptedAt != nil
			})
			ordinary := f.ordinary(t)
			switch mode {
			case "unknown":
				f.release(t, "coordinator")
			case "turn-cancel":
				if _, err := f.db.RequestTurnCancel(context.Background(), f.group.ID, coordinator.RunID); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				if err := stop(); err != nil {
					t.Fatal(err)
				}
			}
			turn := f.turn(t, coordinator.RunID, domain.TurnStateUnknown)
			if mode != "shutdown" {
				// The local native call is drained, but its unknown owner still
				// occupies the SAME runtime consulted by the HTTP Preparer.
				f.workerQueuedAtCapacity(t, base, coordinator)
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			receipt := f.receipt(t, coordinator.RunID)
			if !receipt.Accepted || !receipt.DirectChildReaped || receipt.Outcome != provider.OutcomeUnknown || turn.ClaimToken == "" || turn.ClaimToken != before.ClaimToken || mode == "turn-cancel" && turn.CancelRequestedAt == nil {
				t.Fatal("cancellation/unknown released or replaced the claim, or shutdown skipped the child/receipt drain")
			}
			for _, messageID := range []string{ordinary.ID, assignment.Dispatch.Delivery.MessageID} {
				message, err := f.db.GetMessage(context.Background(), f.group.ID, messageID)
				if err != nil || message.Deliveries[0].TurnID != "" || message.Deliveries[0].State != domain.DeliveryStateQueued {
					t.Fatal("unknown/cancel freed capacity for a role or independently resourced ordinary delivery")
				}
			}
			if coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM resource_claims WHERE turn_id=?", coordinator.RunID) != 1 || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM turns") != 1 || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM task_attempts") != 0 {
				t.Fatal("unknown role lost its resource claim or launched follow-up work")
			}
			if _, err := os.Stat(coordinator.ConfigPath); !os.IsNotExist(err) {
				t.Fatal("drained native execution left its private config behind")
			}
			lock, err := lockDataDir(f.dir)
			if err != nil {
				t.Fatalf("fully drained process retained ownership: %v", err)
			}
			lock.Close()
		})
	}
}

type coordinationCompositionOutput struct {
	mu    sync.Mutex
	body  bytes.Buffer
	lines chan string
}

func (w *coordinationCompositionOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	_, _ = w.body.Write(data)
	w.mu.Unlock()
	select {
	case w.lines <- string(data):
	default: // Diagnostic output must not block the scheduler after startup.
	}
	return len(data), nil
}

func (w *coordinationCompositionOutput) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func startCoordinationComposition(t *testing.T, wait time.Duration, args ...string) (string, func() error, *coordinationCompositionOutput, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	output := &coordinationCompositionOutput{lines: make(chan string, 1)}
	done := make(chan struct{})
	var result error
	go func() {
		result = runWithSearch(ctx, args, output, provider.Search{})
		close(done)
	}()
	stop := func() error {
		cancel()
		select {
		case <-done:
		case <-time.After(wait):
			t.Errorf("composition failed to drain within %s; complete output so far:\n%s", wait, output.text())
			// A failed deadline must not release Store owners before the join.
			<-done
		}
		return result
	}
	t.Cleanup(func() {
		defer func() { t.Logf("complete composition output:\n%s", output.text()) }()
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	select {
	case line := <-output.lines:
		if !chinese(line) || !strings.Contains(line, "群组与成员") || !strings.Contains(line, "其他功能尚未启用") || !strings.Contains(line, "CLI 目录") || !strings.Contains(line, "仅存内存") {
			t.Fatalf("startup must describe the group/member boundary: %s", line)
		}
		parsed, err := parseConfig(args, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		executionStatus := "执行已禁用，投递仅排队"
		if parsed.enableExecution {
			executionStatus = "全新单轮执行已启用，最大并发 1"
		}
		for _, want := range []string{"消息持久化", "历史与详情", "排队投递记录", executionStatus, "不支持恢复", "平台工具", "不是主机读取沙箱", "祖先必须可信"} {
			if !strings.Contains(line, want) {
				t.Fatalf("startup must state execution boundaries: missing %q: %s", want, line)
			}
		}
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "http://") {
				return field, stop, output, done
			}
		}
		t.Fatalf("startup did not report actual URL: %s", line)
	case <-done:
		t.Fatalf("composition exited before startup: %v", result)
	case <-time.After(wait):
		t.Fatal("composition did not announce startup")
	}
	return "", stop, output, done
}

func TestCoordinationCompositionReporterAndOrdinaryProgress(t *testing.T) {
	f := newCoordinationCompositionFixture(t, "normal", "max")
	requests := []domain.DispatchRequest{f.request(t), f.request(t)}
	for i, request := range requests {
		// Real, deterministic rollback faults, not BEGIN pressure or a fake pass.
		query := fmt.Sprintf(`CREATE TRIGGER composition_fault_%d BEFORE UPDATE ON coordination_dispatch_state
		 WHEN NEW.request_id='%s' BEGIN SELECT RAISE(ABORT,'composition-fault-%d'); END`, i, request.ID, i)
		if _, err := f.raw.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	f.ordinary(t)
	base, stop, output, done := startCoordinationComposition(t, 30*time.Second, f.args(t, "max")...)
	// Budget exhaustion may split these causes across callbacks. Require actual
	// observable causes and eventual work, not a fixed number of cycles/candidates.
	f.await(t, "observable materialization causes", func() bool {
		text := output.text()
		for i, request := range requests {
			if !strings.Contains(text, request.ID) || !strings.Contains(text, fmt.Sprintf("composition-fault-%d", i)) {
				return false
			}
		}
		return true
	})
	ordinary := f.report(t, "ordinary")
	f.turn(t, ordinary.RunID, domain.TurnStateSucceeded)
	coordinationCompositionHTTP(t, "GET", base+"/healthz", nil, "", 200, nil)
	select {
	case <-done:
		t.Fatalf("nonfatal intent-pass errors stopped ordinary execution/HTTP: %v", stop())
	default:
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	for _, request := range requests {
		view, err := f.db.GetDispatchRequest(context.Background(), f.group.ID, request.ID)
		if err != nil || view.State.Status != domain.DispatchStatusPending || view.State.Generation != 1 || view.State.Delivery != nil {
			t.Fatal("nontransient pass failure was swallowed as successful materialization/defer")
		}
	}
}

// The existing subprocess launcher executes only this Go-test helper. HTTP calls
// use real postclaim capabilities and real Store receipts, never model authority.
func TestCoordinationCompositionNativeHelper(t *testing.T) {
	root, runID := os.Getenv("MEMBER_TOOLS_FIXTURE_ROOT"), os.Getenv("MEMBER_TOOLS_FIXTURE_RUN")
	if root == "" {
		return
	}
	defer func() {
		if t.Failed() {
			_ = os.WriteFile(filepath.Join(root, "failed"), []byte("coordination native assertion failed"), 0600)
		}
	}()
	var expected coordinationCompositionExpectation
	memberToolsReadJSON(t, filepath.Join(root, "expected.json"), &expected)
	configPath := os.Getenv(memberToolsConfigEnv)
	if filepath.Dir(configPath) != filepath.Join(expected.DataDir, "member-tools") {
		t.Fatal("native helper did not receive the separate private ConfigRoot")
	}
	info, err := os.Lstat(configPath)
	if err != nil || info.Mode() != 0600 {
		t.Fatal("native capability config is not private")
	}
	var config memberToolsNativeConfig
	memberToolsReadJSON(t, configPath, &config)
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || len(prompt) == 0 || config.Token == "" || bytes.Contains(prompt, []byte(config.Token)) || bytes.Contains(prompt, []byte(configPath)) {
		t.Fatal("prompt missing or leaked native capability")
	}
	cwd, err := os.Getwd()
	workspaceRoot, rootErr := filepath.EvalSymlinks(filepath.Join(expected.DataDir, "workspaces"))
	if err != nil || rootErr != nil || filepath.Dir(cwd) != workspaceRoot {
		t.Fatal("role composition changed the trusted private CWD")
	}
	dsn := url.URL{Scheme: "file", Path: filepath.Join(expected.DataDir, "workspace.db"), RawQuery: "mode=ro"}
	raw, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var group, member, delivery, state, session, claim string
	if err := raw.QueryRow("SELECT group_id,member_id,delivery_id,state,native_session_id,claim_token FROM turns WHERE id=?", runID).Scan(&group, &member, &delivery, &state, &session, &claim); err != nil || group != expected.GroupID || state != "starting" || session != "" || claim == "" {
		t.Fatal("native invocation preceded its fresh durable Store claim")
	}
	kind := "ordinary"
	var requestID, assignmentID string
	if err := raw.QueryRow("SELECT request_id FROM coordination_dispatch_state WHERE delivery_id=?", delivery).Scan(&requestID); err == nil {
		kind = "coordinator"
		if member != expected.CoordinatorID {
			t.Fatal("coordinator was selected outside trusted configuration")
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if err := raw.QueryRow("SELECT assignment_id FROM coordination_assignment_dispatch WHERE delivery_id=?", delivery).Scan(&assignmentID); err == nil {
		kind = "worker"
		if member != expected.WorkerID || coordinationCompositionCount(t, raw, "SELECT count(*) FROM coordination_assignment_attempts WHERE assignment_id=? AND turn_id=?", assignmentID, runID) != 1 {
			t.Fatal("worker started without its real bound Assignment attempt")
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if coordinationCompositionCount(t, raw, "SELECT count(DISTINCT turn_id) FROM resource_claims") != 1 {
		t.Fatal("native invocations exceeded the process-wide M=1 capacity")
	}
	tools := "list_members,read_channel,send_message,create_member,search_history,read_history"
	if kind == "ordinary" && expected.Mode != "roles-disabled" {
		tools = "list_members,read_channel,send_message,search_history,read_history"
		if coordinationCompositionCount(t, raw, "SELECT count(*) FROM chat_intake_executions WHERE turn_id=? AND purpose=?", runID, domain.ChatIntakePurpose) != 1 {
			t.Fatal("intake native execution lacks a distinct durable purpose issuance")
		}
	}
	if kind == "coordinator" {
		tools = "list_members,read_channel,search_history,read_history,decide_dispatch"
	} else if kind == "worker" {
		tools = "read_channel,search_history,read_history,submit_result"
	}
	want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-builtin-tools", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-approve",
		"--session-dir", filepath.Join(expected.DataDir, "runtime", runID), "--session-id", runID, "--tools", tools, "--extension", expected.Extension, "--model", "fixture-model"}
	if kind != "ordinary" && expected.Thinking != "" {
		want = append(want, "--thinking", expected.Thinking)
	}
	args, err := os.ReadFile(filepath.Join(root, "args-"+runID))
	if err != nil || !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"), want) {
		t.Fatal("actual native request lost exact role thinking, tool profile or scheduler RuntimeDir")
	}
	encoder := json.NewEncoder(os.Stdout)
	emit := func(value any) {
		t.Helper()
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	emit(map[string]any{"type": "session", "version": 3, "id": runID, "cwd": cwd})
	emit(map[string]any{"type": "agent_start"})
	call := func(operation string, input any, key string, status int) []byte {
		code := ""
		switch status {
		case 403:
			code = "forbidden"
		case 404:
			code = "not_found"
		}
		if expected.Mode == "chat-intake" {
			return memberToolsHTTP(t, config.Endpoint, config.Token, operation, input, key, status, code, coordinationCompositionToolBudget())
		}
		return memberToolsHTTP(t, config.Endpoint, config.Token, operation, input, key, status, code)
	}
	// An issued role capability must work on the ordinary read route's registry.
	channel := call("read_channel", map[string]any{}, "", 200)
	var page struct {
		Items         []domain.Message
		ChatCandidate *domain.ChatCandidate `json:"chat_candidate"`
	}
	if json.Unmarshal(channel, &page) != nil || len(page.Items) == 0 {
		t.Fatal("native capability read a different/empty composition Store")
	}
	for _, message := range page.Items {
		if message.GroupID != expected.GroupID {
			t.Fatal("native capability crossed the server-bound group")
		}
	}
	report := coordinationCompositionReport{RunID: runID, ConfigPath: configPath}
	var operation string
	var input any
	var receipt []byte
	switch kind {
	case "ordinary":
		// Disabled routes are absent; opt-in routes still deny ordinary tokens,
		// including a configured coordinator's non-role delivery.
		status := 403
		if expected.Mode == "roles-disabled" {
			status = 404
		}
		call("decide_dispatch", map[string]any{"kind": "assign", "worker_member_id": expected.WorkerID, "reason": "forged role"}, "ordinary-decide", status)
		call("submit_result", map[string]any{"summary": "forged role"}, "ordinary-submit", status)
		if expected.Mode == "chat-intake" {
			if page.ChatCandidate == nil || page.ChatCandidate.SourceSenderID != "user" || page.ChatCandidate.SourceKind != "local_user_chat" || page.ChatCandidate.SourceID == "" || page.ChatCandidate.Purpose != domain.ChatIntakePurpose || page.ChatCandidate.Policy.Revision != 1 || !bytes.Contains(prompt, []byte("intent_assessment")) {
				t.Fatal("ordinary semantic assessment lacks a captured source/policy and prompt contract")
			}
			var sourceContent string
			for _, message := range page.Items {
				if message.ID == page.ChatCandidate.MessageID {
					sourceContent = message.Content
					break
				}
			}
			if sourceContent == "" {
				t.Fatal("captured source missing from the bound channel snapshot")
			}
			assessment := domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionTextSubmission, Title: "三条讨论摘要", Description: "仅生成群聊文本", Acceptance: "包含三条可核对结论", Reason: "模拟语义评估：用户要求落实前文约定"}
			if sourceContent == "先讨论摘要应当包含哪三条结论。" {
				assessment = domain.ChatAssessment{Disposition: "discuss", Reason: "模拟语义评估：仅讨论，没有工作请求"}
			}
			operation, input = "send_message", map[string]any{"content": "三条结论与边界已说明。", "intent_assessment": assessment}
		}
	case "coordinator":
		call("send_message", map[string]any{"content": "not granted"}, "role-send", 403)
		call("submit_result", map[string]any{"summary": "wrong role"}, "role-submit", 403)
		operation, input = "decide_dispatch", map[string]any{"kind": "assign", "worker_member_id": expected.WorkerID, "reason": "显式允许的工作成员"}
	case "worker":
		call("send_message", map[string]any{"content": "not granted"}, "role-send", 403)
		call("decide_dispatch", map[string]any{"kind": "decline", "reason": "wrong role"}, "role-decide", 403)
		operation, input = "submit_result", map[string]any{"summary": "本地角色工具提交", "artifact_versions": []any{}}
	}
	var messages []any
	if operation != "" {
		key := memberToolsKey(operation, runID)
		receipt = call(operation, input, key, 201)
		if !bytes.Equal(receipt, call(operation, input, key, 201)) {
			t.Fatal("trusted role tool receipt changed on same-key replay")
		}
		var result struct {
			AssignmentID string `json:"assignment_id"`
			SubmissionID string `json:"submission_id"`
		}
		if err := json.Unmarshal(receipt, &result); err != nil || kind == "coordinator" && result.AssignmentID == "" || kind == "worker" && result.SubmissionID == "" {
			t.Fatal("role tool returned no authoritative receipt identity")
		}
		report.AssignmentID, report.SubmissionID = result.AssignmentID, result.SubmissionID
		assistant := memberToolsAssistant([]any{map[string]any{"type": "toolCall", "id": "role-call", "name": operation, "arguments": input}}, "toolUse")
		tool := map[string]any{"role": "toolResult", "toolCallId": "role-call", "toolName": operation, "content": []any{map[string]any{"type": "text", "text": string(receipt)}}, "isError": false, "timestamp": 1}
		emit(map[string]any{"type": "message_end", "message": assistant})
		emit(map[string]any{"type": "message_end", "message": tool})
		emit(map[string]any{"type": "turn_end", "message": assistant, "toolResults": []any{tool}})
		messages = append(messages, assistant, tool)
	}
	memberToolsWriteJSON(t, filepath.Join(root, kind+".json"), report)
	if kind != "ordinary" {
		for {
			if _, err := os.Stat(filepath.Join(root, "release-"+kind)); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if expected.Mode == "unknown" && kind == "coordinator" {
		fmt.Fprintln(os.Stdout, "synthetic-damaged-native-frame")
	}
	final := memberToolsAssistant([]any{map[string]any{"type": "text", "text": "本地组合角色回复"}}, "stop")
	emit(map[string]any{"type": "message_end", "message": final})
	emit(map[string]any{"type": "turn_end", "message": final, "toolResults": []any{}})
	messages = append(messages, final)
	emit(map[string]any{"type": "agent_end", "messages": messages, "willRetry": false})
	emit(map[string]any{"type": "agent_settled", "aborted": false})
	os.Exit(0)
}
