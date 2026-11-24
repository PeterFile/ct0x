package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func backgroundMember(t *testing.T, f backgroundFixture, groupID string, profile store.BackgroundDispatchProfile) domain.Member {
	t.Helper()
	m, err := f.local.CreateMember(context.Background(), groupID, platform.MemberInput{
		Name: "手动成员", Role: "讨论", ProviderID: profile.ProviderID, Model: profile.Model,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func backgroundDelivery(t *testing.T, f backgroundFixture, msg domain.Message) store.PendingDelivery {
	t.Helper()
	got, err := f.db.GetMessage(context.Background(), msg.GroupID, msg.ID)
	if err != nil || len(got.Deliveries) != 1 {
		t.Fatalf("want exactly one delivery for %s: %+v, %v", msg.ID, got.Deliveries, err)
	}
	return store.PendingDelivery{GroupID: msg.GroupID, Delivery: got.Deliveries[0]}
}

func backgroundPreparer(t *testing.T, f backgroundFixture, id, name, version string, intake bool) (*platform.Preparer, string) {
	t.Helper()
	root := t.TempDir()
	workspace, config, execution := filepath.Join(root, "workspace"), filepath.Join(root, "config"), filepath.Join(root, "execution")
	for _, dir := range []string{workspace, config, execution} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(root, "fake-cli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > argv\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	opts := platform.PrepareOptions{PrivateWorkspaceRoot: workspace, Providers: []provider.Provider{
		{ID: id, Name: name, Version: version, Path: bin, Installation: provider.Installed},
	}}
	if intake {
		extension := filepath.Join(root, "fake-extension.ts")
		if err := os.WriteFile(extension, []byte("export default function() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		runtime, err := platform.NewMemberToolRuntime(f.db, platform.MemberToolRuntimeOptions{
			ConfigRoot: config, Endpoint: "http://127.0.0.1:1/api/v1/member-tools",
			Operations:      []string{"create_member", "send_message", "read_channel", "list_members"},
			MaxGroupMembers: 3, RoleMaxActive: 2, RoleExecutionRoot: execution, ChatIntake: coordination.New(f.db),
		})
		if err != nil {
			t.Fatal(err)
		}
		opts.MemberTools, opts.PiMemberToolsExtension = runtime, extension
	}
	p, err := platform.NewPreparer(f.db, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p, execution
}

func TestGroupBackgroundDispatchReviewDisabledRestart(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundFixture(t, "", false)
	legacy := f.group(t, "")
	old := backgroundMember(t, f, legacy.ID, discussionProfile)
	if err := f.db.EnableGroupBackgroundDispatch(ctx); err != nil {
		t.Fatal(err)
	}
	bound := f.group(t, "")
	member := backgroundMember(t, f, bound.ID, discussionProfile)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	off := newBackgroundFixture(t, f.path, false)
	input := platform.MessageInput{Content: "需要回应", TopicID: "同话题"}
	msg := off.post(t, bound.ID, "disabled", input)
	if len(msg.Deliveries) != 0 || msg.SourceKind != "local_user_chat" || msg.SourceID == "" {
		t.Fatalf("disabled bound group bypassed the background route or lost its real source: %+v", msg)
	}
	off.pass(t, backgroundOptions())
	off.assertCount(t, 1, `SELECT count(*) FROM group_background_intents WHERE message_id=? AND source_id=? AND delivery_id IS NULL`, msg.ID, msg.SourceID)
	if got := off.post(t, bound.ID, "disabled", input); !reflect.DeepEqual(got, msg) {
		t.Fatalf("disabled replay changed receipt: %+v", got)
	}
	for _, g := range []struct {
		group, member string
		input         platform.MessageInput
	}{
		{legacy.ID, old.ID, input},
		{bound.ID, member.ID, platform.MessageInput{Content: "明确指定", TopicID: "同话题", RecipientIDs: []string{member.ID}}},
	} {
		got := off.post(t, g.group, "", g.input)
		if len(got.Deliveries) != 1 || got.Deliveries[0].RecipientID != g.member {
			t.Fatalf("changed addressed or legacy route: %+v", got)
		}
	}
	public := off.post(t, bound.ID, "", platform.MessageInput{Content: "仅公开", TopicID: input.TopicID, RecipientIDs: []string{}})
	off.assertCount(t, 0, `SELECT count(*) FROM group_background_intents WHERE message_id=?`, public.ID)
	off.assertCount(t, 0, `SELECT count(*) FROM deliveries WHERE message_id IN (?,?)`, public.ID, msg.ID)
}

func TestGroupBackgroundDispatchReviewExplicitAfterDisabledRestart(t *testing.T) {
	ctx := context.Background()
	const topic = "同话题"
	f := newBackgroundFixture(t, "", true)
	g := f.group(t, "")
	member := backgroundMember(t, f, g.ID, discussionProfile)
	earlier := f.post(t, g.ID, "", platform.MessageInput{Content: "需要默认回应", TopicID: topic})
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}

	off := newBackgroundFixture(t, f.path, false)
	later := off.post(t, g.ID, "", platform.MessageInput{Content: "明确指定", TopicID: topic, RecipientIDs: []string{member.ID}})
	if len(later.Deliveries) != 1 || later.Deliveries[0].RecipientID != member.ID {
		t.Fatalf("want one explicitly addressed delivery: %+v", later.Deliveries)
	}
	off.pass(t, backgroundOptions())
	deliveryID := later.Deliveries[0].ID
	page, err := off.db.ListPendingDeliveries(ctx, store.PendingOptions{Lane: "human"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Delivery.ID != deliveryID {
		t.Errorf("addressed delivery not visible in the human queue after disabled restart: %+v, %v", page.Items, err)
	}
	if _, err := off.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: deliveryID, MaxActive: 1}); err != nil {
		t.Errorf("addressed delivery not claimable after disabled restart: %v", err)
	}
	off.assertCount(t, 1, `SELECT count(*) FROM group_background_intents WHERE message_id=? AND delivery_id IS NULL`, earlier.ID)
}

func TestGroupBackgroundDispatchReviewReadyAtClaim(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundFixture(t, "", true)
	g := f.group(t, "")
	msg := f.post(t, g.ID, "", platform.MessageInput{Content: "需要回应"})
	f.pass(t, backgroundOptions())
	pending := backgroundDelivery(t, f, msg)
	p, _ := backgroundPreparer(t, f, discussionProfile.ProviderID, provider.Pi, "1.4.0", false)
	prepared, err := p.Prepare(ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	input := store.ClaimTurnInput{GroupID: g.ID, DeliveryID: pending.Delivery.ID, ResourceKeys: prepared.ResourceKeys, MaxActive: 1}
	if _, err := f.db.ClaimTurn(ctx, input); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("ordinary/unverified preparation could claim an A1 delivery: %v", err)
	}
	input.BackgroundProfile = prepared.BackgroundProfile
	revoked := backgroundOptions()
	revoked.ReadyProfiles = []store.BackgroundDispatchProfile{{ProviderID: discussionProfile.ProviderID, Model: "another-model"}}
	f.pass(t, revoked)
	if _, err := f.db.ClaimTurn(ctx, input); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("stale Prepare could claim after exact profile withdrawal: %v", err)
	}
	if _, err := p.Prepare(ctx, pending); !errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("withdrawn profile could still prepare: %v", err)
	}
	f.assertCount(t, 1, `SELECT count(*) FROM deliveries WHERE id=? AND turn_id='' AND state='queued'`, pending.Delivery.ID)
	f.assertCount(t, 0, `SELECT count(*) FROM turns`)
	f.assertCount(t, 0, `SELECT count(*) FROM resource_claims`)
	f.pass(t, backgroundOptions())
	empty := backgroundOptions()
	empty.ReadyProfiles = nil
	f.pass(t, empty)
	if _, err := f.db.ClaimTurn(ctx, input); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("empty snapshot did not withdraw readiness: %v", err)
	}
	f.pass(t, backgroundOptions())
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		restart := newBackgroundFixture(t, f.path, enabled)
		p, _ := backgroundPreparer(t, restart, discussionProfile.ProviderID, provider.Pi, "1.4.0", false)
		if _, err := p.Prepare(ctx, pending); !errors.Is(err, scheduler.ErrNotReady) {
			t.Fatalf("restart (enabled=%v) prepared without a current snapshot: %v", enabled, err)
		}
		if _, err := restart.db.ClaimTurn(ctx, input); !errors.Is(err, store.ErrTurnBusy) {
			t.Fatalf("restart (enabled=%v) claimed without a current snapshot: %v", enabled, err)
		}
		if enabled {
			restart.pass(t, backgroundOptions()) // Works even with no unrouted intents left.
			if _, err := p.Prepare(ctx, pending); err != nil {
				t.Fatalf("fresh trusted snapshot did not unblock preparation: %v", err)
			}
			if _, err := restart.db.ClaimTurn(ctx, input); err != nil {
				t.Fatalf("fresh trusted snapshot did not unblock claim: %v", err)
			}
		}
		if err := restart.db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	restarted := newBackgroundFixture(t, f.path, true)
	if n, err := restarted.db.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("existing startup recovery changed: %d, %v", n, err)
	}
	restarted.pass(t, backgroundOptions())
	restarted.assertCount(t, 1, `SELECT count(*) FROM turns WHERE delivery_id=? AND state='unknown'`, pending.Delivery.ID)
	restarted.assertCount(t, len(prepared.ResourceKeys), `SELECT count(*) FROM resource_claims`)
	if _, err := restarted.db.ClaimTurn(ctx, input); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("restart reclaimed unknown execution: %v", err)
	}
}

