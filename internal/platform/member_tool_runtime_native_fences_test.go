package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func claimNativeFenceFixture(t *testing.T, f *rolePrepareFixture, prepared scheduler.PreparedExecution) (provider.ExecuteRequest, store.TurnClaim) {
	t.Helper()
	run := prepared.Executor.(*memberToolRuntimeRun)
	turn, err := f.s.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: run.binding.DeliveryID, MaxActive: 2, ResourceKeys: prepared.ResourceKeys})
	prepareRoleOK(t, err)
	req := prepared.Request
	req.RunID, req.RuntimeDir = turn.ID, f.runtimeOptions.RoleExecutionRoot
	return req, store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
}

func TestNativeSessionRuntimeSameInodeConfigDrift(t *testing.T) {
	for _, name := range []string{provider.Pi, provider.Codex} {
		for _, stage := range []string{"before-adapter", "after-adapter"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				ctx := context.Background()
				f, prepared := nativeSessionPreparation(t, "generic")
				if name == provider.Pi {
					useReleasePiBridge(t, f)
					f.opts.Providers[1].Name, f.opts.Providers[1].Version = provider.Pi, "1.4.0"
					f.opts.NativePiThinking = "off"
					f.exec(t, "UPDATE members SET model='fixture/model' WHERE id=?", f.member.ID)
					var err error
					prepared, err = f.preparer(t).Prepare(ctx, f.pending)
					prepareRoleOK(t, err)
				}
				// A second valid in-memory capability makes syntax checking insufficient.
				other := f.message(t, f.group.ID, "separate-topic", "user", "fixture", f.worker.ID)
				turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: other.Deliveries[0].ID, MaxActive: 2})
				prepareRoleOK(t, err)
				tools := f.opts.MemberTools.Tools()
				otherToken, err := tools.Issue(ctx, store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}, MemberToolGrants{Operations: []string{"list_members"}})
				prepareRoleOK(t, err)
				t.Cleanup(func() { tools.Revoke(otherToken) })
				run := prepared.Executor.(*memberToolRuntimeRun)
				factory := run.nativeSession.factory
				calls, constructed, invoked := 0, false, false
				var currentToken string
				run.nativeSession.factory = func(config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
					calls++
					path := config.Path
					_, currentToken = memberToolRuntimeConfig(t, path)
					rewrite := func() {
						before, err := os.Stat(path)
						prepareRoleOK(t, err)
						data, err := json.Marshal(map[string]string{"endpoint": "http://127.0.0.1:2/api/v1/member-tools", "token": otherToken})
						prepareRoleOK(t, err)
						prepareRoleOK(t, os.WriteFile(path, append(data, '\n'), 0600))
						prepareRoleOK(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
						after, err := os.Stat(path)
						prepareRoleOK(t, err)
						if !os.SameFile(before, after) || before.Size() != after.Size() || after.Mode() != 0600 {
							t.Fatal("fixture did not preserve config identity, size and mode")
						}
					}
					if stage == "before-adapter" {
						rewrite()
					}
					e, err := factory(config, names, resume, binding)
					constructed = e != nil && err == nil
					if stage == "after-adapter" {
						rewrite()
					}
					if !constructed {
						return e, err
					}
					return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
						invoked = true
						return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}
					}), nil
				}
				run.factory = func(string, string, []string) (provider.Executor, error) {
					t.Fatal("config drift attempted legacy fallback")
					return nil, nil
				}
				req, _ := claimNativeFenceFixture(t, f, prepared)
				got := run.Execute(ctx, req, nil)
				if calls != 1 || constructed != (stage == "after-adapter") || invoked || got.Started || got.Outcome != provider.OutcomeNotStarted {
					t.Fatalf("config drift crossed a fence: factories=%d constructed=%t invoked=%t outcome=%s", calls, constructed, invoked, got.Outcome)
				}
				if _, err := tools.TransportOperations(ctx, currentToken); !errors.Is(err, store.ErrMemberToolDenied) {
					t.Fatal("aborted start retained the issued capability")
				}
				_, err = tools.TransportOperations(ctx, otherToken)
				prepareRoleOK(t, err)
				memberToolRuntimeEmpty(t, f.runtimeOptions.ConfigRoot)
				memberToolRuntimeEmpty(t, req.RuntimeDir)
			})
		}
	}
}

