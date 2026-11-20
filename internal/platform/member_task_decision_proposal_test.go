package platform_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

const decisionProofHeader = "X-Agent-Workspace-Task-Decision-Proof"
const decisionCallHeader = "X-Agent-Workspace-Tool-Call-Id"
const decisionOp = "propose_task_decision"
const decisionQuestion = `{"kind":"authorization","expected_version":0,"question":"是否纳入周末样本？","facts":"只验证了工作日。 🧪","recommendation":"先交付工作日。","choices":["仅工作日","加入周末"],"authorization":{"scope":"只读样本","impact":"增加两天"}}`

// F's schema fragments are not in the public registry at v30. Expand the
// actual Go string constants (the answer DDL references shared predicates),
// rather than copying SQL, stubbing a table or assigning a migration slot.
func installTaskDecisionSchemaFixture(t *testing.T, f *roleRuntimeFixture, schema string) {
	t.Helper()
	files, err := filepath.Glob("../store/*.go")
	roleOK(t, err)
	constants := make(map[string]ast.Expr)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		roleOK(t, err)
		for _, decl := range file.Decls {
			if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.CONST {
				for _, spec := range d.Specs {
					v := spec.(*ast.ValueSpec)
					if len(v.Names) == len(v.Values) {
						for i, name := range v.Names {
							constants[name.Name] = v.Values[i]
						}
					}
				}
			}
		}
	}
	var expand func(ast.Expr) string
	expand = func(expr ast.Expr) string {
		switch e := expr.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				text, err := strconv.Unquote(e.Value)
				roleOK(t, err)
				return text
			}
		case *ast.Ident:
			if ref := constants[e.Name]; ref != nil {
				return expand(ref)
			}
			t.Fatalf("missing schema constant %s", e.Name)
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				return expand(e.X) + expand(e.Y)
			}
		}
		t.Fatalf("unsupported expression %T in %s", expr, schema)
		return ""
	}
	ddl := expand(constants[schema])
	tx, err := f.raw.Begin()
	roleOK(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(ddl)
	roleOK(t, err)
	roleOK(t, tx.Commit())
}

func installTaskDecisionProposalFixture(t *testing.T, f *roleRuntimeFixture, engine *coordination.Engine) {
	t.Helper()
	if _, err := engine.ListPendingDecisionRequests(t.Context(), store.ListOptions{Limit: 1}); errors.Is(err, store.ErrDecisionRequestsUnavailable) {
		installTaskDecisionSchemaFixture(t, f, "decisionRequestSchema")
	} else {
		roleOK(t, err)
	}
	if _, err := engine.ListPendingTaskDecisions(t.Context(), store.ListOptions{Limit: 1}); errors.Is(err, store.ErrDecisionAnswersUnavailable) {
		installTaskDecisionSchemaFixture(t, f, "decisionAnswerSchema")
	} else {
		roleOK(t, err)
	}
}