func TestGroupBackgroundDispatchReviewRequiresToolFreeAdapter(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundFixture(t, "", true)
	g := f.group(t, "")
	cfg := backgroundOptions()
	profile := store.BackgroundDispatchProfile{ProviderID: "configured-native", Model: discussionProfile.Model}
	cfg.ReadyProfiles = []store.BackgroundDispatchProfile{profile}
	cfg.Create.Profile = profile
	msg := f.post(t, g.ID, "", platform.MessageInput{Content: "需要回应"})
	f.pass(t, cfg) // Simulate an old or overly broad routing snapshot.
	pending := backgroundDelivery(t, f, msg)
	p, _ := backgroundPreparer(t, f, profile.ProviderID, provider.Codex, "0.166.0", false)
	if p.SupportsBackgroundDiscussionAdapter(profile) {
		t.Fatal("composition could qualify a read-only baseline as tool-free")
	}
	if _, err := p.Prepare(ctx, pending); !errors.Is(err, scheduler.ErrNotReady) {
		t.Fatalf("native read-only baseline is not a tool-free adapter: %v", err)
	}
	targeted := f.post(t, g.ID, "", platform.MessageInput{Content: "明确指定", TopicID: "正常定向", RecipientIDs: []string{pending.Delivery.RecipientID}})
	if _, err := p.Prepare(ctx, backgroundDelivery(t, f, targeted)); err != nil {
		t.Fatalf("A1 adapter requirement leaked into ordinary explicit execution: %v", err)
	}
}

