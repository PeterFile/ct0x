package platform

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type localMessageFixture struct {
	db      *store.Store
	local   *LocalUser
	raw     *sql.DB
	path    string
	groupID string
	otherID string
}

func setupLocalMessages(t *testing.T) localMessageFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workspace.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	local := NewLocalUser(db)
	g, err := local.CreateGroup(context.Background(), GroupInput{Name: "group"}, "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := local.CreateGroup(context.Background(), GroupInput{Name: "other"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return localMessageFixture{db, local, raw, path, g.ID, other.ID}
}

func (f localMessageFixture) member(t *testing.T, groupID string) domain.Member {
	t.Helper()
	m, err := f.local.CreateMember(context.Background(), groupID, MemberInput{Name: "member", Role: "role"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f localMessageFixture) counts(t *testing.T, messages, deliveries, conversations, keys int) {
	t.Helper()
	for table, want := range map[string]int{"messages": messages, "deliveries": deliveries, "conversations": conversations, "mutation_idempotency": keys} {
		var got int
		if err := f.raw.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s count = %d, %v; want %d", table, got, err, want)
		}
	}
}

func TestLocalMessageOriginalRequestAndFrozenRouting(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	a, b := f.member(t, f.groupID), f.member(t, f.groupID)
	input := MessageInput{Content: " exact content ", TopicID: "topic"}
	first, err := f.local.CreateMessage(ctx, f.groupID, input, "default")
	if err != nil || first.ID == "" || first.SenderID != "user" || first.Content != input.Content || first.TopicID != input.TopicID || first.GroupID != f.groupID || len(first.Deliveries) != 1 || first.Deliveries[0].RecipientID != a.ID || first.Deliveries[0].State != domain.DeliveryStateQueued {
		t.Fatalf("default local-user message: %+v, %v", first, err)
	}
	original := store.MessageRequest{GroupID: f.groupID, ActorID: "user", Content: input.Content, TopicID: input.TopicID, IdempotencyKey: "default"}
	if receipt, found, err := f.db.LookupMessageReceipt(ctx, original); err != nil || !found || !reflect.DeepEqual(receipt, first) {
		t.Fatalf("platform rewrote original nil recipients or actor: %+v, %v, %v", receipt, found, err)
	}
	input.RecipientIDs = []string{}
	if _, err := f.local.CreateMessage(ctx, f.groupID, input, "default"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("omitted and explicit empty were conflated: %v", err)
	}
	empty, err := f.local.CreateMessage(ctx, f.groupID, input, "empty")
	if err != nil || empty.Deliveries == nil || len(empty.Deliveries) != 0 {
		t.Fatalf("explicit empty was routed: %+v, %v", empty, err)
	}
	input.RecipientIDs = []string{b.ID, a.ID, b.ID}
	unchanged := slices.Clone(input.RecipientIDs)
	explicit, err := f.local.CreateMessage(ctx, f.groupID, input, "explicit")
	if err != nil || len(explicit.Deliveries) != 2 || !slices.Equal(input.RecipientIDs, unchanged) {
		t.Fatalf("explicit set changed: %+v, %v", explicit, err)
	}
	if _, err := f.raw.Exec("UPDATE members SET status='left' WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := f.local.CreateMessage(ctx, f.groupID, input, "explicit")
	if err != nil || !reflect.DeepEqual(retry, explicit) {
		t.Fatalf("explicit replay revalidated departed recipients: %+v, %v", retry, err)
	}
	input.RecipientIDs = nil
	fresh, err := f.local.CreateMessage(ctx, f.groupID, input, "fresh")
	if err != nil || len(fresh.Deliveries) != 1 || fresh.Deliveries[0].RecipientID != b.ID {
		t.Fatalf("first request did not use current routing: %+v, %v", fresh, err)
	}
	retry, err = f.local.CreateMessage(ctx, f.groupID, input, "default")
	if err != nil || !reflect.DeepEqual(retry, first) {
		t.Fatalf("default routing was not frozen: %+v, %v", retry, err)
	}
	f.counts(t, 4, 4, 2, 4)
}

func TestLocalMessageZeroMembersReplayAfterJoinAndReopen(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	input := MessageInput{Content: "visible without a provider"}
	first, err := f.local.CreateMessage(ctx, f.groupID, input, "zero")
	if err != nil || first.ID == "" || first.Deliveries == nil || len(first.Deliveries) != 0 {
		t.Fatalf("zero-member visible message: %+v, %v", first, err)
	}
	members, err := f.local.ListMembers(ctx, f.groupID, store.ListOptions{})
	if err != nil || len(members.Items) != 0 {
		t.Fatalf("message fabricated an automatic member: %+v, %v", members, err)
	}
	joined := f.member(t, f.groupID)
	retry, err := f.local.CreateMessage(ctx, f.groupID, input, "zero")
	if err != nil || !reflect.DeepEqual(retry, first) {
		t.Fatalf("joining member changed zero-delivery receipt: %+v, %v", retry, err)
	}
	fresh, err := f.local.CreateMessage(ctx, f.groupID, input, "joined")
	if err != nil || len(fresh.Deliveries) != 1 || fresh.Deliveries[0].RecipientID != joined.ID {
		t.Fatalf("new message did not route after join: %+v, %v", fresh, err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	local := NewLocalUser(reopened)
	retry, err = local.CreateMessage(ctx, f.groupID, input, "zero")
	if err != nil || !reflect.DeepEqual(retry, first) {
		t.Fatalf("reopen lost frozen receipt: %+v, %v", retry, err)
	}
	page, err := local.ListMessages(ctx, f.groupID, store.ListOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != fresh.ID || page.NextCursor == nil {
		t.Fatalf("bounded platform history: %+v, %v", page, err)
	}
	tail, err := local.ListMessages(ctx, f.groupID, store.ListOptions{Limit: 1, Before: page.NextCursor})
	if err != nil || len(tail.Items) != 1 || !reflect.DeepEqual(tail.Items[0], first) || tail.NextCursor != nil {
		t.Fatalf("platform before continuation: %+v, %v", tail, err)
	}
}

func TestLocalMessageAtomicFailureAndRetry(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	a, b, foreign := f.member(t, f.groupID), f.member(t, f.groupID), f.member(t, f.otherID)
	for _, tt := range []struct {
		groupID string
		input   MessageInput
		want    error
	}{
		{f.groupID, MessageInput{Content: "hello", RecipientIDs: []string{a.ID, foreign.ID}}, store.ErrNotFound},
		{"missing", MessageInput{Content: "hello"}, store.ErrNotFound},
		{f.groupID, MessageInput{Content: " \t"}, store.ErrInvalid},
	} {
		if _, err := f.local.CreateMessage(ctx, tt.groupID, tt.input, "retry"); !errors.Is(err, tt.want) {
			t.Fatalf("invalid message error: %v, want %v", err, tt.want)
		}
		f.counts(t, 0, 0, 0, 0)
	}
	if _, err := f.raw.Exec(`CREATE TRIGGER reject_second_delivery BEFORE INSERT ON deliveries
WHEN (SELECT count(*) FROM deliveries WHERE message_id=NEW.message_id)=1
BEGIN SELECT RAISE(ABORT, 'second delivery failed'); END`); err != nil {
		t.Fatal(err)
	}
	input := MessageInput{Content: "hello", RecipientIDs: []string{a.ID, b.ID}}
	if message, err := f.local.CreateMessage(ctx, f.groupID, input, "retry"); err == nil || message.ID != "" {
		t.Fatalf("partial transaction returned success: %+v, %v", message, err)
	}
	f.counts(t, 0, 0, 0, 0)
	if _, err := f.raw.Exec("DROP TRIGGER reject_second_delivery"); err != nil {
		t.Fatal(err)
	}
	message, err := f.local.CreateMessage(ctx, f.groupID, input, "retry")
	if err != nil || len(message.Deliveries) != 2 {
		t.Fatalf("atomic retry failed: %+v, %v", message, err)
	}
	if _, err := f.local.GetMessage(ctx, f.otherID, message.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-group message leaked: %v", err)
	}
	if _, err := f.local.ListMessages(ctx, "missing", store.ListOptions{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing group history: %v", err)
	}
	input.Content = "changed"
	if _, err := f.local.CreateMessage(ctx, f.groupID, input, "retry"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed retry did not conflict: %v", err)
	}
	f.counts(t, 1, 2, 2, 1)
}

func TestLocalMessageCompetingDuplicateRequests(t *testing.T) {
	for _, competing := range []bool{false, true} {
		name := "identical"
		if competing {
			name = "conflicting"
		}
		t.Run(name, func(t *testing.T) {
			f := setupLocalMessages(t)
			f.member(t, f.groupID)
			second, err := store.Open(f.path)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			locals := []*LocalUser{f.local, NewLocalUser(second)}
			type result struct {
				message domain.Message
				content string
				err     error
			}
			start, results := make(chan struct{}), make(chan result, 8)
			for i := range 8 {
				go func() {
					<-start
					content := "one"
					if competing && i%2 == 1 {
						content = "two"
					}
					m, err := locals[i%2].CreateMessage(context.Background(), f.groupID, MessageInput{Content: content}, "concurrent")
					results <- result{m, content, err}
				}()
			}
			close(start)
			var winner domain.Message
			successes, conflicts := 0, 0
			for range 8 {
				r := <-results
				if errors.Is(r.err, store.ErrConflict) {
					conflicts++
					continue
				}
				if r.err != nil || r.message.Content != r.content || (winner.ID != "" && !reflect.DeepEqual(winner, r.message)) {
					t.Errorf("duplicate requests diverged: %+v, %v", r.message, r.err)
				}
				winner = r.message
				successes++
			}
			want := 8
			if competing {
				want = 4
			}
			if successes != want || conflicts != 8-want || len(winner.Deliveries) != 1 {
				t.Fatalf("success/conflict counts: %d/%d; winner %+v", successes, conflicts, winner)
			}
			f.counts(t, 1, 1, 1, 1)
		})
	}
}