func TestNativeSessionRuntimeRechecksAfterConstruction(t *testing.T) {
	for _, tc := range []struct{ purpose, change string }{
		{"generic", "workspace"}, {domain.ChatIntakePurpose, "workspace"}, {domain.ChatIntakePurpose, "chat-policy"},
		{"generic", "resource"}, {"generic", "issued-grants"}, {"generic", "selector-error"},
	} {
		t.Run(tc.purpose+"/"+tc.change, func(t *testing.T) {
			ctx := context.Background()
			f, prepared := nativeSessionPreparation(t, tc.purpose)
			p := f.preparer(t)
			run := prepared.Executor.(*memberToolRuntimeRun)
			req, claim := claimNativeFenceFixture(t, f, prepared)
			initial, err := f.s.GetMessage(ctx, f.group.ID, f.ordinary.Delivery.MessageID)
			prepareRoleOK(t, err)
			factory, calls, invoked := run.nativeSession.factory, 0, false
			run.nativeSession.factory = func(config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
				calls++
				_, err := factory(config, names, resume, binding)
				prepareRoleOK(t, err)
				switch tc.change {
				case "workspace":
					_, err = p.AuthorizeWorkspaceGrant(ctx, f.group.ID, run.binding.MemberID, WorkspaceGrantInput{Root: t.TempDir(), Scope: "read_only"}, "factory-grant")
					prepareRoleOK(t, err)
				case "chat-policy":
					_, err = f.s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.group.ID, TopicID: initial.TopicID}, domain.ChatPolicyRequest{Action: domain.ChatActionTextSubmission, MaxAdmissions: 1}, 0, "factory-policy")
					prepareRoleOK(t, err)
				case "resource":
					f.exec(t, "DELETE FROM resource_claims WHERE turn_id=?", claim.TurnID)
				case "issued-grants":
					tools := f.opts.MemberTools.Tools()
					tools.mu.Lock()
					for key, entry := range tools.capabilities {
						if entry.scope.Claim == claim {
							entry.grants.MaxGroupMembers++
							tools.capabilities[key] = entry
						}
					}
					tools.mu.Unlock()
				case "selector-error":
					f.exec(t, "DROP TABLE native_run_bindings")
				}
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					invoked = true
					return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}
				}), nil
			}
			run.factory = func(string, string, []string) (provider.Executor, error) {
				t.Fatal("native startup recheck attempted legacy fallback")
				return nil, nil
			}
			got := run.Execute(ctx, req, nil)
			want := provider.OutcomeNotStarted
			if tc.change == "selector-error" {
				want = provider.OutcomeUnknown // Missing ledger cannot prove absence.
			}
			if calls != 1 || invoked || got.Started || got.Outcome != want || got.Terminal != "" {
				t.Fatalf("stale startup selection reached execution: factories=%d invoked=%t outcome=%s", calls, invoked, got.Outcome)
			}
			memberToolRuntimeEmpty(t, f.runtimeOptions.ConfigRoot)
			memberToolRuntimeEmpty(t, req.RuntimeDir)
		})
	}
}

func TestNativeSessionRuntimeRevokeAfterSelectionBeforeCommit(t *testing.T) {
	for _, purpose := range []string{"generic", "coordinator"} {
		t.Run(purpose, func(t *testing.T) {
			ctx := context.Background()
			f, prepared := nativeSessionPreparation(t, purpose)
			req, claim := claimNativeFenceFixture(t, f, prepared)
			run := prepared.Executor.(*memberToolRuntimeRun)
			tools := run.runtime.tools
			var token string
			rejectNativeFenceInvocation(t, run, &token, nil)
			queries := 0
			selectSession := run.nativeSession.selectSession
			run.nativeSession.selectSession = func(ctx context.Context, scope store.MemberToolScope, binding store.NativeRunBinding) (store.NativeSessionSelection, error) {
				selected, err := selectSession(ctx, scope, binding)
				prepareRoleOK(t, err)
				queries++
				// Same registry and claim. Nothing in Store or on disk changes.
				tools.RevokeTurn(claim.TurnID)
				return selected, nil
			}
			got := run.Execute(ctx, req, nil)
			if queries != 1 || token == "" || got.Started || got.Outcome != provider.OutcomeUnknown {
				t.Fatal("a completed registry revoke did not prevent launch")
			}
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, got)
			prepareRoleOK(t, err)
			if turn.State != domain.TurnStateUnknown || turn.ClaimToken != claim.Token {
				t.Fatal("denied launch settled or changed an uncertain claim")
			}
			assertNativeFenceBarrier(t, f, claim, 1)
			if purpose == "coordinator" {
				owner := run.runtime.roleFence.records[claim.TurnID]
				if owner == nil || owner.invoked || !owner.localDone {
					t.Fatal("revoked role committed invocation or prematurely released its owner")
				}
			}
			memberToolRuntimeEmpty(t, req.RuntimeDir)
			memberToolRuntimeEmpty(t, run.runtime.configRoot)
		})
	}
}

