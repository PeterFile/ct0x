package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

const assessmentProposal = `{"content":"先核对来源。","intent_assessment":{"disposition":"discuss","reason":"仅讨论。"}}`
const assessmentCallID = "exact-assessment"

func TestChatAssessmentTransactionWire(t *testing.T) {
	for _, transport := range []string{"native", "mcp"} {
		for _, tc := range []struct {
			name                    string
			commit, replay, lostAck bool
		}{
			{"lost-new-commit", true, false, true},
			{"lost-replay-commit", true, true, true},
			{"lost-scope-rollback", false, false, true},
			{"confirmed-scope-rollback", false, false, false},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				db, raw, claim, arm := store.ChatAssessmentFaultFixture(t, tc.commit, tc.lostAck)
				tools := platform.NewMemberTools(db)
				if err := tools.EnableChatIntake(coordination.New(db)); err != nil {
					t.Fatal(err)
				}
				token, err := tools.IssueChatIntake(t.Context(), claim)
				if err != nil {
					t.Fatal(err)
				}
				routes := append(httpapi.MemberToolRoutes(tools), httpapi.MemberMCPRoutes(tools)...)
				server, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
				if err != nil {
					t.Fatal(err)
				}
				key := fmt.Sprintf("%x", sha256.Sum256([]byte("send_message"+assessmentCallID)))
				call := func(body string) *httptest.ResponseRecorder {
					path := "/api/v1/member-tools/send_message"
					if transport == "mcp" {
						path = "/api/v1/member-tools/mcp"
						body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":"send_message","arguments":%s}}`, assessmentCallID, body)
					}
					r := httptest.NewRequest(http.MethodPost, "http://"+server.Addr+path, strings.NewReader(body))
					r.Header.Set("Authorization", "Bearer "+token)
					r.Header.Set("Content-Type", "application/json")
					if transport == "mcp" {
						r.Header.Set("Accept", "application/json, text/event-stream")
						r.Header.Set("MCP-Protocol-Version", "2025-06-18")
					} else {
						r.Header.Set("Idempotency-Key", key)
					}
					w := httptest.NewRecorder()
					server.Handler.ServeHTTP(w, r)
					return w
				}
				result := func(w *httptest.ResponseRecorder, status int) string {
					t.Helper()
					if transport == "native" {
						if w.Code != status {
							t.Fatalf("HTTP %d, want %d: %s", w.Code, status, w.Body)
						}
						return w.Body.String()
					}
					var rpc struct {
						ID     string
						Result struct {
							IsError bool
							Content []struct{ Text string }
						}
					}
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rpc) != nil || rpc.ID != assessmentCallID ||
						rpc.Result.IsError != (status >= 400) || len(rpc.Result.Content) != 1 {
						t.Fatalf("unexpected MCP envelope: %d %s", w.Code, w.Body)
					}
					return rpc.Result.Content[0].Text
				}
				var original string
				if tc.replay {
					original = result(call(assessmentProposal), http.StatusCreated)
				}
				disarm := arm()
				var w *httptest.ResponseRecorder
				if node, err := exec.LookPath("node"); transport == "native" && err == nil {
					w = assessmentBridgeFault(t, node, routes, token, key, tc.lostAck)
				} else {
					if transport == "native" {
						t.Log("Node not installed; checking native HTTP without the bridge")
					}
					w = call(assessmentProposal)
				}
				disarm()
				status, code := http.StatusForbidden, "forbidden"
				if tc.lostAck {
					status, code = http.StatusInternalServerError, "internal_error"
				}
				text := result(w, status)
				if !strings.Contains(text, `"code":"`+code+`"`) || strings.Contains(text, "PRIVATE-SQL") ||
					strings.Contains(text, "操作结果尚未确认") != tc.lostAck || tc.lostAck && !strings.Contains(text, "原幂等键") {
					t.Fatalf("incorrect outcome classification or guidance: %s", text)
				}

				checkState := func(want int) string {
					t.Helper()
					var assessments, keys, messages, relay int
					var notice, holder string
					err := raw.QueryRow(`SELECT (SELECT count(*) FROM chat_assessments),
						(SELECT count(*) FROM mutation_idempotency WHERE operation='create_message' AND actor_id<>'user'),
						(SELECT count(*) FROM messages), (SELECT count(*) FROM chat_discussion_events),
						coalesce((SELECT notice_message_id FROM chat_assessments), ''),
						(SELECT turn_id FROM resource_claims WHERE resource_key='assessment-resource')`).
						Scan(&assessments, &keys, &messages, &relay, &notice, &holder)
					if err != nil || assessments != want || keys != want || messages != 1+want || relay != 0 || holder != claim.TurnID {
						t.Fatalf("unexpected durable state for %d assessment(s): assessments=%d keys=%d messages=%d relay=%d claim held=%t err=%v",
							want, assessments, keys, messages, relay, holder == claim.TurnID, err)
					}
					return notice
				}
				want := 0
				if tc.commit {
					want = 1
				}
				notice := checkState(want)
				replay := result(call(assessmentProposal), http.StatusCreated)
				if original != "" && replay != original || tc.commit && !strings.Contains(replay, `"message_id":"`+notice+`"`) {
					t.Fatalf("same key and body did not return the original receipt: %s", replay)
				}
				if transport == "native" {
					changed := result(call(strings.Replace(assessmentProposal, "先核对来源。", "不同正文。", 1)), http.StatusConflict)
					if !strings.Contains(changed, `"code":"conflict"`) {
						t.Fatal("confirmed key conflict lost its classification")
					}
				}
				checkState(1)
				t.Logf("%s; %s; assessment durable before reconciliation=%t; original key and claim preserved", tc.name, code, tc.commit)
			})
		}
	}
}

