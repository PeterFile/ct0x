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
	"agentworkspace/internal/store"
)

const decisionProofHeader = "X-Agent-Workspace-Task-Decision-Proof"
const decisionCallHeader = "X-Agent-Workspace-Tool-Call-Id"
const decisionOp = "propose_task_decision"
const decisionQuestion = `{"kind":"authorization","expected_version":0,"question":"是否纳入周末样本？","facts":"只验证了工作日。 🧪","recommendation":"先交付工作日。","choices":["仅工作日","加入周末"],"authorization":{"scope":"只读样本","impact":"增加两天"}}`

// F #221 provides the exact SQL but intentionally leaves it out of the public
// migration registry. Install its unchanged proposal fragment in a private
// test DB; no SQL/schema is copied into the runtime or replicated in fixtures.
func installTaskDecisionProposalFixture(t *testing.T, f *roleRuntimeFixture, engine *coordination.Engine) {
	t.Helper()
	if _, err := engine.ListPendingDecisionRequests(t.Context(), store.ListOptions{Limit: 1}); !errors.Is(err, store.ErrDecisionRequestsUnavailable) {
		roleOK(t, err) // Works unchanged when the real migration is registered.
		return
	}
	file, err := parser.ParseFile(token.NewFileSet(), "../store/decision_request_schema.go", nil, 0)
	roleOK(t, err)
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.CONST {
			for _, spec := range d.Specs {
				v := spec.(*ast.ValueSpec)
				if len(v.Names) == 1 && v.Names[0].Name == "decisionRequestSchema" {
					literal, ok := v.Values[0].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						t.Fatal("expected F's proposal schema string")
					}
					ddl, err := strconv.Unquote(literal.Value)
					roleOK(t, err)
					_, err = f.raw.Exec(ddl)
					roleOK(t, err)
					return
				}
			}
		}
	}
	t.Fatal("could not find F's proposal schema")
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
	f.options.FreshTaskWork = platform.NewPiFreshTaskWorkLaunch(entry, 0)
	if _, err := engine.ListPendingDecisionRequests(t.Context(), store.ListOptions{Limit: 1}); errors.Is(err, store.ErrDecisionRequestsUnavailable) {
		if runtime, err := platform.NewMemberToolRuntime(f.db, f.options); runtime != nil || !errors.Is(err, store.ErrDecisionRequestsUnavailable) {
			t.Fatalf("v30 incorrectly enabled proposals: %v", err)
		}
	} else {
		roleOK(t, err)
	}
	installTaskDecisionProposalFixture(t, f, engine)
	strong := f.options.FreshTaskWork
	f.options.FreshTaskWork = weak
	if runtime, err := platform.NewMemberToolRuntime(f.db, f.options); runtime != nil || err == nil {
		t.Fatal("unqualified builder enabled proposals even with F's schema")
	}
	f.options.FreshTaskWork = strong
	f.options.EnablePiTaskDecisionProposals = false
	runtime, err := platform.NewMemberToolRuntime(f.db, f.options)
	roleOK(t, err)
	t.Cleanup(func() { _ = runtime.Close() })
	if runtime.Tools().TaskDecisionProposalsEnabled() || !runtime.Tools().FreshTaskWorkEnabled() {
		t.Fatal("proposal support was not a separate opt-in")
	}
	for _, route := range httpapi.MemberRoleToolRoutes(runtime.Tools()) {
		if strings.HasSuffix(route.Pattern, "/"+decisionOp) {
			t.Fatal("proposal mounted by default")
		}
	}
}

