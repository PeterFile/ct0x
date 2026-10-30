package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestMessageHistoryBeforeAndIncrementalAfter(t *testing.T) {
	f := setupMessages(t)
	empty, err := f.s.ListMessages(ctx, f.groupID, MessageListOptions{})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("empty history: %+v, %v", empty, err)
	}
	c, err := f.s.GetOrCreateConversation(ctx, f.groupID, f.a, "")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := make([]domain.Message, 0, 205)
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 205 {
		n := (i * 73) % 205
		stamp := base.Add(time.Duration(n%5) * time.Nanosecond)
		id := fmt.Sprintf("m-%03d", n)
		if _, err := tx.Exec(`INSERT INTO messages(id,group_id,sender_id,content,created_at) VALUES(?,?,'user','fixture',?)`, id, f.groupID, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
		d := domain.Delivery{ID: "d-" + id, MessageID: id, ConversationID: c.ID, RecipientID: f.a, State: domain.DeliveryStateQueued, CreatedAt: stamp}
		if _, err := tx.Exec(`INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at) VALUES(?,?,?,?,?,'',?)`, d.ID, id, c.ID, f.a, f.groupID, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
		want = append(want, domain.Message{ID: id, GroupID: f.groupID, SenderID: "user", Content: "fixture", CreatedAt: stamp, Deliveries: []domain.Delivery{d}})
	}
	if _, err := tx.Exec(`INSERT INTO messages(id,group_id,sender_id,content,created_at) VALUES('foreign',?,'user','secret',?)`, f.otherID, base.Add(time.Hour).Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	position := func(m domain.Message) Cursor { return Cursor{CreatedAt: m.CreatedAt, ID: m.ID} }
	t.Run("before", func(t *testing.T) {
		list := func(q ListOptions) (Page[domain.Message], error) {
			return f.s.ListMessages(ctx, f.groupID, MessageListOptions{Limit: q.Limit, Before: q.Before})
		}
		checkPages(t, want, list, position)
	})
	t.Run("after", func(t *testing.T) {
		ascending := slices.Clone(want)
		slices.Reverse(ascending)
		list := func(q ListOptions) (Page[domain.Message], error) {
			after := q.Before
			if after == nil {
				after = &Cursor{CreatedAt: base.Add(-time.Second), ID: "start"}
			}
			return f.s.ListMessages(ctx, f.groupID, MessageListOptions{Limit: q.Limit, After: after})
		}
		checkPages(t, ascending, list, position)
	})
	foreignCursor := &Cursor{CreatedAt: base.Add(time.Hour), ID: "foreign"}
	page, err := f.s.ListMessages(ctx, f.groupID, MessageListOptions{Before: foreignCursor})
	if err != nil || !reflect.DeepEqual(page.Items, want[:50]) {
		t.Fatalf("foreign cursor escaped group scope: %+v, %v", page, err)
	}
	if _, err := f.s.ListMessages(ctx, "missing", MessageListOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing group history: %v", err)
	}
	for _, options := range []MessageListOptions{
		{Limit: -1}, {Limit: 201}, {Before: foreignCursor, After: foreignCursor},
		{Before: &Cursor{ID: "id"}}, {After: &Cursor{CreatedAt: base}},
		{After: &Cursor{ID: "id", CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}},
	} {
		if _, err := f.s.ListMessages(ctx, f.groupID, options); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid history options %+v: %v", options, err)
		}
	}
}
