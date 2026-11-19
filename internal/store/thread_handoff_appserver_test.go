package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func newAppServerHandoff(t *testing.T, mode string, observe func(*http.Request)) *nativeHandoffFixture {
	t.Helper()
	f := newNativeHandoffFixture(t, true)
	executable, err := os.Executable()
	handoffOK(t, err)
	script := "#!/bin/sh\nAW_F_HANDOFF_CHILD=1 exec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run='^TestNativeThreadHandoffChild$' -- \"$@\"\n"
	handoffOK(t, os.WriteFile(f.executable, []byte(script), 0700))
	t.Setenv("AW_F_HANDOFF_MODE", mode)
	t.Setenv("AW_F_HANDOFF_RUNTIME", f.runtime)
	t.Setenv("HOME", f.root)
	t.Setenv("CODEX_HOME", f.root)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	handoffOK(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	f.endpoint = "http://" + listener.Addr().String() + "/api/v1/member-tools"
	f.configure(t, "0.166.0", true) // The new eligible intake must not borrow the old resume profile.
	handler := f.handler
	if observe != nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			observe(req)
			f.handler.ServeHTTP(w, req)
		})
	}
	server := &http.Server{Handler: handler}
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
	f := newAppServerHandoff(t, "ready", nil)
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
			f := newAppServerHandoff(t, mode, nil)
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

const (
	handoffAckTool      = "ack_thread_decision"
	handoffAckRequest   = `{"jsonrpc":"2.0","id":"same-ack","method":"tools/call","params":{"name":"ack_thread_decision","arguments":{}}}`
	handoffMCPList      = `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`
	handoffInheritedEnv = "AGENT_WORKSPACE_MCP_TOKEN_FIXTURE_INHERITED"
)

func handoffMCPRequest(t *testing.T, endpoint, authorization, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(body))
	handoffOK(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Idempotency-Key", "fixture-header-must-not-select-key")
	return req
}

func TestNativeThreadHandoffExplicitAck(t *testing.T) {
	for _, mode := range []string{"ack", "ack-unknown"} {
		t.Run(mode, func(t *testing.T) {
			var authorization atomic.Pointer[string] // Test-only observation in memory.
			f := newAppServerHandoff(t, mode, func(req *http.Request) {
				value := req.Header.Get("Authorization")
				authorization.CompareAndSwap(nil, &value)
			})
			t.Setenv(handoffInheritedEnv, "fixture-inherited-token")
			ctx := t.Context()
			prepare, err := f.preparer.EnableThreadDecisionHandoff(ctx)
			handoffOK(t, err)
			a, _ := f.seedAnswer(t)
			f.assertSavedOnly(t, a)
			if mode == "ack-unknown" {
				// Writes reach Commit, where the deferred FK makes the outcome unconfirmed.
				_, err := f.raw.Exec(`CREATE TABLE fixture_ack_parent(id INTEGER PRIMARY KEY);
				CREATE TABLE fixture_ack_sink(parent INTEGER REFERENCES fixture_ack_parent(id) DEFERRABLE INITIALLY DEFERRED);
				CREATE TRIGGER fixture_ack_fault AFTER INSERT ON decision_thread_answer_consumptions
				BEGIN INSERT INTO fixture_ack_sink VALUES(123); END;`)
				handoffOK(t, err)
			}
			f.run(t, prepare)
			turn := waitNativeHandoff(t, f.db, a.GroupID, a.ReplyMessageID)
			if turn.AcceptedAt == nil || turn.CandidateTerminal != domain.TurnStateSucceeded || turn.DeliveryID != a.WakeDeliveryID {
				t.Fatal("fixture did not persist acceptance and a native terminal on the exact typed turn")
			}
			var resources int
			handoffOK(t, f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", turn.ID).Scan(&resources))
			if mode == "ack-unknown" {
				if turn.State != domain.TurnStateUnknown || resources != 1 || turn.ReplyMessageID != "" {
					t.Fatal("native terminal overrode uncertain ACK or released the original claim")
				}
				f.assertSavedOnly(t, a)
			} else {
				if turn.State != domain.TurnStateSucceeded || resources != 0 {
					t.Fatal("typed turn with a confirmed ACK did not settle")
				}
				readback, err := store.Open(f.dbPath)
				handoffOK(t, err)
				defer readback.Close()
				view, err := readback.GetDecisionAnswerView(ctx, a.ThreadRef, a.RequestID, a.Version)
				handoffOK(t, err)
				if view.Receipt != a || view.Handling == nil || view.Handling.TurnID != turn.ID || view.Handling.AttemptID != "" || view.Handling.ConsumedAt.Before(*turn.AcceptedAt) {
					t.Fatal("no durable ACK for the exact accepted Thread turn")
				}
				var key string
				handoffOK(t, f.raw.QueryRow(`SELECT call_key FROM decision_thread_answer_consumptions WHERE answer_id=?`, a.ID).Scan(&key))
				if !strings.HasPrefix(key, "mcp-") {
					t.Fatal("ACK did not use the server-derived MCP key")
				}
			}
			bearer := authorization.Load()
			if bearer == nil || !strings.HasPrefix(*bearer, "Bearer ") {
				t.Fatal("no observed native MCP capability")
			}
			for _, frame := range []string{handoffMCPList, handoffAckRequest} {
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, handoffMCPRequest(t, f.endpoint+"/mcp", *bearer, frame))
				if !strings.Contains(w.Body.String(), "forbidden") {
					t.Fatal("replayed use of the revoked typed bearer was not denied")
				}
			}
			for _, name := range []string{"stdout.jsonl", "stderr.log"} {
				data, err := os.ReadFile(filepath.Join(f.runtime, turn.ID, name))
				handoffOK(t, err)
				if bytes.Contains(data, []byte(strings.TrimPrefix(*bearer, "Bearer "))) {
					t.Fatal("native runtime logged the bearer")
				}
			}
			entries, err := os.ReadDir(f.config)
			handoffOK(t, err)
			if len(entries) != 0 {
				t.Fatal("typed MCP should not require a credential file")
			}
		})
	}
}

