package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

type memberToolRuntimeExecutorFunc func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt

func (f memberToolRuntimeExecutorFunc) Execute(ctx context.Context, request provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
	return f(ctx, request, onEvent)
}

type memberToolRuntimeFixture struct {
	localMessageFixture
	runtime *MemberToolRuntime
	options MemberToolRuntimeOptions
	binding MemberToolRunBinding
	request provider.ExecuteRequest
	claim   store.TurnClaim
}

func setupMemberToolRuntime(t *testing.T) memberToolRuntimeFixture {
	t.Helper()
	f := setupLocalMessages(t)
	base := t.TempDir()
	for _, name := range []string{"config", "runtime", "cwd"} {
		if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	member := f.member(t, f.groupID)
	message, err := f.local.CreateMessage(context.Background(), f.groupID, MessageInput{Content: "input", TopicID: "topic", RecipientIDs: []string{member.ID}}, "")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"runtime-test-resource"}, MaxActive: 4})
	if err != nil {
		t.Fatal(err)
	}
	options := MemberToolRuntimeOptions{ConfigRoot: filepath.Join(base, "config"), Endpoint: "http://127.0.0.1:7310/api/v1/member-tools", Operations: []string{"list_members", "read_channel", "send_message", "create_member"}}
	runtime, err := NewMemberToolRuntime(f.db, options)
	if err != nil {
		t.Fatal(err)
	}
	return memberToolRuntimeFixture{
		localMessageFixture: f, runtime: runtime, options: options,
		binding: MemberToolRunBinding{GroupID: turn.GroupID, MemberID: turn.MemberID, ConversationID: turn.ConversationID, DeliveryID: turn.DeliveryID, ProviderID: "trusted-provider", Model: "trusted-model"},
		request: provider.ExecuteRequest{RunID: turn.ID, RuntimeDir: filepath.Join(base, "runtime"), CWD: filepath.Join(base, "cwd"), Model: "trusted-model", Prompt: "PRIVATE-PROMPT", Thinking: "max"},
		claim:   store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken},
	}
}

func (f memberToolRuntimeFixture) wrap(t *testing.T, factory MemberToolExecutorFactory) provider.Executor {
	t.Helper()
	executor, err := f.runtime.Wrap(f.binding, factory)
	if err != nil || executor == nil {
		t.Fatalf("Wrap failed: %v", err)
	}
	return executor
}

func memberToolRuntimeConfig(t *testing.T, path string) (string, string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || info.Mode() != 0600 || !filepath.IsAbs(path) {
		t.Fatalf("config must be an absolute regular 0600 file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]string
	if json.Unmarshal(data, &config) != nil || len(config) != 2 || config["endpoint"] == "" || config["token"] == "" {
		t.Fatal("config must contain exactly endpoint and token")
	}
	return config["endpoint"], config["token"]
}

func memberToolRuntimeEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory was modified or config was not cleaned: %v", err)
	}
}

func (f memberToolRuntimeFixture) requireUnsettled(t *testing.T, state domain.TurnState) {
	t.Helper()
	turn, err := f.db.GetTurn(context.Background(), f.claim.GroupID, f.claim.TurnID)
	if err != nil || turn.State != state || turn.ClaimToken != f.claim.Token || turn.ReplyMessageID != "" {
		t.Fatalf("wrapper changed durable turn ownership: state=%s, error=%v", turn.State, err)
	}
	var resources int
	if err := f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", f.claim.TurnID).Scan(&resources); err != nil || resources != 1 {
		t.Fatalf("wrapper released resource claims: count=%d, error=%v", resources, err)
	}
}

