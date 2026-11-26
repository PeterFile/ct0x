package platform_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type investigationNativeConfig struct {
	Endpoint, Token string
	RunID           string `json:"run_id"`
	provider.PiFreshInvestigationProfile
}

type ownerFixtureConstruct func(context.Context, provider.PiFreshInvestigationBinding, investigationNativeConfig) provider.Executor

func newInvestigationOwnerPreparer(t *testing.T, f *boundWorkReportFixture, assets *platform.MemberArtifactGrant, typed bool,
	construct ownerFixtureConstruct, commit func(context.Context, store.FreshTaskWorkLaunch, func() bool) error) *platform.Preparer {
	t.Helper()
	root := t.TempDir()
	private, cli := filepath.Join(root, "private"), filepath.Join(root, "metadata-only-pi")
	roleOK(t, os.Mkdir(private, 0700))
	roleOK(t, os.Mkdir(filepath.Join(private, platform.InvestigationCWDName), 0700))
	roleOK(t, os.WriteFile(cli, nil, 0700)) // No usable executable, even if invoked erroneously.
	bridges, err := filepath.Abs("../../extensions/pi-bridge")
	roleOK(t, err)
	providers := []provider.Provider{{ID: f.worker.ProviderID, Name: provider.Pi, Version: "1.4.0", Installation: provider.Installed, Path: cli}}
	if commit == nil {
		commit = func(check context.Context, plan store.FreshTaskWorkLaunch, admit func() bool) error {
			return f.db.CommitInvestigationTaskWorkLaunch(check, store.InvestigationTaskWorkLaunch(plan), admit)
		}
	}
	launch := platform.NewPiInvestigationTaskWorkLaunch(filepath.Join(bridges, "fresh-investigation-work.ts"), 0, commit)
	launch.Artifacts = assets
	newExecutor := launch.Construct
	launch.Construct = func(check context.Context, selected provider.Provider, binding provider.PiFreshInvestigationBinding) (provider.Executor, error) {
		// Qualify and consume the actual config with the pinned adapter, then use
		// a directed in-process executor. Discard the adapter's CLI executor.
		if selected.Path != cli || construct == nil {
			t.Fatal("unexpected native construction")
		}
		if _, err := newExecutor(check, selected, binding); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(binding.ConfigPath)
		roleOK(t, err)
		var config investigationNativeConfig
		roleOK(t, json.Unmarshal(data, &config))
		if config.Token == "" || config.Endpoint != f.options.Endpoint || config.RunID != binding.RunID || sha256.Sum256(data) != binding.ConfigSHA256 ||
			config.Action != binding.Profile.Action || config.SourceKind != binding.Profile.SourceKind || !slices.Equal(config.Operations, binding.Profile.Operations) {
			t.Fatal("not the exact issued profile, digest and RunID")
		}
		return construct(check, binding, config), nil
	}
	var launches []*platform.InvestigationTaskWorkLaunch
	if typed {
		launches = append(launches, launch)
	}
	f.options.ChatIntake, f.options.NativeExecutionRoot = f.engine, f.root
	f.runtime, err = platform.NewMemberToolRuntimeWithInvestigationOwnerManagement(f.db, f.options, f.engine, launch.DelegateSupport(providers), launches...)
	roleOK(t, err)
	t.Cleanup(func() { roleOK(t, f.runtime.Close()) })
	f.mount(t)
	prep, err := platform.NewPreparer(f.db, platform.PrepareOptions{Providers: providers, PrivateWorkspaceRoot: private, MemberTools: f.runtime,
		PiMemberToolsExtension: filepath.Join(bridges, "index.ts"), EnableNativeSessionResume: true, NativePiThinking: "max",
		Investigation: &platform.InvestigationPreparationOptions{PiPath: cli, Artifacts: assets}})
	roleOK(t, err)
	return prep
}

type observation struct {
	scheduler.PreparedExecution
	claim   store.TurnClaim
	receipt provider.ExecutionReceipt
}

// Run one real scheduler against the selected fixture wakes. On a successful
// round next is set by the actual B effect before the next scheduler scan.
func runInvestigationOwnerWakes(t *testing.T, f *boundWorkReportFixture, prep *platform.Preparer, next *string, count int) []observation {
	t.Helper()
	// The full SQLite chain is much slower under -race on a cold HDD. This
	// is a fixture deadline, not an extension of the runtime's startup fence.
	ctx, stop := context.WithTimeout(t.Context(), 3*time.Minute)
	defer stop()
	var observed []observation
	err := scheduler.Run(ctx, f.db, scheduler.Options{Recovered: true, RuntimeDir: f.root, MaxActive: 1},
		func(check context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
			if pending.Delivery.ID != *next {
				return scheduler.PreparedExecution{}, scheduler.ErrNotReady
			}
			p, err := prep.Prepare(check, pending)
			if err != nil {
				return p, err
			}
			base := p
			p.Executor = roleNative(func(run context.Context, req provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
				base.Request = req
				obs := observation{PreparedExecution: base, receipt: provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}}
				// If a fixture assertion fails, still return a receipt to the
				// scheduler, rather than exiting its writer goroutine.
				done := make(chan struct{})
				go func() {
					defer close(done)
					turn, err := f.db.GetTurn(run, f.group, req.RunID)
					roleOK(t, err)
					obs.claim = store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
					obs.receipt = base.Executor.Execute(run, req, emit)
				}()
				<-done
				observed = append(observed, obs)
				if len(observed) == count || t.Failed() {
					stop()
				}
				return obs.receipt
			})
			return p, nil
		})
	if err == nil || err.Error() != context.Canceled.Error() || len(observed) != count {
		t.Fatalf("expected %d directed scheduler launches, saw %d: %v", count, len(observed), err)
	}
	files, err := os.ReadDir(f.options.ConfigRoot)
	roleOK(t, err)
	if len(files) != 0 {
		t.Fatal("private bootstrap survived run cleanup")
	}
	for _, obs := range observed {
		if replay := obs.Executor.Execute(t.Context(), obs.Request, nil); replay.Started || replay.Outcome != provider.OutcomeUnknown {
			t.Fatal("prepared execution was reusable")
		}
		ownerNativeRows(t, f, 0, `SELECT count(*) FROM native_run_bindings WHERE turn_id=? AND binding_json IS NOT NULL`, obs.claim.TurnID)
	}
	return observed
}

