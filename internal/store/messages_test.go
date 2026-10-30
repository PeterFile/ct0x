package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

type messageFixture struct {
	s                                     *Store
	path, groupID, otherID, a, b, foreign string
}

func setupMessages(t *testing.T) messageFixture {
	t.Helper()
	s, path := openTestStore(t)
	g, other := group(t, s), group(t, s)
	return messageFixture{s, path, g.ID, other.ID, member(t, s, g.ID).ID, member(t, s, g.ID).ID, member(t, s, other.ID).ID}
}

func (f messageFixture) request() MessageRequest {
	return MessageRequest{GroupID: f.groupID, ActorID: "user", TopicID: "topic", Content: "hello", IdempotencyKey: "message-key"}
}

func assertMessageCounts(t *testing.T, s *Store, messages, deliveries, conversations, keys int) {
	t.Helper()
	for table, want := range map[string]int{"messages": messages, "deliveries": deliveries, "conversations": conversations, "mutation_idempotency": keys} {
		var got int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s count = %d, %v; want %d", table, got, err, want)
		}
	}
}

func TestMessageVisibilityAndDeliveryIdentity(t *testing.T) {
	f := setupMessages(t)
	r := f.request()
	r.IdempotencyKey, r.TopicID = "", ""
	visible, err := f.s.CreateMessage(ctx, r, nil)
	if err != nil || visible.SenderID != "user" || visible.Deliveries == nil || len(visible.Deliveries) != 0 {
		t.Fatalf("visible-only message: %+v, %v", visible, err)
	}
	data, err := json.Marshal(visible)
	if err != nil || !strings.Contains(string(data), `"deliveries":[]`) {
		t.Fatalf("empty deliveries are not an array: %s, %v", data, err)
	}
	if got, found, err := f.s.LookupMessageReceipt(ctx, r); err != nil || found || got.ID != "" {
		t.Fatalf("no-key request acquired a receipt: %+v, %v, %v", got, found, err)
	}
	r.ActorID, r.TopicID = f.a, "topic"
	r.RecipientIDs = []string{f.b, f.a, f.b}
	resolved := []string{f.b, f.b, f.a}
	original, originalResolved := slices.Clone(r.RecipientIDs), slices.Clone(resolved)
	m, err := f.s.CreateMessage(ctx, r, resolved)
	if err != nil || m.SenderID != f.a || m.GroupID != r.GroupID || m.TopicID != r.TopicID || m.Content != r.Content || len(m.Deliveries) != 2 {
		t.Fatalf("member message: %+v, %v", m, err)
	}
	if !slices.Equal(r.RecipientIDs, original) || !slices.Equal(resolved, originalResolved) {
		t.Fatal("caller recipient slices were mutated")
	}
	for _, d := range m.Deliveries {
		c, err := f.s.GetConversation(ctx, f.groupID, d.ConversationID)
		if err != nil || c.MemberID != d.RecipientID || c.GroupID != m.GroupID || c.TopicID != m.TopicID ||
			d.ID == "" || d.MessageID != m.ID || d.State != domain.DeliveryStateQueued || d.TurnID != "" || d.Error != "" || d.CreatedAt.Location() != time.UTC {
			t.Fatalf("delivery identity/state: %+v, %+v, %v", d, c, err)
		}
	}
	if m.CreatedAt.IsZero() || m.CreatedAt.Location() != time.UTC || m.Deliveries[0].RecipientID >= m.Deliveries[1].RecipientID {
		t.Fatal("timestamps or deterministic delivery ordering are invalid")
	}
	again, err := f.s.CreateMessage(ctx, r, resolved)
	if err != nil || again.ID == m.ID || again.Deliveries[0].ConversationID != m.Deliveries[0].ConversationID {
		t.Fatalf("no-key creation or stable conversation failed: %+v, %v", again, err)
	}
	got, err := f.s.GetMessage(ctx, f.groupID, m.ID)
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("get message changed: %+v, %v", got, err)
	}
	for _, pair := range [][2]string{{f.otherID, m.ID}, {f.groupID, "missing"}, {"missing", m.ID}} {
		if _, err := f.s.GetMessage(ctx, pair[0], pair[1]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unscoped/missing message %v: %v", pair, err)
		}
	}
	assertMessageCounts(t, f.s, 3, 4, 2, 0)
}