// These fixtures have one native execution. Its lifecycle log is written on
// the parser goroutine, independently of the scheduler event callback.
func waitHandoffProjection(t *testing.T, runtime string, kind provider.EventKind) {
	t.Helper()
	waitHandoffFixture(t, func() bool {
		paths, _ := filepath.Glob(filepath.Join(runtime, "*", "stdout.jsonl"))
		for _, path := range paths {
			data, _ := os.ReadFile(path)
			if bytes.Contains(data, []byte(`"kind":"`+string(kind)+`"`)) {
				return true
			}
		}
		return false
	})
}

func waitHandoffFixture(t *testing.T, ready func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for the fixture handshake")
		case <-ticker.C:
		}
	}
}

type handoffGatedBody struct {
	io.Reader
	wait func()
}

func (b handoffGatedBody) Read(p []byte) (int, error) {
	b.wait()
	return b.Reader.Read(p)
}

func TestNativeThreadHandoffAckIngressAndTerminalLag(t *testing.T) {
	authorization := make(chan string, 1)
	f := newAppServerHandoff(t, "ack-window", func(req *http.Request) {
		select {
		case authorization <- req.Header.Get("Authorization"):
		default:
		}
	})
	prepare, err := f.preparer.EnableThreadDecisionHandoff(t.Context())
	handoffOK(t, err)
	a, _ := f.seedAnswer(t)
	responseBlocked, releaseResponse := make(chan struct{}), make(chan struct{})
	releaseTerminal := sync.OnceFunc(func() { close(releaseResponse) })
	defer releaseTerminal()
	f.run(t, func(ctx context.Context, p store.PendingDelivery, wake store.ThreadDecisionWake) (scheduler.PreparedThreadDecision, error) {
		plan, err := prepare(ctx, p, wake)
		if err != nil {
			return plan, err
		}
		execute := plan.Execute
		plan.Execute = func(ctx context.Context, claim store.TurnClaim, request provider.ExecuteRequest, event func(provider.Event) error) provider.ExecutionReceipt {
			return execute(ctx, claim, request, func(ev provider.Event) error {
				if ev.Kind == provider.ResponseEvent {
					close(responseBlocked) // Park the queue BEFORE it starts the terminal callback.
					select {
					case <-releaseResponse:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return event(ev)
			})
		}
		return plan, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var bearer string
	select {
	case bearer = <-authorization:
	case <-ctx.Done():
		t.Fatal("fixture did not discover the typed tool")
	}
	call := func(req *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req.WithContext(ctx))
		return w
	}
	request := func() *http.Request { return handoffMCPRequest(t, f.endpoint+"/mcp", bearer, handoffAckRequest) }
	reading, releaseBody := make(chan struct{}), make(chan struct{})
	unblockBody := sync.OnceFunc(func() { close(releaseBody) })
	defer unblockBody()
	early := request()
	early.Body = io.NopCloser(handoffGatedBody{Reader: early.Body, wait: sync.OnceFunc(func() {
		close(reading)
		<-releaseBody
	})})
	earlyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { earlyDone <- call(early) }()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal("fixture did not enter MCP before acceptance")
	}
	handoffOK(t, os.WriteFile(filepath.Join(f.runtime, "allow-acceptance"), nil, 0600))
	waitHandoffProjection(t, f.runtime, provider.AcceptedEvent)
	unblockBody()
	select {
	case w := <-earlyDone:
		if !strings.Contains(w.Body.String(), "forbidden") {
			t.Error("a request that entered before observed acceptance became eligible")
		}
	case <-ctx.Done():
		t.Fatal("pre-acceptance request did not finish")
	}
	handoffOK(t, os.WriteFile(filepath.Join(f.runtime, "allow-terminal"), nil, 0600))
	select {
	case <-responseBlocked:
	case <-ctx.Done():
		t.Fatal("fixture did not park the callback queue after durable acceptance")
	}
	waitHandoffProjection(t, f.runtime, provider.TerminalEvent)
	if w := call(request()); !strings.Contains(w.Body.String(), "forbidden") {
		t.Error("an ACK entered after native terminal but before its durable callback")
	}
	releaseTerminal()
	if turn := waitNativeHandoff(t, f.db, a.GroupID, a.ReplyMessageID); turn.State != domain.TurnStateSucceeded {
		t.Fatal("fixture did not settle after releasing the callback queue")
	}
	f.assertSavedOnly(t, a)
}

func TestNativeThreadHandoffReflectedBearerIsNotProjected(t *testing.T) {
	for _, mode := range []string{"reflect-thread-id", "reflect-turn-id"} {
		t.Run(mode, func(t *testing.T) {
			var authorization atomic.Pointer[string]
			f := newAppServerHandoff(t, mode, func(req *http.Request) {
				value := req.Header.Get("Authorization")
				authorization.CompareAndSwap(nil, &value)
			})
			prepare, err := f.preparer.EnableThreadDecisionHandoff(t.Context())
			handoffOK(t, err)
			a, _ := f.seedAnswer(t)
			f.run(t, prepare)
			turn := waitNativeHandoff(t, f.db, a.GroupID, a.ReplyMessageID)
			bearer := authorization.Load()
			if bearer == nil || !strings.HasPrefix(*bearer, "Bearer thread-ack-") {
				t.Fatal("fixture did not observe the actual typed bearer")
			}
			token := strings.TrimPrefix(*bearer, "Bearer ")
			var receipt string
			handoffOK(t, f.raw.QueryRow(`SELECT receipt_json FROM turns WHERE id=?`, turn.ID).Scan(&receipt))
			wantSession := ""
			if mode == "reflect-turn-id" {
				wantSession = "fixture-thread"
			}
			if turn.State != domain.TurnStateUnknown || turn.AcceptedAt != nil || turn.NativeSessionID != wantSession || strings.Contains(receipt, token) {
				t.Fatal("the parser trusted or persisted a bearer-bearing lifecycle ID")
			}
			for _, name := range []string{"stdout.jsonl", "stderr.log"} {
				data, err := os.ReadFile(filepath.Join(f.runtime, turn.ID, name))
				handoffOK(t, err)
				if bytes.Contains(data, []byte(token)) {
					t.Fatal("native runtime logged the reflected bearer")
				}
			}
			f.assertSavedOnly(t, a)
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
	ackMode := mode == "ack" || mode == "ack-unknown"
	runtime := os.Getenv("AW_F_HANDOFF_RUNTIME")
	waitGate := func(name string) {
		waitHandoffFixture(t, func() bool {
			_, err := os.Stat(filepath.Join(runtime, name))
			return err == nil
		})
	}
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
	serverCount := 1
	if ackMode {
		serverCount++
		config = json.RawMessage(`{"config":{"mcp_servers":{"fixture_inherited":{"command":"/not-an-allowed-server"}}},
			"origins":{"mcp_servers.fixture_inherited":{"name":{"type":"sessionFlags"},"version":"fixture"}},
			"layers":[{"name":{"type":"sessionFlags"},"version":"fixture","config":{}}]}`)
	}
	respond(read("config/read"), config)
	thread := read("thread/start")
	var selected struct {
		CWD, Model     string
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        string
		Config         struct {
			Features map[string]json.RawMessage
			Servers  map[string]struct {
				URL      string
				TokenEnv string   `json:"bearer_token_env_var"`
				Tools    []string `json:"enabled_tools"`
				Enabled  *bool
			} `json:"mcp_servers"`
		}
	}
	handoffOK(t, json.Unmarshal(thread.Params, &selected))
	cwd, err := os.Getwd()
	handoffOK(t, err)
	physical, err := filepath.EvalSymlinks(selected.CWD)
	handoffOK(t, err)
	if cwd != physical || selected.Model != handoffModel || selected.ApprovalPolicy != "never" || selected.Sandbox != "read-only" || len(selected.Config.Servers) != serverCount {
		t.Fatal("fixture received wrong native selection or unsafe profile")
	}
	threadID, turnID := "fixture-thread", "fixture-turn"
	var call func(string) string
	var boundServer string
	var enabledTools []string
	for server, settings := range selected.Config.Servers {
		if settings.Enabled == nil {
			t.Fatal("fixture: missing explicit MCP enablement")
		}
		if server == "fixture_inherited" {
			if *settings.Enabled || settings.URL != "" || settings.TokenEnv != "" || len(settings.Tools) != 0 {
				t.Fatal("fixture: inherited server retained tool authority")
			}
			continue
		}
		boundServer, enabledTools = server, settings.Tools
		token := os.Getenv(settings.TokenEnv)
		if !*settings.Enabled || call != nil || !strings.HasPrefix(settings.URL, "http://127.0.0.1:") ||
			!strings.HasPrefix(settings.TokenEnv, "AGENT_WORKSPACE_MCP_TOKEN_") || token == "" {
			t.Fatal("fixture: unbound endpoint or more than one enabled server")
		}
		call = func(body string) string {
			req := handoffMCPRequest(t, settings.URL, "Bearer "+token, body)
			client := http.Client{Timeout: 7 * time.Second}
			response, err := client.Do(req)
			handoffOK(t, err)
			defer response.Body.Close()
			data, err := io.ReadAll(io.LimitReader(response.Body, 1<<17))
			handoffOK(t, err)
			return string(data)
		}
		switch mode {
		case "reflect-thread-id":
			// Observe the exact secret via the fixture's discovery request,
			// without writing a separate credential file.
			if !strings.Contains(call(handoffMCPList), handoffAckTool) {
				t.Fatal("fixture could not discover the typed ACK before thread binding")
			}
			threadID = token
		case "reflect-turn-id":
			turnID = "reflected-" + token
		}
		respond(thread, map[string]any{"thread": map[string]any{"id": threadID, "turns": []any{}, "ephemeral": false}, "cwd": selected.CWD,
			"model": selected.Model, "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}})
		respond(read("config/read"), config)
		status := read("mcpServerStatus/list")
		tools := map[string]any{}
		if slices.Equal(settings.Tools, []string{handoffAckTool}) {
			// Discover before acceptance, as the real adapter must do.
			var listed struct {
				Result struct{ Tools []struct{ Name string } }
			}
			handoffOK(t, json.Unmarshal([]byte(call(handoffMCPList)), &listed))
			if len(listed.Result.Tools) != 1 || listed.Result.Tools[0].Name != handoffAckTool ||
				string(selected.Config.Features["goals"]) != "false" || os.Getenv(handoffInheritedEnv) != "" {
				t.Fatal("typed continuation inherited authority or failed to discover only its ACK")
			}
			if ackMode {
				_, _ = io.WriteString(os.Stderr, token) // Synthetic credential canary must be discarded.
			}
		}
		for _, name := range settings.Tools {
			tools[name] = map[string]string{"name": name}
		}
		respond(status, map[string]any{"data": []any{map[string]any{"name": server, "runtimeStatus": "connected",
			"httpOrigin": strings.TrimSuffix(settings.URL, "/api/v1/member-tools/mcp"), "authStatus": "bearerToken", "tools": tools}}})
	}
	if call == nil {
		t.Fatal("fixture: expected a bound native MCP server")
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
	if mode == "ack-window" {
		waitGate("allow-acceptance")
	}
	notify("turn/started", map[string]any{"threadId": threadID, "turn": turn("inProgress")})
	if ackMode {
		// Emitting a stdio frame is not proof that the parser has consumed it.
		waitHandoffProjection(t, runtime, provider.AcceptedEvent)
	}
	reply := "续轮文本"
	var items []any
	if !strings.Contains(prompt.Input[0].Text, "thread_decision_reply/v1") {
		inventory := call(handoffMCPList)
		if !strings.Contains(inventory, `"decision_context"`) || strings.Contains(inventory, handoffAckTool) ||
			!strings.Contains(call(handoffAckRequest), "forbidden") {
			t.Fatal("intake discovery or denial of the dedicated ACK changed")
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
			// Still the same live native intake, before the terminal notification.
			for _, attempt := range []struct{ id, args string }{
				{"same-ask", handoffProposal},
				{"new-ask", strings.Replace(handoffProposal, "样本有限", "其他样本", 1)},
				{"plain-clarify", `{"content":"要看全周吗？","intent_assessment":{"disposition":"clarify","reason":"边界未定","question":"要看全周吗？"}}`},
				{"plain-send", `{"content":"另发一条消息。"}`},
			} {
				if followup := send(attempt.id, attempt.args); !strings.Contains(followup, "forbidden") || !strings.Contains(followup, `"isError":true`) {
					t.Fatalf("send_message %q was not fenced after an uncertain proposal", attempt.id)
				}
			}
			const readChannel = `{"jsonrpc":"2.0","id":"read-after-unknown","method":"tools/call","params":{"name":"read_channel","arguments":{}}}`
			if readback := call(readChannel); !strings.Contains(readback, `"isError":false`) || !strings.Contains(readback, `\"chat_candidate\"`) {
				t.Fatal("a read on the live intake was fenced along with writes")
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
		if len(packet.Pending) != 3 || packet.Pending[1].Content != handoffQuestion || packet.Pending[2].Content != handoffAnswer ||
			!slices.Equal(enabledTools, []string{handoffAckTool}) {
			t.Fatal("typed continuation did not receive the exact context and dedicated tool")
		}
		if ackMode {
			item := map[string]any{"id": "native-ack", "type": "mcpToolCall", "server": boundServer, "tool": handoffAckTool,
				"status": "inProgress", "arguments": json.RawMessage(`{}`), "result": nil, "error": nil}
			notify("item/started", map[string]any{"threadId": threadID, "turnId": turnID, "item": item})
			result := call(handoffAckRequest) // Native acceptance is observed; its durable write may still be pending.
			if mode == "ack-unknown" {
				if !strings.Contains(result, "thread_decision_ack_unknown") || strings.Contains(result, "FOREIGN KEY") || !strings.Contains(result, `"isError":true`) {
					t.Fatal("uncertain ACK appeared certain or exposed private diagnostics")
				}
			} else if !strings.Contains(result, `\"handling\"`) || !strings.Contains(result, `"isError":false`) || call(handoffAckRequest) != result {
				t.Fatal("explicit ACK or exact-id replay failed")
			}
			if !strings.Contains(call(strings.Replace(handoffAckRequest, "same-ack", "new-id", 1)), "conflict") ||
				!strings.Contains(call(strings.Replace(handoffAckRequest, `"arguments":{}`, `"arguments":{"answer_id":"foreign"}`, 1)), `"code":-32602`) ||
				!strings.Contains(call(strings.Replace(handoffAckRequest, handoffAckTool, "read_channel", 1)), "forbidden") {
				t.Fatal("typed bearer allowed an identity change, selector, or unrelated operation")
			}
			var native struct{ Result json.RawMessage }
			handoffOK(t, json.Unmarshal([]byte(result), &native))
			item["status"], item["result"] = "completed", native.Result
			notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "item": item})
			items = append(items, item)
		}
	}
	if mode == "ack-window" {
		waitGate("allow-terminal")
	}
	answer := map[string]any{"id": "answer", "type": "agentMessage", "text": reply}
	notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "item": answer})
	terminal := turn("completed")
	terminal["items"] = append(items, answer)
	notify("turn/completed", map[string]any{"threadId": threadID, "turn": terminal})
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
