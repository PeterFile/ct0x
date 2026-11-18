package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func memberOperationsFixture(t *testing.T, operations []string) (localMessageFixture, *MemberTools, string, store.TurnClaim) {
	t.Helper()
	f := setupLocalMessages(t)
	claim := memberCapabilityClaim(t, f, f.groupID)
	tools := NewMemberTools(f.db)
	grants := MemberToolGrants{Operations: operations, MaxGroupMembers: 4, ProviderID: "pi", Model: "selected"}
	token, err := tools.Issue(context.Background(), claim, grants)
	if err != nil {
		t.Fatal(err)
	}
	grants.Operations[0], grants.ProviderID, grants.Model, grants.MaxGroupMembers = "unknown", "attacker", "attacker", 999
	return f, tools, token, claim
}

func TestMemberOperationBoundDTOsAndPersistedEffects(t *testing.T) {
	f, tools, token, claim := memberOperationsFixture(t, []string{"list_members", "read_channel", "send_message", "create_member"})
	ctx := context.Background()
	scope, err := f.db.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	peer, foreign := f.member(t, f.groupID), f.member(t, f.otherID)
	members, err := tools.ListMembers(ctx, token, store.ListOptions{})
	if err != nil || len(members.Items) != 2 {
		t.Fatalf("group members were not scoped: %+v, %v", members, err)
	}
	for _, member := range members.Items {
		if member.ID == foreign.ID || member.GroupID != scope.Claim.GroupID {
			t.Fatal("member list escaped the bound group")
		}
	}
	var input MemberMessageInput
	payload := fmt.Sprintf(`{"content":"bound message","recipient_ids":[%q,%q],"sender_id":%q,"group_id":%q,"topic_id":"forged","conversation_id":"forged"}`, peer.ID, scope.MemberID, peer.ID, f.otherID)
	if err := json.Unmarshal([]byte(payload), &input); err != nil {
		t.Fatal(err)
	}
	message, err := tools.SendMessage(ctx, token, input, "send")
	if err != nil || message.SenderID != scope.MemberID || message.TopicID != scope.TopicID || message.GroupID != scope.Claim.GroupID || len(message.Deliveries) != 2 {
		t.Fatalf("model input selected scope or filtered caller: %+v, %v", message, err)
	}
	for _, recipients := range [][]string{nil, {}} {
		key := fmt.Sprintf("public-%t", recipients == nil)
		public := MemberMessageInput{Content: "no wakeup", RecipientIDs: recipients}
		result, err := tools.SendMessage(ctx, token, public, key)
		if err != nil || len(result.Deliveries) != 0 || result.Deliveries == nil {
			t.Fatalf("member message default-routed: %+v, %v", result, err)
		}
		if recipients == nil {
			public.RecipientIDs = []string{}
		} else {
			public.RecipientIDs = nil
		}
		if _, err := tools.SendMessage(ctx, token, public, key); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("platform lost nil/empty intent: %v", err)
		}
	}
	for _, groupID := range []string{f.groupID, f.otherID} {
		if _, err := f.local.CreateMessage(ctx, groupID, MessageInput{Content: "PRIVATE", TopicID: "other-topic", RecipientIDs: []string{}}, ""); err != nil {
			t.Fatal(err)
		}
	}
	history, err := tools.ReadChannel(ctx, token, store.ListOptions{})
	if err != nil || len(history.Items) != 4 {
		t.Fatalf("channel read lost bound topic or persisted messages: %+v, %v", history, err)
	}
	for _, item := range history.Items {
		if item.GroupID != scope.Claim.GroupID || item.TopicID != scope.TopicID || item.Content == "PRIVATE" {
			t.Fatal("channel history leaked another scope")
		}
	}
	var creation MemberCreateInput
	if err := json.Unmarshal([]byte(`{"name":"Analyst","role":"Analyze data","reason":"Explain dataset A and its outliers","provider_id":"attacker","model":"attacker","workspace":"/external","owner_id":"attacker","creator_id":"attacker"}`), &creation); err != nil {
		t.Fatal(err)
	}
	created, err := tools.CreateMember(ctx, token, creation, "create")
	if err != nil || created.Reused || created.JoinMessage == nil {
		t.Fatalf("member operation did not persist a join: %+v, %v", created, err)
	}
	m := created.Member
	if m.ProviderID != "pi" || m.Model != "selected" || m.Workspace != "" || m.CreatorID != scope.MemberID || m.OwnerID != "user" || !strings.Contains(created.JoinMessage.Content, creation.Reason) {
		t.Fatalf("model or caller mutation selected trusted creation policy: %+v", m)
	}
	stored, err := f.db.GetMessage(ctx, f.groupID, created.JoinMessage.ID)
	if err != nil || len(stored.Deliveries) != 1 || stored.Deliveries[0].RecipientID != m.ID || stored.TopicID != scope.TopicID {
		t.Fatalf("join did not reach exactly the new member: %+v, %v", stored, err)
	}
	creation.Role = "Different role"
	if _, err := tools.CreateMember(ctx, token, creation, "fill"); err != nil {
		t.Fatal(err)
	}
	creation.Role = "Over capacity"
	if _, err := tools.CreateMember(ctx, token, creation, "full"); !errors.Is(err, store.ErrMemberCapacity) {
		t.Fatalf("input grant mutation escalated creation capacity: %v", err)
	}
	var turns int
	if err := f.raw.QueryRow("SELECT count(*) FROM turns").Scan(&turns); err != nil || turns != 1 {
		t.Fatalf("tool operation executed a model: turns=%d, error=%v", turns, err)
	}
}