func TestPiTaskDecisionProposalNeedsExplicitExactLaunchAndSchema(t *testing.T) {
	f := newTaskWorkRoleRuntime(t)
	engine := coordination.New(f.db)
	f.options.ChatIntake = engine
	f.options.EnablePiTaskDecisionProposals = true
	weak := &platform.FreshTaskWorkLaunch{
		Supports: func(provider.Provider) bool { return true },
		Construct: func(context.Context, provider.Provider, provider.FreshTaskWorkBinding) (provider.Executor, error) {
			t.Fatal("weak preflight must not construct a proposal executor")
			return nil, nil
		},
	}
	entry, err := filepath.Abs("../../extensions/pi-bridge/fresh-task-work.ts")
	roleOK(t, err)
	strong := platform.NewPiFreshTaskWorkLaunch(entry, 0)
	f.options.FreshTaskWork = strong
	t.Run("v30-opt-out", func(t *testing.T) {
		if _, err := engine.ListPendingTaskDecisions(t.Context(), store.ListOptions{Limit: 1}); !errors.Is(err, store.ErrDecisionRequestsUnavailable) {
			roleOK(t, err)
			t.Skip("F's schemas are now in the public registry")
		}
		if runtime, err := platform.NewMemberToolRuntime(f.db, f.options); runtime != nil || !errors.Is(err, store.ErrDecisionRequestsUnavailable) {
			t.Fatalf("v30 incorrectly enabled proposals: %v", err)
		}
		off := f.options
		off.EnablePiTaskDecisionProposals = false
		runtime, err := platform.NewMemberToolRuntime(f.db, off)
		roleOK(t, err)
		defer runtime.Close()
		if runtime.Tools().TaskDecisionProposalsEnabled() || !runtime.Tools().FreshTaskWorkEnabled() {
			t.Fatal("v30 did not retain ordinary fresh Task Work without proposals")
		}
		for _, route := range httpapi.MemberRoleToolRoutes(runtime.Tools()) {
			if strings.HasSuffix(route.Pattern, "/"+decisionOp) {
				t.Fatal("proposal mounted by default")
			}
		}
	})
	if _, err := engine.ListPendingDecisionRequests(t.Context(), store.ListOptions{Limit: 1}); errors.Is(err, store.ErrDecisionRequestsUnavailable) {
		installTaskDecisionSchemaFixture(t, f, "decisionRequestSchema")
	} else {
		roleOK(t, err)
	}
	t.Run("request-only", func(t *testing.T) {
		_, err := engine.ListPendingDecisionRequests(t.Context(), store.ListOptions{Limit: 1})
		roleOK(t, err) // The old startup check alone would enable the proposal.
		if _, err := engine.ListPendingTaskDecisions(t.Context(), store.ListOptions{Limit: 1}); !errors.Is(err, store.ErrDecisionAnswersUnavailable) {
			roleOK(t, err)
			t.Skip("F's answer schema is now in the public registry")
		}
		if runtime, err := platform.NewMemberToolRuntime(f.db, f.options); runtime != nil || !errors.Is(err, store.ErrDecisionAnswersUnavailable) {
			t.Fatalf("request-only startup did not fail closed: %v", err)
		}
	})
	t.Run("request-and-answer", func(t *testing.T) {
		installTaskDecisionProposalFixture(t, f, engine)
		_, err := engine.ListPendingTaskDecisions(t.Context(), store.ListOptions{Limit: 1})
		roleOK(t, err)
		unqualified := f.options
		unqualified.FreshTaskWork = weak
		if runtime, err := platform.NewMemberToolRuntime(f.db, unqualified); runtime != nil || err == nil {
			t.Fatal("unqualified builder enabled proposals even with F's schemas")
		}
		runtime, err := platform.NewMemberToolRuntime(f.db, f.options)
		roleOK(t, err)
		defer runtime.Close()
		if !runtime.Tools().TaskDecisionProposalsEnabled() || !runtime.Tools().FreshTaskWorkEnabled() {
			t.Fatal("exact opt-in did not enable proposals with the public pending/answer path installed")
		}
		for _, route := range httpapi.MemberRoleToolRoutes(runtime.Tools()) {
			if strings.HasSuffix(route.Pattern, "/"+decisionOp) {
				return
			}
		}
		t.Fatal("proposal route not mounted")
	})
}

func TestPiTaskDecisionProposalExactAcceptedContinuation(t *testing.T) {
	for _, mode := range []string{"confirmed-two-turns", "stale-replay", "unproven-commit"} {
		t.Run(mode, func(t *testing.T) { testPiTaskDecisionProposal(t, mode) })
	}
}

