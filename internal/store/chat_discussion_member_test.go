package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

const discussionMemberRole = "核对证据"

func discussionMemberInvite(t *testing.T) domain.ChatDiscussionAction {
	t.Helper()
	var input domain.ChatDiscussionAction
	const raw = `{"kind":"invite","content":"请核对摘要所依赖的数据。","expected_reply":"一条可引用的证据",
		"create_member":{"name":"分析员","role":"` + discussionMemberRole + `","reason":"需要独立核对证据。"}}`
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func TestChatDiscussionMemberInviteReuse(t *testing.T) {
	f := setupMessages(t)
	input, policy := discussionMemberInvite(t), memberOperationPolicy()
	// The root, the current caller, and a foreign member are better matches by
	// age, but none is an eligible recipient for the child invitation.
	coordinationDecideExec(t, f.s, `UPDATE members SET role=?,provider_id=?,model=?,created_at='2025-01-01T00:00:00.000000000Z'`,
		discussionMemberRole, policy.ProviderID, policy.Model)
	var wantID string
	for range 2 {
		m, err := f.s.CreateMember(ctx, CreateMemberInput{GroupID: f.groupID, Name: "已有成员", Role: discussionMemberRole,
			ProviderID: policy.ProviderID, Model: policy.Model, OwnerID: "user"})
		if err != nil {
			t.Fatal(err)
		}
		coordinationDecideExec(t, f.s, `UPDATE members SET created_at='2025-02-01T00:00:00.000000000Z' WHERE id=?`, m.ID)
		if wantID == "" || m.ID < wantID {
			wantID = m.ID
		}
	}
	origin := discussionUser(t, f, "evidence", f.a)
	a, ca := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
	discussionAssess(t, f.s, a, ca)
	parent := discussionApply(t, f.s, a, ca, "parent", domain.ChatDiscussionAction{Kind: "invite", Content: "请协调证据核对。", RecipientID: f.b})
	b, cb := discussionArrive(t, f.s, f.groupID, parent.Event.DeliveryID)
	discussionAssess(t, f.s, b, cb)
	wantCounts := memberOperationCounts(t, f.s)
	for _, idx := range []int{1, 2, 3, 4} { // Invitation, delivery, conversation, receipt; no member or join.
		wantCounts[idx]++
	}
	policy.MaxMembers = 4 // This group has four active members already.
	r, err := f.s.ApplyChatDiscussionWithMember(ctx, b, cb, input, "reuse", policy)
	if err != nil {
		t.Fatalf("deterministic reuse at capacity: %v", err)
	}
	discussionCheckPublication(t, f.s, r, origin, "invite", parent.Message.ID, f.b, wantID, "", parent.Message.ID, input.Content, input.ExpectedReply, 1)
	if got := memberOperationCounts(t, f.s); got != wantCounts {
		t.Fatalf("reuse side effects: got %v; want %v", got, wantCounts)
	}
	c, cc := discussionArrive(t, f.s, f.groupID, r.Event.DeliveryID)
	discussionAssess(t, f.s, c, cc)
	// Even with capacity to create a different member, a third hop is forbidden.
	before := messageSequenceSnapshot(t, f.s)
	policy.MaxMembers = 10
	input.CreateMember.Role = "从未见过的职责"
	if _, err := f.s.ApplyChatDiscussionWithMember(ctx, c, cc, input, "third-hop", policy); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("creation bypassed the discussion hop limit: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatDiscussionMemberInviteCreateReplay(t *testing.T) {
	f := setupMessages(t)
	input, policy := discussionMemberInvite(t), memberOperationPolicy()
	origin := discussionUser(t, f, "evidence", f.a)
	a, ca := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
	discussionAssess(t, f.s, a, ca)
	// A match to the caller is not reusable. At capacity an invite must wait.
	coordinationDecideExec(t, f.s, `UPDATE members SET role=?,provider_id=?,model=? WHERE id=?`,
		discussionMemberRole, policy.ProviderID, policy.Model, f.a)
	const key = "create-and-invite"
	policy.MaxMembers = 2
	before := messageSequenceSnapshot(t, f.s)
	if _, err := f.s.ApplyChatDiscussionWithMember(ctx, a, ca, input, key, policy); !errors.Is(err, ErrMemberCapacity) {
		t.Fatalf("at capacity: %v", err)
	}
	messageSequencePreserved(t, f.s, before)

	policy.MaxMembers = 3
	coordinationDecideExec(t, f.s, `CREATE TRIGGER fail_composite_invitation BEFORE INSERT ON chat_discussion_events
	 BEGIN SELECT RAISE(ABORT,'synthetic composite failure'); END`)
	if got, err := f.s.ApplyChatDiscussionWithMember(ctx, a, ca, input, key, policy); err == nil || got.Message.ID != "" {
		t.Fatalf("failed event was not rolled back: %+v %v", got, err)
	}
	messageSequencePreserved(t, f.s, before)
	coordinationDecideExec(t, f.s, `DROP TRIGGER fail_composite_invitation`)
	channelBefore, err := f.s.ReadChannelForTool(ctx, a, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	counts := memberOperationCounts(t, f.s)
	r, err := f.s.ApplyChatDiscussionWithMember(ctx, a, ca, input, key, policy)
	if err != nil {
		t.Fatal(err)
	}
	id := r.Event.RecipientID
	discussionCheckPublication(t, f.s, r, origin, "invite", origin.ID, f.a, id, "", "", input.Content, input.ExpectedReply, 1)
	m, err := f.s.GetMember(ctx, f.groupID, id)
	if err != nil || m.CreatorID != f.a || m.OwnerID != "user" || m.Status != domain.MemberStatusActive ||
		m.Workspace != "" || m.Name != "分析员" || m.Role != discussionMemberRole || m.ProviderID != policy.ProviderID || m.Model != policy.Model {
		t.Fatalf("new member: %+v %v", m, err)
	}
	page, err := f.s.ReadChannelForTool(ctx, a, ListOptions{})
	if err != nil || len(page.Items) != len(channelBefore.Items)+2 || page.Items[0].ID != r.Message.ID || !reflect.DeepEqual(page.Items[2:], channelBefore.Items) {
		t.Fatalf("join not in original timeline before invitation: %+v %v", page, err)
	}
	join := page.Items[1]
	if join.SenderID != f.a || join.GroupID != origin.GroupID || join.TopicID != origin.TopicID || len(join.Deliveries) != 0 ||
		!strings.Contains(join.Content, m.Name) || !strings.Contains(join.Content, "需要独立核对证据。") {
		t.Fatalf("join was missing background or woke a recipient: %+v", join)
	}
	wantCounts := [6]int{counts[0] + 1, counts[1] + 2, counts[2] + 1, counts[3] + 1, counts[4] + 1, counts[5]}
	if got := memberOperationCounts(t, f.s); got != wantCounts {
		t.Fatalf("creation side effects: got %v; want %v", got, wantCounts)
	}

	reopened := queueHandle(t, f.path, false)
	b, cb := discussionArrive(t, reopened, f.groupID, r.Event.DeliveryID)
	discussionCheckContext(t, reopened, b, cb, origin, f.a, id, r.Message.ID, r.Message.ID, r.Message.ID, false, r)
	before = messageSequenceSnapshot(t, reopened)
	policy.MaxMembers = 1
	replay, err := reopened.ApplyChatDiscussionWithMember(ctx, a, ca, input, key, policy)
	if err != nil || !reflect.DeepEqual(replay, r) {
		t.Fatalf("reopened replay returned a changed receipt: %+v %v", replay, err)
	}
	for _, field := range []string{"name", "role", "reason", "expected_reply", "provider", "model"} {
		changed, changedPolicy := input, policy
		create := *input.CreateMember
		changed.CreateMember = &create
		switch field {
		case "name":
			create.Name += "变更"
		case "role":
			create.Role += "变更"
		case "reason":
			create.Reason += "变更"
		case "expected_reply":
			changed.ExpectedReply += "变更"
		case "provider":
			changedPolicy.ProviderID += "-changed"
		case "model":
			changedPolicy.Model += "-changed"
		}
		if _, err := reopened.ApplyChatDiscussionWithMember(ctx, a, ca, changed, key, changedPolicy); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed %s did not conflict: %v", field, err)
		}
	}
	if _, err := reopened.ApplyChatDiscussionWithMember(ctx, a, ca, input, "second-key", policy); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("advanced an already consumed arrival: %v", err)
	}
	messageSequencePreserved(t, reopened, before)
	discussionEnd(t, reopened, a)
	if _, err := reopened.ApplyChatDiscussionWithMember(ctx, a, ca, input, key, policy); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("terminal turn replayed a prior receipt: %v", err)
	}
}

