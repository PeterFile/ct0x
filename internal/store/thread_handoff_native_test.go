package store_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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

const handoffTopic = "原话题"
const handoffModel = "fixture-model"
const handoffQuestion = "要只看工作日吗？"
const handoffAnswer = "  只看工作日🙂\n"

func handoffOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type nativeHandoffFixture struct {
	db                                            *store.Store
	raw                                           *sql.DB
	local                                         *platform.LocalUser
	preparer                                      *platform.Preparer
	member                                        domain.Member
	root, runtime, config, workspaces, executable string
	endpoint                                      string
	handler                                       http.Handler
}

func newNativeHandoffFixture(t *testing.T, withSchema bool) *nativeHandoffFixture {
	t.Helper()
	f := &nativeHandoffFixture{root: t.TempDir()}
	// Borrow F's real unregistered fixture fragments, not production migration slots.
	db, path := store.NativeThreadHandoffFixture(t, withSchema)
	f.db = db
	var err error
	f.raw, err = sql.Open("sqlite", path)
	handoffOK(t, err)
	t.Cleanup(func() { _ = f.raw.Close() })
	f.local = platform.NewLocalUser(f.db)
	group, err := f.local.CreateGroup(t.Context(), platform.GroupInput{Name: "原生交接"}, "")
	handoffOK(t, err)
	f.member, err = f.local.CreateMember(t.Context(), group.ID, platform.MemberInput{Name: "原提问者", Role: "fixture", ProviderID: "codex-fixture", Model: handoffModel}, "")
	handoffOK(t, err)
	f.runtime, f.config, f.workspaces = filepath.Join(f.root, "runtime"), filepath.Join(f.root, "tools"), filepath.Join(f.root, "cwd")
	for _, dir := range []string{f.runtime, f.config, f.workspaces} {
		handoffOK(t, os.Mkdir(dir, 0700))
	}
	f.executable = filepath.Join(f.root, "fake-codex")
	// Inert unless a test explicitly installs a fixture subprocess.
	handoffOK(t, os.WriteFile(f.executable, []byte("#!/bin/sh\nexit 91\n"), 0700))
	return f
}

func (f *nativeHandoffFixture) configure(t *testing.T, version string, resume bool) {
	t.Helper()
	if f.endpoint == "" {
		f.endpoint = "http://127.0.0.1:7310/api/v1/member-tools"
	}
	runtime, err := platform.NewMemberToolRuntime(f.db, platform.MemberToolRuntimeOptions{
		ConfigRoot: f.config, Endpoint: f.endpoint, RoleMaxActive: 2, RoleExecutionRoot: f.runtime,
		ChatIntake: coordination.New(f.db), Operations: []string{"list_members", "read_channel", "send_message", "create_member"},
	})
	handoffOK(t, err)
	t.Cleanup(func() { _ = runtime.Close() })
	f.preparer, err = platform.NewPreparer(f.db, platform.PrepareOptions{
		MemberTools: runtime, EnableCodexAppServer: true, EnableNativeSessionResume: resume, PrivateWorkspaceRoot: f.workspaces,
		Providers: []provider.Provider{{ID: f.member.ProviderID, Name: provider.Codex, Version: version, Path: f.executable, Installation: provider.Installed}},
	})
	handoffOK(t, err)
	routes := append(httpapi.MemberToolRoutes(runtime.Tools()), httpapi.MemberMCPRoutes(runtime.Tools())...)
	address := strings.TrimSuffix(strings.TrimPrefix(f.endpoint, "http://"), "/api/v1/member-tools")
	server, err := httpapi.NewServer(address, routes...)
	handoffOK(t, err)
	f.handler = server.Handler
}

func (f *nativeHandoffFixture) message(t *testing.T, topic string, recipients []string) domain.Message {
	t.Helper()
	m, err := f.local.CreateMessage(t.Context(), f.member.GroupID, platform.MessageInput{
		TopicID: topic, RecipientIDs: recipients, Content: "合成来源：整理已知样本。",
	}, "")
	handoffOK(t, err)
	return m
}