func TestGroupBackgroundDispatchReviewRouteNotJoinOrigin(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundFixture(t, "", true)
	g := f.group(t, "")
	msg := f.post(t, g.ID, "", platform.MessageInput{Content: "初次讨论", TopicID: "讨论"})
	f.pass(t, backgroundOptions())
	initial := backgroundDelivery(t, f, msg)
	manual := backgroundMember(t, f, g.ID, discussionProfile)
	p, execution := backgroundPreparer(t, f, discussionProfile.ProviderID, provider.Pi, "1.4.0", true)
	if !p.SupportsBackgroundDiscussionAdapter(discussionProfile) {
		t.Fatal("the verified Pi one-shot adapter did not qualify")
	}
	first, err := p.Prepare(ctx, initial)
	if err != nil || !strings.Contains(first.Request.Prompt, "background_reason") || strings.Contains(first.Request.Prompt, "chat_intake/v1") {
		t.Fatalf("A1 original did not remain plain text: %v", err)
	}
	for _, id := range []string{initial.Delivery.RecipientID, manual.ID} {
		m := f.post(t, g.ID, "", platform.MessageInput{Content: "正常后续", TopicID: "明确选择", RecipientIDs: []string{id}})
		prepared, err := p.Prepare(ctx, backgroundDelivery(t, f, m))
		if err != nil || !strings.Contains(prepared.Request.Prompt, "chat_intake/v1") || strings.Contains(prepared.Request.Prompt, "background_reason") {
			t.Errorf("independently enabled intake lost for %s: %v", id, err)
		}
	}
	// Exercise the claimed one-shot execution with an argv-only fixture, not a model.
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: initial.Delivery.ID,
		MaxActive: 1, ResourceKeys: first.ResourceKeys, BackgroundProfile: first.BackgroundProfile})
	if err != nil {
		t.Fatal(err)
	}
	first.Request.RunID, first.Request.RuntimeDir = turn.ID, execution
	first.Executor.Execute(ctx, first.Request, nil)
	argv, err := os.ReadFile(filepath.Join(first.Request.CWD, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--no-tools", "--no-extensions", "--no-mcp", "--no-skills"} {
		if !strings.Contains(string(argv), "\n"+flag+"\n") {
			t.Errorf("missing tool-free flag %s", flag)
		}
	}
	if strings.Contains(string(argv), "\n--extension\n") || strings.Contains(string(argv), "\n--session\n") {
		t.Fatalf("fresh A1 run requested extension or session resume: %q", argv)
	}
}

