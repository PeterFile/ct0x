package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/provider"
)

func memberMCPRoleHTTP(t *testing.T, f *roleRuntimeFixture, token, id, method, params string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`, id, method, params)
	r := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.Len() > 65536 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), token) {
		t.Fatalf("unsafe role MCP response: %d %s", w.Code, w.Body)
	}
	return w
}

func memberMCPRoleText(t *testing.T, w *httptest.ResponseRecorder, failed bool) string {
	t.Helper()
	var result struct {
		Result struct {
			Content []struct{ Type, Text string } `json:"content"`
			IsError bool                          `json:"isError"`
		} `json:"result"`
	}
	roleOK(t, json.Unmarshal(w.Body.Bytes(), &result))
	if result.Result.IsError != failed || len(result.Result.Content) != 1 || result.Result.Content[0].Type != "text" {
		t.Fatalf("role outcome reclassified: %s", w.Body)
	}
	return result.Result.Content[0].Text
}

func memberMCPRoleProfile(t *testing.T, f *roleRuntimeFixture, token string, names ...string) {
	t.Helper()
	w := memberMCPRoleHTTP(t, f, token, `"profile"`, "tools/list", `{}`)
	var result struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	roleOK(t, json.Unmarshal(w.Body.Bytes(), &result))
	var got []string
	for _, tool := range result.Result.Tools {
		got = append(got, tool.Name)
	}
	if !slices.Equal(got, names) {
		t.Fatalf("wrong real role profile: %v", got)
	}
}

func memberMCPRoleMount(t *testing.T, f *roleRuntimeFixture) {
	t.Helper()
	routes := append(httpapi.MemberMCPRoutes(f.runtime.Tools()), httpapi.MemberRoleToolRoutes(f.runtime.Tools())...)
	server, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
	roleOK(t, err)
	f.handler = server.Handler
}

func TestMemberMCPRealRoleHandlersReplayAndUnknown(t *testing.T) {
	f := newRoleRuntime(t)
	memberMCPRoleMount(t, f)
	ctx := context.Background()
	run := f.coordinator(t)
	factories, natives := 0, 0
	var revoked string
	executor := f.wrap(t, run, func(token string) provider.ExecutionReceipt {
		revoked = token
		memberMCPRoleProfile(t, f, token, "list_members", "read_channel", "search_history", "read_history", "decide_dispatch")
		params := fmt.Sprintf(`{"name":"decide_dispatch","arguments":{"kind":"assign","worker_member_id":%q,"reason":"eligible"}}`, f.worker.ID)
		first := memberMCPRoleHTTP(t, f, token, `"decision"`, "tools/call", params)
		text := memberMCPRoleText(t, first, false)
		before := f.decisionSnapshot(t)
		replay := memberMCPRoleHTTP(t, f, token, `"decision"`, "tools/call", params)
		if first.Body.String() != replay.Body.String() || !reflect.DeepEqual(before, f.decisionSnapshot(t)) {
			t.Fatal("real MCP decision replay changed durable rows")
		}
		var key string
		roleOK(t, f.raw.QueryRow("SELECT idempotency_key FROM mutation_idempotency WHERE operation='decide_dispatch' AND actor_id=?", f.lead.ID).Scan(&key))
		native := f.http(t, token, "decide_dispatch", fmt.Sprintf(`{"kind":"assign","worker_member_id":%q,"reason":"eligible"}`, f.worker.ID), key)
		if native.Code != 201 || native.Body.String() != text {
			t.Fatal("MCP decision did not reuse the native handler/key namespace")
		}
		conflict := memberMCPRoleHTTP(t, f, token, `"decision"`, "tools/call", strings.Replace(params, "eligible", "changed", 1))
		unknown := memberMCPRoleText(t, conflict, true)
		// Decide's public native handler deliberately preserves an opaque conflict
		// as unconfirmed; MCP must not turn it into a known rollback or retry.
		if !strings.Contains(unknown, `"code":"internal_error"`) || !strings.Contains(unknown, "未确认") || !strings.Contains(unknown, "相同 JSON-RPC id") || !strings.Contains(unknown, "原幂等键") || !strings.Contains(unknown, "不要自动重试") || !reflect.DeepEqual(before, f.decisionSnapshot(t)) {
			t.Fatal("MCP changed the native role unknown boundary")
		}
		denied := memberMCPRoleHTTP(t, f, token, "10", "tools/call", `{"name":"submit_result","arguments":{"summary":"wrong role"}}`)
		if !strings.Contains(memberMCPRoleText(t, denied, true), `"code":"forbidden"`) {
			t.Fatal("coordinator acquired worker authority")
		}
		return roleNotStarted()
	}, &factories, &natives)
	receipt := executor.Execute(ctx, run.request, nil)
	if receipt != roleNotStarted() || factories != 1 || natives != 1 {
		t.Fatal("synthetic native role was not invoked exactly once")
	}
	operations, err := f.runtime.Tools().TransportOperations(ctx, revoked)
	if err == nil || operations != nil {
		t.Fatal("MCP retained a revoked runtime capability")
	}
	_, err = f.db.ApplyTurnReceipt(ctx, run.claim, receipt)
	roleOK(t, err)
	decision, err := f.db.GetDispatchDecision(ctx, f.group, run.provenance.RequestID)
	roleOK(t, err)
	assignment, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	roleOK(t, err)
	worker := f.claim(t, assignment.Delivery.DeliveryID)
	executor = f.wrap(t, worker, func(token string) provider.ExecutionReceipt {
		memberMCPRoleProfile(t, f, token, "read_channel", "search_history", "read_history", "submit_result")
		params := `{"name":"submit_result","arguments":{"summary":"text evidence","artifact_versions":[]}}`
		first := memberMCPRoleHTTP(t, f, token, "11", "tools/call", params)
		text := memberMCPRoleText(t, first, false)
		replay := memberMCPRoleHTTP(t, f, token, "11", "tools/call", params)
		if first.Body.String() != replay.Body.String() {
			t.Fatal("real submission did not replay")
		}
		var key string
		roleOK(t, f.raw.QueryRow("SELECT idempotency_key FROM mutation_idempotency WHERE operation='submit_task' AND actor_id=?", f.worker.ID).Scan(&key))
		if native := f.http(t, token, "submit_result", `{"summary":"text evidence","artifact_versions":[]}`, key); native.Code != 201 || native.Body.String() != text {
			t.Fatal("MCP submission did not preserve native receipt/key")
		}
		changed := memberMCPRoleHTTP(t, f, token, "11", "tools/call", strings.Replace(params, "text evidence", "changed", 1))
		if !strings.Contains(memberMCPRoleText(t, changed, true), `"code":"conflict"`) {
			t.Fatal("confirmed submission conflict became unknown or success")
		}
		wrong := memberMCPRoleHTTP(t, f, token, "12", "tools/call", `{"name":"decide_dispatch","arguments":{"kind":"decline","reason":"forged"}}`)
		if !strings.Contains(memberMCPRoleText(t, wrong, true), `"code":"forbidden"`) {
			t.Fatal("worker gained coordinator authority")
		}
		task, err := f.db.GetTask(ctx, f.group, worker.provenance.TaskID)
		roleOK(t, err)
		if task.Status != domain.TaskStatusSubmitted {
			t.Fatal("submission was treated as acceptance")
		}
		f.claims(t, worker, domain.TurnStateStarting, 1)
		return roleNotStarted()
	}, &factories, &natives)
	if executor.Execute(ctx, worker.request, nil) != roleNotStarted() || factories != 2 || natives != 2 {
		t.Fatal("MCP started or retried native execution")
	}
}

func TestMemberMCPRoleReadFailureKeepsNativeUnknown(t *testing.T) {
	f := newRoleRuntime(t)
	memberMCPRoleMount(t, f)
	run := f.coordinator(t)
	factories, natives := 0, 0
	f.wrap(t, run, func(token string) provider.ExecutionReceipt {
		before := f.decisionSnapshot(t)
		roleOK(t, f.db.Close())
		native := f.http(t, token, "decide_dispatch", `{"kind":"decline","reason":"legal"}`, "baseline")
		if native.Code != 500 || !strings.Contains(native.Body.String(), `"code":"internal_error"`) {
			t.Fatalf("fixture must preserve the native unknown read error: %d %s", native.Code, native.Body)
		}
		w := memberMCPRoleHTTP(t, f, token, "1", "tools/call", `{"name":"decide_dispatch","arguments":{"kind":"decline","reason":"legal"}}`)
		text := memberMCPRoleText(t, w, true)
		if !strings.HasPrefix(text, native.Body.String()) || !reflect.DeepEqual(before, f.decisionSnapshot(t)) {
			t.Fatal("discovery preflight replaced the native unknown outcome with an authorization rejection")
		}
		return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, ExitCode: -1}
	}, &factories, &natives).Execute(context.Background(), run.request, nil)
	if factories != 1 || natives != 1 {
		t.Fatal("read failure started or retried execution")
	}
}

func TestMemberMCPRealCommitRejectionPreservesNativeCode(t *testing.T) {
	f := newRoleRuntime(t)
	memberMCPRoleMount(t, f)
	run := f.coordinator(t)
	_, err := f.raw.Exec("CREATE TABLE member_mcp_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)")
	roleOK(t, err)
	_, err = f.raw.Exec("CREATE TRIGGER member_mcp_commit_fault AFTER INSERT ON coordination_decisions BEGIN INSERT INTO member_mcp_commit_guard VALUES('PRIVATE-MISSING'); END")
	roleOK(t, err)
	factories, natives := 0, 0
	receipt := f.wrap(t, run, func(token string) provider.ExecutionReceipt {
		before := f.decisionSnapshot(t)
		w := memberMCPRoleHTTP(t, f, token, "1", "tools/call", `{"name":"decide_dispatch","arguments":{"kind":"decline","reason":"legal"}}`)
		text := memberMCPRoleText(t, w, true)
		if !strings.Contains(text, `"code":"internal_error"`) || !strings.Contains(text, "未确认") || strings.Contains(text, "PRIVATE") || !strings.Contains(text, "不要自动重试") || !reflect.DeepEqual(before, f.decisionSnapshot(t)) {
			t.Fatal("real deferred-FK Commit failure lost native uncertainty or leaked SQL")
		}
		f.claims(t, run, domain.TurnStateStarting, 1)
		return roleNotStarted()
	}, &factories, &natives).Execute(context.Background(), run.request, nil)
	if receipt != roleNotStarted() || factories != 1 || natives != 1 {
		t.Fatal("tool Commit error changed native receipt or retried execution")
	}
}
