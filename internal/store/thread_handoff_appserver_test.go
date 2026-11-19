package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func newAppServerHandoff(t *testing.T, mode string) *nativeHandoffFixture {
	t.Helper()
	f := newNativeHandoffFixture(t, true)
	executable, err := os.Executable()
	handoffOK(t, err)
	script := "#!/bin/sh\nAW_F_HANDOFF_CHILD=1 exec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run='^TestNativeThreadHandoffChild$' -- \"$@\"\n"
	handoffOK(t, os.WriteFile(f.executable, []byte(script), 0700))
	t.Setenv("AW_F_HANDOFF_MODE", mode)
	t.Setenv("HOME", f.root)
	t.Setenv("CODEX_HOME", f.root)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	handoffOK(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	f.endpoint = "http://" + listener.Addr().String() + "/api/v1/member-tools"
	f.configure(t, "0.166.0", true) // The new eligible intake must not borrow the old resume profile.
	server := &http.Server{Handler: f.handler}
	stopped := make(chan struct{})
	go func() { _ = server.Serve(listener); close(stopped) }()
	t.Cleanup(func() { _ = server.Close(); <-stopped })
	return f
}

func (f *nativeHandoffFixture) run(t *testing.T, typed scheduler.PrepareThreadDecisionFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- scheduler.Run(ctx, f.db, scheduler.Options{RuntimeDir: f.runtime, Recovered: true, MaxActive: 2,
			PollInterval: 15 * time.Millisecond, PrepareThreadDecision: typed}, f.preparer.Prepare)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
			t.Errorf("scheduler did not drain cleanly: %v", err)
		}
	})
}

func TestNativeThreadHandoffInboxThroughExistingScheduler(t *testing.T) {
	ctx := t.Context()
	f := newAppServerHandoff(t, "ready")
	prepare, err := f.preparer.EnableThreadDecisionHandoff(ctx)
	handoffOK(t, err)
	source := f.message(t, handoffTopic, []string{f.member.ID})
	var typed atomic.Int32
	f.run(t, func(ctx context.Context, p store.PendingDelivery, wake store.ThreadDecisionWake) (scheduler.PreparedThreadDecision, error) {
		typed.Add(1)
		return prepare(ctx, p, wake)
	})
	asked := waitNativeHandoff(t, f.db, source.GroupID, source.ID)
	if asked.State != domain.TurnStateSucceeded || typed.Load() != 0 {
		t.Fatalf("asking turn or premature continuation: %+v, typed=%d", asked, typed.Load())
	}
	h := threadDecisionHTTP(t, f.db)
	w := threadDecisionRequest(h, http.MethodGet, "/api/v1/me/pending-decisions", nil, "")
	var page struct{ Items []domain.DecisionRequest }
	handoffOK(t, json.Unmarshal(w.Body.Bytes(), &page))
	var q domain.DecisionRequest
	for _, item := range page.Items {
		if item.GroupID == source.GroupID {
			if q.ID != "" {
				t.Fatal("native replay duplicated the decision")
			}
			q = item
		}
	}
	if w.Code != 200 || q.ID == "" || q.Version != 1 || q.Chat == nil || q.Task != nil || q.TopicID != source.TopicID ||
		q.SourceMessageID != source.ID || q.Chat.SourceID != source.SourceID || q.AuthorTurnID != asked.ID || q.AuthorMemberID != f.member.ID || q.Question != handoffQuestion {
		t.Fatalf("inbox lost the original claim/source: %+v", q)
	}
	var keyCount int
	handoffOK(t, f.raw.QueryRow(`SELECT count(*) FROM mutation_idempotency WHERE operation='create_message' AND record_id=? AND idempotency_key LIKE 'mcp-%'`, q.Chat.AssessmentID).Scan(&keyCount))
	if keyCount != 1 {
		t.Fatal("MCP did not preserve the same scoped write key")
	}
	body, err := json.Marshal(struct {
		domain.ThreadRef
		domain.DecisionAnswerInput
	}{q.ThreadRef, domain.DecisionAnswerInput{Version: q.Version, Text: handoffAnswer}})
	handoffOK(t, err)
	route := "/api/v1/me/decisions/" + q.ID + "/answer"
	w = threadDecisionRequest(h, http.MethodPost, route, body, "one-inbox-answer")
	var a domain.DecisionAnswerReceipt
	handoffOK(t, json.Unmarshal(w.Body.Bytes(), &a))
	if w.Code != 200 || a.ID == "" || a.RequestID != q.ID || a.Text != handoffAnswer || a.ThreadRef != q.ThreadRef {
		t.Fatalf("local-user answer not saved: %d %s", w.Code, w.Body)
	}
	f.assertSavedOnly(t, a)
	continued := waitNativeHandoff(t, f.db, a.GroupID, a.ReplyMessageID)
	if continued.State != domain.TurnStateSucceeded || continued.ConversationID != asked.ConversationID || continued.MemberID != f.member.ID ||
		continued.DeliveryID != a.WakeDeliveryID || continued.ID == asked.ID || continued.AcceptedAt == nil || typed.Load() != 1 {
		t.Fatalf("answer did not run exactly once in the original scope: %+v, typed=%d", continued, typed.Load())
	}
	reply, err := f.db.GetMessage(ctx, a.GroupID, continued.ReplyMessageID)
	handoffOK(t, err)
	if reply.TopicID != q.TopicID || reply.Content != "续轮文本" || len(reply.Deliveries) != 0 {
		t.Fatal("did not publish only a same-thread native text response")
	}
	f.assertSavedOnly(t, a)
	if replay := threadDecisionRequest(h, http.MethodPost, route, body, "one-inbox-answer"); replay.Code != 200 || !bytes.Equal(replay.Body.Bytes(), w.Body.Bytes()) {
		t.Fatal("historical answer replay produced a second effect")
	}
	for table, want := range map[string]int{"tasks": 0, "chat_candidates": 1, "turns": 2, "resource_claims": 0} {
		var count int
		stmt := "SELECT count(*) FROM " + table + " WHERE group_id=?"
		if table == "resource_claims" {
			stmt = "SELECT count(*) FROM resource_claims WHERE turn_id IN (SELECT id FROM turns WHERE group_id=?)"
		}
		handoffOK(t, f.raw.QueryRow(stmt, a.GroupID).Scan(&count))
		if count != want {
			t.Errorf("unexpected %s: got %d want %d", table, count, want)
		}
	}
}