// The actual ordinary bridge speaks to the faulted Go handler, not a canned
// status. No Pi session, model, or provider is started by this test.
func assessmentBridgeFault(t *testing.T, node string, routes []httpapi.Route, token, key string, unknown bool) *httptest.ResponseRecorder {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	api, err := httpapi.NewServer(server.Listener.Addr().String(), routes...)
	if err != nil {
		t.Fatal(err)
	}
	type response struct {
		key string
		w   *httptest.ResponseRecorder
	}
	requests := make(chan response, 4)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		api.Handler.ServeHTTP(rec, r)
		requests <- response{r.Header.Get("Idempotency-Key"), rec}
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
	server.Start()
	configPath := filepath.Join(t.TempDir(), "bridge.json")
	config, err := json.Marshal(map[string]any{"endpoint": server.URL + "/api/v1/member-tools", "token": token, "discussion_relay": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	bridge, err := filepath.Abs("../../extensions/pi-bridge/bridge.mjs")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";
const [bridge, path, digest, body, callId, unknown] = process.argv.slice(1);
const { openBridge } = await import(pathToFileURL(bridge));
const { invoke } = openBridge(path, digest);
const args = JSON.parse(body);
const send = id => invoke("send_message", args, id);
if (unknown === "true") {
  let original;
  await assert.rejects(send(callId), err => {
    original = err;
    return /internal_error.*结果未知.*对账/.test(err.message);
  });
  await assert.rejects(send(callId), err => err === original);
  await assert.rejects(invoke("send_message", {content: "请核对。", discussion: {kind: "invite", recipient_id: "peer"}}, "next"), /阻止后续写入/);
} else {
  await assert.rejects(send(callId), /forbidden/);
  await assert.rejects(send("next"), /forbidden/);
}
`
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "--eval", script, "--", bridge, configPath,
		fmt.Sprintf("%x", sha256.Sum256(config)), assessmentProposal, assessmentCallID, fmt.Sprint(unknown))
	cmd.Env = append(os.Environ(), "NODE_OPTIONS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("offline bridge fixture: %v\n%s", err, out)
	}
	want := 1
	if !unknown {
		want = 2
	}
	if len(requests) != want {
		t.Fatalf("bridge made %d HTTP requests, want %d", len(requests), want)
	}
	first := <-requests
	if first.key != key {
		t.Fatal("the bridge did not send the original key")
	}
	t.Logf("actual bridge: %d native HTTP request(s); follow-on write fenced=%t", want, unknown)
	return first.w
}
