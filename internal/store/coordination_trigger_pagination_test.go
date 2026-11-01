package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func coordinationTriggerSeed(t *testing.T, tx *sql.Tx, thread domain.ThreadRef, id string, stamp time.Time) domain.Trigger {
	t.Helper()
	message := domain.Message{ID: "message-" + id, GroupID: thread.GroupID, TopicID: thread.TopicID, SenderID: "user", Content: "fixture", CreatedAt: stamp.Add(time.Hour)}
	if err := insertMessageRow(ctx, tx, message); err != nil {
		t.Fatal(err)
	}
	trigger := domain.Trigger{ID: id, SourceKind: domain.TriggerSourceLocalIM, GroupID: thread.GroupID, TopicID: thread.TopicID, MessageID: message.ID, CreatedAt: stamp}
	if err := tx.QueryRow("SELECT sequence FROM message_log WHERE message_id=?", message.ID).Scan(&trigger.MessageSequence); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO coordination_triggers(id,source_kind,group_id,topic_id,message_id,message_sequence,created_at)
	 VALUES(?,?,?,?,?,?,?)`, trigger.ID, trigger.SourceKind, trigger.GroupID, trigger.TopicID, trigger.MessageID, trigger.MessageSequence, stamp.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	return trigger
}

func TestCoordinationTriggerBoundedListAndCursorValidation(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	thread := domain.ThreadRef{GroupID: g.ID, TopicID: "topic"}
	empty, err := s.ListTriggers(ctx, thread, ListOptions{})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("empty page: %+v, %v", empty, err)
	}
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	want := make([]domain.Trigger, 0, 205)
	for i := range 205 {
		n := (i * 73) % 205
		want = append(want, coordinationTriggerSeed(t, tx, thread, fmt.Sprintf("trigger-%03d", n), base.Add(time.Duration(n%5)*time.Nanosecond)))
	}
	main := coordinationTriggerSeed(t, tx, domain.ThreadRef{GroupID: g.ID}, "main", base.Add(time.Hour))
	foreign := coordinationTriggerSeed(t, tx, domain.ThreadRef{GroupID: other.ID, TopicID: thread.TopicID}, "foreign", base.Add(time.Hour))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	checkPages(t, want, func(options ListOptions) (Page[domain.Trigger], error) {
		return s.ListTriggers(ctx, thread, options)
	}, func(trigger domain.Trigger) Cursor { return Cursor{CreatedAt: trigger.CreatedAt, ID: trigger.ID} })
	for _, limit := range []int{1, 50, 200} {
		// Slice a tail of exactly limit rows, proving limit+1 does not invent a continuation.
		previous := want[len(want)-limit-1]
		page, err := s.ListTriggers(ctx, thread, ListOptions{Limit: limit, Before: &Cursor{CreatedAt: previous.CreatedAt, ID: previous.ID}})
		if err != nil || !reflect.DeepEqual(page.Items, want[len(want)-limit:]) || page.NextCursor != nil {
			t.Fatalf("exact final page, limit=%d: %+v, %v", limit, page, err)
		}
	}
	last := want[len(want)-1]
	page, err := s.ListTriggers(ctx, thread, ListOptions{Before: &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty final tail: %+v, %v", page, err)
	}
	for _, cursor := range []*Cursor{
		{CreatedAt: main.CreatedAt, ID: main.ID}, {CreatedAt: foreign.CreatedAt, ID: foreign.ID},
		{CreatedAt: base, ID: "missing"}, {CreatedAt: want[0].CreatedAt.Add(time.Second), ID: want[0].ID},
	} {
		if _, err := s.ListTriggers(ctx, thread, ListOptions{Before: cursor}); !errors.Is(err, ErrNotFound) {
			t.Errorf("nonexistent/foreign/forged cursor accepted: %+v, %v", cursor, err)
		}
	}
	for _, options := range []ListOptions{
		{Limit: -1}, {Limit: 201}, {Before: &Cursor{ID: "id"}}, {Before: &Cursor{CreatedAt: base}},
		{Before: &Cursor{ID: strings.Repeat("x", 4097), CreatedAt: base}},
		{Before: &Cursor{ID: "id", CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}},
	} {
		if _, err := s.ListTriggers(ctx, thread, options); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid list options: %v", err)
		}
	}
	// Cursor timestamps denote instants, not a required input timezone.
	position := want[0]
	zoned := position.CreatedAt.In(time.FixedZone("offset", 3600))
	page, err = s.ListTriggers(ctx, thread, ListOptions{Limit: 1, Before: &Cursor{CreatedAt: zoned, ID: position.ID}})
	if err != nil || !reflect.DeepEqual(page.Items, want[1:2]) {
		t.Fatalf("equivalent cursor instant rejected: %+v, %v", page, err)
	}
}

func TestCoordinationTriggerIndexedReads(t *testing.T) {
	s, _ := openTestStore(t)
	for name, query := range map[string]string{
		"initial": `SELECT id,source_kind,group_id,topic_id,message_id,message_sequence,created_at FROM coordination_triggers
		 WHERE group_id=? AND topic_id=? ORDER BY created_at DESC,id DESC LIMIT ?`,
		"continuation": `SELECT id,source_kind,group_id,topic_id,message_id,message_sequence,created_at FROM coordination_triggers
		 WHERE group_id=? AND topic_id=? AND (created_at,id)<(?,?) ORDER BY created_at DESC,id DESC LIMIT ?`,
	} {
		t.Run(name, func(t *testing.T) {
			args := []any{"group", "topic"}
			if name == "continuation" {
				args = append(args, time.Now().UTC().Format(timestampLayout), "cursor")
			}
			rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, append(args, 51)...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(details, "\n")
			if !strings.Contains(plan, "SEARCH coordination_triggers USING INDEX coordination_triggers_thread_created_id") || strings.Contains(plan, "SCAN coordination_triggers") || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("unbounded scan/sort plan: %s", plan)
			}
		})
	}
}