func TestGroupBackgroundDispatchReviewExplicitDoesNotOvertake(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundFixture(t, "", true)
	g := f.group(t, "")
	member := backgroundMember(t, f, g.ID, discussionProfile)
	older := f.post(t, g.ID, "", platform.MessageInput{Content: "先发", TopicID: "同话题"})
	later := f.post(t, g.ID, "", platform.MessageInput{Content: "后发", TopicID: "同话题", RecipientIDs: []string{member.ID}})
	input := store.ClaimTurnInput{GroupID: g.ID, DeliveryID: later.Deliveries[0].ID, MaxActive: 2}
	assertWaiting := func() {
		t.Helper()
		page, err := f.db.ListPendingDeliveries(ctx, store.PendingOptions{Lane: "human"})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if item.Delivery.ID == input.DeliveryID {
				t.Error("later explicit delivery was exposed to the scheduler before the earlier source was claimed")
			}
		}
		if _, err := f.db.ClaimTurn(ctx, input); !errors.Is(err, store.ErrTurnBusy) {
			t.Fatalf("later explicit delivery overtook earlier source: %v", err)
		}
	}
	assertWaiting()
	f.pass(t, backgroundOptions())
	assertWaiting() // Delivery creation now has the reverse timestamp order.
	first := backgroundDelivery(t, f, older)
	if first.Delivery.ConversationID != later.Deliveries[0].ConversationID {
		t.Fatal("fixture did not share the conversation")
	}
	p, _ := backgroundPreparer(t, f, discussionProfile.ProviderID, provider.Pi, "1.4.0", false)
	prepared, err := p.Prepare(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: first.Delivery.ID,
		MaxActive: 2, ResourceKeys: prepared.ResourceKeys, BackgroundProfile: prepared.BackgroundProfile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: g.ID, TurnID: turn.ID, Token: turn.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ClaimTurn(ctx, input); err != nil {
		t.Fatalf("explicit route did not proceed after the barrier: %v", err)
	}
}