func (f *nativeHandoffFixture) seedAnswer(t *testing.T) (domain.DecisionAnswerReceipt, provider.ExecuteRequest) {
	t.Helper()
	ctx := t.Context()
	m := f.message(t, handoffTopic, []string{f.member.ID})
	p, err := f.preparer.Prepare(ctx, store.PendingDelivery{GroupID: m.GroupID, SenderID: m.SenderID, Delivery: m.Deliveries[0]})
	handoffOK(t, err) // Materializes the existing default CWD, no process runs.
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: m.GroupID, DeliveryID: m.Deliveries[0].ID, ResourceKeys: p.ResourceKeys, MaxActive: 2})
	handoffOK(t, err)
	scope, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	handoffOK(t, err)
	engine := coordination.New(f.db)
	candidate, err := engine.CaptureChatCandidate(ctx, scope)
	handoffOK(t, err)
	q, err := engine.AssessChatDecisionClarification(ctx, scope, candidate.ID,
		domain.ChatAssessment{Disposition: "clarify", Reason: "边界未定", Question: handoffQuestion},
		domain.DecisionContext{Facts: "样本有限", Recommendation: "先看工作日"}, handoffQuestion, "seed-ask")
	handoffOK(t, err)
	_, err = f.db.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted})
	handoffOK(t, err)
	a, err := f.db.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, domain.DecisionAnswerInput{Version: q.Version, Text: handoffAnswer}, "seed-answer")
	handoffOK(t, err)
	return a, p.Request
}

func (f *nativeHandoffFixture) assertSavedOnly(t *testing.T, a domain.DecisionAnswerReceipt) {
	t.Helper()
	view, err := f.db.GetDecisionAnswerView(t.Context(), a.ThreadRef, a.RequestID, a.Version)
	handoffOK(t, err)
	if view.Receipt != a || view.Handling != nil {
		t.Fatalf("receipt or handling changed: saved=%t handling=%+v", view.Receipt == a, view.Handling)
	}
}

func TestNativeThreadHandoffRequiresInstalledFAndVerifiedAdapter(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		schema, ready bool
	}{
		{"v30", "0.166.0", false, false}, {"unverified", "0.166.1", true, false}, {"ready", "0.166.0", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeHandoffFixture(t, tc.schema)
			f.configure(t, tc.version, false)
			prepare, err := f.preparer.EnableThreadDecisionHandoff(t.Context())
			if (err == nil) != tc.ready || (prepare != nil) != tc.ready {
				t.Fatalf("readiness: callback=%t error=%v", prepare != nil, err)
			}
		})
	}
}

func TestNativeThreadHandoffFencesFrozenStartupAndRetainsUnknown(t *testing.T) {
	for _, change := range []string{"provider", "model", "workspace", "replaced-directory", "request-cwd", "resource-set", "claim-token", "superseded", "cancel", "logged"} {
		t.Run(change, func(t *testing.T) {
			ctx := t.Context()
			f := newNativeHandoffFixture(t, true)
			f.configure(t, "0.166.0", false)
			prepare, err := f.preparer.EnableThreadDecisionHandoff(ctx)
			handoffOK(t, err)
			a, original := f.seedAnswer(t)
			wake, err := f.db.ResolveThreadDecisionWake(ctx, a.GroupID, a.WakeDeliveryID)
			handoffOK(t, err)
			page, err := f.db.ListPendingDeliveries(ctx, store.PendingOptions{Lane: store.PendingThreadDecisions})
			handoffOK(t, err)
			if len(page.Items) != 1 || page.Items[0].Delivery.ID != a.WakeDeliveryID {
				t.Fatal("typed lane did not select only the exact answer")
			}
			d := page.Items[0]
			foreign := wake
			foreign.ConversationID = "foreign-conversation"
			if plan, err := prepare(ctx, d, foreign); !errors.Is(err, scheduler.ErrNotReady) || plan.Execute != nil {
				t.Fatal("mismatched scope reached continuation planning")
			}
			plan, err := prepare(ctx, d, wake)
			handoffOK(t, err)
			if plan.Request.CWD != original.CWD || plan.Request.Model != handoffModel || len(plan.ResourceKeys) != 1 {
				t.Fatal("typed plan did not use the original private scope and resource")
			}
			keys := plan.ResourceKeys
			if change == "resource-set" {
				keys = append([]string{"extra-resource"}, keys...)
			}
			turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: a.GroupID, DeliveryID: a.WakeDeliveryID,
				ThreadDecisionAnswerID: a.ID, ResourceKeys: keys, MaxActive: 2})
			handoffOK(t, err)
			claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
			invokeClaim := claim
			request := plan.Request
			request.RunID, request.RuntimeDir = turn.ID, f.runtime
			switch change {
			case "provider", "model", "workspace":
				column := map[string]string{"provider": "provider_id", "model": "model", "workspace": "workspace"}[change]
				_, err = f.raw.Exec("UPDATE members SET "+column+"=? WHERE id=?", f.root, f.member.ID)
			case "replaced-directory":
				handoffOK(t, os.Rename(request.CWD, request.CWD+"-old"))
				err = os.Mkdir(request.CWD, 0700)
			case "request-cwd":
				request.CWD = f.root
			case "claim-token":
				invokeClaim.Token = "not-the-claimed-generation"
			case "superseded":
				f.message(t, handoffTopic, []string{})
			case "cancel":
				_, err = f.db.RequestTurnCancel(ctx, a.GroupID, turn.ID)
			case "logged":
				err = os.Mkdir(filepath.Join(f.runtime, turn.ID), 0700)
			}
			handoffOK(t, err)
			receipt := plan.Execute(ctx, invokeClaim, request, func(provider.Event) error { t.Fatal("stale plan invoked provider"); return nil })
			if receipt.Outcome != provider.OutcomeUnknown || receipt.Started {
				t.Fatalf("stale start was not kept unknown: %+v", receipt)
			}
			if _, err := os.Stat(filepath.Join(f.runtime, turn.ID, "stdout.jsonl")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("stale plan reached native invocation")
			}
			settled, err := f.db.ApplyTurnReceipt(ctx, claim, receipt)
			handoffOK(t, err)
			held, err := f.db.HasResourceClaim(ctx, plan.ResourceKeys[0])
			handoffOK(t, err)
			if settled.State != domain.TurnStateUnknown || !held {
				t.Fatal("unknown released its claim")
			}
			f.assertSavedOnly(t, a)
		})
	}
}

