package platform_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// All authority is established through v29 APIs. The only substitutions are a
// metadata-only Pi executable and an in-process executor/HTTP recorder.
func TestFreshTaskWorkB29NativeWire(t *testing.T) {
	for _, mode := range []string{
		"confirmed", "commit-unknown", "wrong-resource", "extra-resource", "late-resource", "late-config", "late-policy",
		"context-after-handback", "replaced-original-cwd", "workspace-before-gate", "policy-before-gate", "cancel-before-gate",
		"failed-launch-commit", "policy-after-gate",
	} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newRoleRuntime(t)
			f.options.RoleMaxActive = 2 // The assessor can record input while the prior worker is unsettled.
			engine := coordination.New(f.db)
			f.options.ChatIntake = engine
			root := t.TempDir()
			private, cli, ordinary := filepath.Join(root, "workspaces"), filepath.Join(root, "metadata-only"), filepath.Join(root, "index.ts")
			roleOK(t, os.Mkdir(private, 0700))
			roleOK(t, os.WriteFile(cli, nil, 0700))
			roleOK(t, os.WriteFile(ordinary, nil, 0600))
			selected := provider.Provider{ID: f.worker.ProviderID, Name: provider.Pi, Path: cli, Installation: provider.Installed, Version: "1.4.0"}
			opts := platform.PrepareOptions{Providers: []provider.Provider{selected}, PrivateWorkspaceRoot: private,
				MemberTools: f.runtime, PiMemberToolsExtension: ordinary}
			off, err := platform.NewPreparer(f.db, opts)
			roleOK(t, err)
			thread := domain.ThreadRef{GroupID: f.group, TopicID: "fresh-text"}
			policy := domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1}
			policySnap, err := engine.SetChatPolicy(ctx, thread, policy, 0, "chat-policy")
			roleOK(t, err)
			settle := func(claim store.TurnClaim, outcome provider.Outcome) domain.Turn {
				t.Helper()
				turn, err := f.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: outcome})
				roleOK(t, err)
				return turn
			}
			claim := func(delivery string, keys []string) store.MemberToolScope {
				t.Helper()
				turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: delivery, MaxActive: f.options.RoleMaxActive, ResourceKeys: keys})
				roleOK(t, err)
				scope, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: f.group, TurnID: turn.ID, Token: turn.ClaimToken})
				roleOK(t, err)
				return scope
			}
			capture := func(text, key string) (store.MemberToolScope, domain.ChatCandidate) {
				t.Helper()
				msg, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: thread.TopicID, Content: text, RecipientIDs: []string{f.lead.ID}}, key)
				roleOK(t, err)
				scope := claim(msg.Deliveries[0].ID, nil)
				c, err := engine.CaptureChatCandidate(ctx, scope)
				roleOK(t, err)
				return scope, c
			}
			pending := func(id string) store.PendingDelivery {
				t.Helper()
				msg, err := f.db.GetMessage(ctx, f.group, id)
				roleOK(t, err)
				return store.PendingDelivery{GroupID: f.group, SenderID: msg.SenderID, Delivery: msg.Deliveries[0]}
			}

			intake, c := capture("请完成纯文本摘要。", "source")
			admission, err := engine.AssessChatIntent(ctx, intake, c.ID, domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionTextSubmission,
				Title: "原任务摘要", Description: "只交付文本", Acceptance: "列出结论", Reason: "明确文本请求"}, "确认请求。", "admit")
			roleOK(t, err)
			settle(intake.Claim, provider.OutcomeNotStarted)
			dispatch, err := f.db.MaterializeDispatchRequest(ctx, admission.DispatchRequestID, 1, time.Now().UTC())
			roleOK(t, err)
			coord := f.claim(t, dispatch.Delivery.DeliveryID)
			decider, err := f.db.BindMemberTool(ctx, coord.claim)
			roleOK(t, err)
			decision, err := f.db.DecideDispatch(ctx, store.CoordinationToolScope{Member: decider, Provenance: coord.provenance},
				store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "原文本工作"}, "assign")
			roleOK(t, err)
			settle(coord.claim, provider.OutcomeNotStarted)
			assignment, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
			roleOK(t, err)
			original := pending(assignment.Delivery.MessageID)
			originalPrep, err := off.Prepare(ctx, original)
			roleOK(t, err)
			state, err := f.db.ResolveCoordinationDelivery(ctx, f.group, original.Delivery.ID)
			roleOK(t, err)
			oldScope := claim(original.Delivery.ID, originalPrep.ResourceKeys)
			prior, err := f.db.BindAssignmentAttempt(ctx, store.CoordinationToolScope{Member: oldScope, Provenance: state.Provenance})
			roleOK(t, err)

			followup := func(constraint string) domain.ChatAssessment {
				return domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "续办原文本任务",
					Followup: &domain.ChatFollowupAssessment{TaskID: admission.TaskID, Constraint: constraint}}
			}
			var confirmedID string
			const confirmedQuote = "注明摘要依据"
			if mode == "context-after-handback" {
				intake, c = capture("请继续 "+admission.TaskID+"。"+confirmedQuote, "earlier-source")
				ack, err := f.db.ApplyChatAssessmentDormant(ctx, intake, c.ID, followup(confirmedQuote), "记录补充。", "prior-dormant")
				roleOK(t, err)
				settle(intake.Claim, provider.OutcomeNotStarted)
				confirmedID = ack.WorkInputID
				_, err = f.db.AcknowledgeWorkInputWithKey(ctx, oldScope, confirmedID, "prior-ack")
				roleOK(t, err)
			}
			const returnedQuote = "另写项目文件"
			intake, c = capture("请继续 "+admission.TaskID+"。"+returnedQuote+"。", "older-source")
			older, err := f.db.ApplyChatAssessmentDormant(ctx, intake, c.ID, followup(returnedQuote), "记录补充。", "dormant")
			roleOK(t, err)
			settle(intake.Claim, provider.OutcomeNotStarted)
			const detail = "请保留原格式🧪"
			constraint := detail
			if mode == "context-after-handback" {
				_, err := f.db.HandBackWorkInputWithKey(ctx, oldScope, older.WorkInputID, "不在原授权内", "prior-return")
				roleOK(t, err)
				constraint = ""
			}
			if mode != "replaced-original-cwd" {
				settle(oldScope.Claim, provider.OutcomeNotStarted)
			}
			body := "请继续 " + admission.TaskID + "。" + detail
			intake, c = capture(body, "latest-source")
			assess := func() (domain.ChatAssessmentReceipt, error) {
				return engine.AssessChatIntent(ctx, intake, c.ID, followup(constraint), "记录续办。", "continue")
			}
			if _, err := assess(); !errors.Is(err, store.ErrTaskWorkExecutionNotReady) {
				t.Fatalf("default readiness was not closed: %v", err)
			}
			engine.EnableTaskWorkExecution()
			wake, err := assess()
			roleOK(t, err)
			settle(intake.Claim, provider.OutcomeNotStarted)
			if wake.WorkInputID == "" || wake.WorkDeliveryID == "" || wake.MandateID != admission.MandateID {
				t.Fatal("v29 did not bind the input and typed wake to the original Mandate")
			}
			queued := pending(wake.WorkWakeMessageID)
			if got, err := off.Prepare(ctx, queued); got.Executor != nil || !errors.Is(err, scheduler.ErrNotReady) {
				t.Fatalf("ordinary role profile could prepare a typed delivery: %v", err)
			}
			if got, err := off.InspectRoleReadiness(ctx, queued); err != nil || got.Blocked == nil || got.HistoryLimit != 0 {
				t.Fatalf("typed delivery inspected as legacy: %+v %v", got, err)
			}

			var token, configPath, typedCWD string
			var live store.MemberToolScope
			calls := 0
			beforeGateChanged := false
			var peer *store.Store
			switch mode {
			case "workspace-before-gate", "policy-before-gate", "cancel-before-gate", "policy-after-gate":
				peer, err = store.Open(f.path)
				roleOK(t, err)
				t.Cleanup(func() { roleOK(t, peer.Close()) })
			}
			revokePolicy := func() domain.ChatPolicy {
				t.Helper()
				disabled := policy
				disabled.Enabled = false
				saved, err := coordination.New(peer).SetChatPolicy(ctx, thread, disabled, policySnap.Revision, "gate-policy")
				roleOK(t, err)
				if saved.Enabled || saved.Revision <= policySnap.Revision {
					t.Fatal("fixture did not commit a new disabled Chat Policy")
				}
				return saved
			}
			cancelClaim := func() {
				t.Helper()
				turn, err := peer.RequestTurnCancel(ctx, f.group, live.Claim.TurnID)
				roleOK(t, err)
				if turn.State != domain.TurnStateCancelRequested || turn.CancelRequestedAt == nil || turn.ClaimToken != live.Claim.Token {
					t.Fatal("fixture did not commit cancellation on the same claim")
				}
			}
			beforeGate := func() {
				t.Helper()
				switch mode {
				case "workspace-before-gate":
					grantPreparer, err := platform.NewPreparer(peer, platform.PrepareOptions{PrivateWorkspaceRoot: private})
					roleOK(t, err)
					external := filepath.Join(root, "external")
					roleOK(t, os.Mkdir(external, 0700))
					_, err = grantPreparer.AuthorizeWorkspaceGrant(ctx, f.group, f.worker.ID, platform.WorkspaceGrantInput{Root: external, Scope: "read_write"}, "gate-grant")
					roleOK(t, err)
					member, err := f.db.GetMember(ctx, f.group, f.worker.ID)
					roleOK(t, err)
					if member.Workspace != external {
						t.Fatal("grant did not update the configured member workspace")
					}
				case "policy-before-gate":
					revokePolicy()
				case "cancel-before-gate":
					cancelClaim()
				default:
					return
				}
				beforeGateChanged = true
			}
			call := func(op, body, key string, status int) string {
				t.Helper()
				w := f.http(t, token, op, body, key)
				if w.Code != status {
					t.Fatalf("%s: status %d, expected %d: %s", op, w.Code, status, w.Body)
				}
				return w.Body.String()
			}
			f.options.FreshTaskWork = &platform.FreshTaskWorkLaunch{
				Supports: func(p provider.Provider) bool { return p.ID == selected.ID },
				Construct: func(_ context.Context, p provider.Provider, b provider.FreshTaskWorkBinding) (provider.Executor, error) {
					configPath = b.ConfigPath
					bytes, err := os.ReadFile(configPath)
					roleOK(t, err)
					var config struct{ Token, Endpoint string }
					roleOK(t, json.Unmarshal(bytes, &config))
					token = config.Token
					if token == "" || token == live.Claim.Token || config.Endpoint != f.options.Endpoint || b.ConfigSHA256 != sha256.Sum256(bytes) ||
						b.RunID != live.Claim.TurnID || p.ID != selected.ID || !slices.Equal(b.Operations, provider.PiFreshTaskWorkToolNames()) {
						t.Fatal("wrong fresh bootstrap binding")
					}
					switch mode {
					case "late-resource":
						_, err := f.raw.Exec(`INSERT INTO resource_claims(resource_key,turn_id) VALUES('late-foreign-key',?)`, b.RunID)
						roleOK(t, err)
					case "late-config":
						roleOK(t, os.WriteFile(configPath, append(bytes, ' '), 0600))
					case "late-policy":
						policy.Enabled = false
						_, err := engine.SetChatPolicy(ctx, thread, policy, policySnap.Revision, "revoke-before-launch")
						roleOK(t, err)
					case "failed-launch-commit":
						// Only the private fixture's launch transaction uses this scope
						// write. All checks can pass but COMMIT hits a deferred FK.
						_, err := f.raw.Exec(`CREATE TABLE launch_fault(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
							CREATE TRIGGER fail_launch_commit AFTER UPDATE OF claim_token ON turns
							BEGIN INSERT INTO launch_fault VALUES('missing-group'); END`)
						roleOK(t, err)
					case "replaced-original-cwd":
						// Even after the exact new key was claimed, swapping the typed CWD must stop launch.
						roleOK(t, os.Rename(typedCWD, typedCWD+"-moved"))
						roleOK(t, os.Mkdir(typedCWD, 0700))
					}
					beforeGate()
					return roleNative(func(_ context.Context, req provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
						calls++
						switch mode {
						case "confirmed", "commit-unknown", "context-after-handback", "policy-after-gate":
						default:
							t.Fatal("synthetic executor entered despite a committed pre-launch change or invalid binding")
						}
						if !strings.Contains(req.Prompt, admission.MandateID) || !strings.Contains(req.Prompt, wake.WorkInputID) ||
							strings.Contains(req.Prompt, detail) || strings.Contains(req.Prompt, token) || strings.Contains(req.Prompt, returnedQuote) {
							t.Fatal("prompt was not confined to the Mandate and input handle")
						}
						id := fmt.Sprintf(`{"input_id":%q}`, wake.WorkInputID)
						if mode == "policy-after-gate" {
							// This runs at entry to a synthetic executor, after the launch
							// gate. Live tools use B's authority now, not the launch permit.
							call("read_work_inputs", id, "", 200)
							revoked := revokePolicy()
							for _, denied := range []string{
								call("read_work_inputs", `{}`, "", 403), call("read_work_inputs", id, "", 403),
								call("ack_work_input", id, "denied-ack", 403), call("submit_result", `{"summary":"denied"}`, "denied-submit", 409),
							} {
								if strings.Contains(denied, detail) || strings.Contains(denied, wake.WorkInputID) {
									t.Fatal("revoked tool disclosed input")
								}
							}
							// B still allows the canonical owner to hand back one input under
							// disabled chat authority, without returning text or granting work.
							returned := call("hand_back_work_input", fmt.Sprintf(`{"input_id":%q,"reason":"当前授权已撤销"}`, older.WorkInputID), "live-return", 200)
							if strings.Contains(returned, returnedQuote) || !strings.Contains(returned, `"disposition":"returned"`) {
								t.Fatal("live handback was not receipt-only")
							}
							var disposition string
							var revision int64
							roleOK(t, f.raw.QueryRow(`SELECT disposition,policy_revision FROM task_work_input_returns WHERE input_id=?`, older.WorkInputID).Scan(&disposition, &revision))
							if disposition != string(domain.TaskWorkInputReturnOriginalAuthorizationUnavailable) || revision != revoked.Revision {
								t.Fatal("handback did not use the latest B authority")
							}
							cancelClaim()
							call("hand_back_work_input", fmt.Sprintf(`{"input_id":%q,"reason":"取消后不得处理"}`, wake.WorkInputID), "cancelled-return", 403)
							return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
						}
						if mode == "context-after-handback" {
							contextOnly, err := f.db.GetWorkInputForMember(ctx, live, wake.WorkInputID)
							roleOK(t, err)
							if contextOnly.BeforeConstraintRevision != 2 || contextOnly.AfterConstraintRevision != contextOnly.BeforeConstraintRevision ||
								contextOnly.BeforeConstraint != returnedQuote || contextOnly.AfterConstraint != returnedQuote || contextOnly.Content != body {
								t.Fatal("fixture did not inherit the returned B proposal's historical snapshot")
							}
							read := func(input, content, quote, disposition string) {
								t.Helper()
								var segment struct {
									Complete bool
									Text     string
								}
								roleOK(t, json.Unmarshal([]byte(call("read_work_inputs", fmt.Sprintf(`{"input_id":%q}`, input), "", 200)), &segment))
								var value struct{ Content, Constraint, Disposition string }
								roleOK(t, json.Unmarshal([]byte(segment.Text), &value))
								if !segment.Complete || value.Content != content || value.Constraint != quote || value.Disposition != disposition {
									t.Fatalf("wrong projected %s input: %+v", disposition, value)
								}
							}
							read(wake.WorkInputID, body, "", "pending")
							read(older.WorkInputID, "", "", "returned")
							read(confirmedID, "请继续 "+admission.TaskID+"。"+confirmedQuote, confirmedQuote, "confirmed")
							call("ack_work_input", id, "context-ack", 200)
							read(wake.WorkInputID, body, "", "confirmed")
							return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
						}
						call("list_members", `{}`, "", 403)
						call("report_work", `{}`, "no-report", 404)
						call("hand_back_work", `{}`, "no-task-handback", 404)
						call("mcp", `{}`, "", 403)
						call("read_work_inputs", id[:len(id)-1]+`,"limit":1}`, "", 400)
						call("submit_result", `{"summary":"text","artifact_versions":[]}`, "refs", 400)
						call("submit_result", `{"summary":"too early"}`, "premature", 409)
						if mode == "commit-unknown" {
							_, err := f.raw.Exec(`CREATE TABLE commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
								CREATE TRIGGER ack_commit_fault AFTER INSERT ON mutation_idempotency
								BEGIN INSERT INTO commit_guard VALUES('nonexistent-group'); END`)
							roleOK(t, err)
							text := call("ack_work_input", id, "unknown-key", 500)
							if !strings.Contains(text, "未确认") || strings.Contains(text, "commit_guard") {
								t.Fatal("B commit uncertainty leaked details or lost reconciliation guidance")
							}
							var n int
							roleOK(t, f.raw.QueryRow(`SELECT count(*) FROM task_work_input_consumptions`).Scan(&n))
							if n != 0 {
								t.Fatal("failed COMMIT left a durable confirmation")
							}
							return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
						}
						var page struct {
							Items []struct {
								InputID string `json:"input_id"`
							}
							NextCursor string `json:"next_cursor"`
						}
						roleOK(t, json.Unmarshal([]byte(call("read_work_inputs", `{"limit":1}`, "", 200)), &page))
						if len(page.Items) != 1 || page.Items[0].InputID != wake.WorkInputID || page.NextCursor == "" {
							t.Fatal("directory lost the exact B order or cursor")
						}
						if next := call("read_work_inputs", fmt.Sprintf(`{"before":%q}`, page.NextCursor), "", 200); !strings.Contains(next, older.WorkInputID) {
							t.Fatal("cursor lost the older input")
						}
						call("read_work_inputs", fmt.Sprintf(`{"before":%q}`, page.NextCursor+"A"), "", 400)
						var contents strings.Builder
						for offset := 0; ; {
							var segment struct {
								Text     string
								Complete bool
								Next     int `json:"next_offset"`
							}
							roleOK(t, json.Unmarshal([]byte(call("read_work_inputs", fmt.Sprintf(`{"input_id":%q,"offset":%d,"max_bytes":13}`, wake.WorkInputID, offset), "", 200)), &segment))
							if segment.Next <= offset || len(segment.Text) != segment.Next-offset || segment.Next > 1000 {
								t.Fatal("segment did not advance in UTF-8 bytes")
							}
							contents.WriteString(segment.Text)
							if segment.Complete {
								break
							}
							offset = segment.Next
						}
						if !strings.Contains(contents.String(), detail) {
							t.Fatal("segment reconstruction lost the input")
						}
						call("submit_result", `{"summary":"read is not acknowledgement"}`, "still-premature", 409)
						returned := fmt.Sprintf(`{"input_id":%q,"reason":"超出原文本范围"}`, older.WorkInputID)
						if got := call("hand_back_work_input", returned, "one-input", 200); !strings.Contains(got, `"disposition":"returned"`) || got != call("hand_back_work_input", returned, "one-input", 200) {
							t.Fatal("single-input handback receipt was not stable")
						}
						if got := call("ack_work_input", id, "ack", 200); !strings.Contains(got, `"disposition":"confirmed"`) || got != call("ack_work_input", id, "ack", 200) {
							t.Fatal("confirmation receipt was not stable")
						}
						const body = `{"summary":"完成原文本摘要。"}`
						if got := call("submit_result", body, "submit", 201); !strings.Contains(got, `"artifact_version_count":0`) || got != call("submit_result", body, "submit", 201) {
							t.Fatal("submission receipt was not stable")
						}
						task, err := f.db.GetTask(ctx, f.group, admission.TaskID)
						roleOK(t, err)
						sub, err := f.db.GetTaskSubmission(ctx, f.group, task.ID, task.CurrentSubmissionID)
						roleOK(t, err)
						if task.Status != domain.TaskStatusSubmitted || sub.AttemptID == prior.AttemptID || sub.SubmittedBy != f.worker.ID || len(sub.ArtifactVersions) != 0 {
							t.Fatal("wrong typed Attempt or submission scope")
						}
						return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted} // No model ran.
					}), nil
				},
			}
			f.runtime, err = platform.NewMemberToolRuntime(f.db, f.options)
			roleOK(t, err)
			defer f.runtime.Close()
			tools := f.runtime.Tools()
			if tools.WorkInputsEnabled() {
				t.Fatal("fresh profile enabled the legacy intake seam")
			}
			routes := append(httpapi.MemberToolRoutes(tools), httpapi.MemberRoleToolRoutes(tools)...)
			routes = append(routes, httpapi.MemberMCPRoutes(tools)...)
			server, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
			roleOK(t, err)
			f.handler = server.Handler
			opts.MemberTools = f.runtime
			prep, err := platform.NewPreparer(f.db, opts)
			roleOK(t, err)
			assertBlocked := func(code string) {
				t.Helper()
				if got, err := prep.InspectRoleReadiness(ctx, queued); err != nil || got.Blocked == nil || got.Blocked.Code != code {
					t.Fatalf("inspection did not report %s: %+v %v", code, got, err)
				}
				var blocked *platform.RolePreparationBlocked
				if got, err := prep.Prepare(ctx, queued); got.Executor != nil || !errors.As(err, &blocked) || blocked.Code != code {
					t.Fatalf("preparation did not report %s: %v", code, err)
				}
			}
			var replacedKey string
			if mode == "replaced-original-cwd" {
				assertPriorFence := func(want domain.TurnState) {
					t.Helper()
					assertBlocked("prior_turn_unsettled")
					if _, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: queued.Delivery.ID,
						MaxActive: f.options.RoleMaxActive, ResourceKeys: []string{"different-resource"}}); !errors.Is(err, store.ErrTurnBusy) {
						t.Fatalf("a different key bypassed the prior claim: %v", err)
					}
					turn, err := f.db.GetTurn(ctx, f.group, oldScope.Claim.TurnID)
					roleOK(t, err)
					held, err := f.db.HasResourceClaim(ctx, originalPrep.ResourceKeys[0])
					roleOK(t, err)
					if !held || turn.State != want || turn.ClaimToken != oldScope.Claim.Token {
						t.Fatal("the prior claim was released or replaced")
					}
				}
				_, err := f.db.RequestTurnCancel(ctx, f.group, oldScope.Claim.TurnID)
				roleOK(t, err)
				assertPriorFence(domain.TurnStateCancelRequested)
				settle(oldScope.Claim, provider.OutcomeUnknown)
				assertPriorFence(domain.TurnStateUnknown)
				// Only authoritative terminal evidence may release the original resource.
				terminal, err := f.db.ApplyTurnReceipt(ctx, oldScope.Claim, provider.ExecutionReceipt{
					Outcome: provider.OutcomeAborted, Terminal: provider.OutcomeAborted, NativeSessionID: "synthetic",
					Started: true, Accepted: true, DirectChildReaped: true, OutputComplete: true,
				})
				roleOK(t, err)
				held, err := f.db.HasResourceClaim(ctx, originalPrep.ResourceKeys[0])
				roleOK(t, err)
				if held || terminal.State != domain.TurnStateAborted {
					t.Fatal("terminal settlement did not remove the original claim")
				}
				roleOK(t, os.Rename(originalPrep.Request.CWD, originalPrep.Request.CWD+"-moved"))
				roleOK(t, os.Mkdir(originalPrep.Request.CWD, 0700))
				roleOK(t, os.WriteFile(filepath.Join(originalPrep.Request.CWD, "not-task-work"), []byte("old path replacement"), 0600))
				keys, err := scheduler.WorkspaceResourceKeys([]string{originalPrep.Request.CWD})
				roleOK(t, err)
				replacedKey = keys[0]
				rootInfo, err := os.Stat(private)
				roleOK(t, err)
				if got, err := prep.InspectRoleReadiness(ctx, queued); err != nil || got.Blocked != nil || got.HistoryLimit != 0 {
					t.Fatalf("settled pure-text work was not ready: %+v %v", got, err)
				}
				after, err := os.Stat(private)
				roleOK(t, err)
				if !rootInfo.ModTime().Equal(after.ModTime()) {
					t.Fatal("inspection provisioned a directory")
				}
			}
			prepared, err := prep.Prepare(ctx, queued)
			roleOK(t, err)
			typedCWD = prepared.Request.CWD
			if mode == "replaced-original-cwd" {
				if typedCWD == originalPrep.Request.CWD || slices.Contains(prepared.ResourceKeys, replacedKey) || slices.Equal(prepared.ResourceKeys, originalPrep.ResourceKeys) {
					t.Fatal("typed work adopted the replaced original directory instead of a dedicated CWD")
				}
				keys, err := scheduler.WorkspaceResourceKeys([]string{typedCWD})
				roleOK(t, err)
				again, err := prep.Prepare(ctx, queued)
				roleOK(t, err)
				if filepath.Dir(typedCWD) != private || typedCWD != again.Request.CWD || !slices.Equal(keys, prepared.ResourceKeys) || !slices.Equal(keys, again.ResourceKeys) {
					t.Fatal("typed CWD was not stable under the private root with its own exact key")
				}
				// Even a real 0700 legacy directory moved to the typed name is not a C binding.
				roleOK(t, os.Rename(typedCWD, typedCWD+"-saved"))
				roleOK(t, os.Rename(originalPrep.Request.CWD, typedCWD))
				assertBlocked("typed_private_cwd_unavailable")
				roleOK(t, os.Rename(typedCWD, originalPrep.Request.CWD))
				roleOK(t, os.Rename(typedCWD+"-saved", typedCWD))
			}
			if token != "" {
				t.Fatal("preparation acquired tool authority")
			}
			keys := slices.Clone(prepared.ResourceKeys)
			if mode == "wrong-resource" {
				keys = []string{"wrong-key"}
			}
			if mode == "extra-resource" {
				keys = append(keys, "extra-key")
			}
			live = claim(queued.Delivery.ID, keys)
			prepared.Request.RunID, prepared.Request.RuntimeDir = live.Claim.TurnID, f.root
			out := prepared.Executor.Execute(ctx, prepared.Request, nil)
			want := provider.OutcomeUnknown
			if mode == "confirmed" || mode == "context-after-handback" {
				want = provider.OutcomeNotStarted
			}
			wantCalls := 0
			if mode == "confirmed" || mode == "context-after-handback" || mode == "commit-unknown" || mode == "policy-after-gate" {
				wantCalls = 1
			}
			if strings.HasSuffix(mode, "-before-gate") && !beforeGateChanged {
				t.Fatal("controller did not reach the pre-launch mutation")
			}
			if out.Outcome != want || calls != wantCalls || out.NativeRef != nil ||
				mode == "replaced-original-cwd" && (token == "" || !strings.Contains(out.Reason, "专用目录身份")) {
				t.Fatalf("wrong guarded result: %v calls=%d", out.Outcome, calls)
			}
			var boundAttempts, nativeRecords int
			roleOK(t, f.raw.QueryRow(`SELECT (SELECT count(*) FROM task_attempts WHERE turn_id=?),
				(SELECT count(*) FROM native_run_bindings WHERE turn_id=?)`, live.Claim.TurnID, live.Claim.TurnID).Scan(&boundAttempts, &nativeRecords))
			wantAttempts := 1 // B's original continuation binder ran once, before construction.
			if mode == "wrong-resource" || mode == "extra-resource" {
				wantAttempts = 0
			}
			if boundAttempts != wantAttempts || nativeRecords != 0 {
				t.Fatalf("startup changed B binding or wrote a native record: attempts=%d native=%d", boundAttempts, nativeRecords)
			}
			if mode == "failed-launch-commit" {
				var faultRows int
				roleOK(t, f.raw.QueryRow(`SELECT count(*) FROM launch_fault`).Scan(&faultRows))
				if faultRows != 0 {
					t.Fatal("failed launch commit left durable effects")
				}
				_, err := f.raw.Exec(`DROP TRIGGER fail_launch_commit`)
				roleOK(t, err)
			}
			if again := prepared.Executor.Execute(ctx, prepared.Request, nil); again.Outcome != provider.OutcomeUnknown {
				t.Fatal("fresh executor was reusable")
			}
			if token != "" {
				call("ack_work_input", fmt.Sprintf(`{"input_id":%q}`, wake.WorkInputID), "retired", 403)
				if _, err := os.Stat(configPath); !os.IsNotExist(err) {
					t.Fatal("config survived revocation")
				}
			}
			turn := settle(live.Claim, out.Outcome)
			if want == provider.OutcomeUnknown {
				if turn.State != domain.TurnStateUnknown || turn.ClaimToken != live.Claim.Token ||
					((mode == "cancel-before-gate" || mode == "policy-after-gate") && turn.CancelRequestedAt == nil) {
					t.Fatal("unknown settled, changed claim ownership or lost cancellation")
				}
				if _, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: queued.Delivery.ID,
					MaxActive: f.options.RoleMaxActive, ResourceKeys: []string{"different-resource"}}); !errors.Is(err, store.ErrTurnBusy) {
					t.Fatalf("unknown followup was claimable again: %v", err)
				}
			}
			for _, key := range keys {
				held, err := f.db.HasResourceClaim(ctx, key)
				roleOK(t, err)
				if held != (want == provider.OutcomeUnknown) {
					t.Fatal("resource settlement did not match B outcome")
				}
			}
		})
	}
}