func (f memberToolRuntimeFixture) requirePreparationReceipt(t *testing.T, receipt provider.ExecutionReceipt, outcome provider.Outcome) {
	t.Helper()
	if receipt.Outcome != outcome || receipt.Started || receipt.Accepted || receipt.Terminal != "" || receipt.NativeSessionID != "" || receipt.OutputComplete || receipt.DirectChildReaped || receipt.DeliveryFailed || receipt.Reason == "" {
		t.Fatalf("incorrect preparation receipt: outcome=%s", receipt.Outcome)
	}
	if outcome == provider.OutcomeUnknown && !strings.Contains(receipt.Reason, "对账") {
		t.Error("unknown receipt must request reconciliation")
	}
	for _, secret := range []string{"PRIVATE-", f.claim.Token, f.request.Prompt, f.options.ConfigRoot, f.request.RuntimeDir} {
		if strings.Contains(receipt.Reason, secret) {
			t.Error("preparation reason leaked private input")
		}
	}
}

func TestMemberToolRuntimeWrapHasNoSideEffects(t *testing.T) {
	f := setupMemberToolRuntime(t)
	called := false
	executor := f.wrap(t, func(string, string, []string) (provider.Executor, error) {
		called = true
		return nil, errors.New("PRIVATE-FACTORY")
	})
	if called || len(f.runtime.Tools().capabilities) != 0 || f.runtime.Tools() != f.runtime.Tools() || f.runtime.Tools().db != f.db {
		t.Fatal("Wrap invoked a factory, issued authority, or changed registry/store")
	}
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
	memberToolRuntimeEmpty(t, f.request.RuntimeDir)
	f.requireUnsettled(t, domain.TurnStateStarting)
	// No actual claim exists for this run, despite a syntactically valid binding.
	request := f.request
	request.RunID = "unclaimed"
	f.requirePreparationReceipt(t, executor.Execute(context.Background(), request, nil), provider.OutcomeUnknown)
	if called || len(f.runtime.Tools().capabilities) != 0 {
		t.Fatal("an unclaimed run obtained authority")
	}
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
}

