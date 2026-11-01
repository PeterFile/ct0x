package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func memberOperationFixture(t *testing.T) (messageFixture, MemberToolScope) {
	t.Helper()
	f, _, claim := queueClaimFixture(t)
	scope, err := f.s.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	return f, scope
}

func memberOperationPolicy() MemberToolCreatePolicy {
	return MemberToolCreatePolicy{ProviderID: "pi", Model: "selected", MaxMembers: 3}
}

func memberOperationInput() MemberToolCreateInput {
	return MemberToolCreateInput{Name: "Analyst", Role: "Analyze measurements", Reason: "Check dataset A and explain the outliers in this discussion."}
}

func memberOperationCounts(t *testing.T, s *Store) [6]int {
	t.Helper()
	var counts [6]int
	for i, table := range []string{"members", "messages", "deliveries", "conversations", "mutation_idempotency", "turns"} {
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func TestMemberToolOperationListAndChannelBoundedIsolation(t *testing.T) {
	f, scope := memberOperationFixture(t)
	members, err := f.s.ListMembers(ctx, f.groupID, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantMembers := members.Items
	history, err := f.s.ListMessages(ctx, f.groupID, MessageListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantMessages := history.Items
	base := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 205 {
		stamp := base.Add(time.Duration(i%5) * time.Nanosecond)
		m := domain.Member{ID: fmt.Sprintf("member-%03d", i), GroupID: f.groupID, Name: "fixture", Role: "role", Status: domain.MemberStatusActive, CreatedAt: stamp}
		if _, err := tx.Exec(`INSERT INTO members(id,group_id,name,role,created_at) VALUES(?,?,?,?,?)`, m.ID, m.GroupID, m.Name, m.Role, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
		wantMembers = append(wantMembers, m)
		message, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: f.groupID, ActorID: "user", TopicID: scope.TopicID, Content: "visible", RecipientIDs: []string{f.b}}, []string{f.b})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("UPDATE messages SET created_at=? WHERE id=?", stamp.Format(timestampLayout), message.ID); err != nil {
			t.Fatal(err)
		}
		message.CreatedAt = stamp
		wantMessages = append(wantMessages, message)
	}
	for _, groupID := range []string{f.groupID, f.otherID} {
		topic := "other-topic"
		if groupID == f.otherID {
			topic = scope.TopicID
		}
		noise, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: groupID, ActorID: "user", TopicID: topic, Content: "PRIVATE"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("UPDATE messages SET created_at=? WHERE id=?", base.Add(time.Hour).Format(timestampLayout), noise.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(wantMembers, func(i, j int) bool {
		if wantMembers[i].CreatedAt.Equal(wantMembers[j].CreatedAt) {
			return wantMembers[i].ID > wantMembers[j].ID
		}
		return wantMembers[i].CreatedAt.After(wantMembers[j].CreatedAt)
	})
	sort.Slice(wantMessages, func(i, j int) bool {
		if wantMessages[i].CreatedAt.Equal(wantMessages[j].CreatedAt) {
			return wantMessages[i].ID > wantMessages[j].ID
		}
		return wantMessages[i].CreatedAt.After(wantMessages[j].CreatedAt)
	})
	reopened := queueHandle(t, f.path, false)
	checkPages(t, wantMembers, func(options ListOptions) (Page[domain.Member], error) {
		return reopened.ListMembersForTool(ctx, scope, options)
	}, func(m domain.Member) Cursor { return Cursor{ID: m.ID, CreatedAt: m.CreatedAt} })
	checkPages(t, wantMessages, func(options ListOptions) (Page[domain.Message], error) {
		return reopened.ReadChannelForTool(ctx, scope, options)
	}, func(m domain.Message) Cursor { return Cursor{ID: m.ID, CreatedAt: m.CreatedAt} })
	for _, options := range []ListOptions{{Limit: -1}, {Limit: 201}, {Before: &Cursor{ID: "missing-time"}}} {
		if _, err := reopened.ListMembersForTool(ctx, scope, options); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid member paging: %v", err)
		}
		if _, err := reopened.ReadChannelForTool(ctx, scope, options); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid channel paging: %v", err)
		}
	}
}

func TestMemberToolOperationSendPublicExplicitAndReplay(t *testing.T) {
	f, scope := memberOperationFixture(t)
	for _, recipients := range [][]string{nil, {}} {
		key := fmt.Sprintf("public-%t", recipients == nil)
		input := MemberToolMessageInput{Content: "public only", RecipientIDs: recipients}
		message, err := f.s.SendMessageForTool(ctx, scope, input, key)
		if err != nil || message.SenderID != f.a || message.GroupID != f.groupID || message.TopicID != scope.TopicID || message.Deliveries == nil || len(message.Deliveries) != 0 {
			t.Fatalf("member public message routed or lost identity: %+v, %v", message, err)
		}
		if recipients == nil {
			input.RecipientIDs = []string{}
		} else {
			input.RecipientIDs = nil
		}
		if _, err := f.s.SendMessageForTool(ctx, scope, input, key); !errors.Is(err, ErrConflict) {
			t.Fatalf("nil and empty recipient fingerprints merged: %v", err)
		}
	}
	input := MemberToolMessageInput{Content: "answer this", RecipientIDs: []string{f.b, f.a, f.b}}
	originalRecipients := slices.Clone(input.RecipientIDs)
	first, err := f.s.SendMessageForTool(ctx, scope, input, "explicit")
	if err != nil || len(first.Deliveries) != 2 || !slices.Equal(input.RecipientIDs, originalRecipients) {
		t.Fatalf("explicit recipients changed: %+v, %v", first, err)
	}
	for _, delivery := range first.Deliveries {
		if delivery.State != domain.DeliveryStateQueued || delivery.TurnID != "" {
			t.Fatal("sending started or accepted a model")
		}
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.b); err != nil {
		t.Fatal(err)
	}
	input.RecipientIDs = []string{f.a, f.b}
	replay, err := queueHandle(t, f.path, false).SendMessageForTool(ctx, scope, input, "explicit")
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatalf("receipt rerouted or revalidated departed recipients: %+v, %v", replay, err)
	}
	input.Content = "changed"
	if _, err := f.s.SendMessageForTool(ctx, scope, input, "explicit"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	before := memberOperationCounts(t, f.s)
	for _, recipients := range [][]string{{f.foreign}, {"missing"}, {f.a, f.foreign}, {f.b}} {
		got, err := f.s.SendMessageForTool(ctx, scope, MemberToolMessageInput{Content: "PRIVATE", RecipientIDs: recipients}, "invalid-recipient")
		if !errors.Is(err, ErrNotFound) || got.ID != "" || memberOperationCounts(t, f.s) != before {
			t.Fatalf("recipient rejection left partial effects: %v", err)
		}
	}
}

func TestMemberToolOperationCreateJoinReceiptAndOwnership(t *testing.T) {
	f, scope := memberOperationFixture(t)
	input, policy := memberOperationInput(), memberOperationPolicy()
	first, err := f.s.CreateMemberForTool(ctx, scope, input, policy, "create")
	if err != nil || first.Reused || first.JoinMessage == nil {
		t.Fatalf("new member was not joined: %+v, %v", first, err)
	}
	m, join := first.Member, first.JoinMessage
	if m.CreatorID != f.a || m.OwnerID != "user" || m.Workspace != "" || m.Status != domain.MemberStatusActive || m.ProviderID != policy.ProviderID || m.Model != policy.Model {
		t.Fatalf("creation changed trusted policy or lifecycle ownership: %+v", m)
	}
	if join.SenderID != f.a || join.GroupID != f.groupID || join.TopicID != scope.TopicID || !strings.Contains(join.Content, input.Name) || !strings.Contains(join.Content, input.Reason) || len(join.Deliveries) != 1 || join.Deliveries[0].RecipientID != m.ID || join.Deliveries[0].State != domain.DeliveryStateQueued || join.Deliveries[0].TurnID != "" {
		t.Fatalf("join was not durably delivered with background: %+v", join)
	}
	stored, err := f.s.GetMessage(ctx, f.groupID, join.ID)
	if err != nil || !reflect.DeepEqual(stored, *join) {
		t.Fatalf("join output differs from actual durable delivery: %+v, %v", stored, err)
	}
	counts := memberOperationCounts(t, f.s)
	if counts != ([6]int{4, 2, 2, 2, 1, 1}) {
		t.Fatalf("join did not commit exactly once: %v", counts)
	}
	policy.MaxMembers = 1 // A receipt is not a fresh capacity decision.
	replay, err := queueHandle(t, f.path, false).CreateMemberForTool(ctx, scope, input, policy, "create")
	if err != nil || !reflect.DeepEqual(replay, first) || memberOperationCounts(t, f.s) != counts {
		t.Fatalf("reopened replay changed the receipt: %+v, %v", replay, err)
	}
	for _, change := range []func(*MemberToolCreateInput){
		func(i *MemberToolCreateInput) { i.Name += " other" },
		func(i *MemberToolCreateInput) { i.Role += " other" },
		func(i *MemberToolCreateInput) { i.Reason += " other" },
	} {
		bad := input
		change(&bad)
		if _, err := f.s.CreateMemberForTool(ctx, scope, bad, policy, "create"); !errors.Is(err, ErrConflict) {
			t.Fatalf("altered original input did not conflict: %v", err)
		}
	}
	for _, changed := range []MemberToolCreatePolicy{{ProviderID: "other", Model: policy.Model, MaxMembers: 3}, {ProviderID: policy.ProviderID, Model: "other", MaxMembers: 3}} {
		if _, err := f.s.CreateMemberForTool(ctx, scope, input, changed, "create"); !errors.Is(err, ErrConflict) {
			t.Fatalf("altered trusted selection did not conflict: %v", err)
		}
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	owned, err := f.s.GetMember(ctx, f.groupID, m.ID)
	if err != nil || owned.Status != domain.MemberStatusActive || owned.OwnerID != "user" {
		t.Fatalf("creator departure changed lifecycle ownership: %+v, %v", owned, err)
	}
}

func TestMemberToolOperationExactReuseBeforeCapacity(t *testing.T) {
	f, scope := memberOperationFixture(t)
	input, policy := memberOperationInput(), memberOperationPolicy()
	var matches []domain.Member
	for i := range 7 {
		in := CreateMemberInput{GroupID: f.groupID, Name: fmt.Sprintf("existing-%d", i), Role: input.Role, ProviderID: policy.ProviderID, Model: policy.Model, OwnerID: "user"}
		switch i {
		case 0:
			in.GroupID = f.otherID
		case 1:
			in.Workspace = "/external"
		case 2:
			in.Model = "different"
		case 3:
			in.ProviderID = "different"
		}
		m, err := f.s.CreateMember(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if i == 4 {
			if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", m.ID); err != nil {
				t.Fatal(err)
			}
		}
		if i >= 5 {
			matches = append(matches, m)
		}
	}
	// Every ineligible candidate is older, so omitting any exact-match predicate fails.
	stamp := time.Date(2025, 1, 1, 0, 0, 0, 9, time.UTC)
	if _, err := f.s.db.Exec("UPDATE members SET created_at=?", stamp.Add(-time.Nanosecond).Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE members SET created_at=? WHERE id IN (?,?)", stamp.Format(timestampLayout), matches[0].ID, matches[1].ID); err != nil {
		t.Fatal(err)
	}
	wantID := min(matches[0].ID, matches[1].ID)
	before := memberOperationCounts(t, f.s)
	policy.MaxMembers = 1
	result, err := f.s.CreateMemberForTool(ctx, scope, input, policy, "reuse")
	if err != nil || !result.Reused || result.Member.ID != wantID || result.Member.Name == input.Name || result.JoinMessage != nil {
		t.Fatalf("full-capacity exact reuse was not deterministic: %+v, %v", result, err)
	}
	wantCounts := before
	wantCounts[4]++
	if memberOperationCounts(t, f.s) != wantCounts {
		t.Fatal("reuse inserted a member, join, or delivery")
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", result.Member.ID); err != nil {
		t.Fatal(err)
	}
	replayed, err := f.s.CreateMemberForTool(ctx, scope, input, policy, "reuse")
	if err != nil || !replayed.Reused || replayed.Member.ID != result.Member.ID || replayed.JoinMessage != nil {
		t.Fatalf("replay reselected or woke a matching member: %+v, %v", replayed, err)
	}
}

func TestMemberToolOperationValidationCapacityAndRollback(t *testing.T) {
	f, scope := memberOperationFixture(t)
	input, policy := memberOperationInput(), memberOperationPolicy()
	before := memberOperationCounts(t, f.s)
	for _, key := range []string{"", " ", "padded ", "a,b", "line\nkey", "nul\x00key", "中文", strings.Repeat("x", 129)} {
		if got, err := f.s.SendMessageForTool(ctx, scope, MemberToolMessageInput{Content: "message"}, key); !errors.Is(err, ErrInvalid) || got.ID != "" {
			t.Fatalf("invalid send key accepted: %v", err)
		}
		if got, err := f.s.CreateMemberForTool(ctx, scope, input, policy, key); !errors.Is(err, ErrInvalid) || got.Member.ID != "" || got.JoinMessage != nil {
			t.Fatalf("invalid creation key accepted: %v", err)
		}
	}
	for _, field := range []string{"name", "role", "reason"} {
		maximum := map[string]int{"name": 160, "role": 8192, "reason": 16384}[field]
		for _, invalid := range []string{"", " \n", "nul\x00value", "\xff", strings.Repeat("x", maximum+1)} {
			bad := input
			switch field {
			case "name":
				bad.Name = invalid
			case "role":
				bad.Role = invalid
			case "reason":
				bad.Reason = invalid
			}
			if got, err := f.s.CreateMemberForTool(ctx, scope, bad, policy, "invalid"); !errors.Is(err, ErrInvalid) || got.Member.ID != "" {
				t.Fatalf("invalid %s accepted: %v", field, err)
			}
		}
	}
	for _, bad := range []MemberToolCreatePolicy{{}, {ProviderID: "pi", Model: "selected", MaxMembers: -1}, {ProviderID: "pi", MaxMembers: 3}} {
		if _, err := f.s.CreateMemberForTool(ctx, scope, input, bad, "policy"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid trusted policy accepted: %v", err)
		}
	}
	policy.MaxMembers = 2
	result, err := f.s.CreateMemberForTool(ctx, scope, input, policy, "capacity")
	if !errors.Is(err, ErrMemberCapacity) || result.Member.ID != "" || result.JoinMessage != nil || memberOperationCounts(t, f.s) != before {
		t.Fatalf("capacity rejection left a partial mutation: %+v, %v", result, err)
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.b); err != nil {
		t.Fatal(err)
	}
	input.Name, input.Role, input.Reason = strings.Repeat("n", 160), strings.Repeat("r", 8192), strings.Repeat("b", 16384)
	if result, err := f.s.CreateMemberForTool(ctx, scope, input, policy, "capacity"); err != nil || !strings.Contains(result.JoinMessage.Content, input.Reason) {
		t.Fatalf("exact byte limits or rollback retry failed: %v", err)
	}
}

func TestMemberToolOperationForgedScopeDeniedEverywhere(t *testing.T) {
	f, scope := memberOperationFixture(t)
	conversation, err := f.s.GetOrCreateConversation(ctx, f.groupID, f.b, scope.TopicID)
	if err != nil {
		t.Fatal(err)
	}
	before := memberOperationCounts(t, f.s)
	for _, forge := range []func(*MemberToolScope){
		func(s *MemberToolScope) { s.MemberID, s.ConversationID = f.b, conversation.ID },
		func(s *MemberToolScope) { s.Claim.GroupID = f.otherID },
		func(s *MemberToolScope) { s.TopicID = "other-topic" },
	} {
		bad := scope
		forge(&bad)
		_, err := f.s.ListMembersForTool(ctx, bad, ListOptions{})
		requireMemberToolDenied(t, err)
		_, err = f.s.ReadChannelForTool(ctx, bad, ListOptions{})
		requireMemberToolDenied(t, err)
		_, err = f.s.SendMessageForTool(ctx, bad, MemberToolMessageInput{Content: "forged"}, "forged")
		requireMemberToolDenied(t, err)
		_, err = f.s.CreateMemberForTool(ctx, bad, memberOperationInput(), memberOperationPolicy(), "forged")
		requireMemberToolDenied(t, err)
	}
	if memberOperationCounts(t, f.s) != before {
		t.Fatal("forged identity changed durable state")
	}
}
