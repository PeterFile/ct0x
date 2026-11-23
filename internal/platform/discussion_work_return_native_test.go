//go:build unix

package platform_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

const a7Reply = "已收到回传，材料不足的问题仍待确认。"

// Exercises the ordinary Pi adapter and the two #200 profiles using only a
// synthetic CLI. No launch here invokes a real model or Pi installation.
func TestDiscussionWorkReturnNative(t *testing.T) {
	ctx := t.Context()
	f := newDiscussionReturnFixture(t)
	private := t.TempDir()
	roleOK(t, os.Chmod(private, 0700))
	roleOK(t, os.Mkdir(filepath.Join(private, platform.InvestigationCWDName), 0700))
	cli := filepath.Join(private, "fixture-pi")
	binary, err := os.Executable()
	roleOK(t, err)
	t.Setenv("A7_TEST_BINARY", binary)
	t.Setenv("A7_FIXTURE_WORKER", f.worker.ID)
	roleOK(t, os.WriteFile(cli, []byte("#!/bin/sh\nexec \"$A7_TEST_BINARY\" -test.run '^TestA7PiFixture$' -- \"$@\"\n"), 0700))
	bridges, err := filepath.Abs("../../extensions/pi-bridge")
	roleOK(t, err)
	selected := provider.Provider{ID: f.worker.ProviderID, Name: provider.Pi, Version: "1.4.0", Installation: provider.Installed, Path: cli}
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	f.options.Endpoint = "http://" + server.Listener.Addr().String() + "/api/v1/member-tools"
	f.options.ChatIntake = f.engine
	var followup domain.ChatAssessmentReceipt
	var handbackID, noticeID, token string
	const summary = "已核对本话题资料；补充材料不足，暂不能完成。"
	const constraint = "核对新增证据。"
	launch := platform.NewPiInvestigationTaskWorkLaunch(filepath.Join(bridges, "fresh-investigation-work.ts"), 0,
		func(check context.Context, plan store.FreshTaskWorkLaunch, admit func() bool) error {
			return f.db.CommitInvestigationTaskWorkLaunch(check, store.InvestigationTaskWorkLaunch(plan), admit)
		})
	launch.EnableDiscussionWorkReturn = true
	construct := launch.Construct
	launch.Construct = func(check context.Context, p provider.Provider, b provider.PiFreshInvestigationBinding) (provider.Executor, error) {
		if _, err := construct(check, p, b); err != nil { // Qualify the actual issued bytes/digest.
			return nil, err
		}
		raw, err := os.ReadFile(b.ConfigPath)
		roleOK(t, err)
		var c investigationNativeConfig
		roleOK(t, json.Unmarshal(raw, &c))
		token = c.Token
		return roleNative(func(run context.Context, req provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
			const handback = platform.HandbackWorkOperation
			want := slices.Concat([]string{platform.ReadInvestigationChannelOperation}, provider.PiFreshTaskWorkToolNames(), []string{handback})
			names, err := f.runtime.Tools().TransportOperations(run, token)
			roleOK(t, err)
			if c.SourceKind != domain.TaskWorkSourceFollowup || req.RunID != c.RunID || !slices.Equal(names, want) || !slices.Equal(c.Operations, want) ||
				strings.Contains(req.Prompt, platform.DelegateWorkOperation) || !strings.Contains(req.Prompt, followup.WorkInputID) {
				t.Fatal("followup was not the exact current owner profile with only the handback suffix")
			}
			ownerNativeEvent(t, emit, req, provider.AcceptedEvent)
			f.expectHTTP(t, token, platform.DelegateWorkOperation, fmt.Sprintf(`{"member_id":%q,"question":"越界委派"}`, f.inviter.ID), "deny-delegation", http.StatusForbidden)
			f.expectHTTP(t, token, "read_channel", `{}`, "", http.StatusForbidden)
			id := fmt.Sprintf(`{"input_id":%q}`, followup.WorkInputID)
			if data := f.expectHTTP(t, token, "read_work_inputs", id, "", http.StatusOK); !strings.Contains(data, constraint) {
				t.Fatal("bound owner did not read the new scoped supplement")
			}
			if got := f.expectHTTP(t, token, "ack_work_input", id, "ack", http.StatusOK); !strings.Contains(got, `"disposition":"confirmed"`) {
				t.Fatal("typed input acknowledgment did not confirm the exact input")
			}
			body, key := fmt.Sprintf(`{"summary":%q}`, summary), ownerNativeKey(handback, "return")
			result := f.expectHTTP(t, token, handback, body, key, http.StatusCreated)
			if f.expectHTTP(t, token, handback, body, key, http.StatusCreated) != result {
				t.Fatal("same-claim handback replay changed the receipt")
			}
			var receipt map[string]string
			roleOK(t, json.Unmarshal([]byte(result), &receipt))
			handbackID, noticeID = receipt["handback_id"], receipt["notice_message_id"]
			if len(receipt) != 3 || handbackID == "" || noticeID == "" {
				t.Fatal("A7 changed the B handback HTTP receipt")
			}
			return ownerNativeStop(t, emit, req, provider.OutcomeSucceeded)
		}), nil
	}
	// Enable #208 as a control: even with sampling of eligible peers, the
	// structured first-turn Worker gets no initial-owner management profile.
	f.runtime, err = platform.NewMemberToolRuntimeWithInvestigationOwnerManagement(f.db, f.options, f.engine, launch.DelegateSupport([]provider.Provider{selected}), launch)
	roleOK(t, err)
	t.Cleanup(func() { roleOK(t, f.runtime.Close()) })
	mux := http.NewServeMux()
	for _, r := range f.memberToolRoutes() {
		mux.HandleFunc(r.Method+" "+r.Pattern, r.Handler)
	}
	server.Config.Handler = mux
	server.Start()
	f.mount(t)
	prep, err := platform.NewPreparer(f.db, platform.PrepareOptions{Providers: []provider.Provider{selected}, PrivateWorkspaceRoot: private, MemberTools: f.runtime,
		PiMemberToolsExtension: filepath.Join(bridges, "index.ts"), EnableNativeSessionResume: true,
		Investigation: &platform.InvestigationPreparationOptions{PiPath: cli}})
	roleOK(t, err)
	run := func(phase, next string, member domain.Member) observation {
		t.Helper()
		t.Setenv("A7_FIXTURE_PHASE", phase)
		o := runInvestigationOwnerWakes(t, f.boundWorkReportFixture, prep, &next, 1)[0]
		turn, err := f.db.GetTurn(ctx, f.group, o.claim.TurnID)
		roleOK(t, err)
		if turn.MemberID != member.ID || turn.DeliveryID != next || turn.State != domain.TurnStateSucceeded || turn.AcceptedAt == nil ||
			o.receipt.Outcome != provider.OutcomeSucceeded || o.receipt.NativeRef != nil || !o.receipt.OutputComplete {
			t.Fatalf("%s did not settle the selected fresh Turn: state=%s outcome=%s reason=%q exit=%d", phase, turn.State, o.receipt.Outcome, o.receipt.Reason, o.receipt.ExitCode)
		}
		f.claims(t, roleRun{claim: o.claim}, domain.TurnStateSucceeded, 0)
		return o
	}

	// The same member and wording in neighboring unstructured chat get no
	// investigation intake action, even with an equivalent local-user policy.
	legacySource, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: "legacy-neighbor", Content: "普通讨论。", RecipientIDs: []string{f.worker.ID}}, "legacy")
	roleOK(t, err)
	policy := f.policy.ChatPolicyRequest
	_, err = f.engine.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.group, TopicID: legacySource.TopicID}, policy, 0, "legacy-policy")
	roleOK(t, err)
	publisher, _ := f.capture(t, legacySource)
	legacy, err := f.db.SendMessageForTool(ctx, publisher, store.MemberToolMessageInput{Content: f.invite.Content, RecipientIDs: []string{f.receiver.ID}}, "legacy-neighbor")
	roleOK(t, err)
	f.settleWork(t, publisher, true)
	run("legacy", legacy.Deliveries[0].ID, f.receiver)

	intake := run("intake", f.invite.Deliveries[0].ID, f.receiver)
	var taskID string
	roleOK(t, f.raw.QueryRow(`SELECT id FROM tasks WHERE group_id=?`, f.group).Scan(&taskID))
	a, err := f.db.GetChatAdmission(ctx, f.group, taskID)
	roleOK(t, err)
	if a.MessageID != f.invite.ID || a.SourceKind != "member" || a.TurnID != intake.claim.TurnID || !a.AuthorityEvidenceVerified {
		t.Fatal("investigation did not bind the actual invitation and receiving Turn")
	}
	dispatch, err := f.db.MaterializeDispatchRequest(ctx, a.DispatchRequestID, 1, time.Now().UTC())
	roleOK(t, err)
	run("coordinator", dispatch.Delivery.DeliveryID, f.lead)
	var assignmentID string
	roleOK(t, f.raw.QueryRow(`SELECT id FROM coordination_assignments WHERE request_id=?`, a.DispatchRequestID).Scan(&assignmentID))
	assigned, err := f.db.MaterializeAssignment(ctx, assignmentID, 1, time.Now().UTC())
	roleOK(t, err)
	worker := run("worker", assigned.Delivery.DeliveryID, f.worker)
	supplement, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: f.policy.TopicID,
		Content: "请继续任务 " + taskID + "。" + constraint, RecipientIDs: []string{f.receiver.ID}}, "supplement")
	roleOK(t, err)
	scope, c := f.capture(t, supplement)
	followup, err = f.engine.AssessChatIntent(ctx, scope, c.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
		Reason: "补充原调查", Followup: &domain.ChatFollowupAssessment{TaskID: taskID, Constraint: constraint}}, "记录补充。", "supplement")
	roleOK(t, err)
	if followup.Outcome != "continued" || followup.WorkDeliveryID == "" {
		t.Fatalf("no scoped followup: %s/%s", followup.Outcome, followup.ReasonCode)
	}
	f.settleWork(t, scope, true)
	owner := run("followup", followup.WorkDeliveryID, f.worker)
	f.expectHTTP(t, token, platform.HandbackWorkOperation, fmt.Sprintf(`{"summary":%q}`, summary), "revoke-check", http.StatusForbidden)
	ownerNativeRows(t, f.boundWorkReportFixture, 1, `SELECT count(*) FROM task_work_handbacks h JOIN task_work_continuations c ON h.author_attempt_id=c.attempt_id
	 WHERE h.id=? AND h.author_turn_id=c.turn_id AND c.turn_id=? AND c.turn_id<>? AND h.task_id=? AND h.mandate_id=? AND h.assignment_id=? AND h.coordinator_member_id=?`,
		handbackID, owner.claim.TurnID, worker.claim.TurnID, a.TaskID, a.MandateID, assignmentID, f.lead.ID)
	r, message := f.savedReturn(t, handbackID)
	notice, err := f.db.GetMessage(ctx, f.group, noticeID)
	roleOK(t, err)
	if r.HandbackNoticeMessageID != notice.ID || notice.ID == message.ID || len(notice.Deliveries) != 0 || !strings.HasSuffix(message.Content, summary) {
		t.Fatal("the B notice was not separate and non-waking")
	}
	f.expectReturnQueued(t, r) // Persistence does not prove consumption.
	roleOK(t, os.Chmod(cli, 0600))
	if p, err := prep.Prepare(ctx, queuedFromMessage(message)); p.Executor != nil || !errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("unavailable Pi did not leave the return queued: %v", err)
	}
	roleOK(t, os.Chmod(cli, 0700))
	bad := queuedFromMessage(message)
	bad.SenderID = f.lead.ID
	if p, err := prep.Prepare(ctx, bad); p.Executor != nil || err == nil {
		t.Fatal("a mismatched sender reached the executor")
	}
	bad = queuedFromMessage(message)
	bad.Delivery.RecipientID = f.lead.ID
	if p, err := prep.Prepare(ctx, bad); p.Executor != nil || err == nil {
		t.Fatal("a mismatched recipient reached the executor")
	}
	before := f.workCounts(t)
	returned := run("return", r.DeliveryID, f.inviter)
	var packet struct {
		Pending []contextpack.Message
		History []contextpack.Message
	}
	roleOK(t, json.Unmarshal([]byte(returned.Request.Prompt), &packet))
	if len(packet.Pending) != 1 || packet.Pending[0].ID != r.MessageID || packet.Pending[0].Content != message.Content || len(packet.History) != 0 ||
		returned.Request.Model != f.inviter.Model || !strings.Contains(returned.Request.Prompt, "discussion_work_return") {
		t.Fatal("the bound recipient did not receive the exact return-only prompt")
	}
	turn, err := f.db.GetTurn(ctx, f.group, returned.claim.TurnID)
	roleOK(t, err)
	reply, err := f.db.GetMessage(ctx, f.group, turn.ReplyMessageID)
	roleOK(t, err)
	before[0]++
	if f.workCounts(t) != before || reply.SenderID != f.inviter.ID || reply.TopicID != r.TopicID || turn.ConversationID != r.ConversationID ||
		reply.Content != a7Reply || reply.SourceKind != "member" || reply.SourceID != "turn:"+turn.ID || reply.Discussion != nil || len(reply.Deliveries) != 0 {
		t.Fatal("return consumption was not a single ordinary public reply from the root inviter")
	}
	ownerNativeRows(t, f.boundWorkReportFixture, 0, `SELECT count(*) FROM chat_candidates WHERE turn_id=?`, turn.ID)
	ownerNativeRows(t, f.boundWorkReportFixture, 1, `SELECT count(*) FROM deliveries WHERE id=? AND turn_id=? AND state='accepted' AND error=''`, r.DeliveryID, turn.ID)
	ownerNativeRows(t, f.boundWorkReportFixture, 1, `SELECT count(*) FROM turns WHERE delivery_id=? AND json_extract(receipt_json,'$.outcome')='succeeded'`, r.DeliveryID)
	ownerNativeRows(t, f.boundWorkReportFixture, 0, `SELECT count(*) FROM task_work_delegations`)
}