func ownerNativeRows(t *testing.T, f *boundWorkReportFixture, want int, query string, args ...any) {
	t.Helper()
	var rows int
	roleOK(t, f.raw.QueryRow(query, args...).Scan(&rows))
	if rows != want {
		t.Fatalf("%s: got %d rows, want %d", query, rows, want)
	}
}

func ownerNativeEvent(t *testing.T, emit func(provider.Event) error, req provider.ExecuteRequest, kind provider.EventKind) {
	t.Helper()
	e := provider.Event{Kind: kind, NativeSessionID: req.RunID}
	if kind == provider.TerminalEvent {
		e.Terminal = provider.OutcomeSucceeded
	}
	roleOK(t, emit(e))
}

func ownerNativeStop(t *testing.T, emit func(provider.Event) error, req provider.ExecuteRequest, outcome provider.Outcome) provider.ExecutionReceipt {
	t.Helper()
	ownerNativeEvent(t, emit, req, provider.TerminalEvent)
	return provider.ExecutionReceipt{Outcome: outcome, Started: true, Accepted: true, Terminal: provider.OutcomeSucceeded,
		NativeSessionID: req.RunID, DirectChildReaped: true, OutputComplete: true}
}

func ownerNativeKey(op, call string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(op+call))) }

const investigationTaskStateMarker = "Task state is not material in this investigation profile."