func TestNativeSessionRuntimeRevokeAfterCommitDoesNotWaitForExecutor(t *testing.T) {
	for _, purpose := range []string{"generic", "coordinator"} {
		t.Run(purpose, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			f, prepared := nativeSessionPreparation(t, purpose)
			req, claim := claimNativeFenceFixture(t, f, prepared)
			run := prepared.Executor.(*memberToolRuntimeRun)
			tools := run.runtime.tools
			entered, release := make(chan string, 1), make(chan struct{})
			run.nativeSession.factory = func(config provider.NativeMemberToolConfig, _ []string, _ *provider.NativeSessionReference, _ json.RawMessage) (provider.Executor, error) {
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					_, token := memberToolRuntimeConfig(t, config.Path)
					entered <- token // The launch is committed, native has not returned.
					select {
					case <-release:
					case <-ctx.Done():
					}
					return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown, ExitCode: 17}
				}), nil
			}
			finished := make(chan provider.ExecutionReceipt, 1)
			go func() { finished <- run.Execute(ctx, req, nil) }()
			var token string
			select {
			case token = <-entered:
			case got := <-finished:
				t.Fatalf("launch denied before revoke: %s", got.Outcome)
			case <-ctx.Done():
				t.Fatal("native executor did not enter")
			}
			revoked := make(chan struct{})
			go func() {
				tools.RevokeTurn(claim.TurnID)
				close(revoked)
			}()
			select {
			case <-revoked:
			case <-ctx.Done():
				t.Fatal("registry lock survived into provider I/O")
			}
			if _, err := tools.TransportOperations(ctx, token); !errors.Is(err, store.ErrMemberToolDenied) {
				t.Fatal("committed invocation retained tool authority after revoke")
			}
			if turn, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID); err != nil || turn.CancelRequestedAt == nil {
				t.Fatalf("Store could not record cancel while provider was blocked: %v", err)
			}
			close(release)
			select {
			case got := <-finished:
				if !got.Started || got.NativePreflightFailure || got.Outcome != provider.OutcomeUnknown || got.ExitCode != 17 {
					t.Fatal("post-commit revoke/cancel changed actual provider evidence")
				}
				turn, err := f.s.ApplyTurnReceipt(ctx, claim, got)
				prepareRoleOK(t, err)
				if turn.State != domain.TurnStateUnknown || turn.CancelRequestedAt == nil || turn.ClaimToken != claim.Token {
					t.Fatal("unknown invocation did not retain cancellation and exact claim")
				}
				assertNativeFenceBarrier(t, f, claim, 1)
			case <-ctx.Done():
				t.Fatal("native executor did not return")
			}
		})
	}
}