func TestNativeThreadHandoffUncertainWriteAndDefaultRoute(t *testing.T) {
	for _, mode := range []string{"unknown", "default-route"} {
		t.Run(mode, func(t *testing.T) {
			f := newAppServerHandoff(t, mode)
			prepare, err := f.preparer.EnableThreadDecisionHandoff(t.Context())
			handoffOK(t, err)
			var recipients []string // Same actual recipient, not directly addressed.
			if mode == "unknown" {
				recipients = []string{f.member.ID}
				// The F writes all succeed until Commit trips the deferred FK.
				_, err = f.raw.Exec(`CREATE TABLE fixture_request_parent(id INTEGER PRIMARY KEY);
				CREATE TABLE fixture_request_sink(parent INTEGER REFERENCES fixture_request_parent(id) DEFERRABLE INITIALLY DEFERRED);
				CREATE TRIGGER fixture_request_fault AFTER INSERT ON decision_request_versions
				BEGIN INSERT INTO fixture_request_sink VALUES(123); END;`)
				handoffOK(t, err)
			}
			source := f.message(t, handoffTopic, recipients)
			f.run(t, prepare)
			turn := waitNativeHandoff(t, f.db, source.GroupID, source.ID)
			want, held := domain.TurnStateSucceeded, 0
			if mode == "unknown" {
				want, held = domain.TurnStateUnknown, 1 // Even after a valid native terminal.
			}
			var resources, rows, assessments int
			handoffOK(t, f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", turn.ID).Scan(&resources))
			handoffOK(t, f.raw.QueryRow("SELECT count(*) FROM decision_requests WHERE group_id=?", source.GroupID).Scan(&rows))
			handoffOK(t, f.raw.QueryRow("SELECT count(*) FROM chat_assessments WHERE candidate_id IN (SELECT id FROM chat_candidates WHERE turn_id=?)", turn.ID).Scan(&assessments))
			if turn.State != want || turn.CandidateTerminal != domain.TurnStateSucceeded || resources != held || rows != 0 || assessments != 0 {
				t.Fatalf("%s: outcome=%s evidence=%s resources=%d requests=%d assessments=%d", mode, turn.State, turn.CandidateTerminal, resources, rows, assessments)
			}
		})
	}
}

const handoffProposal = `{"content":"要只看工作日吗？","intent_assessment":{"disposition":"clarify","reason":"边界未定","question":"要只看工作日吗？","decision_context":{"facts":"样本有限","recommendation":"先看工作日","choices":["工作日","全周"]}}}`

// This subprocess is the local Go test binary. It never consults any provider
// or account. It speaks the selected native protocol and only calls fixture MCP.
func TestNativeThreadHandoffChild(t *testing.T) {
	if os.Getenv("AW_F_HANDOFF_CHILD") != "1" {
		t.Skip("subprocess fixture")
	}
	mode := os.Getenv("AW_F_HANDOFF_MODE")
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	type frame struct {
		ID     json.RawMessage
		Method string
		Params json.RawMessage
	}
	read := func(method string) frame {
		t.Helper()
		var f frame
		handoffOK(t, decoder.Decode(&f))
		if f.Method != method {
			t.Fatalf("method=%s want %s", f.Method, method)
		}
		return f
	}
	respond := func(req frame, result any) {
		handoffOK(t, encoder.Encode(map[string]any{"id": req.ID, "result": result}))
	}
	notify := func(method string, params any) {
		handoffOK(t, encoder.Encode(map[string]any{"method": method, "params": params}))
	}
	respond(read("initialize"), map[string]string{"userAgent": "fixture", "platformFamily": "unix", "platformOs": "fixture", "codexHome": "/fixture-home"})
	read("initialized")
	config := json.RawMessage(`{"config":{"mcp_servers":{}},"origins":{},"layers":[{"name":{"type":"sessionFlags"},"version":"fixture","config":{}}]}`)
	respond(read("config/read"), config)
	thread := read("thread/start")
	var selected struct {
		CWD, Model     string
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        string
		Config         struct {
			Servers map[string]struct {
				URL      string
				TokenEnv string   `json:"bearer_token_env_var"`
				Tools    []string `json:"enabled_tools"`
			} `json:"mcp_servers"`
		}
	}
	handoffOK(t, json.Unmarshal(thread.Params, &selected))
	cwd, err := os.Getwd()
	handoffOK(t, err)
	physical, err := filepath.EvalSymlinks(selected.CWD)
	handoffOK(t, err)
	if cwd != physical || selected.Model != handoffModel || selected.ApprovalPolicy != "never" || selected.Sandbox != "read-only" || len(selected.Config.Servers) > 1 {
		t.Fatal("fixture received wrong native selection or unsafe profile")
	}
	const threadID, turnID = "fixture-thread", "fixture-turn"
	respond(thread, map[string]any{"thread": map[string]any{"id": threadID, "turns": []any{}, "ephemeral": false}, "cwd": selected.CWD,
		"model": selected.Model, "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}})
	respond(read("config/read"), config)
	var call func(string) string
	var boundServer string
	for server, settings := range selected.Config.Servers {
		boundServer = server
		if !strings.HasPrefix(settings.URL, "http://127.0.0.1:") || !strings.HasPrefix(settings.TokenEnv, "AGENT_WORKSPACE_MCP_TOKEN_") || os.Getenv(settings.TokenEnv) == "" {
			t.Fatal("fixture: unbound endpoint")
		}
		status := read("mcpServerStatus/list")
		tools := map[string]any{}
		for _, name := range settings.Tools {
			tools[name] = map[string]string{"name": name}
		}
		respond(status, map[string]any{"data": []any{map[string]any{"name": server, "runtimeStatus": "connected",
			"httpOrigin": strings.TrimSuffix(settings.URL, "/api/v1/member-tools/mcp"), "authStatus": "bearerToken", "tools": tools}}})
		call = func(body string) string {
			req, err := http.NewRequest("POST", settings.URL, strings.NewReader(body))
			handoffOK(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", "2025-06-18")
			req.Header.Set("Authorization", "Bearer "+os.Getenv(settings.TokenEnv))
			client := http.Client{Timeout: 7 * time.Second}
			response, err := client.Do(req)
			handoffOK(t, err)
			defer response.Body.Close()
			data, err := io.ReadAll(io.LimitReader(response.Body, 1<<17))
			handoffOK(t, err)
			return string(data)
		}
	}
	turn := func(status string) map[string]any {
		return map[string]any{"id": turnID, "status": status, "items": []any{}}
	}
	start := read("turn/start")
	var prompt struct{ Input []struct{ Text string } }
	handoffOK(t, json.Unmarshal(start.Params, &prompt))
	if len(prompt.Input) != 1 {
		t.Fatal("fixture: expected a single prompt")
	}
	respond(start, map[string]any{"turn": turn("inProgress")})
	notify("turn/started", map[string]any{"threadId": threadID, "turn": turn("inProgress")})
	reply := "续轮文本"
	var items []any
	if call != nil {
		if !strings.Contains(call(`{"jsonrpc":"2.0","id":"list","method":"tools/list"}`), `"decision_context"`) {
			t.Fatal("genuine scoped intake did not discover the option")
		}
		send := func(id, args string) string {
			return call(`{"jsonrpc":"2.0","id":` + strconv.Quote(id) + `,"method":"tools/call","params":{"name":"send_message","arguments":` + args + `}}`)
		}
		item := map[string]any{"id": "native-ask", "type": "mcpToolCall", "server": boundServer, "tool": "send_message",
			"status": "inProgress", "arguments": json.RawMessage(handoffProposal), "result": nil, "error": nil}
		notify("item/started", map[string]any{"threadId": threadID, "turnId": turnID, "item": item})
		result := send("same-ask", handoffProposal)
		switch mode {
		case "unknown":
			if !strings.Contains(result, "decision_request_unknown") || strings.Contains(result, "FOREIGN KEY") || strings.Contains(result, `"isError":false`) {
				t.Fatal("opaque F write failure appeared certain or exposed private diagnostics")
			}
		case "default-route":
			if !strings.Contains(result, "forbidden") || strings.Contains(result, "decision_request_unknown") {
				t.Fatal("default recipient acquired explicit request authority")
			}
		default:
			if !strings.Contains(result, `\"decision_request\"`) || !strings.Contains(result, `"isError":false`) || send("same-ask", handoffProposal) != result ||
				!strings.Contains(send("same-ask", strings.Replace(handoffProposal, "样本有限", "其他样本", 1)), "conflict") ||
				!strings.Contains(send("scope-selector", strings.Replace(handoffProposal, `"facts":`, `"group_id":"foreign","facts":`, 1)), "invalid_input") {
				t.Fatal("native projection, replay, conflict or scope isolation failed")
			}
		}
		var native struct{ Result json.RawMessage }
		handoffOK(t, json.Unmarshal([]byte(result), &native))
		item["status"], item["result"] = "completed", native.Result
		notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "item": item})
		items = append(items, item)
		reply = "提问轮文本"
	} else {
		var packet struct{ Pending []struct{ Content string } }
		handoffOK(t, json.Unmarshal([]byte(prompt.Input[0].Text), &packet))
		if !strings.Contains(prompt.Input[0].Text, "thread_decision_reply/v1") || len(packet.Pending) != 3 || packet.Pending[1].Content != handoffQuestion || packet.Pending[2].Content != handoffAnswer {
			t.Fatal("typed continuation did not receive exactly the stored question and answer")
		}
	}
	answer := map[string]any{"id": "answer", "type": "agentMessage", "text": reply}
	notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "item": answer})
	terminal := turn("completed")
	terminal["items"] = append(items, answer)
	notify("turn/completed", map[string]any{"threadId": threadID, "turn": terminal})
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