func TestDiscussionWorkReturnStartup(t *testing.T) {
	for _, fault := range []string{"missing_claim", "stale_claim", "wrong_holder", "extra_lock"} {
		t.Run(fault, func(t *testing.T) { testDiscussionWorkReturnStartup(t, fault) })
	}
}

func testDiscussionWorkReturnStartup(t *testing.T, fault string) {
	t.Helper()
	ctx := t.Context()
	f := newDiscussionReturnFixture(t)
	r, message := f.seedReturn(t)
	private := t.TempDir()
	roleOK(t, os.Chmod(private, 0700))
	roleOK(t, os.Mkdir(filepath.Join(private, platform.InvestigationCWDName), 0700))
	cli := filepath.Join(private, "fixture-pi")
	roleOK(t, os.WriteFile(cli, []byte("#!/bin/sh\nexit 0\n"), 0700))
	bridges, err := filepath.Abs("../../extensions/pi-bridge")
	roleOK(t, err)
	launch := platform.NewPiInvestigationTaskWorkLaunch(filepath.Join(bridges, "fresh-investigation-work.ts"), 0,
		func(check context.Context, plan store.FreshTaskWorkLaunch, admit func() bool) error {
			return f.db.CommitInvestigationTaskWorkLaunch(check, store.InvestigationTaskWorkLaunch(plan), admit)
		})
	launch.EnableDiscussionWorkReturn = true
	f.options.ChatIntake = f.engine
	runtime, err := platform.NewMemberToolRuntimeWithTaskWorkReports(f.db, f.options, f.engine, launch)
	roleOK(t, err)
	t.Cleanup(func() { roleOK(t, runtime.Close()) })
	selected := provider.Provider{ID: f.inviter.ProviderID, Name: provider.Pi, Version: "1.4.0", Installation: provider.Installed, Path: cli}
	prep, err := platform.NewPreparer(f.db, platform.PrepareOptions{Providers: []provider.Provider{selected}, PrivateWorkspaceRoot: private,
		MemberTools: runtime, PiMemberToolsExtension: filepath.Join(bridges, "index.ts"), Investigation: &platform.InvestigationPreparationOptions{PiPath: cli}})
	roleOK(t, err)
	prepared, err := prep.Prepare(ctx, queuedFromMessage(message))
	roleOK(t, err)
	if len(prepared.ResourceKeys) != 1 || prepared.BindTurnClaim == nil {
		t.Fatal("expected a return CWD key and scheduler claim binding")
	}
	cwdKey, maxActive := prepared.ResourceKeys[0], 1
	var holderID string
	wantState := domain.TurnStateUnknown
	switch fault {
	case "missing_claim":
		prepared.BindTurnClaim = nil
	case "stale_claim":
		// Change only the persisted token before the first verification. ClaimTurn
		// still returns its original token, and the row stays starting.
		_, err := f.raw.Exec(`CREATE TRIGGER replace_return_claim AFTER INSERT ON turns
		 BEGIN UPDATE turns SET claim_token=claim_token || '-fixture' WHERE id=NEW.id; END`)
		roleOK(t, err)
		wantState = domain.TurnStateStarting // The stale scheduler cannot settle the replacement claim.
	case "wrong_holder":
		other, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: r.TopicID, Content: "另一个轮次", RecipientIDs: []string{f.lead.ID}}, "other-turn")
		roleOK(t, err)
		holder, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: other.Deliveries[0].ID, ResourceKeys: prepared.ResourceKeys, MaxActive: 2})
		roleOK(t, err)
		holderID, maxActive = holder.ID, 2
		prepared.ResourceKeys = nil
	case "extra_lock":
		prepared.ResourceKeys = []string{cwdKey, "extra-fixture-lock"}
	}

	// Keep the real launch path and executor. Observe the receipt before the
	// scheduler applies it, then let Run drain the settlement before checking state.
	runCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	var got provider.ExecutionReceipt
	var runID string
	executor := prepared.Executor
	prepared.Executor = roleNative(func(run context.Context, req provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
		runID = req.RunID
		got = executor.Execute(run, req, emit)
		stop()
		return got
	})
	// Prevent cancellation polling from finding a stale token before Execute does.
	err = scheduler.Run(runCtx, f.db, scheduler.Options{Recovered: true, RuntimeDir: f.root, MaxActive: maxActive, PollInterval: time.Hour},
		func(_ context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
			if pending.Delivery.ID != r.DeliveryID {
				return scheduler.PreparedExecution{}, scheduler.ErrNotReady
			}
			return prepared, nil
		})
	if !errors.Is(err, context.Canceled) || runID == "" {
		t.Fatalf("scheduler did not drain the prepared return: %v", err)
	}
	if (err.Error() != context.Canceled.Error()) != (fault == "stale_claim") {
		t.Fatalf("unexpected scheduler settlement result: %v", err)
	}
	if got.Started || got.Accepted || got.Outcome != provider.OutcomeUnknown {
		t.Fatalf("startup gate did not preserve uncertainty: started=%t accepted=%t outcome=%s", got.Started, got.Accepted, got.Outcome)
	}
	if _, err := os.Lstat(filepath.Join(f.root, runID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied startup reached native log setup: %v", err)
	}
	turn, err := f.db.GetTurn(ctx, f.group, runID)
	roleOK(t, err)
	if turn.State != wantState || turn.AcceptedAt != nil || turn.NativeSessionID != "" || turn.ReplyMessageID != "" {
		t.Fatalf("unexpected durable evidence after denied startup: state=%s", turn.State)
	}
	if fault == "stale_claim" {
		// The replacement would otherwise pass the startup fence. It is not the
		// claim that the scheduler received and was not used to settle this run.
		current := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
		roleOK(t, f.db.VerifyDiscussionWorkReturnStartup(ctx, current, r, selected.ID, prepared.Request.Model, cwdKey))
	}
	if holderID == "" {
		holderID = runID
	}
	ownerNativeRows(t, f.boundWorkReportFixture, 1, `SELECT count(*) FROM deliveries WHERE id=? AND turn_id=? AND state='queued' AND error=''`, r.DeliveryID, runID)
	ownerNativeRows(t, f.boundWorkReportFixture, len(prepared.ResourceKeys), `SELECT count(*) FROM resource_claims WHERE turn_id=?`, runID)
	ownerNativeRows(t, f.boundWorkReportFixture, 1, `SELECT count(*) FROM resource_claims WHERE resource_key=? AND turn_id=?`, cwdKey, holderID)
}