func TestNativeSessionRuntimeFinalFenceClassifiesCurrentEvidence(t *testing.T) {
	for _, tc := range []struct {
		purpose, change string
		want            provider.Outcome
	}{
		{"generic", "only-cwd", provider.OutcomeNotStarted},
		{domain.ChatIntakePurpose, "only-config", provider.OutcomeNotStarted},
		{"generic", "unknown-receipt", provider.OutcomeUnknown},
		{"coordinator", "unknown-receipt", provider.OutcomeUnknown},
		{domain.ChatIntakePurpose, "cancel", provider.OutcomeUnknown},
		{domain.ChatIntakePurpose, "policy", provider.OutcomeNotStarted},
		{"worker", "policy", provider.OutcomeUnknown},
		{"generic", "recover", provider.OutcomeUnknown},
		{"generic", "unverifiable-run-directory", provider.OutcomeUnknown},
	} {
		t.Run(tc.purpose+"/"+tc.change, func(t *testing.T) {
			ctx := context.Background()
			f, prepared := nativeSessionPreparation(t, tc.purpose)
			req, claim := claimNativeFenceFixture(t, f, prepared)
			run := prepared.Executor.(*memberToolRuntimeRun)
			var token, configPath string
			rejectNativeFenceInvocation(t, run, &token, &configPath)
			queries := 0
			var firstJSON string
			var firstSeq int
			selectSession := run.nativeSession.selectSession
			run.nativeSession.selectSession = func(ctx context.Context, scope store.MemberToolScope, binding store.NativeRunBinding) (store.NativeSessionSelection, error) {
				selected, err := selectSession(ctx, scope, binding)
				prepareRoleOK(t, err)
				queries++
				return selected, nil
			}
			factory := run.nativeSession.factory
			run.nativeSession.factory = func(config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
				native, err := factory(config, names, resume, binding)
				prepareRoleOK(t, err)
				if queries != 1 {
					t.Fatal("fixture did not run after a successful Store selection")
				}
				// Only explicit directory/config cases touch the filesystem. The
				// post-selection Store mutations must independently stop launch.
				switch tc.change {
				case "only-cwd":
					prepareRoleOK(t, os.Rename(req.CWD, req.CWD+"-old"))
					prepareRoleOK(t, os.Mkdir(req.CWD, 0700))
				case "only-config":
					data, err := os.ReadFile(configPath)
					prepareRoleOK(t, err)
					data[len(data)-1] = ' ' // Same valid JSON and inode, different issued bytes.
					prepareRoleOK(t, os.WriteFile(configPath, data, 0600))
				case "unknown-receipt":
					first := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true, NativeSessionID: "prior-fixture",
						DirectChildReaped: true, ExitCode: 17, StdoutPath: "/fixture/prior.jsonl", LogsTruncated: true}
					_, err := f.s.ApplyTurnReceipt(ctx, claim, first)
					prepareRoleOK(t, err)
					prepareRoleOK(t, f.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", claim.TurnID).Scan(&firstJSON))
					prepareRoleOK(t, f.db.QueryRow("SELECT seq FROM native_run_bindings WHERE turn_id=?", claim.TurnID).Scan(&firstSeq))
				case "cancel":
					_, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
					prepareRoleOK(t, err)
				case "policy":
					if tc.purpose == domain.ChatIntakePurpose {
						message, err := f.s.GetMessage(ctx, f.group.ID, f.ordinary.Delivery.MessageID)
						prepareRoleOK(t, err)
						_, err = f.s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.group.ID, TopicID: message.TopicID}, domain.ChatPolicyRequest{Action: domain.ChatActionTextSubmission, MaxAdmissions: 1}, 0, "before-launch")
						prepareRoleOK(t, err)
					} else {
						_, err := f.s.SetDispatchPolicy(ctx, f.group.ID, nil, 1, "before-launch")
						prepareRoleOK(t, err)
					}
				case "recover":
					_, err := f.s.RecoverInFlight(ctx)
					prepareRoleOK(t, err)
				case "unverifiable-run-directory":
					prepareRoleOK(t, os.Rename(req.RuntimeDir, req.RuntimeDir+"-old"))
					prepareRoleOK(t, os.Mkdir(req.RuntimeDir, 0700))
				}
				return native, nil
			}
			got := run.Execute(ctx, req, nil)
			if queries != 1 || token == "" || got.Outcome != tc.want || !got.NativePreflightFailure || got.NativeRef != nil || (tc.change == "unknown-receipt") != got.Started {
				t.Fatalf("final startup failure misclassified: queries=%d outcome=%s", queries, got.Outcome)
			}
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, got)
			if tc.change == "recover" {
				if !errors.Is(err, store.ErrStaleClaim) {
					t.Fatalf("old claim applied evidence after recovery: %v", err)
				}
				turn, err = f.s.GetTurn(ctx, claim.GroupID, claim.TurnID)
				prepareRoleOK(t, err)
				if turn.ClaimToken == claim.Token || turn.State != domain.TurnStateUnknown {
					t.Fatal("claim rotation was bypassed")
				}
				var barrier, resources int
				prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM native_run_bindings WHERE turn_id=?", claim.TurnID).Scan(&barrier))
				prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", claim.TurnID).Scan(&resources))
				if barrier != 0 || resources != 1 {
					t.Fatal("a stale claim changed the barrier or retained resource")
				}
			} else {
				prepareRoleOK(t, err)
				if turn.State != domain.TurnState(tc.want) || turn.ClaimToken != claim.Token || (tc.change == "cancel") != (turn.CancelRequestedAt != nil) {
					t.Fatal("the scheduler's Apply did not preserve the exact claim and classification")
				}
				retained := 0
				if tc.want == provider.OutcomeUnknown {
					retained = 1
				}
				seq := assertNativeFenceBarrier(t, f, claim, retained)
				if firstJSON != "" {
					var stored string
					prepareRoleOK(t, f.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", claim.TurnID).Scan(&stored))
					returned, err := json.Marshal(got)
					prepareRoleOK(t, err)
					if stored != firstJSON || string(returned) != firstJSON || seq != firstSeq {
						t.Fatal("startup failure lost the first unknown receipt or noRef barrier")
					}
				}
			}
			memberToolRuntimeEmpty(t, req.RuntimeDir)
			memberToolRuntimeEmpty(t, run.runtime.configRoot)
		})
	}
}