func TestMemberToolRuntimeClaimAndLifecycleDenials(t *testing.T) {
	for _, state := range []string{"group", "member", "conversation", "delivery", "model", "run-id", "cancel", "session", "running", "candidate-terminal", "unknown", "recovered", "left", "not_started", "succeeded", "failed", "aborted", "truncated"} {
		t.Run(state, func(t *testing.T) {
			f := setupMemberToolRuntime(t)
			ctx := context.Background()
			var err error
			switch state {
			case "group":
				f.binding.GroupID = f.otherID
			case "member":
				f.binding.MemberID = "wrong-member"
			case "conversation":
				f.binding.ConversationID = "wrong-conversation"
			case "delivery":
				f.binding.DeliveryID = "wrong-delivery"
			case "model":
				f.request.Model = "PRIVATE-MODEL"
			case "run-id":
				f.request.RunID = "../PRIVATE-PATH"
			case "cancel":
				_, err = f.db.RequestTurnCancel(ctx, f.claim.GroupID, f.claim.TurnID)
			case "session":
				err = f.db.RecordTurnEvent(ctx, f.claim, provider.Event{Kind: provider.SessionEvent, NativeSessionID: "native"})
			case "running", "candidate-terminal":
				err = f.db.RecordTurnEvent(ctx, f.claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native"})
				if err == nil && state == "candidate-terminal" {
					err = f.db.RecordTurnEvent(ctx, f.claim, provider.Event{Kind: provider.TerminalEvent, NativeSessionID: "native", Terminal: provider.OutcomeSucceeded})
				}
			case "recovered":
				_, err = f.db.RecoverInFlight(ctx)
			case "left":
				_, err = f.raw.Exec("UPDATE members SET status='left' WHERE id=?", f.binding.MemberID)
			default:
				outcome := provider.Outcome(state)
				receipt := provider.ExecutionReceipt{Outcome: outcome}
				if state != "unknown" && state != "not_started" {
					receipt = provider.ExecutionReceipt{Outcome: outcome, Terminal: outcome, Started: true, Accepted: true, NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true}
				}
				_, err = f.db.ApplyTurnReceipt(ctx, f.claim, receipt)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := f.db.GetTurn(ctx, f.claim.GroupID, f.claim.TurnID)
			if err != nil {
				t.Fatal(err)
			}
			called := false
			executor := f.wrap(t, func(string, string, []string) (provider.Executor, error) {
				called = true
				return nil, errors.New("PRIVATE-FACTORY")
			})
			f.requirePreparationReceipt(t, executor.Execute(ctx, f.request, nil), provider.OutcomeUnknown)
			after, err := f.db.GetTurn(ctx, f.claim.GroupID, f.claim.TurnID)
			if called || len(f.runtime.Tools().capabilities) != 0 || err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("denied execution launched native code, issued authority, or changed turn")
			}
			memberToolRuntimeEmpty(t, f.options.ConfigRoot)
			memberToolRuntimeEmpty(t, f.request.RuntimeDir)
		})
	}
}

func TestMemberToolRuntimeNativeReceiptAndSecretIsolation(t *testing.T) {
	for _, outcome := range []provider.Outcome{provider.OutcomeSucceeded, provider.OutcomeFailed, provider.OutcomeAborted, provider.OutcomeTruncated, provider.OutcomeUnknown, provider.OutcomeNotStarted} {
		t.Run(string(outcome), func(t *testing.T) {
			f := setupMemberToolRuntime(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := provider.ExecutionReceipt{Started: true, Accepted: true, Outcome: outcome, Terminal: provider.OutcomeFailed, NativeSessionID: "native-session", DeliveryFailed: true, DirectChildReaped: true, OutputComplete: true, ExitCode: 7, LogsTruncated: true, StdoutPath: "native-stdout", StderrPath: "native-stderr", Reason: "原生回执"}
			event := provider.Event{Kind: provider.ResponseEvent, NativeSessionID: "native-session", Text: "原生输出"}
			var path, token string
			var events []provider.Event
			factoryCalls, nativeCalls := 0, 0
			executor := f.wrap(t, func(configPath, runID string, names []string) (provider.Executor, error) {
				factoryCalls++
				path = configPath
				endpoint, secret := memberToolRuntimeConfig(t, path)
				token = secret
				if endpoint != f.options.Endpoint || runID != f.claim.TurnID || !slices.Equal(names, f.options.Operations) || filepath.Dir(path) != f.options.ConfigRoot || token == f.claim.Token {
					t.Error("factory received incorrect trusted arguments/config")
				}
				memberToolRuntimeEmpty(t, f.request.RuntimeDir)
				names[0] = "attacker-operation"
				return memberToolRuntimeExecutorFunc(func(gotCtx context.Context, request provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
					nativeCalls++
					if gotCtx != ctx || request != f.request || onEvent == nil {
						t.Error("wrapper changed native execution arguments")
					}
					if _, err := f.runtime.Tools().ListMembers(ctx, token, store.ListOptions{}); err != nil {
						t.Error("capability was unusable during native execution")
					}
					if err := onEvent(event); err == nil {
						t.Error("wrapper swallowed the callback's error")
					}
					if outcome == provider.OutcomeAborted || outcome == provider.OutcomeUnknown {
						cancel()
					}
					return want
				}), nil
			})
			got := executor.Execute(ctx, f.request, func(event provider.Event) error {
				events = append(events, event)
				return errors.New("PRIVATE-EVENT-ERROR")
			})
			if got != want || factoryCalls != 1 || nativeCalls != 1 || !reflect.DeepEqual(events, []provider.Event{event}) {
				t.Fatal("wrapper changed native receipt/events or invoked more than once")
			}
			data, err := json.Marshal([]any{f.request, events, got, []any{path, f.claim.TurnID, f.options.Operations}})
			if err != nil || strings.Contains(string(data), token) || strings.Contains(f.request.Prompt, path) {
				t.Fatal("capability leaked outside its private config")
			}
			requireCapabilityDenied(t, capabilityUse(context.Background(), f.runtime.Tools(), token, "list_members"))
			memberToolRuntimeEmpty(t, f.options.ConfigRoot)
			memberToolRuntimeEmpty(t, f.request.RuntimeDir)
			f.requireUnsettled(t, domain.TurnStateStarting)
			f.counts(t, 1, 1, 1, 0)
		})
	}
}

func TestMemberToolRuntimeSubsetWithoutCreation(t *testing.T) {
	f := setupMemberToolRuntime(t)
	options := f.options
	options.Operations = []string{"list_members", "read_channel"}
	var err error
	f.runtime, err = NewMemberToolRuntime(f.db, options)
	if err != nil {
		t.Fatal(err)
	}
	f.binding.ProviderID, f.binding.Model, f.request.Model = "", "", ""
	var token string
	nativeCalled := false
	executor := f.wrap(t, func(path, runID string, names []string) (provider.Executor, error) {
		_, token = memberToolRuntimeConfig(t, path)
		if runID != f.claim.TurnID || !slices.Equal(names, options.Operations) {
			t.Error("factory advertised more than the granted subset")
		}
		return memberToolRuntimeExecutorFunc(func(ctx context.Context, request provider.ExecuteRequest, onEvent func(provider.Event) error) provider.ExecutionReceipt {
			nativeCalled = true
			if request != f.request || onEvent != nil {
				t.Error("wrapper changed native arguments")
			}
			if _, err := f.runtime.Tools().ReadChannel(ctx, token, store.ListOptions{}); err != nil {
				t.Error("granted subset was not usable")
			}
			_, err := f.runtime.Tools().SendMessage(ctx, token, MemberMessageInput{Content: "denied"}, "denied")
			requireCapabilityDenied(t, err)
			_, err = f.runtime.Tools().CreateMember(ctx, token, MemberCreateInput{Name: "denied", Role: "denied", Reason: "denied"}, "denied")
			requireCapabilityDenied(t, err)
			return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
		}), nil
	})
	executor.Execute(context.Background(), f.request, nil)
	if !nativeCalled {
		t.Fatal("valid subset failed to reach native execution")
	}
	requireCapabilityDenied(t, capabilityUse(context.Background(), f.runtime.Tools(), token, "read_channel"))
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
	f.requireUnsettled(t, domain.TurnStateStarting)
}

func TestMemberToolRuntimeCopiesTrustedGrantsAndBinding(t *testing.T) {
	for _, capacity := range []int{0, 2} {
		t.Run(map[int]string{0: "default-four", 2: "explicit-two"}[capacity], func(t *testing.T) {
			f := setupMemberToolRuntime(t)
			options, binding := f.options, f.binding
			options.MaxGroupMembers = capacity
			var err error
			f.runtime, err = NewMemberToolRuntime(f.db, options)
			if err != nil {
				t.Fatal(err)
			}
			wantOperations := slices.Clone(options.Operations)
			wantCapacity := capacity
			if wantCapacity == 0 {
				wantCapacity = 4
			}
			executor, err := f.runtime.Wrap(binding, func(path, runID string, names []string) (provider.Executor, error) {
				endpoint, token := memberToolRuntimeConfig(t, path)
				if endpoint != f.options.Endpoint || runID != f.claim.TurnID || !slices.Equal(names, wantOperations) {
					t.Error("caller mutation changed factory selection")
				}
				return memberToolRuntimeExecutorFunc(func(ctx context.Context, request provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
					err := f.runtime.Tools().withAuthorized(ctx, token, "create_member", func(scope store.MemberToolScope, grants MemberToolGrants) error {
						if scope.Claim != f.claim || scope.MemberID != f.binding.MemberID || grants.ProviderID != f.binding.ProviderID || grants.Model != f.binding.Model || grants.MaxGroupMembers != wantCapacity || !slices.Equal(grants.Operations, wantOperations) {
							t.Error("trusted policy or claim was not copied exactly")
						}
						grants.Operations[0] = "attacker-operation"
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					for i := 1; i <= wantCapacity; i++ {
						input := MemberCreateInput{Name: "helper", Role: strings.Repeat("role", i), Reason: "help with the current discussion"}
						result, err := f.runtime.Tools().CreateMember(ctx, token, input, input.Role)
						if i == wantCapacity {
							if !errors.Is(err, store.ErrMemberCapacity) {
								t.Fatal("caller mutation bypassed trusted capacity")
							}
						} else if err != nil || result.Member.ProviderID != f.binding.ProviderID || result.Member.Model != f.binding.Model || result.Member.Workspace != "" || result.Member.CreatorID != f.binding.MemberID {
							t.Fatal("creation did not inherit trusted provider/model without external cwd")
						}
					}
					return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
				}), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			options.Operations[0], options.Endpoint, options.ConfigRoot, options.MaxGroupMembers = "attacker-operation", "PRIVATE-ENDPOINT", "PRIVATE-PATH", 200
			binding.GroupID, binding.MemberID, binding.ConversationID, binding.DeliveryID, binding.ProviderID, binding.Model = "other", "other", "other", "other", "other", "other"
			executor.Execute(context.Background(), f.request, nil)
			memberToolRuntimeEmpty(t, f.options.ConfigRoot)
			f.requireUnsettled(t, domain.TurnStateStarting)
		})
	}
}

func TestMemberToolRuntimeRevokeDrainsAfterCancellation(t *testing.T) {
	f := setupMemberToolRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, nativeReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	used, result := make(chan error, 1), make(chan provider.ExecutionReceipt, 1)
	var path, token string
	want := provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown, Terminal: provider.OutcomeFailed, Reason: "原生取消回执"}
	executor := f.wrap(t, func(configPath, _ string, _ []string) (provider.Executor, error) {
		path = configPath
		_, token = memberToolRuntimeConfig(t, path)
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			go func() {
				used <- f.runtime.Tools().withAuthorized(context.Background(), token, "list_members", func(scope store.MemberToolScope, _ MemberToolGrants) error {
					if _, err := f.db.ListMembersForTool(context.Background(), scope, store.ListOptions{}); err != nil {
						return err
					}
					close(entered)
					<-release
					return nil
				})
			}()
			<-entered
			cancel()
			close(nativeReturned)
			return want
		}), nil
	})
	go func() { result <- executor.Execute(ctx, f.request, nil) }()
	<-nativeReturned
	select {
	case <-result:
		t.Fatal("wrapper bypassed revocation drain after cancellation")
	case <-time.After(6 * time.Second):
		// Revocation must still join after the independent verification window.
	}
	f.requireUnsettled(t, domain.TurnStateStarting)
	if _, err := os.Lstat(path); err != nil {
		t.Error("config disappeared before authorized operations drained")
	}
	once.Do(func() { close(release) })
	if err := <-used; err != nil {
		t.Fatal(err)
	}
	if got := <-result; got != want {
		t.Fatal("revocation changed native cancellation evidence")
	}
	requireCapabilityDenied(t, capabilityUse(context.Background(), f.runtime.Tools(), token, "list_members"))
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
	f.requireUnsettled(t, domain.TurnStateStarting)
}

func TestMemberToolRuntimeDuplicateExecutionRetainsUnknownClaims(t *testing.T) {
	f := setupMemberToolRuntime(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var factories, natives atomic.Int32
	executor := f.wrap(t, func(string, string, []string) (provider.Executor, error) {
		factories.Add(1)
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			natives.Add(1)
			close(entered)
			<-release
			return provider.ExecutionReceipt{Started: true, Outcome: provider.OutcomeUnknown}
		}), nil
	})
	first := make(chan provider.ExecutionReceipt, 1)
	go func() { first <- executor.Execute(context.Background(), f.request, nil) }()
	<-entered
	duplicate := executor.Execute(context.Background(), f.request, nil)
	f.requirePreparationReceipt(t, duplicate, provider.OutcomeUnknown)
	once.Do(func() { close(release) })
	<-first
	f.requirePreparationReceipt(t, executor.Execute(context.Background(), f.request, nil), provider.OutcomeUnknown)
	if factories.Load() != 1 || natives.Load() != 1 {
		t.Fatal("duplicate wrapper execution launched native twice")
	}
	f.requireUnsettled(t, domain.TurnStateStarting)
	if turn, err := f.db.ApplyTurnReceipt(context.Background(), f.claim, duplicate); err != nil || turn.State != domain.TurnStateUnknown {
		t.Fatalf("scheduler-equivalent application lost unknown: %v", err)
	}
	f.requireUnsettled(t, domain.TurnStateUnknown)
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
	memberToolRuntimeEmpty(t, f.request.RuntimeDir)
}

func TestMemberToolRuntimeIssueFailureIsUnknownAndDoesNotRevokeOthers(t *testing.T) {
	f := setupMemberToolRuntime(t)
	ctx := context.Background()
	token, err := f.runtime.Tools().Issue(ctx, f.claim, MemberToolGrants{Operations: []string{"list_members"}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.runtime.Tools().Revoke(token)
	called := false
	executor := f.wrap(t, func(string, string, []string) (provider.Executor, error) {
		called = true
		return nil, errors.New("PRIVATE-FACTORY")
	})
	receipt := executor.Execute(ctx, f.request, nil)
	f.requirePreparationReceipt(t, receipt, provider.OutcomeUnknown)
	if called || capabilityUse(ctx, f.runtime.Tools(), token, "list_members") != nil {
		t.Fatal("failed Issue launched native or revoked prior authority")
	}
	f.requireUnsettled(t, domain.TurnStateStarting)
	if _, err := f.db.ApplyTurnReceipt(ctx, f.claim, receipt); err != nil {
		t.Fatal(err)
	}
	f.requireUnsettled(t, domain.TurnStateUnknown)
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
}

func TestMemberToolRuntimeFactoryFailuresAndGenerationFence(t *testing.T) {
	for _, failure := range []string{"error", "nil", "cancel", "recovered", "terminal", "cancel-turn", "recovered-error", "logs-error", "cancel-turn-error", "terminal-error", "cancel-recovered", "generation"} {
		t.Run(failure, func(t *testing.T) {
			f := setupMemberToolRuntime(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var token string
			nativeCalled := false
			executor := f.wrap(t, func(path, _ string, _ []string) (provider.Executor, error) {
				_, token = memberToolRuntimeConfig(t, path)
				var err error
				switch failure {
				case "error":
					return nil, errors.New("PRIVATE-FACTORY " + token + " " + path + " " + f.request.Prompt)
				case "nil":
					return nil, nil
				case "cancel":
					cancel()
				case "recovered", "recovered-error", "cancel-recovered":
					_, err = f.db.RecoverInFlight(context.Background())
					if failure == "cancel-recovered" {
						cancel()
					}
				case "terminal", "terminal-error":
					_, err = f.db.ApplyTurnReceipt(ctx, f.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted})
				case "cancel-turn", "cancel-turn-error":
					_, err = f.db.RequestTurnCancel(ctx, f.claim.GroupID, f.claim.TurnID)
				case "logs-error":
					err = os.Mkdir(filepath.Join(f.request.RuntimeDir, f.request.RunID), 0700)
				case "generation":
					_, err = f.raw.Exec("UPDATE turns SET claim_token='replacement-claim' WHERE id=?", f.claim.TurnID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(failure, "-error") {
					return nil, errors.New("PRIVATE-FACTORY " + token)
				}
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					nativeCalled = true
					return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded}
				}), nil
			})
			want := provider.OutcomeUnknown
			if failure == "error" || failure == "nil" || failure == "cancel" {
				want = provider.OutcomeNotStarted
			}
			f.requirePreparationReceipt(t, executor.Execute(ctx, f.request, nil), want)
			if nativeCalled {
				t.Fatal("failed preparation or fenced generation reached native execution")
			}
			requireCapabilityDenied(t, capabilityUse(context.Background(), f.runtime.Tools(), token, "list_members"))
			memberToolRuntimeEmpty(t, f.options.ConfigRoot)
			if failure != "logs-error" {
				memberToolRuntimeEmpty(t, f.request.RuntimeDir)
			}
			if failure == "error" || failure == "nil" || failure == "cancel" {
				f.requireUnsettled(t, domain.TurnStateStarting)
			}
			f.requirePreparationReceipt(t, executor.Execute(context.Background(), f.request, nil), provider.OutcomeUnknown)
		})
	}
}
