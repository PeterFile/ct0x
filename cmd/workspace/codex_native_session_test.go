//go:build unix

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const codexIntakeFixtureModel = "fixture-model"
const codexIntakeFixtureQuote = "本轮只讨论。"
const codexIntakeFixtureThread = "synthetic-intake-thread"

func TestCodexNativeIntakeColdResume(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	cli := filepath.Join(root, "codex-fixture")
	script := `#!/bin/sh
set -eu
if [ "$*" = --version ]; then printf 'codex-cli 0.166.0\n'; exit 0; fi
case "$*" in
*' app-server --listen stdio://') ;;
*) exit 99 ;;
esac
` + "export AW_CODEX_INTAKE_FIXTURE=" + quote(root) + " HOME=" + quote(root) + " CODEX_HOME=" + quote(root) + "\n" +
		"exec " + quote(bin) + " -test.run='^TestCodexIntakeAppServerFixture$'\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--listen", "127.0.0.1:0", "--data-dir", data, "--codex-path", cli,
		"--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--role-thinking=max",
		"--enable-codex-app-server", "--enable-native-session-resume"}
	dbPath := filepath.Join(data, "workspace.db")
	var group domain.Group
	var member domain.Member
	var first, second struct {
		turn, claim, receiptJSON, refJSON string
		binding                           store.NativeRunBinding
		ref                               provider.NativeSessionReference
	}
	for n := 1; n <= 3; n++ {
		// Stop the normal command and close its SQLite handle between turns;
		// discovery on restart only queries the fake CLI's exact version.
		endpoint, stop := startRun(t, args...)
		selected := executionProvider(t, endpoint)
		if selected.Version != "0.166.0" || selected.Installation != provider.Installed || n > 1 && selected.ID != member.ProviderID {
			t.Fatalf("fixture discovery did not select its exact local CLI on startup %d", n)
		}
		if n == 1 {
			executionJSON(t, "POST", endpoint+"/api/v1/groups", platform.GroupInput{Name: "offline Codex discussion"}, &group)
			executionJSON(t, "POST", endpoint+"/api/v1/groups/"+group.ID+"/members", platform.MemberInput{
				Name: "fixture", Role: "仅讨论", ProviderID: selected.ID, Model: codexIntakeFixtureModel,
			}, &member)
		}
		var msg domain.Message
		executionJSON(t, "POST", endpoint+"/api/v1/groups/"+group.ID+"/messages", platform.MessageInput{
			Content: fmt.Sprintf("%s 第 %d 条独立本机输入", codexIntakeFixtureQuote, n), RecipientIDs: []string{member.ID},
		}, &msg)
		dsn := url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro"}
		raw, err := sql.Open("sqlite", dsn.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		var turn string
		var state domain.TurnState
		await := func(ready func() bool) {
			t.Helper()
			// Each turn crosses subprocess, SQLite and loopback I/O on the
			// same disk as other Go fixtures; do not assume a fast scheduler.
			deadline, poll := time.NewTimer(30*time.Second), time.NewTicker(25*time.Millisecond)
			defer deadline.Stop()
			defer poll.Stop()
			for {
				if failed, err := os.ReadFile(filepath.Join(root, "fixture-failure")); err == nil {
					t.Fatalf("synthetic Codex child %d failed at %s", n, failed)
				}
				if ready() {
					return
				}
				select {
				case <-poll.C:
				case <-deadline.C:
					t.Fatalf("synthetic Codex turn %d did not reach its checkpoint (last state %q)", n, state)
				}
			}
		}
		delivery := msg.Deliveries[0].ID
		await(func() bool {
			err := raw.QueryRow("SELECT id,state FROM turns WHERE delivery_id=?", delivery).Scan(&turn, &state)
			if err != nil {
				return false
			}
			if n != 3 {
				return state != domain.TurnStateStarting && state != domain.TurnStateRunning && state != domain.TurnStateCancelRequested
			}
			_, err = os.Stat(filepath.Join(root, "resume-pending"))
			return err == nil
		})
		if n == 3 {
			// The child saw thread/resume but sent no response and no turn was
			// accepted. Cancel through Store so the real scheduler cancels it.
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.RequestTurnCancel(t.Context(), group.ID, turn); err != nil {
				t.Fatal(err)
			}
			var jsonReceipt string
			var retained int
			var noRef bool
			await(func() bool {
				return raw.QueryRow(`SELECT t.receipt_json,n.native_ref_json IS NULL AND n.resumed_from_turn_id IS NULL,
				 (SELECT count(*) FROM resource_claims WHERE turn_id=t.id) FROM turns t JOIN native_run_bindings n ON n.turn_id=t.id
				 WHERE t.id=? AND t.state='unknown' AND t.cancel_requested_at IS NOT NULL`, turn).Scan(&jsonReceipt, &noRef, &retained) == nil
			})
			var receipt provider.ExecutionReceipt
			if json.Unmarshal([]byte(jsonReceipt), &receipt) != nil || receipt.Outcome != provider.OutcomeUnknown || !receipt.Started || !receipt.DirectChildReaped ||
				receipt.Accepted || receipt.NativeSessionID != "" || receipt.Terminal != "" || !noRef || retained != 1 {
				t.Fatal("cancelled pending resume adopted history, invented a terminal or released its resource")
			}
			var waiting domain.Message
			executionJSON(t, "POST", endpoint+"/api/v1/groups/"+group.ID+"/messages", platform.MessageInput{
				Content: "未对账前不得续接", RecipientIDs: []string{member.ID},
			}, &waiting)
			if _, err := db.ClaimTurn(t.Context(), store.ClaimTurnInput{GroupID: group.ID, DeliveryID: waiting.Deliveries[0].ID, MaxActive: 2}); !errors.Is(err, store.ErrTurnBusy) {
				t.Fatal("unknown resume let a new claim cross its conversation fence", err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		} else {
			got := &first
			if n == 2 {
				got = &second
			}
			var binding string
			err := raw.QueryRow(`SELECT t.id,t.claim_token,t.receipt_json,n.binding_json,n.native_ref_json FROM turns t
			 JOIN native_run_bindings n ON n.turn_id=t.id WHERE t.id=? AND t.state='succeeded'`, turn).
				Scan(&got.turn, &got.claim, &got.receiptJSON, &binding, &got.refJSON)
			var receipt provider.ExecutionReceipt
			if err != nil || json.Unmarshal([]byte(binding), &got.binding) != nil || json.Unmarshal([]byte(got.refJSON), &got.ref) != nil ||
				json.Unmarshal([]byte(got.receiptJSON), &receipt) != nil || !receipt.Accepted || !receipt.DirectChildReaped || !receipt.OutputComplete ||
				receipt.NativeSessionID != codexIntakeFixtureThread || got.ref.Codex == nil || got.ref.Codex.HistoryTurns != n ||
				got.ref.Codex.NativeTurnID != fmt.Sprintf("native-turn-%d", n) || got.binding.Purpose != domain.ChatIntakePurpose || got.binding.CandidateID == "" {
				t.Fatalf("turn %d did not persist trusted Codex success and a qualified reference: state=%s, err=%v", n, state, err)
			}
			var assessmentJSON string
			var assessment domain.ChatAssessmentReceipt
			if err := raw.QueryRow("SELECT receipt_json FROM chat_assessments WHERE candidate_id=?", got.binding.CandidateID).Scan(&assessmentJSON); err != nil ||
				json.Unmarshal([]byte(assessmentJSON), &assessment) != nil || assessment.MessageID != msg.ID || assessment.CandidateID != got.binding.CandidateID ||
				assessment.TurnID != got.turn || assessment.SourceKind != "local_user_chat" || assessment.TaskID != "" || assessment.MandateID != "" ||
				!assessment.DiscussionOnly || assessment.DiscussionOnlyExcerpt != codexIntakeFixtureQuote {
				t.Fatalf("current claim did not exercise the actual v30 discussion handler: %v", err)
			}
		}
		if n > 1 {
			var receiptJSON, refJSON string
			if err := raw.QueryRow(`SELECT t.receipt_json,n.native_ref_json FROM turns t JOIN native_run_bindings n ON n.turn_id=t.id WHERE t.id=?`, first.turn).
				Scan(&receiptJSON, &refJSON); err != nil || receiptJSON != first.receiptJSON || refJSON != first.refJSON {
				t.Fatal("cold resume rewrote its original terminal evidence")
			}
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := stop(); err != nil {
			t.Fatal(err)
		}
	}
	if first.turn == second.turn || first.claim == second.claim || first.binding.CandidateID == second.binding.CandidateID ||
		first.binding.ConversationID != second.binding.ConversationID || first.ref.Codex.NativeThreadID != second.ref.Codex.NativeThreadID ||
		first.binding.ResumedFromTurnID != "" || second.binding.ResumedFromTurnID != first.turn {
		t.Fatal("cold resume did not retain native identity with a new claim and candidate")
	}
}

// A test-binary child, never the local Codex installation or an actual model.
// It accepts only the intended wire sequence and talks only to the fixture's
// original loopback MCP handler. Its synthetic history lives under t.TempDir.
func TestCodexIntakeAppServerFixture(t *testing.T) {
	root := os.Getenv("AW_CODEX_INTAKE_FIXTURE")
	if root == "" {
		return
	}
	must := func(ok bool, phase string) {
		if !ok {
			_ = os.WriteFile(filepath.Join(root, "fixture-failure"), []byte(phase), 0600)
			os.Exit(8)
		}
	}
	var previous struct {
		History       []json.RawMessage
		Token, Server string
	}
	state := filepath.Join(root, "synthetic-history.json")
	data, err := os.ReadFile(state)
	must(errors.Is(err, os.ErrNotExist) || (err == nil && json.Unmarshal(data, &previous) == nil), "load synthetic history")
	input, output := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	type frame struct {
		ID     json.RawMessage
		Method string
		Params map[string]json.RawMessage
	}
	expect := func(method string) frame {
		var f frame
		must(input.Decode(&f) == nil && f.Method == method, "expected "+method)
		return f
	}
	reply := func(f frame, result any) {
		must(output.Encode(map[string]any{"id": f.ID, "result": result}) == nil, "reply "+f.Method)
	}
	notify := func(method string, value any) {
		must(output.Encode(map[string]any{"method": method, "params": value}) == nil, "notify "+method)
	}
	reply(expect("initialize"), map[string]string{"userAgent": "fixture", "platformFamily": "unix", "platformOs": "fixture", "codexHome": root})
	expect("initialized")
	config := json.RawMessage(`{"config":{"mcp_servers":{},"model_provider":"synthetic","model_reasoning_effort":"high"},"origins":{},"layers":[{"name":{"type":"sessionFlags"},"version":"fixture","config":{}}]}`)
	reply(expect("config/read"), config)
	n := len(previous.History) + 1
	f := frame{}
	if n == 1 {
		f = expect("thread/start")
	} else {
		f = expect("thread/resume")
		must(string(f.Params["threadId"]) == `"`+codexIntakeFixtureThread+`"` && string(f.Params["excludeTurns"]) == "true", "exact resume anchor")
		notify("warning", map[string]string{"threadId": codexIntakeFixtureThread, "message": "synthetic pre-bind warning"})
	}
	if n == 3 {
		must(os.WriteFile(filepath.Join(root, "resume-pending"), nil, 0600) == nil, "mark pending resume")
		must(input.Decode(&frame{}) == io.EOF, "cancel without another RPC or fallback")
		os.Exit(0)
	}
	cwd, err := os.Getwd()
	must(err == nil && string(f.Params["model"]) == `"`+codexIntakeFixtureModel+`"`, "configured model")
	thread := func(history any) map[string]any {
		return map[string]any{"id": codexIntakeFixtureThread, "turns": history, "ephemeral": false, "status": map[string]string{"type": "idle"},
			"cliVersion": "0.166.0", "cwd": cwd, "modelProvider": "synthetic", "source": "vscode", "historyMode": "legacy"}
	}
	var conf struct {
		Servers map[string]struct {
			URL      string
			Enabled  bool
			TokenEnv string   `json:"bearer_token_env_var"`
			Tools    []string `json:"enabled_tools"`
		} `json:"mcp_servers"`
	}
	must(json.Unmarshal(f.Params["config"], &conf) == nil && len(conf.Servers) == 1, "one platform MCP")
	var server, endpoint, token string
	want := []string{"list_members", "read_channel", "read_history", "search_history", "send_message"}
	for name, entry := range conf.Servers {
		server, endpoint, token = name, entry.URL, os.Getenv(entry.TokenEnv)
		slices.Sort(entry.Tools)
		must(entry.Enabled && slices.Equal(entry.Tools, want) && server != previous.Server && strings.HasPrefix(entry.TokenEnv, "AGENT_WORKSPACE_MCP_TOKEN_") && token != "" && token != previous.Token, "exact issued tools and rotated capability")
	}
	u, err := url.Parse(endpoint)
	must(err == nil && u.Scheme == "http" && u.Hostname() == "127.0.0.1" && u.Port() != "" && u.Path == "/api/v1/member-tools/mcp" && u.User == nil,
		"fixture loopback only")
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	rpc := func(bearer string, id int, method string, params any, status int) json.RawMessage {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		must(err == nil, "construct MCP request")
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		r, err := client.Do(req)
		must(err == nil && r.StatusCode == status, "MCP "+method+" or revoked token status")
		defer r.Body.Close()
		var envelope struct{ Result json.RawMessage }
		must(json.NewDecoder(r.Body).Decode(&envelope) == nil && (status != 200 || len(envelope.Result) != 0), "MCP result")
		return envelope.Result
	}
	reply(f, map[string]any{"thread": thread([]any{}), "approvalPolicy": "never", "approvalsReviewer": "user", "cwd": cwd, "model": codexIntakeFixtureModel,
		"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "modelProvider": "synthetic", "reasoningEffort": "high"})
	reply(expect("config/read"), config)
	status := expect("mcpServerStatus/list")
	if n == 2 {
		rpc(previous.Token, 1, "tools/list", map[string]any{}, http.StatusForbidden)
	}
	list := rpc(token, 1, "tools/list", map[string]any{}, http.StatusOK)
	var listed struct{ Tools []json.RawMessage }
	must(json.Unmarshal(list, &listed) == nil && len(listed.Tools) == len(want), "actual MCP inventory")
	// Pin the v30 declarations behind the profile marker, not just the names.
	// Changes here require a distinct compatibility version or a fresh path.
	declarationHash := fmt.Sprintf("%x", sha256.Sum256(list))
	must(declarationHash == "e7ad68f868e8889a17e98fa123c3202aab2e6bc50db5406f16cbf02cb443c9fc", "v30 MCP declaration hash: "+declarationHash)
	tools := make(map[string]json.RawMessage, len(listed.Tools))
	for _, tool := range listed.Tools {
		var decl struct{ Name string }
		must(json.Unmarshal(tool, &decl) == nil && slices.Contains(want, decl.Name), "issued MCP declaration name")
		tools[decl.Name] = tool
	}
	reply(status, map[string]any{"data": []any{map[string]any{"name": server, "runtimeStatus": "connected", "authStatus": "bearerToken",
		"httpOrigin": "http://" + u.Host, "tools": tools}}, "nextCursor": nil})
	if n == 2 {
		reply(expect("thread/read"), map[string]any{"thread": thread(previous.History)})
	}
	start := expect("turn/start")
	must(string(start.Params["effort"]) == `"high"` && string(start.Params["model"]) == `"`+codexIntakeFixtureModel+`"` &&
		strings.Contains(string(start.Params["input"]), codexIntakeFixtureQuote), "new turn uses the current packet and Codex effort, not Pi thinking")
	turnID := fmt.Sprintf("native-turn-%d", n)
	turn := func(status string, items []any) map[string]any {
		return map[string]any{"id": turnID, "status": status, "items": items, "error": nil}
	}
	reply(start, map[string]any{"turn": turn("inProgress", []any{})})
	notify("turn/started", map[string]any{"threadId": codexIntakeFixtureThread, "turn": turn("inProgress", []any{})})
	item := func(method string, value any) {
		notify(method, map[string]any{"threadId": codexIntakeFixtureThread, "turnId": turnID, "item": value})
	}
	args := map[string]any{"content": "合成讨论评估", "intent_assessment": map[string]any{"disposition": "discuss", "reason": "尊重范围", "discussion_only_excerpt": codexIntakeFixtureQuote}}
	call := map[string]any{"id": "mcp-" + turnID, "type": "mcpToolCall", "server": server, "tool": "send_message", "status": "inProgress", "arguments": args, "result": nil, "error": nil}
	item("item/started", call)
	result := rpc(token, 2, "tools/call", map[string]any{"name": "send_message", "arguments": args}, http.StatusOK)
	var assessment struct {
		Content json.RawMessage
		IsError bool
	}
	must(json.Unmarshal(result, &assessment) == nil && !assessment.IsError && len(assessment.Content) != 0, "discussion assessment succeeded")
	call["status"], call["result"] = "completed", map[string]any{"content": assessment.Content}
	item("item/completed", call)
	answer := map[string]any{"id": "answer-" + turnID, "type": "agentMessage", "text": "合成回复", "phase": "final_answer"}
	item("item/completed", answer)
	user := map[string]any{"id": "user-" + turnID, "type": "userMessage", "content": start.Params["input"]}
	terminal := turn("completed", []any{user, call, answer})
	notify("turn/completed", map[string]any{"threadId": codexIntakeFixtureThread, "turn": terminal})
	encoded, err := json.Marshal(terminal)
	must(err == nil, "synthetic terminal history")
	previous.History, previous.Token, previous.Server = append(previous.History, encoded), token, server
	data, err = json.Marshal(previous)
	must(err == nil && os.WriteFile(state, data, 0600) == nil, "persist synthetic native history")
	reply(expect("thread/read"), map[string]any{"thread": thread(previous.History)})
	must(input.Decode(&frame{}) == io.EOF, "no extra input")
	os.Exit(0)
}