func TestNativeSessionRuntimeFirstReceiptBetweenFailureAndApply(t *testing.T) {
	for _, purpose := range []string{"generic", "coordinator"} {
		t.Run(purpose, func(t *testing.T) {
			ctx := context.Background()
			f, prepared := nativeSessionPreparation(t, purpose)
			req, claim := claimNativeFenceFixture(t, f, prepared)
			run := prepared.Executor.(*memberToolRuntimeRun)
			// A failed first write must not export its candidate not_started as
			// evidence. After rollback another writer records the actual first
			// result, BEFORE the scheduler applies the returned refusal.
			f.exec(t, `CREATE TRIGGER refuse_preflight BEFORE UPDATE OF receipt_json ON turns
				BEGIN SELECT RAISE(ABORT,'fixture refusal'); END`)
			run.nativeSession.factory = func(provider.NativeMemberToolConfig, []string, *provider.NativeSessionReference, json.RawMessage) (provider.Executor, error) {
				return nil, errors.New("fixture factory rejection")
			}
			got := run.Execute(ctx, req, nil)
			if got.Started || got.Outcome != provider.OutcomeUnknown || !got.NativePreflightFailure {
				t.Fatal("uncommitted preflight failure claimed nonexecution")
			}
			var before string
			prepareRoleOK(t, f.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", claim.TurnID).Scan(&before))
			if before != "" {
				t.Fatal("failed preflight write did not roll back")
			}
			f.exec(t, "DROP TRIGGER refuse_preflight")
			_, err := f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown,
				Started: true, Accepted: true, NativeSessionID: "first-unknown", ExitCode: 19, StderrPath: "/fixture/stderr", DirectChildReaped: true})
			prepareRoleOK(t, err)
			_, err = f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
			prepareRoleOK(t, err)
			var first, after string
			prepareRoleOK(t, f.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", claim.TurnID).Scan(&first))
			seq := assertNativeFenceBarrier(t, f, claim, 1)
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, got)
			prepareRoleOK(t, err)
			prepareRoleOK(t, f.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", claim.TurnID).Scan(&after))
			if first == "" || first != after || seq != assertNativeFenceBarrier(t, f, claim, 1) || turn.State != domain.TurnStateUnknown ||
				turn.ClaimToken != claim.Token || turn.CancelRequestedAt == nil || turn.AcceptedAt == nil {
				t.Fatal("scheduler's late preflight refusal replaced the first receipt, cancel or retained claims")
			}
		})
	}
}

func rejectNativeFenceInvocation(t *testing.T, run *memberToolRuntimeRun, token, configPath *string) {
	t.Helper()
	run.nativeSession.factory = func(config provider.NativeMemberToolConfig, _ []string, _ *provider.NativeSessionReference, _ json.RawMessage) (provider.Executor, error) {
		_, *token = memberToolRuntimeConfig(t, config.Path)
		if configPath != nil {
			*configPath = config.Path
		}
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			t.Fatal("failed final fence invoked native execution")
			return provider.ExecutionReceipt{}
		}), nil
	}
	run.factory = func(string, string, []string) (provider.Executor, error) {
		t.Fatal("failed final fence attempted a fresh legacy launch")
		return nil, nil
	}
}

func assertNativeFenceBarrier(t *testing.T, f *rolePrepareFixture, claim store.TurnClaim, retained int) int {
	t.Helper()
	var seq, resources int
	var hasRef, hasSource bool
	prepareRoleOK(t, f.db.QueryRow("SELECT seq,native_ref_json IS NOT NULL,resumed_from_turn_id IS NOT NULL FROM native_run_bindings WHERE turn_id=?", claim.TurnID).Scan(&seq, &hasRef, &hasSource))
	prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", claim.TurnID).Scan(&resources))
	if hasRef || hasSource || resources != retained {
		t.Fatal("failed startup created a reference or changed expected claim resources")
	}
	return seq
}