func TestChatDiscussionMemberInviteConcurrentAcrossStores(t *testing.T) {
	for _, mode := range []string{"same-key", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			f := setupMessages(t)
			handles := [2]*Store{queueHandle(t, f.path, true), queueHandle(t, f.path, true)}
			input, policy := discussionMemberInvite(t), memberOperationPolicy()
			policy.MaxMembers = 3    // Two active members in this group leave one slot.
			input.ExpectedReply = "" // The default must survive receipt replay.
			type request struct {
				origin         domain.Message
				scope          MemberToolScope
				candidate, key string
				input          domain.ChatDiscussionAction
			}
			newRequest := func(owner, key string, input domain.ChatDiscussionAction) request {
				origin := discussionUser(t, f, "evidence", owner)
				scope, candidate := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
				discussionAssess(t, f.s, scope, candidate)
				return request{origin, scope, candidate, key, input}
			}
			first := newRequest(f.a, "invite", input)
			requests := [2]request{first, first}
			if mode == "capacity" {
				create := *input.CreateMember
				create.Role = "核对出处" // Cannot reuse the other request's member.
				input.CreateMember = &create
				requests[1] = newRequest(f.b, "other-invite", input)
			}
			counts := memberOperationCounts(t, f.s)
			type outcome struct {
				index   int
				receipt domain.ChatDiscussionReceipt
				err     error
			}
			start, results := make(chan struct{}), make(chan outcome, len(requests))
			for i, req := range requests {
				go func() {
					<-start
					r, err := handles[i].ApplyChatDiscussionWithMember(ctx, req.scope, req.candidate, req.input, req.key, policy)
					results <- outcome{i, r, err}
				}()
			}
			close(start)
			winner, other := <-results, <-results
			if winner.err != nil {
				winner, other = other, winner
			}
			if winner.err != nil {
				t.Fatalf("neither concurrent invitation succeeded: %v; %v", winner.err, other.err)
			}
			if mode == "same-key" {
				if other.err != nil || !reflect.DeepEqual(winner.receipt, other.receipt) {
					t.Fatalf("same-key submissions returned different receipts: %+v; %+v (%v)", winner.receipt, other.receipt, other.err)
				}
			} else if !errors.Is(other.err, ErrMemberCapacity) || !reflect.DeepEqual(other.receipt, domain.ChatDiscussionReceipt{}) {
				t.Fatalf("competing request did not fail cleanly at capacity: %+v %v", other.receipt, other.err)
			}
			req, r := requests[winner.index], winner.receipt
			discussionCheckPublication(t, f.s, r, req.origin, domain.ChatDiscussionInvite, req.origin.ID, req.scope.MemberID,
				r.Event.RecipientID, "", "", req.input.Content, req.input.Content, 1)
			wantCounts := [6]int{counts[0] + 1, counts[1] + 2, counts[2] + 1, counts[3] + 1, counts[4] + 1, counts[5]}
			if got := memberOperationCounts(t, f.s); got != wantCounts {
				t.Fatalf("want one member, one join and one invitation/wake: got counts %v; want %v", got, wantCounts)
			}
		})
	}
}