func TestChatDiscussionPolicyGenericMemberSend(t *testing.T) {
	// With roles/intake disabled, the runtime issues the same generic capability.
	f, tools, token, claim := memberOperationsFixture(t, []string{"send_message"})
	ctx := context.Background()
	available, err := f.local.ChatDiscussionPolicyAvailable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Skip("requires the discussion policy migration registration")
	}
	scope, err := f.db.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	thread := domain.ThreadRef{GroupID: scope.Claim.GroupID, TopicID: scope.TopicID}
	setPolicy := func(ref domain.ThreadRef, hops int, expected int64, key string) {
		t.Helper()
		if _, err := f.local.SetChatDiscussionPolicy(ctx, ref, hops, expected, key); err != nil {
			t.Fatal(err)
		}
	}
	setPolicy(domain.ThreadRef{GroupID: thread.GroupID, TopicID: "other-topic"}, 0, 0, "other-topic")
	setPolicy(domain.ThreadRef{GroupID: f.otherID, TopicID: thread.TopicID}, 0, 0, "other-group")

	peer := f.member(t, f.groupID)
	directed := MemberMessageInput{Content: "review the conclusion", RecipientIDs: []string{peer.ID}}
	send := func(key string) domain.Message {
		t.Helper()
		message, err := tools.SendMessage(ctx, token, directed, key)
		if err != nil || message.ID == "" || message.GroupID != thread.GroupID || message.TopicID != thread.TopicID ||
			message.SenderID != scope.MemberID || message.SourceKind != "member" || len(message.Deliveries) != 1 ||
			message.Deliveries[0].RecipientID != peer.ID || message.Deliveries[0].State != domain.DeliveryStateQueued {
			t.Fatalf("expected a bound generic delivery: %+v, %v", message, err)
		}
		return message
	}
	prior := send("prior") // Zeros in another topic or group do not apply.
	setPolicy(thread, 0, 0, "disable")
	_, err = tools.SendMessage(ctx, token, directed, "fresh")
	requireCapabilityDenied(t, err)
	if replay := send("prior"); !reflect.DeepEqual(replay, prior) {
		t.Fatalf("zero policy changed the exact replay: %+v", replay)
	}
	changed := directed
	changed.Content = "different request"
	if _, err := tools.SendMessage(ctx, token, changed, "prior"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("zero policy masked an idempotency conflict: %v", err)
	}
	f.counts(t, 2, 2, 2, 4)

	for _, recipients := range [][]string{nil, {}} {
		public, err := tools.SendMessage(ctx, token, MemberMessageInput{Content: "public output", RecipientIDs: recipients}, fmt.Sprintf("public-%t", recipients == nil))
		if err != nil || public.ID == "" || len(public.Deliveries) != 0 {
			t.Fatalf("zero policy blocked or routed public output: %+v, %v", public, err)
		}
	}
	user, err := f.local.CreateMessage(ctx, thread.GroupID, MessageInput{Content: "local user @ delivery", TopicID: thread.TopicID, RecipientIDs: []string{peer.ID}}, "user")
	if err != nil || user.SenderID != "user" || len(user.Deliveries) != 1 || user.Deliveries[0].RecipientID != peer.ID {
		t.Fatalf("zero policy changed explicit local-user routing: %+v, %v", user, err)
	}
	setPolicy(thread, 1, 1, "enable")
	send("fresh") // The denied send reserved no key, and positive limits do not meter generic sends.
	send("another")
}

func TestMemberOperationGrantsAndTokensFailClosed(t *testing.T) {
	f, tools, token, claim := memberOperationsFixture(t, []string{"list_members"})
	ctx := context.Background()
	if _, err := tools.ListMembers(ctx, token, store.ListOptions{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.ReadChannel(ctx, token, store.ListOptions{}); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal(err)
	}
	if _, err := tools.SendMessage(ctx, token, MemberMessageInput{Content: "denied"}, "denied"); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal(err)
	}
	if _, err := tools.CreateMember(ctx, token, MemberCreateInput{Name: "member", Role: "role", Reason: "background"}, "denied"); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal(err)
	}
	for _, bad := range []string{"wrong", claim.Token} {
		_, err := tools.ListMembers(ctx, bad, store.ListOptions{})
		requireCapabilityDenied(t, err)
	}
	_, err := NewMemberTools(f.db).Issue(ctx, claim, MemberToolGrants{Operations: []string{"unknown"}})
	requireCapabilityDenied(t, err)
	f.counts(t, 1, 1, 1, 0)
}

func TestMemberOperationDurableInvalidationDeniesAllOperations(t *testing.T) {
	for _, transition := range []string{"cancel", "recovery", "terminal"} {
		t.Run(transition, func(t *testing.T) {
			f, tools, token, claim := memberOperationsFixture(t, []string{"list_members", "read_channel", "send_message", "create_member"})
			ctx := context.Background()
			var err error
			switch transition {
			case "cancel":
				_, err = f.db.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
			case "recovery":
				_, err = f.db.RecoverInFlight(ctx)
			default:
				_, err = f.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, Started: true, Accepted: true, NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true})
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = tools.ListMembers(ctx, token, store.ListOptions{})
			requireCapabilityDenied(t, err)
			_, err = tools.ReadChannel(ctx, token, store.ListOptions{})
			requireCapabilityDenied(t, err)
			_, err = tools.SendMessage(ctx, token, MemberMessageInput{Content: "denied"}, "denied")
			requireCapabilityDenied(t, err)
			_, err = tools.CreateMember(ctx, token, MemberCreateInput{Name: "member", Role: "role", Reason: "background"}, "denied")
			requireCapabilityDenied(t, err)
			f.counts(t, 1, 1, 1, 0)
		})
	}
}