func TestNativeSessionRuntimeKeepsEvidenceAfterAuthorityChanges(t *testing.T) {
	for _, purpose := range []string{domain.ChatIntakePurpose, "coordinator"} {
		t.Run(purpose, func(t *testing.T) {
			ctx := context.Background()
			f, prepared := nativeSessionPreparation(t, purpose)
			run := prepared.Executor.(*memberToolRuntimeRun)
			req, claim := claimNativeFenceFixture(t, f, prepared)
			message, err := f.s.GetMessage(ctx, f.group.ID, f.ordinary.Delivery.MessageID)
			prepareRoleOK(t, err)
			factory := run.nativeSession.factory
			run.nativeSession.factory = func(config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
				_, err := factory(config, names, resume, binding)
				prepareRoleOK(t, err)
				return memberToolRuntimeExecutorFunc(func(ctx context.Context, _ provider.ExecuteRequest, event func(provider.Event) error) provider.ExecutionReceipt {
					const id = "synthetic-native"
					prepareRoleOK(t, event(provider.Event{Kind: provider.SessionEvent, NativeSessionID: id}))
					prepareRoleOK(t, event(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: id}))
					_, err := f.s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.group.ID, TopicID: message.TopicID}, domain.ChatPolicyRequest{Action: domain.ChatActionTextSubmission, MaxAdmissions: 1}, 0, "after-acceptance")
					prepareRoleOK(t, err)
					if purpose == "coordinator" {
						_, err := f.s.SetDispatchPolicy(ctx, f.group.ID, nil, 1, "after-acceptance")
						prepareRoleOK(t, err)
					}
					prepareRoleOK(t, event(provider.Event{Kind: provider.TerminalEvent, NativeSessionID: id, Terminal: provider.OutcomeSucceeded}))
					return provider.ExecutionReceipt{NativeSessionID: id, Started: true, Accepted: true, DirectChildReaped: true,
						OutputComplete: true, Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded}
				}), nil
			}
			got := run.Execute(ctx, req, func(event provider.Event) error { return f.s.RecordTurnEvent(ctx, claim, event) })
			if !got.Started || !got.Accepted || got.Outcome != provider.OutcomeSucceeded || got.Terminal != got.Outcome || !got.DirectChildReaped || !got.OutputComplete {
				t.Fatal("current authority erased known execution evidence")
			}
			turn, err := f.s.ApplyTurnReceipt(ctx, claim, got)
			prepareRoleOK(t, err)
			if turn.State != domain.TurnStateSucceeded {
				t.Fatal("known terminal evidence did not settle the turn")
			}
		})
	}
}

func TestNativeSessionRuntimeFencesReplacedExecutionRoot(t *testing.T) {
	for _, purpose := range []string{"generic", domain.ChatIntakePurpose} {
		for _, stage := range []string{"before-construction", "after-construction"} {
			t.Run(purpose+"/"+stage, func(t *testing.T) {
				f, prepared := nativeSessionPreparation(t, purpose)
				req, _ := claimNativeFenceFixture(t, f, prepared)
				replace := func() {
					prepareRoleOK(t, os.Rename(req.RuntimeDir, req.RuntimeDir+"-original"))
					prepareRoleOK(t, os.Mkdir(req.RuntimeDir, 0700))
				}
				run := prepared.Executor.(*memberToolRuntimeRun)
				factory, calls, invoked := run.nativeSession.factory, 0, false
				run.nativeSession.factory = func(config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
					calls++
					_, err := factory(config, names, resume, binding)
					prepareRoleOK(t, err)
					if stage == "after-construction" {
						replace()
					}
					return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
						invoked = true
						return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}
					}), nil
				}
				if stage == "before-construction" {
					replace()
				}
				got := run.Execute(context.Background(), req, nil)
				if calls > 1 || (calls == 1) != (stage == "after-construction") || invoked || got.Started {
					t.Fatalf("replaced root reached native factory/execute: factories=%d invoked=%t", calls, invoked)
				}
				memberToolRuntimeEmpty(t, req.RuntimeDir)
				memberToolRuntimeEmpty(t, filepath.Clean(req.RuntimeDir)+"-original")
				memberToolRuntimeEmpty(t, f.runtimeOptions.ConfigRoot)
			})
		}
	}
}