func TestPiTaskDecisionProposalExactAcceptedContinuation(t *testing.T) {
	for _, mode := range []string{"confirmed", "stale-replay", "unproven-commit"} {
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
	f.options.FreshTaskWork = platform.NewPiFreshTaskWorkLaunch(entry, 20*time.Second)
	rt, err := platform.NewMemberToolRuntime(f.db, f.options)
	roleOK(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	tools := rt.Tools()
	routes := append(httpapi.MemberRoleToolRoutes(tools), httpapi.MemberMCPRoutes(tools)...)
	srv, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
	roleOK(t, err)
	engine.EnableTaskWorkExecution()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()

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

	intake, followup := capture("请继续 " + admit.TaskID + "。注明依据。")
	wake, err := engine.AssessChatIntent(ctx, intake, followup.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
		Reason: "续办", Followup: &domain.ChatFollowupAssessment{TaskID: admit.TaskID, Constraint: "注明依据"}}, "记录续办。", "continue")
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
	req := prepared.Request
	req.RuntimeDir, req.RunID = f.root, live.Claim.TurnID
	otherMsg, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: thread.TopicID, Content: "另一成员的普通消息", RecipientIDs: []string{f.lead.ID}}, "")
	roleOK(t, err)
	other := claim(otherMsg.Deliveries[0].ID, nil)
	roleOK(t, f.db.RecordTurnEvent(ctx, other.Claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: other.Claim.TurnID}))
	if token, err := tools.Issue(ctx, other.Claim, platform.MemberToolGrants{Operations: []string{decisionOp}}); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("generic Issue minted a proposal grant")
	}
	generic, err := tools.Issue(ctx, other.Claim, platform.MemberToolGrants{Operations: []string{"read_channel"}})
	roleOK(t, err)
	defer tools.Revoke(generic)

	checkpoints := make(chan provider.EventKind)
	advance := make(chan struct{})
	finished := make(chan struct{})
	var receipt provider.ExecutionReceipt
	go func() {
		defer close(finished)
		receipt = prepared.Executor.Execute(ctx, req, func(event provider.Event) error {
			if err := f.db.RecordTurnEvent(ctx, live.Claim, event); err != nil {
				return err
			}
			if event.Kind == provider.SessionEvent || event.Kind == provider.AcceptedEvent {
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
	defer func() { cancel(); <-finished }()
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
	wait(provider.SessionEvent)
	files, err := os.ReadDir(f.options.ConfigRoot)
	roleOK(t, err)
	if len(files) != 1 {
		t.Fatal("missing exact native config")
	}
	data, err := os.ReadFile(filepath.Join(f.options.ConfigRoot, files[0].Name()))
	roleOK(t, err)
	var config struct {
		Token string `json:"token"`
		Proof string `json:"task_decision_proof"`
	}
	roleOK(t, json.Unmarshal(data, &config))
	if config.Token == "" || config.Token == live.Claim.Token || len(config.Proof) != 64 {
		t.Fatal("missing distinct native proposal credential")
	}
	call := func(bearer, proof, op, id, body string, code int) string {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/"+op, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", fmt.Sprintf("%x", sha256.Sum256([]byte(op+id))))
		if proof != "" {
			r.Header.Set(decisionProofHeader, proof)
			r.Header.Set(decisionCallHeader, id)
		}
		w := httptest.NewRecorder()
		srv.Handler.ServeHTTP(w, r)
		if w.Code != code || strings.Contains(w.Body.String(), config.Proof) || strings.Contains(w.Body.String(), config.Token) {
			t.Fatalf("%s: got status %d, want %d, or response leaked config", op, w.Code, code)
		}
		return w.Body.String()
	}
	propose := func(id, body string, code int) string {
		t.Helper()
		return call(config.Token, config.Proof, decisionOp, id, body, code)
	}
	const originalCall = "native|proposal"
	propose(originalCall, decisionQuestion, 403) // Session id is not acceptance.
	advance <- struct{}{}
	wait(provider.AcceptedEvent)
	call(generic, config.Proof, decisionOp, originalCall, decisionQuestion, 403)
	call(config.Token, "", decisionOp, originalCall, decisionQuestion, 403)
	call(config.Token, strings.Repeat("0", 64), decisionOp, originalCall, decisionQuestion, 403)
	call(config.Token, "", "mcp", "other-wire", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 403)
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
		page, err := engine.ListPendingDecisionRequests(ctx, store.ListOptions{Limit: 2})
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
		versions = 2
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
	input := fmt.Sprintf(`{"input_id":%q}`, wake.WorkInputID)
	call(config.Token, "", "read_work_inputs", "read", input, 200)
	if mode == "confirmed" {
		call(config.Token, "", "ack_work_input", "ack", input, 200)
		tools.Revoke(config.Token)
		propose("revoked", decisionQuestion, 403)
	} else {
		propose(originalCall, decisionQuestion, 500)
		propose("new-key", strings.Replace(decisionQuestion, "两天", "三天", 1), 500)
		call(config.Token, "", "ack_work_input", "ack", input, 500)
		call(config.Token, "", "hand_back_work_input", "return", strings.TrimSuffix(input, "}")+`,"reason":"return"}`, 500)
		call(config.Token, "", "submit_result", "submit", `{"summary":"not allowed"}`, 500)
		var handled int
		roleOK(t, f.raw.QueryRow(`SELECT count(*) FROM task_work_input_consumptions WHERE input_id=?`, wake.WorkInputID).Scan(&handled))
		if handled != 0 {
			t.Fatal("unconfirmed proposal allowed a later WorkInput mutation")
		}
	}
	advance <- struct{}{}
	<-finished
	want := provider.OutcomeUnknown
	if mode == "confirmed" {
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
}