func TestMessageReceiptOriginalRequestAndReopen(t *testing.T) {
	f := setupMessages(t)
	r := f.request()
	if got, found, err := f.s.LookupMessageReceipt(ctx, r); err != nil || found || got.ID != "" {
		t.Fatalf("pre-routing miss: %+v, %v, %v", got, found, err)
	}
	assertMessageCounts(t, f.s, 0, 0, 0, 0)
	m, err := f.s.CreateMessage(ctx, r, []string{f.a, f.b})
	if err != nil {
		t.Fatal(err)
	}
	check := func(s *Store) {
		t.Helper()
		got, found, err := s.LookupMessageReceipt(ctx, r)
		if err != nil || !found || !reflect.DeepEqual(got, m) {
			t.Fatalf("receipt replay: %+v, %v, %v", got, found, err)
		}
		// A retry must not validate or use a newly resolved, now-invalid target.
		got, err = s.CreateMessage(ctx, r, []string{f.foreign, "missing"})
		if err != nil || !reflect.DeepEqual(got, m) {
			t.Fatalf("routing result was not frozen: %+v, %v", got, err)
		}
		got, err = s.GetMessage(ctx, r.GroupID, m.ID)
		if err != nil || !reflect.DeepEqual(got, m) {
			t.Fatalf("durable message/deliveries changed: %+v, %v", got, err)
		}
		assertMessageCounts(t, s, 1, 2, 2, 1)
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	check(f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
}

func TestMessageOriginalPayloadConflictsAndSets(t *testing.T) {
	f := setupMessages(t)
	r := f.request()
	r.RecipientIDs = []string{f.b, f.a, f.b}
	m, err := f.s.CreateMessage(ctx, r, []string{f.a, f.b})
	if err != nil {
		t.Fatal(err)
	}
	equivalent := r
	equivalent.RecipientIDs = []string{f.a, f.b, f.a}
	if got, found, err := f.s.LookupMessageReceipt(ctx, equivalent); err != nil || !found || !reflect.DeepEqual(got, m) {
		t.Fatalf("explicit set equivalence: %+v, %v, %v", got, found, err)
	}
	if got, err := f.s.CreateMessage(ctx, equivalent, nil); err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("explicit retry compared new resolved targets: %+v, %v", got, err)
	}
	for _, change := range []func(*MessageRequest){
		func(r *MessageRequest) { r.Content += " " }, func(r *MessageRequest) { r.Content = " " },
		func(r *MessageRequest) { r.TopicID = "other" }, func(r *MessageRequest) { r.RecipientIDs = []string{f.a} },
		func(r *MessageRequest) { r.RecipientIDs = nil }, func(r *MessageRequest) { r.RecipientIDs = []string{} },
	} {
		changed := r
		change(&changed)
		if got, found, err := f.s.LookupMessageReceipt(ctx, changed); !errors.Is(err, ErrConflict) || found || got.ID != "" {
			t.Errorf("lookup changed original payload: %+v, %v, %v", got, found, err)
		}
		if got, err := f.s.CreateMessage(ctx, changed, []string{f.a}); !errors.Is(err, ErrConflict) || got.ID != "" {
			t.Errorf("create changed original payload: %+v, %v", got, err)
		}
	}
	empty := f.request()
	empty.IdempotencyKey = "empty"
	if _, err := f.s.CreateMessage(ctx, empty, nil); err != nil {
		t.Fatal(err)
	}
	empty.RecipientIDs = []string{}
	if _, _, err := f.s.LookupMessageReceipt(ctx, empty); !errors.Is(err, ErrConflict) {
		t.Fatalf("omitted and explicit empty were conflated: %v", err)
	}
	empty.IdempotencyKey = "explicit-empty"
	if got, err := f.s.CreateMessage(ctx, empty, nil); err != nil || got.Deliveries == nil || len(got.Deliveries) != 0 {
		t.Fatalf("explicit empty request was not visible-only: %+v, %v", got, err)
	}
	assertMessageCounts(t, f.s, 3, 2, 2, 3)
}

func TestMessageAccessAndRecipientValidation(t *testing.T) {
	f := setupMessages(t)
	left := member(t, f.s, f.groupID)
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", left.ID); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"missing", "user"}, {f.groupID, ""}, {f.groupID, " user"}, {f.groupID, "missing"}, {f.groupID, f.foreign}, {f.groupID, left.ID}} {
		r := f.request()
		r.GroupID, r.ActorID = pair[0], pair[1]
		if _, err := f.s.CreateMessage(ctx, r, nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("invalid sender %v: %v", pair, err)
		}
		for _, key := range []string{"", r.IdempotencyKey} {
			r.IdempotencyKey = key
			if got, found, err := f.s.LookupMessageReceipt(ctx, r); !errors.Is(err, ErrNotFound) || found || got.ID != "" {
				t.Errorf("receipt access %v: %+v, %v, %v", pair, got, found, err)
			}
		}
	}
	for _, recipient := range []string{"", "zz-missing", f.foreign, left.ID} {
		r := f.request()
		if _, err := f.s.CreateMessage(ctx, r, []string{f.a, recipient}); !errors.Is(err, ErrNotFound) {
			t.Errorf("invalid recipient %q: %v", recipient, err)
		}
	}
	for _, explicit := range [][]string{{f.a}, {}} {
		r := f.request()
		r.RecipientIDs = explicit
		if _, err := f.s.CreateMessage(ctx, r, []string{f.b}); !errors.Is(err, ErrInvalid) {
			t.Errorf("explicit targets were rewritten: %v", err)
		}
	}
	r := f.request()
	r.Content = " \t"
	if _, err := f.s.CreateMessage(ctx, r, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank content: %v", err)
	}
	assertMessageCounts(t, f.s, 0, 0, 0, 0)
	if err := json.Unmarshal([]byte(`{"ActorID":"forged","actor_id":"forged","SenderID":"forged"}`), &r); err != nil || r.ActorID != "user" {
		t.Fatalf("transport supplied actor identity: %+v, %v", r, err)
	}
	// A current access check precedes even a used-key payload conflict.
	r = f.request()
	r.ActorID = f.a
	if _, err := f.s.CreateMessage(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	r.Content = "changed"
	if _, _, err := f.s.LookupMessageReceipt(ctx, r); !errors.Is(err, ErrNotFound) {
		t.Fatalf("receipt leaked after sender left: %v", err)
	}
	if _, err := f.s.CreateMessage(ctx, r, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create leaked after sender left: %v", err)
	}
}

func TestMessageAtomicRollbackOnLaterDeliveryFailure(t *testing.T) {
	f := setupMessages(t)
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_later_delivery BEFORE INSERT ON deliveries
WHEN (SELECT count(*) FROM deliveries WHERE message_id=NEW.message_id)=1
BEGIN SELECT RAISE(ABORT, 'later delivery failure'); END`); err != nil {
		t.Fatal(err)
	}
	r := f.request()
	if got, err := f.s.CreateMessage(ctx, r, []string{f.a, f.b}); err == nil || got.ID != "" {
		t.Fatalf("partial mutation returned success: %+v, %v", got, err)
	}
	assertMessageCounts(t, f.s, 0, 0, 0, 0)
	if _, err := f.s.db.Exec("DROP TRIGGER fail_later_delivery"); err != nil {
		t.Fatal(err)
	}
	r.Content = "fixed"
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.s.CreateMessage(cancelled, r, []string{f.a}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	if _, err := f.s.CreateMessage(ctx, r, []string{f.a, f.b}); err != nil {
		t.Fatal(err)
	}
	assertMessageCounts(t, f.s, 1, 2, 2, 1)
}

func TestMessageScopesAndConcurrentRoutingFreeze(t *testing.T) {
	f := setupMessages(t)
	second, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	type result struct {
		m   domain.Message
		err error
	}
	results := make(chan result, 12)
	start := make(chan struct{})
	for i := range 12 {
		go func() {
			<-start
			s := []*Store{f.s, second}[i%2]
			m, err := s.CreateMessage(ctx, f.request(), []string{[]string{f.a, f.b}[i%2]})
			results <- result{m, err}
		}()
	}
	close(start)
	var winner domain.Message
	for range 12 {
		r := <-results
		if r.err != nil || (winner.ID != "" && !reflect.DeepEqual(winner, r.m)) {
			t.Errorf("routing retries did not converge: %+v, %v", r.m, r.err)
		}
		winner = r.m
	}
	if len(winner.Deliveries) != 1 {
		t.Fatalf("unexpected frozen targets: %+v", winner)
	}
	assertMessageCounts(t, f.s, 1, 1, 1, 1)
	for _, scope := range [][2]string{{f.groupID, f.a}, {f.otherID, "user"}} {
		r := f.request()
		r.GroupID, r.ActorID = scope[0], scope[1]
		if got, found, err := f.s.LookupMessageReceipt(ctx, r); err != nil || found || got.ID != "" {
			t.Fatalf("foreign scope read receipt: %+v, %v, %v", got, found, err)
		}
		if got, err := f.s.CreateMessage(ctx, r, nil); err != nil || got.ID == winner.ID {
			t.Fatalf("actor/group scope collision: %+v, %v", got, err)
		}
	}
	assertMessageCounts(t, f.s, 3, 1, 1, 3)
}

func TestDeliveryCompositeConstraints(t *testing.T) {
	f := setupMessages(t)
	r := f.request()
	m, err := f.s.CreateMessage(ctx, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	conversation := func(groupID, memberID, topicID string) domain.Conversation {
		c, err := f.s.GetOrCreateConversation(ctx, groupID, memberID, topicID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a := conversation(f.groupID, f.a, r.TopicID)
	b := conversation(f.groupID, f.b, r.TopicID)
	otherTopic := conversation(f.groupID, f.a, "other")
	foreign := conversation(f.otherID, f.foreign, r.TopicID)
	query := `INSERT INTO deliveries(id, message_id, conversation_id, recipient_id, group_id, topic_id, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`
	for i, args := range [][]string{
		{a.ID, f.b, f.groupID, r.TopicID},
		{otherTopic.ID, f.a, f.groupID, r.TopicID},
		{foreign.ID, f.foreign, f.groupID, r.TopicID},
		{foreign.ID, f.foreign, f.otherID, r.TopicID},
		{otherTopic.ID, f.a, f.groupID, "other"},
	} {
		if _, err := f.s.db.Exec(query, fmt.Sprintf("bad-%d", i), m.ID, args[0], args[1], args[2], args[3], m.CreatedAt.Format(timestampLayout)); err == nil {
			t.Fatalf("inconsistent delivery accepted: %v", args)
		}
	}
	if _, err := f.s.db.Exec(query, "valid", m.ID, a.ID, f.a, f.groupID, r.TopicID, m.CreatedAt.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(query, "duplicate", m.ID, a.ID, f.a, f.groupID, r.TopicID, m.CreatedAt.Format(timestampLayout)); err == nil {
		t.Fatal("duplicate message/recipient delivery accepted")
	}
	if _, err := f.s.db.Exec(query, "second", m.ID, b.ID, f.b, f.groupID, r.TopicID, m.CreatedAt.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
}

func TestV2MessageMigrationPreservesReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY CHECK(version>0));` + migrations[0] + migrations[1] + `
INSERT INTO schema_migrations VALUES(1),(2);
INSERT INTO groups(id,name,description,created_at) VALUES('old','group','description','2026-10-30T12:00:00.000000000Z');`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO mutation_idempotency VALUES('create_group','actor','','retry',?,'old')`, mutationPayload("group", "description")); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, count int
	if err := s.db.QueryRow("SELECT max(version),count(*) FROM schema_migrations").Scan(&version, &count); err != nil || version != 3 || count != 3 {
		t.Fatalf("v2 upgrade: %d, %d, %v", version, count, err)
	}
	if g, err := s.CreateGroup(ctx, keyedGroup()); err != nil || g.ID != "old" {
		t.Fatalf("v2 receipt changed: %+v, %v", g, err)
	}
	if _, err := s.CreateMessage(ctx, MessageRequest{GroupID: "old", ActorID: "user", Content: "new"}, nil); err != nil {
		t.Fatal(err)
	}
}