func TestGroupBackgroundDispatchReviewRoutingFailureIsTopicLocal(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundFixture(t, "", true)
	g, other := f.group(t, ""), f.group(t, "")
	first := f.post(t, g.ID, "", platform.MessageInput{Content: "先发", TopicID: "同话题"})
	later := f.post(t, g.ID, "", platform.MessageInput{Content: "后发", TopicID: "同话题"})
	public := f.post(t, g.ID, "", platform.MessageInput{Content: "仅公开", TopicID: "同话题", RecipientIDs: []string{}})
	independent := []domain.Message{
		f.post(t, g.ID, "", platform.MessageInput{Content: "独立话题", TopicID: "另一个话题"}),
		f.post(t, other.ID, "", platform.MessageInput{Content: "独立群组", TopicID: "同话题"}),
	}
	f.exec(t, fmt.Sprintf(`CREATE TRIGGER reject_first_route BEFORE INSERT ON deliveries WHEN NEW.message_id='%s' BEGIN SELECT RAISE(ABORT,'test fixture'); END`, first.ID))
	if err := f.db.RunGroupBackgroundDispatch(ctx, backgroundOptions()); err == nil {
		t.Fatal("fixture should reject earlier delivery")
	}
	f.assertCount(t, 0, `SELECT count(*) FROM deliveries WHERE message_id IN (?,?,?)`, first.ID, later.ID, public.ID)
	p, _ := backgroundPreparer(t, f, discussionProfile.ProviderID, provider.Pi, "1.4.0", false)
	for _, msg := range independent {
		pending := backgroundDelivery(t, f, msg)
		prepared, err := p.Prepare(ctx, pending)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: msg.GroupID, DeliveryID: pending.Delivery.ID,
			MaxActive: 2, ResourceKeys: prepared.ResourceKeys, BackgroundProfile: prepared.BackgroundProfile}); err != nil {
			t.Fatalf("routing failure blocked an independent topic or group: %v", err)
		}
	}
	f.exec(t, `DROP TRIGGER reject_first_route`)
	f.pass(t, backgroundOptions())
	backgroundDelivery(t, f, first)
	backgroundDelivery(t, f, later)
}

