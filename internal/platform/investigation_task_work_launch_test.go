package platform_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

// Real v30 queue/claim, Attempt, issued HTTP configuration and final B commit.
// Native invocation is in-process; no Pi executable or model is started.
func TestInvestigationTaskWorkLaunchSQLite(t *testing.T) {
	for _, tc := range []struct {
		kind domain.TaskWorkSourceKind
		mode string
	}{
		{domain.TaskWorkSourceFollowup, "admitted"},
		{domain.TaskWorkSourceDelegation, "admitted"},
		{domain.TaskWorkSourceReport, "admitted"},
		{domain.TaskWorkSourceFollowup, "commit-error"},
		{domain.TaskWorkSourceDelegation, "no-callback"},
		{domain.TaskWorkSourceDelegation, "revoked-token"},
		{domain.TaskWorkSourceDelegation, "wrong-resource"},
		{domain.TaskWorkSourceDelegation, "cancel-after-callback"},
		{domain.TaskWorkSourceDelegation, "config-after-commit"},
	} {
		t.Run(string(tc.kind)+"/"+tc.mode, func(t *testing.T) {
			ctx := t.Context()
			f := newBoundWorkReportFixture(t)
			f.delegate(t, nil, nil, false)
			msgID := f.delegation.MessageID
			var sourceText string
			var wantOps []string
			switch tc.kind {
			case domain.TaskWorkSourceFollowup:
				f.settleWork(t, f.claimWork(t, f.delegation.DeliveryID), false)
				const quote = "仅核对第一条材料的引用。"
				input, _ := queueInvestigationWorkInput(t, f, quote, false)
				msgID, sourceText = input.WorkWakeMessageID, quote
				wantOps = append([]string{platform.ReadInvestigationChannelOperation}, provider.PiFreshTaskWorkToolNames()...)
			case domain.TaskWorkSourceDelegation:
				wantOps = []string{platform.ReportWorkOperation} // No parent's material grants.
			case domain.TaskWorkSourceReport:
				child := f.claimWork(t, f.delegation.DeliveryID)
				f.acceptWork(t, child)
				const summary = "已在本轮报告中核对来源。"
				report, err := f.engine.ReportWork(ctx, child, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: summary}, "report-for-owner")
				roleOK(t, err)
				f.settleWork(t, child, true)
				msgID, sourceText = report.MessageID, summary
				wantOps = append([]string{platform.ReadWorkReportOperation}, provider.PiFreshTaskWorkToolNames()...)
			}
			msg, err := f.db.GetMessage(ctx, f.group, msgID)
			roleOK(t, err)
			wake := msg.Deliveries[0]
			work, err := f.engine.ResolveTaskWorkDelivery(ctx, f.group, wake.ID)
			roleOK(t, err)
			if work.SourceKind != tc.kind || work.Blocked != "" {
				t.Fatal("expected a persisted ready B wake")
			}
			assignment, err := f.db.GetAssignment(ctx, f.group, work.AssignmentID)
			roleOK(t, err)
			root := t.TempDir()
			private, cli := filepath.Join(root, "private"), filepath.Join(root, "metadata-only")
			roleOK(t, os.Mkdir(private, 0700))
			roleOK(t, os.WriteFile(cli, nil, 0700))
			bridgeRoot, err := filepath.Abs("../../extensions/pi-bridge")
			roleOK(t, err)
			selected := provider.Provider{ID: f.worker.ProviderID, Name: provider.Pi, Path: cli, Installation: provider.Installed, Version: "1.4.0"}
			runCtx, stop := context.WithTimeout(ctx, 15*time.Second)
			defer stop()
			var prepared scheduler.PreparedExecution
			var path, token, resource string
			var physicalClaim store.TurnClaim
			var constructed, commitCalls, nativeCalls int
			var admitted, commitReturned bool
			launch := platform.NewPiInvestigationTaskWorkLaunch(filepath.Join(bridgeRoot, "fresh-investigation-work.ts"), 0,
				func(check context.Context, plan store.FreshTaskWorkLaunch, admit func() bool) error {
					commitCalls++
					if constructed != 1 || nativeCalls != 0 || tc.mode == "revoked-token" || check.Err() != nil {
						t.Fatal("commit callback reached in the wrong phase")
					}
					if plan.Delivery != work || plan.Scope.Claim != physicalClaim || plan.Scope.MemberID != work.MemberID ||
						plan.Scope.ConversationID != work.ConversationID || plan.Scope.TopicID != work.TopicID || plan.RequestID != assignment.Assignment.RequestID ||
						plan.ProviderID != selected.ID || plan.Model != f.worker.Model || plan.ResourceKey != resource {
						t.Fatal("fixture did not receive the exact plan and scope")
					}
					var n int
					roleOK(t, f.raw.QueryRowContext(check, `SELECT count(*) FROM task_work_continuations WHERE turn_id=?`, plan.Scope.Claim.TurnID).Scan(&n))
					if tc.kind.IsOwnerContinuation() {
						var bound string
						roleOK(t, f.raw.QueryRowContext(check, `SELECT attempt_id FROM task_work_continuations WHERE turn_id=? AND delivery_id=?`, plan.Scope.Claim.TurnID, wake.ID).Scan(&bound))
						if n != 1 || plan.AttemptID != bound || bound == f.original.AttemptID || bound == "" {
							t.Fatal("owner launch lacked the B-bound current Attempt")
						}
					} else if n != 0 || plan.AttemptID != "" {
						t.Fatal("delegate launch acquired an owner Attempt")
					}
					var held string
					roleOK(t, f.raw.QueryRowContext(check, `SELECT resource_key FROM resource_claims WHERE turn_id=?`, plan.Scope.Claim.TurnID).Scan(&held))
					wantHeld := plan.ResourceKey
					if tc.mode == "wrong-resource" {
						wantHeld = "wrong-resource"
					}
					if held != wantHeld {
						t.Fatal("scheduler did not claim the intended physical resource")
					}
					if tc.mode == "no-callback" {
						return nil // Deliberate fixture violation of the trusted contract.
					}
					err := f.db.CommitInvestigationTaskWorkLaunch(check, store.InvestigationTaskWorkLaunch(plan), func() bool {
						admitted = admit()
						return admitted
					})
					switch tc.mode {
					case "wrong-resource":
						if admitted || !errors.Is(err, store.ErrMemberToolDenied) {
							t.Fatal("B spent the callback despite a wrong resource or did not definitively deny it")
						}
						return err
					case "commit-error":
						if err == nil || !admitted {
							t.Fatal("fixture did not fail B COMMIT after the callback")
						}
						var faultRows int
						roleOK(t, f.raw.QueryRowContext(check, `SELECT count(*) FROM launch_fault`).Scan(&faultRows))
						if faultRows != 0 {
							t.Fatal("failed B launch left fixture writes")
						}
						// Do not inject a second failure into scheduler receipt IO.
						_, dropErr := f.raw.ExecContext(check, `DROP TRIGGER fail_typed_launch_commit`)
						roleOK(t, dropErr)
						return err
					}
					roleOK(t, err)
					if !admitted || nativeCalls != 0 {
						t.Fatal("B committed without exhausting the callback, or native ran inside it")
					}
					switch tc.mode {
					case "cancel-after-callback":
						stop()
					case "config-after-commit":
						bytes, err := os.ReadFile(path)
						roleOK(t, err)
						bytes[0] = '[' // Same inode and size; the digest must still refuse it.
						roleOK(t, os.WriteFile(path, bytes, 0600))
					}
					commitReturned = true
					return nil
				})
			construct := launch.Construct
			launch.Construct = func(check context.Context, p provider.Provider, b provider.PiFreshInvestigationBinding) (provider.Executor, error) {
				constructed++
				if _, err := construct(check, p, b); err != nil {
					return nil, err // Actual pinned construction; never invoke this executor.
				}
				path = b.ConfigPath
				bytes, err := os.ReadFile(path)
				roleOK(t, err)
				var cfg struct {
					Token, Endpoint string
					RunID           string `json:"run_id"`
					provider.PiFreshInvestigationProfile
				}
				roleOK(t, json.Unmarshal(bytes, &cfg))
				token = cfg.Token
				if filepath.Dir(path) != f.options.ConfigRoot || token == "" || token == physicalClaim.Token || cfg.Endpoint != f.options.Endpoint ||
					cfg.RunID != physicalClaim.TurnID || cfg.RunID != b.RunID || sha256.Sum256(bytes) != b.ConfigSHA256 || cfg.Action != domain.ChatActionReadOnlyInvestigation ||
					cfg.SourceKind != tc.kind || !slices.Equal(cfg.Operations, wantOps) || !slices.Equal(b.Profile.Operations, wantOps) {
					t.Fatal("construction did not receive the exact closed typed bootstrap")
				}
				switch tc.mode {
				case "revoked-token":
					f.runtime.Tools().Revoke(token)
				case "commit-error":
					// Fixture-only fault on B's scope write. All checks and the
					// callback can pass, but the real COMMIT must fail.
					_, err := f.raw.ExecContext(check, `CREATE TABLE launch_fault(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
						CREATE TRIGGER fail_typed_launch_commit AFTER UPDATE OF claim_token ON turns
						BEGIN INSERT INTO launch_fault VALUES('missing-group'); END`)
					roleOK(t, err)
				}
				return roleNative(func(check context.Context, req provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
					nativeCalls++
					if tc.mode != "admitted" || !admitted || !commitReturned || check.Err() != nil || req != prepared.Request {
						t.Fatal("native entered before successful final admission or with a changed request")
					}
					ops, err := f.runtime.Tools().TransportOperations(check, token)
					roleOK(t, err)
					if !slices.Equal(ops, wantOps) || strings.Contains(req.Prompt, token) || strings.Contains(req.Prompt, f.source.Content) ||
						sourceText != "" && strings.Contains(req.Prompt, sourceText) || tc.kind == domain.TaskWorkSourceDelegation && !strings.Contains(req.Prompt, f.delegation.Question) {
						t.Fatal("native context exceeded or lost the typed plan")
					}
					switch tc.kind {
					case domain.TaskWorkSourceFollowup:
						f.expectHTTP(t, token, "read_work_inputs", fmt.Sprintf(`{"input_id":%q}`, work.SourceID), "", http.StatusOK)
					case domain.TaskWorkSourceReport:
						f.expectHTTP(t, token, platform.ReadWorkReportOperation, `{}`, "", http.StatusOK)
					}
					f.expectHTTP(t, token, "read_channel", `{}`, "", http.StatusForbidden)
					roleOK(t, emit(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: req.RunID}))
					roleOK(t, emit(provider.Event{Kind: provider.TerminalEvent, NativeSessionID: req.RunID, Terminal: provider.OutcomeSucceeded}))
					return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Started: true, Accepted: true,
						Terminal: provider.OutcomeSucceeded, NativeSessionID: req.RunID, DirectChildReaped: true, OutputComplete: true}
				}), nil
			}
			f.options.InvestigationTaskWork = launch
			f.runtime, err = platform.NewMemberToolRuntime(f.db, f.options)
			roleOK(t, err)
			f.mount(t)
			prep, err := platform.NewPreparer(f.db, platform.PrepareOptions{Providers: []provider.Provider{selected}, PrivateWorkspaceRoot: private,
				MemberTools: f.runtime, PiMemberToolsExtension: filepath.Join(bridgeRoot, "index.ts")})
			roleOK(t, err)
			err = scheduler.Run(runCtx, f.db, scheduler.Options{Recovered: true, RuntimeDir: f.root, MaxActive: f.options.RoleMaxActive},
				func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
					if pending.Delivery.ID != wake.ID {
						return scheduler.PreparedExecution{}, scheduler.ErrNotReady
					}
					p, err := prep.Prepare(ctx, pending)
					if err != nil {
						return scheduler.PreparedExecution{}, err
					}
					keys, err := scheduler.WorkspaceResourceKeys([]string{p.Request.CWD})
					if err != nil || !slices.Equal(keys, p.ResourceKeys) || filepath.Dir(p.Request.CWD) != private || p.Request.RunID != "" || p.Request.RuntimeDir != "" {
						return scheduler.PreparedExecution{}, errors.New("fixture: invalid prepared workspace or request")
					}
					resource, prepared = keys[0], p
					if tc.mode == "wrong-resource" {
						p.ResourceKeys = []string{"wrong-resource"}
					}
					p.Executor = roleNative(func(run context.Context, req provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
						defer stop()
						out := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
						done := make(chan struct{})
						go func() {
							// A fixture assertion must not Goexit the scheduler's receipt writer.
							defer close(done)
							turn, err := f.db.GetTurn(run, f.group, req.RunID)
							roleOK(t, err)
							physicalClaim = store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
							prepared.Request.RunID, prepared.Request.RuntimeDir = turn.ID, f.root
							out = prepared.Executor.Execute(run, req, emit)
						}()
						<-done
						return out
					})
					return p, nil
				})
			if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() || physicalClaim.TurnID == "" {
				t.Fatalf("normal scheduler did not claim and drain the fixture: %v", err)
			}
			wantCommitCalls, wantNativeCalls := 1, 0
			wantState, wantClaims := domain.TurnStateUnknown, 1
			if tc.mode == "admitted" {
				wantNativeCalls, wantState, wantClaims = 1, domain.TurnStateSucceeded, 0
			} else if tc.mode == "revoked-token" {
				wantCommitCalls = 0
			}
			if constructed != 1 || commitCalls != wantCommitCalls || nativeCalls != wantNativeCalls || token == "" {
				t.Fatalf("unexpected construct/commit/native counts: %d/%d/%d", constructed, commitCalls, nativeCalls)
			}
			f.claims(t, roleRun{claim: physicalClaim}, wantState, wantClaims)
			turn, err := f.db.GetTurn(ctx, f.group, physicalClaim.TurnID)
			roleOK(t, err)
			if turn.ReplyMessageID != "" || (wantState == domain.TurnStateUnknown && (turn.AcceptedAt != nil || turn.NativeSessionID != "")) {
				t.Fatal("fixture setup published a reply or treated a denied launch as accepted")
			}
			if files, err := os.ReadDir(f.options.ConfigRoot); err != nil || len(files) != 0 {
				t.Fatal("issued config outlived revoke/drain", err)
			}
			if _, err := f.runtime.Tools().TransportOperations(ctx, token); !errors.Is(err, store.ErrMemberToolDenied) {
				t.Fatal("typed token not revoked on exit", err)
			}
			if retry := prepared.Executor.Execute(ctx, prepared.Request, nil); retry.Outcome != provider.OutcomeUnknown || nativeCalls != wantNativeCalls {
				t.Fatal("prepared typed run was replayed")
			}
		})
	}
}