func TestNativeThreadHandoffOwnerIsOneUsePerClaim(t *testing.T) {
	ctx := t.Context()
	f := newNativeHandoffFixture(t, true)
	f.configure(t, "0.166.0", false)
	prepare, err := f.preparer.EnableThreadDecisionHandoff(ctx)
	handoffOK(t, err)
	a, _ := f.seedAnswer(t)
	wake, err := f.db.ResolveThreadDecisionWake(ctx, a.GroupID, a.WakeDeliveryID)
	handoffOK(t, err)
	deliveries, err := f.db.ListPendingDeliveries(ctx, store.PendingOptions{Lane: store.PendingThreadDecisions})
	handoffOK(t, err)
	if len(deliveries.Items) != 1 {
		t.Fatal("missing exact typed delivery")
	}
	first, err := prepare(ctx, deliveries.Items[0], wake)
	handoffOK(t, err)
	second, err := prepare(ctx, deliveries.Items[0], wake)
	handoffOK(t, err)
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: a.GroupID, DeliveryID: a.WakeDeliveryID,
		ThreadDecisionAnswerID: a.ID, MaxActive: 2, ResourceKeys: first.ResourceKeys})
	handoffOK(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	request := first.Request
	request.RunID, request.RuntimeDir = turn.ID, f.runtime
	result := first.Execute(ctx, claim, request, nil) // The inert fixture starts but emits no native evidence.
	if !result.Started || result.Outcome != provider.OutcomeUnknown || result.Accepted {
		t.Fatalf("fixture did not launch once with unknown outcome: %+v", result)
	}
	// Test-only remove the file barrier. The claim is still starting and two
	// requests had independent prepared wrappers. Only the shared owner now
	// excludes a new wrapper from regaining launch authority.
	handoffOK(t, os.RemoveAll(filepath.Join(f.runtime, turn.ID)))
	for _, plan := range []scheduler.PreparedThreadDecision{first, second} {
		if repeat := plan.Execute(ctx, claim, request, nil); repeat.Started || repeat.Outcome != provider.OutcomeUnknown {
			t.Fatalf("same claim launched again: %+v", repeat)
		}
	}
	_, err = f.db.ApplyTurnReceipt(ctx, claim, result)
	handoffOK(t, err)
	f.assertSavedOnly(t, a)
}

func waitNativeHandoff(t *testing.T, db *store.Store, group, message string) domain.Turn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		m, err := db.GetMessage(ctx, group, message)
		handoffOK(t, err)
		if id := m.Deliveries[0].TurnID; id != "" {
			turn, err := db.GetTurn(ctx, group, id)
			handoffOK(t, err)
			switch turn.State {
			case domain.TurnStateStarting, domain.TurnStateRunning, domain.TurnStateCancelRequested:
			default:
				return turn
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("scheduler did not settle the delivery")
		case <-ticker.C:
		}
	}
}