func TestGroupBackgroundDispatchReviewUnreadyOrLeftOlderDoesNotDeadlockTopic(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundFixture(t, "", true)
	p, _ := backgroundPreparer(t, f, discussionProfile.ProviderID, provider.Pi, "1.4.0", false)
	hasPending := func(deliveryID string) bool {
		t.Helper()
		page, err := f.db.ListPendingDeliveries(ctx, store.PendingOptions{Lane: "human"})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if item.Delivery.ID == deliveryID {
				return true
			}
		}
		return false
	}

	// 1. Earlier unready explicit delivery to another member must not deadlock
	// subsequent A1 or active-member explicit deliveries in the same topic.
	g1 := f.group(t, "no-deadlock-unready")
	ready1 := backgroundMember(t, f, g1.ID, discussionProfile)
	unready1 := backgroundMember(t, f, g1.ID, store.BackgroundDispatchProfile{})
	_ = f.post(t, g1.ID, "", platform.MessageInput{Content: "先发给未配运行器成员", TopicID: "同话题", RecipientIDs: []string{unready1.ID}})
	a1Msg := f.post(t, g1.ID, "", platform.MessageInput{Content: "无收件人续谈", TopicID: "同话题"})
	explicitAfter := f.post(t, g1.ID, "", platform.MessageInput{Content: "再发给就绪成员", TopicID: "同话题", RecipientIDs: []string{ready1.ID}})
	f.pass(t, backgroundOptions())
	a1Pending := backgroundDelivery(t, f, a1Msg)
	if !hasPending(a1Pending.Delivery.ID) {
		t.Fatal("earlier unready explicit delivery blocked later active A1 delivery")
	}
	if hasPending(explicitAfter.Deliveries[0].ID) {
		t.Fatal("later explicit delivery overtook earlier ready A1 delivery")
	}
	preparedA1, err := p.Prepare(ctx, a1Pending)
	if err != nil {
		t.Fatal(err)
	}
	turnA1, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{
		GroupID: g1.ID, DeliveryID: a1Pending.Delivery.ID, MaxActive: 2,
		ResourceKeys: preparedA1.ResourceKeys, BackgroundProfile: preparedA1.BackgroundProfile,
	})
	if err != nil {
		t.Fatalf("claim later A1 past unready explicit failed: %v", err)
	}
	if _, err := f.db.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: g1.ID, TurnID: turnA1.ID, Token: turnA1.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	if !hasPending(explicitAfter.Deliveries[0].ID) {
		t.Fatal("subsequent explicit delivery remained blocked after A1 left queued")
	}
	tExplicit, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g1.ID, DeliveryID: explicitAfter.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatalf("claim subsequent explicit delivery failed: %v", err)
	}
	if _, err := f.db.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: g1.ID, TurnID: tExplicit.ID, Token: tExplicit.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}

	// 2. An earlier A1 delivery whose recipient became left or workspace-bound
	// must not block later active-member deliveries in that topic.
	for _, kind := range []string{"left", "workspace"} {
		g2 := f.group(t, "no-deadlock-"+kind)
		ready2 := backgroundMember(t, f, g2.ID, discussionProfile)
		olderA1 := f.post(t, g2.ID, "", platform.MessageInput{Content: "早期自动路由", TopicID: "同话题"})
		f.pass(t, backgroundOptions())
		if kind == "left" {
			f.exec(t, `UPDATE members SET status='left' WHERE id=? AND group_id=?`, ready2.ID, g2.ID)
		} else {
			f.exec(t, `UPDATE members SET workspace='bound-ws' WHERE id=? AND group_id=?`, ready2.ID, g2.ID)
		}
		_ = olderA1
		otherActive := backgroundMember(t, f, g2.ID, discussionProfile)
		followup := f.post(t, g2.ID, "", platform.MessageInput{Content: "后续显式追问", TopicID: "同话题", RecipientIDs: []string{otherActive.ID}})
		if !hasPending(followup.Deliveries[0].ID) {
			t.Fatalf("stale %s A1 delivery blocked active member in same topic", kind)
		}
		tFollow, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g2.ID, DeliveryID: followup.Deliveries[0].ID, MaxActive: 2})
		if err != nil {
			t.Fatalf("claim past stale %s A1 delivery failed: %v", kind, err)
		}
		if _, err := f.db.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: g2.ID, TurnID: tFollow.ID, Token: tFollow.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
			t.Fatal(err)
		}
	}

	// 3. Same active recipient explicit delivery still precedes a later A1 delivery.
	g3 := f.group(t, "same-member-fifo")
	ready3 := backgroundMember(t, f, g3.ID, discussionProfile)
	sameFirst := f.post(t, g3.ID, "", platform.MessageInput{Content: "先显式提问", TopicID: "同话题", RecipientIDs: []string{ready3.ID}})
	sameLater := f.post(t, g3.ID, "", platform.MessageInput{Content: "后补无收件人", TopicID: "同话题"})
	f.pass(t, backgroundOptions())
	samePending := backgroundDelivery(t, f, sameLater)
	if hasPending(samePending.Delivery.ID) {
		t.Fatal("same-member later A1 overtook earlier active explicit delivery in pending list")
	}
	preparedSame, err := p.Prepare(ctx, samePending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{
		GroupID: g3.ID, DeliveryID: samePending.Delivery.ID, MaxActive: 2,
		ResourceKeys: preparedSame.ResourceKeys, BackgroundProfile: preparedSame.BackgroundProfile,
	}); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("same-member later A1 bypassed earlier active explicit delivery: %v", err)
	}
	tFirst, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g3.ID, DeliveryID: sameFirst.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ApplyTurnReceipt(ctx, store.TurnClaim{GroupID: g3.ID, TurnID: tFirst.ID, Token: tFirst.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{
		GroupID: g3.ID, DeliveryID: samePending.Delivery.ID, MaxActive: 2,
		ResourceKeys: preparedSame.ResourceKeys, BackgroundProfile: preparedSame.BackgroundProfile,
	}); err != nil {
		t.Fatalf("same-member later A1 did not unblock after earlier explicit settled: %v", err)
	}
}