func TestChatDiscussionMemberInviteGuards(t *testing.T) {
	f := setupMessages(t)
	input, policy := discussionMemberInvite(t), memberOperationPolicy()
	origin := discussionUser(t, f, "evidence", f.a)
	a, ca := discussionArrive(t, f.s, f.groupID, origin.Deliveries[0].ID)
	before := messageSequenceSnapshot(t, f.s)
	if _, err := f.s.ApplyChatDiscussionWithMember(ctx, a, ca, input, "unassessed", policy); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("unassessed intake could create a member: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
	discussionAssess(t, f.s, a, ca)
	before = messageSequenceSnapshot(t, f.s)
	const key = "guarded"
	explicit := domain.ChatDiscussionAction{Kind: domain.ChatDiscussionInvite, Content: input.Content, RecipientID: f.b}
	ambiguous := input
	ambiguous.RecipientID = f.b
	for _, invalid := range []domain.ChatDiscussionAction{
		explicit,
		ambiguous,
		{Kind: domain.ChatDiscussionInvite, Content: input.Content, CreateMember: &domain.ChatDiscussionCreateMember{}},
		{Kind: domain.ChatDiscussionReply, Content: input.Content, CreateMember: input.CreateMember},
		{Kind: domain.ChatDiscussionResolve, Content: input.Content, CreateMember: input.CreateMember},
	} {
		if _, err := f.s.ApplyChatDiscussionWithMember(ctx, a, ca, invalid, key, policy); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted invalid creation form %+v: %v", invalid, err)
		}
	}
	for _, invalid := range []domain.ChatDiscussionAction{input, ambiguous} {
		if _, err := f.s.ApplyChatDiscussion(ctx, a, ca, invalid, key); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unprivileged invitation accepted create_member: %v", err)
		}
	}
	if _, err := f.s.ApplyChatDiscussionWithMember(ctx, a, ca, input, "bad,key", policy); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid key accepted: %v", err)
	}
	if _, err := f.s.ApplyChatDiscussionWithMember(ctx, a, ca, input, key, MemberToolCreatePolicy{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid policy accepted: %v", err)
	}
	wrong := a
	wrong.TopicID = "other-topic"
	if _, err := f.s.ApplyChatDiscussionWithMember(ctx, wrong, ca, input, key, policy); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("forged topic accepted: %v", err)
	}
	if _, err := f.s.ApplyChatDiscussionWithMember(ctx, a, "other-candidate", input, key, policy); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("unbound candidate accepted: %v", err)
	}
	messageSequencePreserved(t, f.s, before)

	if _, err := f.s.SendMessageForChatIntake(ctx, a, ca, MemberToolMessageInput{Content: "旧式提问", RecipientIDs: []string{f.b}}, key); err != nil {
		t.Fatal(err)
	}
	before = messageSequenceSnapshot(t, f.s)
	if _, err := f.s.ApplyChatDiscussionWithMember(ctx, a, ca, input, key, policy); !errors.Is(err, ErrConflict) {
		t.Fatalf("invitation did not share the send_message namespace: %v", err)
	}
	if _, err := f.s.ApplyChatDiscussionWithMember(ctx, a, ca, input, "another-key", policy); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("creation bypassed the per-turn directed slot: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
}