func seedInvestigationTaskState(t *testing.T, f *boundWorkReportFixture, scope domain.ThreadStateScope) {
	t.Helper()
	var installed bool
	roleOK(t, f.raw.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='thread_state_revisions')`).Scan(&installed))
	if !installed {
		return
	}
	source, err := f.local.CreateMessage(t.Context(), scope.GroupID, platform.MessageInput{TopicID: scope.TopicID,
		Content: investigationTaskStateMarker, RecipientIDs: []string{}}, "investigation-state-source")
	roleOK(t, err)
	state, err := f.local.SetThreadState(t.Context(), scope, store.ThreadStateUpdate{Kind: domain.ThreadStateConstraint,
		Action: "set", Content: source.Content, SourceMessage: source.ID}, "investigation-state")
	roleOK(t, err)
	if state.Scope != scope || state.Revision != 1 || len(state.Records) != 1 {
		t.Fatal("fixture did not confirm nonzero state on the exact investigation Task")
	}
}

func assertNoInvestigationTaskState(t *testing.T, prepared scheduler.PreparedExecution) {
	t.Helper()
	if prepared.PreparedThreadState != nil || strings.Contains(prepared.Request.Prompt, `"thread_state"`) || strings.Contains(prepared.Request.Prompt, investigationTaskStateMarker) {
		t.Fatal("investigation inherited Task state or its formal claim stamp")
	}
}

func TestPiInvestigationOwnerNativeSQLiteRoundTrip(t *testing.T) {
	// Do not overlap this round trip with the SQLite-heavy race fixtures;
	// keep the production HTTP deadline on its scoped tool calls.
	f := newBoundWorkReportFixture(t)
	ownerNativeRows(t, f, 1, `SELECT count(*) FROM schema_migrations WHERE version=30`)
	assets, approved, foreign := ownerFixtureMaterials(t, f)
	child := ownerFixtureMember(t, f, f.group, "reporter")
	peer := ownerFixtureMember(t, f, f.group, "second reporter")
	outside := ownerFixtureMember(t, f, f.group, "not in policy")
	unsupported, err := f.local.CreateMember(t.Context(), f.group, platform.MemberInput{Name: "unsupported", Role: "fixture", ProviderID: "other-provider", Model: "fixture"}, "")
	roleOK(t, err)
	next := f.queueInvestigationOwner(t, []domain.ArtifactVersionRef{approved}, 2, child.ID, peer.ID, unsupported.ID)
	original, err := f.db.ResolveCoordinationDelivery(t.Context(), f.group, next)
	roleOK(t, err)
	role := original.Provenance
	seedInvestigationTaskState(t, f, domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: role.GroupID, TopicID: role.TopicID},
		TaskID: role.TaskID, TaskRevision: role.TaskRevision})
	const channel, artifact = platform.ReadInvestigationChannelOperation, platform.ReadInvestigationArtifactOperation
	var tokens []string
	var typedCommits, nativeCalls int
	var report domain.TaskWorkReport
	var second domain.TaskWorkDelegation
	var noticeID string
	first := domain.DelegateWorkInput{MemberID: child.ID, Question: "核对这一版的资料。", ArtifactVersions: []domain.ArtifactVersionRef{approved}}
	firstBody, firstKey := ownerWorkJSON(t, first), ownerNativeKey(delegateWorkOp, "initial-call")
	prep := newInvestigationOwnerPreparer(t, f, assets, true,
		func(_ context.Context, _ provider.PiFreshInvestigationBinding, c investigationNativeConfig) provider.Executor {
			tokens = append(tokens, c.Token)
			return roleNative(func(run context.Context, req provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
				nativeCalls++
				ops, err := f.runtime.Tools().TransportOperations(run, c.Token)
				roleOK(t, err)
				turn, err := f.db.GetTurn(run, f.group, req.RunID)
				roleOK(t, err)
				if req.RunID != c.RunID || turn.DeliveryID != next || turn.State != domain.TurnStateStarting || c.Token == turn.ClaimToken ||
					!slices.Equal(ops, c.Operations) || strings.Contains(req.Prompt, c.Token) || strings.Contains(req.Prompt, f.source.Content) {
					t.Fatal("native entered with wrong Turn/registration or leaked context")
				}
				ownerNativeEvent(t, emit, req, provider.AcceptedEvent)
				f.expectHTTP(t, c.Token, "read_channel", `{}`, "", http.StatusForbidden)
				if w := f.http(t, c.Token, "hand_back_work", `{"summary":"not the wire"}`, "alias"); w.Code != http.StatusNotFound {
					t.Fatal("a non-ABI handback alias was mounted")
				}
				delegate := func(body, key string) domain.TaskWorkDelegation {
					var saved struct {
						ID string `json:"delegation_id"`
					}
					roleOK(t, json.Unmarshal([]byte(f.expectHTTP(t, c.Token, delegateWorkOp, body, key, http.StatusCreated)), &saved))
					d, err := f.engine.GetTaskWorkDelegation(run, f.group, saved.ID)
					roleOK(t, err)
					if d.AuthorTurnID != req.RunID || d.AssignmentID != original.Provenance.AssignmentID || d.TaskID != original.Provenance.TaskID || d.AuthorAttemptID == "" {
						t.Fatal("not a write by the bound owner of this Task/Assignment")
					}
					return d
				}
				switch nativeCalls {
				case 1:
					if typedCommits != 0 || c.SourceKind != provider.PiInitialInvestigationOwnerRole || !slices.Equal(ops, []string{channel, artifact, "submit_result", delegateWorkOp}) ||
						!strings.Contains(req.Prompt, child.ID) || !strings.Contains(req.Prompt, peer.ID) || strings.Contains(req.Prompt, outside.ID) ||
						strings.Contains(req.Prompt, unsupported.ID) || strings.Contains(req.Prompt, f.lead.ID) {
						t.Fatal("initial owner did not get only its exact profile and sampled peers")
					}
					f.expectHTTP(t, c.Token, channel, `{}`, "", http.StatusOK)
					f.expectHTTP(t, c.Token, artifact, ownerWorkJSON(t, approved), "", http.StatusOK)
					f.expectHTTP(t, c.Token, handbackWorkOp, `{"summary":"too early"}`, "initial-handback", http.StatusForbidden)
					for _, bad := range []string{outside.ID, unsupported.ID, f.lead.ID, f.worker.ID} {
						f.expectHTTP(t, c.Token, delegateWorkOp, strings.Replace(firstBody, child.ID, bad, 1), firstKey, http.StatusForbidden)
					}
					f.delegation = delegate(firstBody, firstKey)
					if f.delegation.AuthorDeliveryID != turn.DeliveryID || f.delegation.Ordinal != 1 || f.delegation.MemberID != child.ID || f.delegation.AllowTopicMessages ||
						!slices.Equal(f.delegation.ArtifactVersions, []domain.ArtifactVersionRef{approved}) {
						t.Fatal("original owner did not record the first exact material subset")
					}
					next = f.delegation.DeliveryID
				case 2:
					if typedCommits != 1 || c.SourceKind != domain.TaskWorkSourceDelegation || !slices.Equal(ops, []string{artifact, platform.ReportWorkOperation}) ||
						turn.MemberID != child.ID || !strings.Contains(req.Prompt, first.Question) || strings.Contains(req.Prompt, peer.ID) {
						t.Fatal("child inherited owner tools or context")
					}
					f.expectHTTP(t, c.Token, channel, `{}`, "", http.StatusForbidden)
					f.expectHTTP(t, c.Token, delegateWorkOp, firstBody, firstKey, http.StatusForbidden)
					f.expectHTTP(t, c.Token, handbackWorkOp, `{"summary":"not owner"}`, "child-handback", http.StatusForbidden)
					f.expectHTTP(t, c.Token, artifact, ownerWorkJSON(t, approved), "", http.StatusOK)
					f.expectHTTP(t, c.Token, artifact, ownerWorkJSON(t, foreign), "", http.StatusForbidden)
					var id struct {
						ID string `json:"report_id"`
					}
					roleOK(t, json.Unmarshal([]byte(f.expectHTTP(t, c.Token, platform.ReportWorkOperation, testDeliveredBody, ownerNativeKey(platform.ReportWorkOperation, "child-call"), http.StatusCreated)), &id))
					report, err = f.engine.GetTaskWorkReport(run, f.group, id.ID)
					roleOK(t, err)
					if report.DelegationID != f.delegation.ID || report.ActorID != child.ID || report.SourceTurnID != req.RunID || report.DeliveryID == "" {
						t.Fatal("child did not create a bound report wake")
					}
					next = report.DeliveryID
				case 3:
					want := slices.Concat([]string{platform.ReadWorkReportOperation}, provider.PiFreshTaskWorkToolNames(), []string{delegateWorkOp, handbackWorkOp})
					if typedCommits != 2 || c.SourceKind != domain.TaskWorkSourceReport || !slices.Equal(ops, want) || turn.MemberID != f.worker.ID ||
						turn.ConversationID != original.Provenance.ConversationID || strings.Contains(req.Prompt, report.Summary) || strings.Contains(req.Prompt, approved.VersionID) {
						t.Fatal("report owner was not fresh, same owner, and bound to only the report profile")
					}
					if got := f.expectHTTP(t, c.Token, platform.ReadWorkReportOperation, `{}`, "", http.StatusOK); !strings.Contains(got, report.ID) {
						t.Fatal("report wake did not read its B report")
					}
					f.expectHTTP(t, c.Token, artifact, ownerWorkJSON(t, approved), "", http.StatusForbidden)
					second = delegate(ownerWorkJSON(t, domain.DelegateWorkInput{MemberID: peer.ID, Question: "核对剩余的疑问。"}), ownerNativeKey(delegateWorkOp, "report-call"))
					if second.Ordinal != 2 || second.AuthorAttemptID == f.delegation.AuthorAttemptID || second.AuthorDeliveryID != report.DeliveryID || second.MemberID != peer.ID {
						t.Fatal("second delegation did not use the B-bound report continuation Attempt")
					}
					f.expectHTTP(t, c.Token, delegateWorkOp, firstBody, ownerNativeKey(delegateWorkOp, "budget-exhausted"), http.StatusConflict)
					var handback struct {
						ID     string `json:"handback_id"`
						Notice string `json:"notice_message_id"`
					}
					roleOK(t, json.Unmarshal([]byte(f.expectHTTP(t, c.Token, handbackWorkOp, `{"summary":"请协调员复核待决范围。"}`, ownerNativeKey(handbackWorkOp, "whole-task"), http.StatusCreated)), &handback))
					if handback.ID == "" || handback.Notice == "" {
						t.Fatal("missing whole-task handback receipt")
					}
					noticeID = handback.Notice
				default:
					t.Fatal("unexpected replay or extra native run")
				}
				return ownerNativeStop(t, emit, req, provider.OutcomeSucceeded)
			})
		}, func(check context.Context, plan store.FreshTaskWorkLaunch, admit func() bool) error {
			typedCommits++
			if plan.Delivery.DeliveryID != next || plan.RequestID != original.Provenance.RequestID ||
				plan.Delivery.SourceKind.IsOwnerContinuation() != (plan.AttemptID != "") {
				t.Fatal("typed launch didn't reach B #203 with the exact work/Attempt")
			}
			return f.db.CommitInvestigationTaskWorkLaunch(check, store.InvestigationTaskWorkLaunch(plan), admit)
		})
	obs := runInvestigationOwnerWakes(t, f, prep, &next, 3)
	for i, o := range obs {
		assertNoInvestigationTaskState(t, o.PreparedExecution) // Initial investigation, delegation, report.
		f.claims(t, roleRun{claim: o.claim}, domain.TurnStateSucceeded, 0)
		if o.receipt.NativeRef != nil || o.receipt.Outcome != provider.OutcomeSucceeded {
			t.Fatal("typed run did not settle as fresh-only")
		}
		f.expectHTTP(t, tokens[i], delegateWorkOp, firstBody, firstKey, http.StatusForbidden)
	}
	notice, err := f.db.GetMessage(t.Context(), f.group, noticeID)
	roleOK(t, err)
	task, err := f.db.GetTask(t.Context(), f.group, f.delegation.TaskID)
	roleOK(t, err)
	if len(notice.Deliveries) != 0 || !strings.Contains(notice.Content, f.lead.ID) || task.Status != domain.TaskStatusOpen || task.CurrentSubmissionID != "" {
		t.Fatal("handback was not a non-waking coordinator notice or changed the Task")
	}
	ownerNativeRows(t, f, 1, `SELECT count(*) FROM task_work_handbacks WHERE author_turn_id=? AND author_attempt_id=? AND coordinator_member_id=?`, obs[2].claim.TurnID, second.AuthorAttemptID, f.lead.ID)
	ownerNativeRows(t, f, 3, `SELECT count(*) FROM mutation_idempotency WHERE actor_id=? AND operation IN ('delegate_work','handback_work')`, f.worker.ID)
	ownerNativeRows(t, f, 0, `SELECT count(*) FROM turns WHERE delivery_id=?`, second.DeliveryID)
	t.Log("same scheduler: original delegate, scoped child read/report, same owner bound report/re-delegate/handback; no extra native starts or budget effect")
}

func TestPiInvestigationOwnerNativeUnknownAndCurrentFences(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"lost-write-reply", "current-policy-changed", "config-changed", "cancel-before-invoke"} {
		t.Run(mode, func(t *testing.T) {
			f := newBoundWorkReportFixture(t)
			peer := ownerFixtureMember(t, f, f.group, "delegate")
			next := f.queueInvestigationOwner(t, nil, 1, peer.ID)
			body := ownerWorkJSON(t, domain.DelegateWorkInput{MemberID: peer.ID, Question: "核对摘要。"})
			key := ownerNativeKey(delegateWorkOp, "exact-call")
			var constructed, native, requests int
			var token string
			prep := newInvestigationOwnerPreparer(t, f, nil, true, func(ctx context.Context, b provider.PiFreshInvestigationBinding, c investigationNativeConfig) provider.Executor {
				constructed++
				token = c.Token
				switch mode {
				case "current-policy-changed":
					f.stopScope(t)
				case "config-changed":
					buf, err := os.ReadFile(b.ConfigPath)
					roleOK(t, err)
					buf[0] = '[' // Same file identity, different digest after construction.
					roleOK(t, os.WriteFile(b.ConfigPath, buf, 0600))
				case "cancel-before-invoke":
					_, err := f.db.RequestTurnCancel(ctx, f.group, b.RunID)
					roleOK(t, err)
				}
				return roleNative(func(run context.Context, req provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
					native++
					if mode != "lost-write-reply" {
						t.Fatal("current fault did not block native invocation")
					}
					ownerNativeEvent(t, emit, req, provider.AcceptedEvent)
					r := httptest.NewRequestWithContext(run, http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/"+delegateWorkOp, strings.NewReader(body))
					r.RemoteAddr = "127.0.0.1:12345"
					r.Header.Set("Authorization", "Bearer "+token)
					r.Header.Set("Idempotency-Key", key)
					r.Header.Set("Content-Type", "application/json")
					w := &ownerWorkLostResponse{ResponseRecorder: httptest.NewRecorder()}
					requests++
					f.handler.ServeHTTP(w, r)
					if w.Code != http.StatusCreated || w.writes != 1 || w.Body.Len() != 0 {
						t.Fatal("B didn't commit exactly once and lose the response")
					}
					// The SDK/adapter tests exercise this exact Unknown/Terminal
					// split from a missing HTTP response on the real bridge.
					return ownerNativeStop(t, emit, req, provider.OutcomeUnknown)
				})
			}, nil)
			o := runInvestigationOwnerWakes(t, f, prep, &next, 1)[0]
			want := 0
			if mode == "lost-write-reply" {
				want = 1
			}
			if constructed != 1 || requests != want || native != want || o.receipt.Outcome != provider.OutcomeUnknown || o.receipt.Started != (want == 1) {
				t.Fatal("expected one-shot construction and fail-closed outcome")
			}
			f.claims(t, roleRun{claim: o.claim}, domain.TurnStateUnknown, 1)
			ownerNativeRows(t, f, want, `SELECT count(*) FROM task_work_delegations WHERE author_turn_id=?`, o.claim.TurnID)
			ownerNativeRows(t, f, want, `SELECT count(*) FROM mutation_idempotency WHERE operation=? AND actor_id=? AND idempotency_key=?`, delegateWorkOp, f.worker.ID, key)
			f.expectHTTP(t, token, delegateWorkOp, body, key, http.StatusForbidden)
			if want == 1 {
				var delivery string
				roleOK(t, f.raw.QueryRow(`SELECT delivery_id FROM task_work_delegations WHERE author_turn_id=?`, o.claim.TurnID).Scan(&delivery))
				work, err := f.engine.ResolveTaskWorkDelivery(t.Context(), f.group, delivery)
				roleOK(t, err)
				if work.Blocked != "prior_turn_unsettled" || o.receipt.Terminal != provider.OutcomeSucceeded {
					t.Fatal("an observed native terminal was treated as confirmation of the B write")
				}
				ownerNativeRows(t, f, 0, `SELECT count(*) FROM turns WHERE delivery_id=?`, delivery)
			}
		})
	}
}

func TestPiInvestigationOwnerNativeFollowupAndAdmissionRefusal(t *testing.T) {
	t.Parallel()
	testPiInvestigationOwnerNativeFollowup(t, "admitted", "declined-callback", "outcome-false")
}

func TestPiInvestigationOwnerNativeDisabledTypedExtensionLeavesWakeQueued(t *testing.T) {
	testPiInvestigationOwnerNativeFollowup(t, "typed-launch-disabled")
}

func testPiInvestigationOwnerNativeFollowup(t *testing.T, modes ...string) {
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			f := newBoundWorkReportFixture(t)
			f.delegate(t, nil, nil, false) // Seed only the prior work for this followup fixture.
			f.settleWork(t, f.claimWork(t, f.delegation.DeliveryID), false)
			input, _ := queueInvestigationWorkInput(t, f, "请进一步查对原材料。", false)
			if mode == "admitted" {
				parent := f.delegation.TaskWorkParent
				seedInvestigationTaskState(t, f, domain.ThreadStateScope{ThreadRef: parent.ThreadRef, TaskID: parent.TaskID, TaskRevision: parent.TaskRevision})
			}
			next := input.WorkDeliveryID
			var constructCalls, nativeCalls, gateCalls int
			prep := newInvestigationOwnerPreparer(t, f, nil, mode != "typed-launch-disabled",
				func(_ context.Context, _ provider.PiFreshInvestigationBinding, c investigationNativeConfig) provider.Executor {
					constructCalls++
					return roleNative(func(run context.Context, req provider.ExecuteRequest, emit func(provider.Event) error) provider.ExecutionReceipt {
						nativeCalls++
						want := slices.Concat([]string{platform.ReadInvestigationChannelOperation}, provider.PiFreshTaskWorkToolNames(), []string{delegateWorkOp, handbackWorkOp})
						if mode != "admitted" || c.SourceKind != domain.TaskWorkSourceFollowup || !slices.Equal(c.Operations, want) || gateCalls != 1 {
							t.Fatal("wrong followup tool set or native started without permission")
						}
						ownerNativeEvent(t, emit, req, provider.AcceptedEvent)
						f.expectHTTP(t, c.Token, platform.ReadWorkReportOperation, `{}`, "", http.StatusForbidden)
						f.expectHTTP(t, c.Token, "read_work_inputs", fmt.Sprintf(`{"input_id":%q}`, input.WorkInputID), "", http.StatusOK)
						returned := f.expectHTTP(t, c.Token, "hand_back_work_input", fmt.Sprintf(`{"input_id":%q,"reason":"不在本轮材料范围内"}`, input.WorkInputID),
							ownerNativeKey("hand_back_work_input", "one-input"), http.StatusOK)
						notice := f.expectHTTP(t, c.Token, handbackWorkOp, `{"summary":"整项范围需协调员复核。"}`, ownerNativeKey(handbackWorkOp, "whole-task"), http.StatusCreated)
						if !strings.Contains(returned, `"disposition":"returned"`) || strings.Contains(returned, `"handback_id"`) ||
							!strings.Contains(notice, `"handback_id"`) || strings.Contains(notice, `"input_id"`) {
							t.Fatal("single input and whole-task handback were conflated")
						}
						return ownerNativeStop(t, emit, req, provider.OutcomeSucceeded)
					})
				}, func(check context.Context, plan store.FreshTaskWorkLaunch, admit func() bool) error {
					gateCalls++
					switch mode {
					case "outcome-false":
						// A nil returned error is not a positive launch outcome.
						return nil
					case "declined-callback":
						err := f.db.CommitInvestigationTaskWorkLaunch(check, store.InvestigationTaskWorkLaunch(plan), func() bool { return false })
						if !errors.Is(err, store.ErrMemberToolDenied) {
							t.Fatal("B did not decline a false callback", err)
						}
						return err
					default:
						return f.db.CommitInvestigationTaskWorkLaunch(check, store.InvestigationTaskWorkLaunch(plan), admit)
					}
				})
			if mode == "admitted" {
				f.mountMemberToolHandlers(t.Context())
			}
			wantWrites := 0
			if mode == "typed-launch-disabled" {
				ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
				defer stop()
				checked := false
				err := scheduler.Run(ctx, f.db, scheduler.Options{Recovered: true, RuntimeDir: f.root, MaxActive: 1},
					func(check context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
						if pending.Delivery.ID != next {
							return scheduler.PreparedExecution{}, scheduler.ErrNotReady
						}
						p, err := prep.Prepare(check, pending)
						var blocked *platform.RolePreparationBlocked
						checked = p.Executor == nil && errors.As(err, &blocked) && blocked.Code == "typed_profile_unavailable"
						stop()
						return p, err
					})
				if !checked || !errors.Is(err, context.Canceled) || constructCalls+gateCalls+nativeCalls != 0 {
					t.Fatal("disabled typed path did not stay queued")
				}
				ownerNativeRows(t, f, 0, `SELECT count(*) FROM turns WHERE delivery_id=?`, next)
			} else {
				o := runInvestigationOwnerWakes(t, f, prep, &next, 1)[0]
				if constructCalls != 1 || gateCalls != 1 {
					t.Fatal("fresh typed owner missed its exact launch fence")
				}
				if mode == "admitted" {
					assertNoInvestigationTaskState(t, o.PreparedExecution) // Follow-up on the same exact Task.
					wantWrites = 1
					f.claims(t, roleRun{claim: o.claim}, domain.TurnStateSucceeded, 0)
				} else {
					f.claims(t, roleRun{claim: o.claim}, domain.TurnStateUnknown, 1)
					if o.receipt.Started || o.receipt.Accepted || o.receipt.Outcome != provider.OutcomeUnknown {
						t.Fatal("declined postclaim gate became native evidence or a terminal result")
					}
				}
			}
			if nativeCalls != wantWrites {
				t.Fatal("wrong count of native invocations")
			}
			ownerNativeRows(t, f, wantWrites, `SELECT count(*) FROM task_work_handbacks`)
		})
	}
}

func TestInvestigationOwnerManagementAllowsOriginalUserFollowup(t *testing.T) {
	t.Parallel()
	testInvestigationOwnerManagementAdmission(t, "local_user_chat", domain.TaskWorkSourceFollowup)
}

func TestInvestigationOwnerManagementOriginalUserReport(t *testing.T) {
	t.Parallel()
	testInvestigationOwnerManagementAdmission(t, "local_user_chat", domain.TaskWorkSourceReport)
}

func TestInvestigationOwnerManagementMemberFollowupStaysUnmanaged(t *testing.T) {
	testInvestigationOwnerManagementAdmission(t, "member", domain.TaskWorkSourceFollowup)
}

func TestInvestigationOwnerManagementUsesUnmanagedReportForMemberOrigin(t *testing.T) {
	t.Parallel()
	testInvestigationOwnerManagementAdmission(t, "member", domain.TaskWorkSourceReport)
}

func testInvestigationOwnerManagementAdmission(t *testing.T, sourceKind string, kind domain.TaskWorkSourceKind) {
	t.Helper()
	t.Run(string(kind), func(t *testing.T) {
		ctx := t.Context()
		f := newBoundWorkReportFixture(t)
		peer := ownerFixtureMember(t, f, f.group, "peer")
		strict := sourceKind == "local_user_chat"
		var sender string
		if !strict {
			sender = peer.ID
		}
		admitted := f.assessInvestigationFixtureFrom(t, true, sender, nil, 2, peer.ID)
		if admitted.Outcome != "admitted" || !admitted.AuthorityEvidenceVerified || admitted.SourceKind != sourceKind ||
			admitted.SourceID != f.source.SourceID || admitted.MessageID != f.source.ID || admitted.SourceSenderID != f.source.SenderID {
			t.Fatalf("fixture did not legally admit the original %s source: %s/%s", sourceKind, admitted.Outcome, admitted.ReasonCode)
		}
		decision := f.decideInvestigationFixture(t, admitted, "assign")
		assigned, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
		roleOK(t, err)
		initial := f.claim(t, assigned.Delivery.DeliveryID)
		initialScope, err := f.db.BindMemberTool(ctx, initial.claim)
		roleOK(t, err)
		original := f.bindInvestigationOwner(t, store.CoordinationToolScope{Member: initialScope, Provenance: initial.provenance})
		f.acceptWork(t, original.Member)
		// Seed real B history without assuming the original owner had C management tools.
		f.delegation, err = f.engine.DelegateWork(ctx, original.Member, domain.DelegateWorkInput{MemberID: peer.ID, Question: "核对原材料。"}, "seed-delegation")
		roleOK(t, err)
		f.settleWork(t, original.Member, true)
		child := f.claimWork(t, f.delegation.DeliveryID)
		isReport := kind == domain.TaskWorkSourceReport
		var report domain.TaskWorkReport
		if isReport {
			f.acceptWork(t, child)
			report, err = f.engine.ReportWork(ctx, child, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "已核对原材料。"}, "seed-report")
			roleOK(t, err)
		}
		f.settleWork(t, child, isReport)
		input, userReply := queueInvestigationWorkInput(t, f, "保留原文。", isReport)
		messageID, wakeSourceID, read := input.WorkWakeMessageID, input.WorkInputID, platform.ReadInvestigationChannelOperation
		if isReport {
			messageID, wakeSourceID, read = report.MessageID, report.ID, platform.ReadWorkReportOperation
		}
		msg, err := f.db.GetMessage(ctx, f.group, messageID)
		roleOK(t, err)
		wake := msg.Deliveries[0]
		work, err := f.engine.ResolveTaskWorkDelivery(ctx, f.group, wake.ID)
		roleOK(t, err)
		if work.Blocked != "" || work.SourceKind != kind || work.SourceID != wakeSourceID {
			t.Fatalf("fixture lacks its exact ready B continuation: %s/%s", work.SourceKind, work.Blocked)
		}
		want := append([]string{read}, provider.PiFreshTaskWorkToolNames()...)
		var candidates []string
		if strict {
			want = append(want, delegateWorkOp, handbackWorkOp)
			candidates = []string{peer.ID}
		}
		var offered provider.PiFreshInvestigationProfile
		launch := &platform.InvestigationTaskWorkLaunch{
			Supports: func(_ provider.Provider, p provider.PiFreshInvestigationProfile) bool { offered = p; return true },
			Construct: func(context.Context, provider.Provider, provider.PiFreshInvestigationBinding) (provider.Executor, error) {
				t.Fatal("preflight must not construct an adapter")
				return nil, nil
			},
			Commit: func(context.Context, store.FreshTaskWorkLaunch, func() bool) error {
				t.Fatal("preflight must not admit a launch")
				return nil
			},
		}
		f.runtime, err = platform.NewMemberToolRuntimeWithInvestigationOwnerManagement(f.db, f.options, f.engine,
			func(domain.Member, provider.PiFreshInvestigationProfile) bool { return true }, launch)
		roleOK(t, err)
		t.Cleanup(func() { roleOK(t, f.runtime.Close()) })
		extension := filepath.Join(t.TempDir(), "unused-extension.ts")
		roleOK(t, os.WriteFile(extension, nil, 0600))
		private := filepath.Join(t.TempDir(), "private")
		roleOK(t, os.Mkdir(private, 0700))
		prep, err := platform.NewPreparer(f.db, platform.PrepareOptions{PrivateWorkspaceRoot: private, MemberTools: f.runtime,
			PiMemberToolsExtension: extension, Providers: []provider.Provider{{ID: f.worker.ProviderID, Name: provider.Pi}}})
		roleOK(t, err)
		prepared, err := prep.Prepare(ctx, store.PendingDelivery{GroupID: f.group, Delivery: wake, SenderID: msg.SenderID})
		roleOK(t, err)
		if prepared.Executor == nil || offered.SourceKind != kind || !slices.Equal(offered.Operations, want) ||
			strings.Contains(prepared.Request.Prompt, "delegate_candidate_ids") != strict ||
			strings.Contains(prepared.Request.Prompt, delegateWorkOp) != strict || strings.Contains(prepared.Request.Prompt, handbackWorkOp) != strict {
			t.Errorf("preflight advertised %v, want %v", offered.Operations, want)
		}

		state, err := f.db.ResolveCoordinationDelivery(ctx, f.group, wake.ID)
		roleOK(t, err)
		owner := f.claimWork(t, wake.ID)
		attempt, err := f.engine.BindTaskWorkContinuation(ctx, owner)
		roleOK(t, err)
		role := store.CoordinationToolScope{Member: owner, Provenance: state.Provenance, AttemptID: attempt.ID}
		tools := f.runtime.Tools()
		token, managed, err := f.runtime.IssueFreshInvestigationOwnerManagementTools(ctx, role, work, nil)
		if strict {
			roleOK(t, err)
			if token == "" || !slices.Equal(managed.Operations, want) || !slices.Equal(managed.DelegateCandidateIDs, candidates) {
				t.Fatal("strict original user admission lost the managed owner profile")
			}
		} else {
			if token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
				t.Fatalf("explicit issuance did not deny management for the original member source: %v", err)
			}
			// A denial must not occupy the Turn's one registration slot.
			token, _, err = f.runtime.IssueFreshInvestigationOwnerTools(ctx, role, work, nil)
			roleOK(t, err)
		}
		defer tools.Revoke(token)
		ops, err := tools.TransportOperations(ctx, token)
		roleOK(t, err)
		if !slices.Equal(ops, want) {
			t.Fatalf("registered %v, want %v", ops, want)
		}
		f.acceptWork(t, owner)
		if isReport {
			got, err := tools.ReadWorkReport(ctx, token)
			roleOK(t, err)
			if got.ReportID != report.ID || got.Summary != report.Summary {
				t.Fatal("did not read the exact wake-selected report")
			}
		} else {
			_, err := tools.ReadInvestigationChannel(ctx, token, store.ListOptions{})
			roleOK(t, err)
		}
		got, err := tools.GetFreshTaskWorkInput(ctx, token, input.WorkInputID)
		roleOK(t, err)
		if got.Content != userReply.Content || got.Disposition != "pending" {
			t.Fatal("did not read the exact pending input")
		}
		roleOK(t, tools.AckFreshTaskWorkInput(ctx, token, input.WorkInputID, "ack"))
		sub, err := tools.SubmitFreshTaskWorkResult(ctx, token, "本轮调查结论。", "submit")
		roleOK(t, err)
		if sub.TaskID != admitted.TaskID || sub.AttemptID != attempt.ID {
			t.Fatal("submission was not bound to the original task and current Attempt")
		}
	})
}

func TestPiInvestigationOwnerNativeNoAuthorityNoLaunch(t *testing.T) {
	for _, mode := range []string{"admission-disabled", "coordinator-declined"} {
		t.Run(mode, func(t *testing.T) {
			f := newBoundWorkReportFixture(t)
			peer := ownerFixtureMember(t, f, f.group, "would-be delegate")
			prep := newInvestigationOwnerPreparer(t, f, nil, true, nil, nil)
			admission := f.assessInvestigationFixture(t, mode != "admission-disabled", nil, 1, peer.ID)
			if mode == "admission-disabled" {
				if admission.Outcome != "not_admitted" || admission.TaskID != "" || admission.DispatchRequestID != "" {
					t.Fatal("C owner opt-in supplied B admission authority")
				}
			} else {
				decision := f.decideInvestigationFixture(t, admission, "decline")
				if decision.Decision.Kind != domain.CoordinatorDecisionKindDecline || decision.Assignment != nil {
					t.Fatal("fixture lacks a definitive B decline")
				}
			}
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			seen := 0
			err := scheduler.Run(ctx, f.db, scheduler.Options{Recovered: true, RuntimeDir: f.root, MaxActive: 1, PollInterval: time.Millisecond,
				IntentPass: func(context.Context) {
					seen++
					if seen == 2 {
						stop() // One complete scan without any provider invocation.
					}
				}}, prep.Prepare)
			if !errors.Is(err, context.Canceled) {
				t.Fatal("scheduler did not complete the empty scan", err)
			}
			ownerNativeRows(t, f, 0, `SELECT count(*) FROM turns WHERE member_id IN (?,?)`, f.worker.ID, peer.ID)
			ownerNativeRows(t, f, 0, `SELECT count(*) FROM coordination_assignments`)
			ownerNativeRows(t, f, 0, `SELECT count(*) FROM task_work_delegations`)
			ownerNativeRows(t, f, 0, `SELECT count(*) FROM resource_claims`)
		})
	}
}