// Test-only child executed by the shell shim. It verifies actual argv/config
// and uses only the already-mounted in-process test server on loopback.
func TestA7PiFixture(t *testing.T) {
	phase := os.Getenv("A7_FIXTURE_PHASE")
	if phase == "" || !slices.Contains(os.Args, "--") {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	arg := func(f string) string {
		i := slices.Index(args, f)
		if i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	if arg("--session-id") == "" || slices.Contains(args, "--session") || slices.Contains(args, "--continue") {
		t.Fatal("fixture did not receive a fresh session")
	}
	_, err := io.Copy(io.Discard, os.Stdin)
	roleOK(t, err)
	cwd, err := os.Getwd()
	roleOK(t, err)
	fmt.Printf("{\"type\":\"session\",\"version\":3,\"id\":%q,\"cwd\":%q}\n", arg("--session-id"), cwd)
	fmt.Println(`{"type":"agent_start"}`)
	if phase == "return" {
		for _, flag := range []string{"--no-tools", "--no-mcp", "--no-extensions", "--no-skills", "--no-session", "--no-context-files"} {
			if !slices.Contains(args, flag) {
				t.Fatalf("return missing %s", flag)
			}
		}
		if arg("--system-prompt") == "" || arg("--append-system-prompt") == "" {
			t.Fatal("return left the ambient system prompts enabled")
		}
		for _, flag := range []string{"--extension", "--tools"} {
			if slices.Contains(args, flag) {
				t.Fatalf("return selected %s", flag)
			}
		}
	} else {
		raw, err := os.ReadFile(os.Getenv("AGENT_WORKSPACE_MEMBER_TOOL_CONFIG"))
		roleOK(t, err)
		var c struct {
			Endpoint, Token string
			IntakeAction    string   `json:"intake_assessment_action"`
			Tools           []string `json:"investigation_tools"`
		}
		roleOK(t, json.Unmarshal(raw, &c))
		call := func(op, body string, status int) string {
			t.Helper()
			req, err := http.NewRequest(http.MethodPost, c.Endpoint+"/"+op, strings.NewReader(body))
			roleOK(t, err)
			req.Header.Set("Authorization", "Bearer "+c.Token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "stub-"+phase+"-"+op)
			response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
			roleOK(t, err)
			defer response.Body.Close()
			data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
			roleOK(t, err)
			if response.StatusCode != status {
				t.Fatalf("%s failed: status %d (expected %d)", op, response.StatusCode, status)
			}
			return string(data)
		}
		const assess = `{"content":"记录调查范围。","intent_assessment":{"disposition":"prepare_work","action":"read_only_investigation","reason":"按书面授权调查"}}`
		switch phase {
		case "legacy":
			if c.IntakeAction != "" || len(c.Tools) != 0 {
				t.Fatal("unstructured member message acquired investigation intake")
			}
			call("send_message", assess, http.StatusForbidden)
		case "intake":
			if c.IntakeAction != domain.ChatActionReadOnlyInvestigation || len(c.Tools) != 0 ||
				!strings.Contains(call("read_channel", `{}`, http.StatusOK), `"source_kind":"member"`) ||
				!strings.Contains(call("send_message", assess, http.StatusCreated), `"outcome":"admitted"`) {
				t.Fatal("structured invitation did not get the exact authorized intake")
			}
		case "coordinator", "worker":
			command := "decide_dispatch"
			if phase == "worker" {
				command = "submit_result"
			}
			want := []string{platform.ReadInvestigationChannelOperation, command}
			if c.IntakeAction != "" || !slices.Equal(c.Tools, want) || arg("--tools") != strings.Join(want, ",") {
				t.Fatal("formal role acquired an extra or missing operation")
			}
			call(platform.ReadInvestigationChannelOperation, `{}`, http.StatusOK)
			if phase == "coordinator" {
				call(command, fmt.Sprintf(`{"kind":"assign","worker_member_id":%q,"reason":"受限调查"}`, os.Getenv("A7_FIXTURE_WORKER")), http.StatusCreated)
			} else {
				call(platform.HandbackWorkOperation, `{"summary":"首轮不可交回"}`, http.StatusForbidden)
				call(platform.DelegateWorkOperation, fmt.Sprintf(`{"member_id":%q,"question":"首轮不可委派"}`, os.Getenv("A7_FIXTURE_WORKER")), http.StatusForbidden)
			}
		default:
			t.Fatal("unexpected synthetic phase")
		}
	}
	if phase == "return" {
		fmt.Printf(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":%q}]}}`+"\n", a7Reply)
	} else {
		fmt.Println(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[]}}`)
	}
	fmt.Println(`{"type":"agent_settled","aborted":false}`)
	os.Exit(0) // Do not mix testing's PASS line into native JSONL.
}