func testPiTaskDecisionProposal(t *testing.T, mode string) {
	f := newTaskWorkRoleRuntime(t)
	f.options.RoleMaxActive = 2
	engine := coordination.New(f.db)
	installTaskDecisionProposalFixture(t, f, engine)
	f.options.ChatIntake, f.options.EnablePiTaskDecisionProposals = engine, true
	entry, err := filepath.Abs("../../extensions/pi-bridge/fresh-task-work.ts")
	roleOK(t, err)
	const runTimeout = 45 * time.Second
	f.options.FreshTaskWork = platform.NewPiFreshTaskWorkLaunch(entry, runTimeout)
	rt, err := platform.NewMemberToolRuntime(f.db, f.options)
	roleOK(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	tools := rt.Tools()
	routes := append(httpapi.MemberRoleToolRoutes(tools), httpapi.MemberMCPRoutes(tools)...)
	srv, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
	roleOK(t, err)
	engine.EnableTaskWorkExecution()
	ctx := t.Context()

	root := t.TempDir()
	cli, ordinary, workspaces := filepath.Join(root, "fake-pi"), filepath.Join(root, "index.ts"), filepath.Join(root, "workspaces")
	roleOK(t, os.Mkdir(workspaces, 0700))
	roleOK(t, os.WriteFile(ordinary, nil, 0600))
	// Controlled local child: only native lifecycle, no tools, model or personal
	// settings. The Go test exercises HTTP at synchronized acceptance boundaries.
	roleOK(t, os.WriteFile(cli, []byte(`#!/bin/sh
set -eu
while [ "$1" != "--session-id" ]; do shift; done
printf '{"type":"session","version":3,"id":"%s","cwd":"%s"}\n' "$2" "$PWD"
printf '%s\n' '{"type":"agent_start"}' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"done"}]}}' '{"type":"agent_settled","aborted":false}'
`), 0700))
	selected := provider.Provider{ID: f.worker.ProviderID, Name: provider.Pi, Path: cli, Version: "1.4.0", Installation: provider.Installed}
	prep, err := platform.NewPreparer(f.db, platform.PrepareOptions{Providers: []provider.Provider{selected}, PrivateWorkspaceRoot: workspaces,
		MemberTools: rt, PiMemberToolsExtension: ordinary})
	roleOK(t, err)

	thread := domain.ThreadRef{GroupID: f.group, TopicID: "proposal-topic"}
	_, err = engine.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1}, 0, "chat-policy")
	roleOK(t, err)
	claim := func(delivery string, keys []string) store.MemberToolScope {
		t.Helper()
		turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: delivery, ResourceKeys: keys, MaxActive: 2})
		roleOK(t, err)
		scope, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: f.group, TurnID: turn.ID, Token: turn.ClaimToken})
		roleOK(t, err)
		return scope
	}
	settle := func(scope store.MemberToolScope) {
		t.Helper()
		_, err := f.db.ApplyTurnReceipt(ctx, scope.Claim, roleNotStarted())
		roleOK(t, err)
	}
	capture := func(text string) (store.MemberToolScope, domain.ChatCandidate) {
		t.Helper()
		msg, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: thread.TopicID, Content: text, RecipientIDs: []string{f.lead.ID}}, "")
		roleOK(t, err)
		scope := claim(msg.Deliveries[0].ID, nil)
		candidate, err := engine.CaptureChatCandidate(ctx, scope)
		roleOK(t, err)
		return scope, candidate
	}
	intake, source := capture("请完成纯文本摘要。")
	admit, err := engine.AssessChatIntent(ctx, intake, source.ID, domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionTextSubmission,
		Title: "摘要", Description: "只交付文本", Acceptance: "列出依据", Reason: "纯文本"}, "记录。", "admit")
	roleOK(t, err)
	settle(intake)
	chosen, err := f.db.MaterializeDispatchRequest(ctx, admit.DispatchRequestID, 1, time.Now().UTC())
	roleOK(t, err)
	coordinator := f.claim(t, chosen.Delivery.DeliveryID)
	decider, err := f.db.BindMemberTool(ctx, coordinator.claim)
	roleOK(t, err)
	assignment, err := f.db.DecideDispatch(ctx, store.CoordinationToolScope{Member: decider, Provenance: coordinator.provenance},
		store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "原文本工作"}, "assign")
	roleOK(t, err)
	settle(decider)
	original, err := f.db.MaterializeAssignment(ctx, assignment.Assignment.ID, 1, time.Now().UTC())
	roleOK(t, err)
	state, err := f.db.ResolveCoordinationDelivery(ctx, f.group, original.Delivery.DeliveryID)
	roleOK(t, err)
	old := claim(original.Delivery.DeliveryID, nil)
	prior, err := f.db.BindAssignmentAttempt(ctx, store.CoordinationToolScope{Member: old, Provenance: state.Provenance})
	roleOK(t, err)
	settle(old)

	prepareFollowup := func(constraint, key string) (scheduler.PreparedExecution, store.MemberToolScope, string) {
		t.Helper()
		intake, followup := capture("请继续 " + admit.TaskID + "。" + constraint + "。")
		wake, err := engine.AssessChatIntent(ctx, intake, followup.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
			Reason: "续办", Followup: &domain.ChatFollowupAssessment{TaskID: admit.TaskID, Constraint: constraint}}, "记录续办。", key)
		roleOK(t, err)
		settle(intake)
		msg, err := f.db.GetMessage(ctx, f.group, wake.WorkWakeMessageID)
		roleOK(t, err)
		prepared, err := prep.Prepare(ctx, store.PendingDelivery{GroupID: f.group, SenderID: msg.SenderID, Delivery: msg.Deliveries[0]})
		roleOK(t, err)
		if !strings.Contains(prepared.Request.Prompt, decisionOp) {
			t.Fatal("the exact enabled profile did not explain proposal semantics")
		}
		live := claim(msg.Deliveries[0].ID, prepared.ResourceKeys)
		prepared.Request.RuntimeDir, prepared.Request.RunID = f.root, live.Claim.TurnID
		return prepared, live, wake.WorkInputID
	}
	prepared, live, inputID := prepareFollowup("注明依据", "continue")
	observer, err := f.local.CreateMember(ctx, f.group, platform.MemberInput{Name: "ordinary", Role: "untrusted", ProviderID: selected.ID, Model: f.worker.Model}, "")
	roleOK(t, err)
	otherMsg, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: thread.TopicID, Content: "另一成员的普通消息", RecipientIDs: []string{observer.ID}}, "")
	roleOK(t, err)
	other := claim(otherMsg.Deliveries[0].ID, nil)
	roleOK(t, f.db.RecordTurnEvent(ctx, other.Claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: other.Claim.TurnID}))
	if token, err := tools.Issue(ctx, other.Claim, platform.MemberToolGrants{Operations: []string{decisionOp}}); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("generic Issue minted a proposal grant")
	}
	generic, err := tools.Issue(ctx, other.Claim, platform.MemberToolGrants{Operations: []string{"read_channel"}})
	roleOK(t, err)
	defer tools.Revoke(generic)

	startNative := func(prepared scheduler.PreparedExecution, live store.MemberToolScope) (func(provider.EventKind), func(), func() provider.ExecutionReceipt) {
		// Bound each native run separately from admission and fixture setup.
		ctx, cancel := context.WithTimeout(ctx, runTimeout)
		checkpoints := make(chan provider.EventKind)
		advance := make(chan struct{})
		finished := make(chan struct{})
		var receipt provider.ExecutionReceipt
		go func() {
			defer close(finished)
			receipt = prepared.Executor.Execute(ctx, prepared.Request, func(event provider.Event) error {
				if err := f.db.RecordTurnEvent(ctx, live.Claim, event); err != nil {
					return err
				}
				if event.Kind == provider.SessionEvent || event.Kind == provider.AcceptedEvent || event.Kind == provider.TerminalEvent {
					select {
					case checkpoints <- event.Kind:
					case <-ctx.Done():
						return ctx.Err()
					}
					select {
					case <-advance:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			})
		}()
		t.Cleanup(func() { cancel(); <-finished })
		wait := func(kind provider.EventKind) {
			t.Helper()
			select {
			case got := <-checkpoints:
				if got != kind {
					t.Fatalf("event %s, want %s", got, kind)
				}
			case <-finished:
				t.Fatalf("missing %s: %+v", kind, receipt)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		resume := func() {
			t.Helper()
			select {
			case advance <- struct{}{}:
			case <-finished:
				t.Fatalf("native run finished at a paused checkpoint: %+v", receipt)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		return wait, resume, func() provider.ExecutionReceipt { <-finished; return receipt }
	}
	type nativeConfig struct {
		Token string `json:"token"`
		Proof string `json:"task_decision_proof"`
		RunID string `json:"run_id"`
	}
	readConfig := func(live store.MemberToolScope) nativeConfig {
		t.Helper()
		files, err := os.ReadDir(f.options.ConfigRoot)
		roleOK(t, err)
		if len(files) != 1 {
			t.Fatal("missing exact native config")
		}
		data, err := os.ReadFile(filepath.Join(f.options.ConfigRoot, files[0].Name()))
		roleOK(t, err)
		var config nativeConfig
		roleOK(t, json.Unmarshal(data, &config))
		if config.Token == "" || config.Token == live.Claim.Token || len(config.Proof) != 64 || config.RunID != live.Claim.TurnID {
			t.Fatal("native proposal credential did not bind the actual Turn")
		}
		return config
	}
	wait, resume, finish := startNative(prepared, live)
	wait(provider.SessionEvent)
	config := readConfig(live)
	proposalKey := func(runID, id string) string {
		return fmt.Sprintf("%x", sha256.Sum256([]byte(decisionOp+"\x00"+runID+"\x00"+id)))
	}
	call := func(bearer, proof, op, id, key, body string, code int) string {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/"+op, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		if proof != "" {
			r.Header.Set(decisionProofHeader, proof)
			r.Header.Set(decisionCallHeader, id)
		}
		w := httptest.NewRecorder()
		srv.Handler.ServeHTTP(w, r)
		if w.Code != code || strings.Contains(w.Body.String(), config.Proof) || strings.Contains(w.Body.String(), config.Token) {
			t.Fatalf("%s: got status %d, want %d, or response leaked config", op, w.Code, code)
		}
		wantReceiptKey := ""
		if op == decisionOp && code == http.StatusCreated {
			wantReceiptKey = key
		}
		if w.Header().Get("Idempotency-Key") != wantReceiptKey {
			t.Fatalf("%s: receipt did not match verified write identity", op)
		}
		return w.Body.String()
	}
	propose := func(id, body string, code int) string {
		t.Helper()
		return call(config.Token, config.Proof, decisionOp, id, proposalKey(config.RunID, id), body, code)
	}
	workInput := func(op, id, body string, code int) string {
		t.Helper()
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(op+id)))
		return call(config.Token, "", op, id, key, body, code)
	}
	const originalCall = "native|proposal"
	firstKey := proposalKey(live.Claim.TurnID, originalCall)
	propose(originalCall, decisionQuestion, 403) // Session id is not acceptance.
	resume()
	wait(provider.AcceptedEvent)
	call(generic, config.Proof, decisionOp, originalCall, firstKey, decisionQuestion, 403)
	call(config.Token, "", decisionOp, originalCall, firstKey, decisionQuestion, 403)
	call(config.Token, strings.Repeat("0", 64), decisionOp, originalCall, firstKey, decisionQuestion, 403)
	legacyKey := fmt.Sprintf("%x", sha256.Sum256([]byte(decisionOp+originalCall)))
	call(config.Token, config.Proof, decisionOp, originalCall, legacyKey, decisionQuestion, 403)
	workInput("mcp", "other-wire", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 403)
	for _, invalid := range []string{
		strings.Replace(decisionQuestion, `"expected_version":0`, `"expected_version":null`, 1),
		strings.Replace(decisionQuestion, `"expected_version":0`, `"expected_version":9007199254740992`, 1),
		strings.Replace(decisionQuestion, `"scope":"只读样本"`, `"scope":"只读样本","grant":true`, 1),
		strings.Replace(decisionQuestion, `"scope":"只读样本"`, `"scope":"只读样本","scope":"重复"`, 1),
		strings.Replace(decisionQuestion, `"question":"是否纳入周末样本？"`, `"question":"\ud800"`, 1),
		strings.TrimSuffix(decisionQuestion, "}") + `,"assignment_id":"forged"}`,
		strings.TrimSuffix(decisionQuestion, "}") + `,"expires_at":"2099-01-01T00:00:00Z"}`,
	} {
		propose("bad-input", invalid, 400)
	}
	counts := func() [4]int {
		t.Helper()
		var n [4]int
		for i, table := range []string{"messages", "deliveries", "resource_claims", "task_attempts"} {
			roleOK(t, f.raw.QueryRow(`SELECT count(*) FROM `+table).Scan(&n[i]))
		}
		return n
	}
	before := counts()
	var versions int
	var firstProposal domain.DecisionRequest
	if mode == "unproven-commit" {
		_, err := f.raw.Exec(`CREATE TABLE proposal_fault (group_id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
			CREATE TRIGGER proposal_fixture_commit_fault AFTER INSERT ON decision_request_versions
			BEGIN INSERT INTO proposal_fault VALUES('missing-group'); END`)
		roleOK(t, err)
		if body := propose(originalCall, decisionQuestion, 500); !strings.Contains(body, "未确认") || strings.Contains(body, "proposal_fault") {
			t.Fatal("no conservative, sanitized failure receipt")
		}
	} else {
		first := propose(originalCall, decisionQuestion, 201)
		if first != propose(originalCall, decisionQuestion, 201) || first != propose("coalesce", decisionQuestion, 201) {
			t.Fatal("exact replay or identical slot was not stable")
		}
		changed := strings.Replace(strings.Replace(decisionQuestion, `"expected_version":0`, `"expected_version":1`, 1), "两天", "一天", 1)
		amended := propose("amend", changed, 201)
		if !strings.Contains(amended, `"version":2`) || amended != propose("amend", changed, 201) {
			t.Fatal("exact version CAS or amendment replay failed")
		}
		var saved struct {
			ID string `json:"decision_request_id"`
		}
		roleOK(t, json.Unmarshal([]byte(first), &saved))
		page, err := engine.ListPendingTaskDecisions(ctx, store.ListOptions{Limit: 2})
		roleOK(t, err)
		if len(page.Items) != 1 {
			t.Fatal("proposal didn't coalesce to one pending question")
		}
		q := page.Items[0]
		if q.ID != saved.ID || q.Version != 2 || q.Task == nil || q.ThreadRef != thread || q.Task.TaskID != admit.TaskID ||
			q.Task.AssignmentID != assignment.Assignment.ID || q.Task.MandateID != admit.MandateID || q.Task.OwnerMemberID != f.worker.ID ||
			q.Task.TaskRevision != 1 || q.Task.AuthorityGeneration != 1 || q.SourceMessageID != source.MessageID || q.Relay != domain.DecisionRelayTaskOwner ||
			q.AuthorTurnID != live.Claim.TurnID || q.AuthorAttemptID == "" || q.AuthorAttemptID == prior.AttemptID || q.RecipientID != "user" ||
			q.Facts != "只验证了工作日。 🧪" || q.Authorization == nil || q.Authorization.Impact != "增加一天" {
			t.Fatal("question was not persisted with F's exact server-derived scope and payload")
		}
		firstProposal, versions = q, 2
		if mode == "stale-replay" {
			propose(originalCall, decisionQuestion, 500) // F gives no rollback proof on stale replay.
		}
	}
	if before != counts() {
		t.Fatal("proposal changed execution, delivery or Task records")
	}
	var stored int
	roleOK(t, f.raw.QueryRow(`SELECT count(*) FROM decision_request_versions`).Scan(&stored))
	if stored != versions {
		t.Fatalf("unexpected persisted proposal versions: %d, want %d", stored, versions)
	}
	input := fmt.Sprintf(`{"input_id":%q}`, inputID)
	workInput("read_work_inputs", "read", input, 200)
	if mode == "confirmed-two-turns" {
		workInput("ack_work_input", "ack", input, 200)
		tools.Revoke(config.Token)
		propose("revoked", decisionQuestion, 403)
	} else {
		propose(originalCall, decisionQuestion, 500)
		propose("new-key", strings.Replace(decisionQuestion, "两天", "三天", 1), 500)
		workInput("ack_work_input", "ack", input, 500)
		workInput("hand_back_work_input", "return", strings.TrimSuffix(input, "}")+`,"reason":"return"}`, 500)
		workInput("submit_result", "submit", `{"summary":"not allowed"}`, 500)
		var handled int
		roleOK(t, f.raw.QueryRow(`SELECT count(*) FROM task_work_input_consumptions WHERE input_id=?`, inputID).Scan(&handled))
		if handled != 0 {
			t.Fatal("unconfirmed proposal allowed a later WorkInput mutation")
		}
	}
	resume()
	wait(provider.TerminalEvent)
	resume()
	receipt := finish()
	want := provider.OutcomeUnknown
	if mode == "confirmed-two-turns" {
		want = provider.OutcomeSucceeded
	}
	if receipt.Outcome != want || receipt.Terminal != provider.OutcomeSucceeded || !receipt.Accepted || !receipt.OutputComplete || receipt.NativeRef != nil {
		t.Fatalf("native success and proposal settlement were not kept separate: %+v", receipt)
	}
	settled, err := f.db.ApplyTurnReceipt(ctx, live.Claim, receipt)
	roleOK(t, err)
	held, err := f.db.HasResourceClaim(ctx, prepared.ResourceKeys[0])
	roleOK(t, err)
	if want == provider.OutcomeUnknown && (!held || settled.State != domain.TurnStateUnknown || settled.ReplyMessageID != "") {
		t.Fatal("unknown proposal released work or published a reply")
	}
	propose("after-drain", decisionQuestion, 403)
	if mode != "confirmed-two-turns" {
		return
	}

	// Same group, owner, Task slot and native call ID; a new accepted Turn
	// can amend the question, but cannot enter F using the previous key.
	next, nextLive, _ := prepareFollowup("核对页码", "continue-again")
	wait, resume, finish = startNative(next, nextLive)
	wait(provider.SessionEvent)
	config = readConfig(nextLive)
	resume()
	wait(provider.AcceptedEvent)
	amend := strings.Replace(strings.Replace(decisionQuestion, `"expected_version":0`, `"expected_version":2`, 1), "两天", "三天", 1)
	call(config.Token, config.Proof, decisionOp, originalCall, firstKey, amend, 403)
	second := propose(originalCall, amend, 201)
	if second != propose(originalCall, amend, 201) {
		t.Fatal("second Turn's exact replay was not stable")
	}
	page, err := engine.ListPendingTaskDecisions(ctx, store.ListOptions{Limit: 2})
	roleOK(t, err)
	if len(page.Items) != 1 {
		t.Fatal("amendment did not keep the same pending question")
	}
	q := page.Items[0]
	if q.ID != firstProposal.ID || q.Version != 3 || q.Authorization == nil || q.Authorization.Impact != "增加三天" ||
		q.AuthorMemberID != firstProposal.AuthorMemberID || q.AuthorTurnID != nextLive.Claim.TurnID || q.AuthorTurnID == firstProposal.AuthorTurnID ||
		q.AuthorAttemptID == "" || q.AuthorAttemptID == firstProposal.AuthorAttemptID {
		t.Fatal("reused call ID did not bind the amendment to the second Turn and Attempt")
	}
	roleOK(t, f.raw.QueryRow(`SELECT count(*) FROM mutation_idempotency
	 WHERE operation='task_decision_request' AND group_id=? AND actor_id=? AND idempotency_key IN (?,?)`,
		f.group, f.worker.ID, firstKey, proposalKey(nextLive.Claim.TurnID, originalCall)).Scan(&stored))
	if stored != 2 {
		t.Fatal("F did not persist both verified Run keys")
	}
	resume()
	wait(provider.TerminalEvent)
	propose("after-terminal", amend, 403) // The capability has not yet been drained.
	resume()
	receipt = finish()
	if receipt.Outcome != provider.OutcomeSucceeded || receipt.Terminal != provider.OutcomeSucceeded || !receipt.Accepted {
		t.Fatalf("the second native Turn did not settle: %+v", receipt)
	}
	_, err = f.db.ApplyTurnReceipt(ctx, nextLive.Claim, receipt)
	roleOK(t, err)
}
