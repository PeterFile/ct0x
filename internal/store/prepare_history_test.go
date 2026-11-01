package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func preparationHistoryMessage(id, groupID, topicID string, stamp time.Time) domain.Message {
	return domain.Message{
		ID: id, GroupID: groupID, TopicID: topicID, SenderID: "user",
		Content: "原文 " + id + "🙂", CreatedAt: stamp, Deliveries: []domain.Delivery{},
	}
}

func insertPreparationHistoryMessages(t *testing.T, s *Store, messages ...domain.Message) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, m := range messages {
		if err := insertMessageRow(ctx, tx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestListPreparationHistoryScopesBeforeLimit(t *testing.T) {
	f := setupMessages(t)
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := []domain.Message{
		preparationHistoryMessage("history-1", f.groupID, "current", base),
		preparationHistoryMessage("history-2", f.groupID, "current", base.Add(time.Second)),
	}
	want[0].SenderID = f.a
	trigger := preparationHistoryMessage("trigger", f.groupID, "current", base.Add(2*time.Minute))
	foreign := preparationHistoryMessage("foreign", f.otherID, "current", base.Add(time.Second))
	messages := append([]domain.Message{}, want...)
	messages = append(messages, trigger, foreign,
		preparationHistoryMessage("main-topic", f.groupID, "", base.Add(2*time.Second)))
	// All 60 other-topic messages are newer than the relevant history, but before the trigger.
	for n := range 60 {
		messages = append(messages, preparationHistoryMessage(fmt.Sprintf("noise-%02d", n),
			f.groupID, "other", base.Add(time.Duration(n+10)*time.Second)))
	}
	insertPreparationHistoryMessages(t, f.s, messages...)

	t.Run("topic scoped history", func(t *testing.T) {
		got, err := f.s.ListPreparationHistory(ctx, f.groupID, trigger.ID, 0)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("topic history was crowded out or scope escaped: %+v, %v; want %+v", got, err, want)
		}
	})
	for _, tc := range []struct{ name, groupID, triggerID string }{
		{"missing trigger", f.groupID, "missing"},
		{"foreign trigger", f.groupID, foreign.ID},
		{"wrong group", f.otherID, trigger.ID},
		{"missing group", "missing", trigger.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.s.ListPreparationHistory(ctx, tc.groupID, tc.triggerID, 50)
			if got != nil || !errors.Is(err, ErrNotFound) {
				t.Fatalf("inaccessible trigger: %+v, %v", got, err)
			}
		})
	}
}

func TestListPreparationHistoryExclusiveTupleAndOrder(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := []domain.Message{
		preparationHistoryMessage("z-older", g.ID, "topic", base.Add(-time.Nanosecond)),
		preparationHistoryMessage("a-tied", g.ID, "topic", base),
		preparationHistoryMessage("b-tied", g.ID, "topic", base),
		preparationHistoryMessage("c-tied", g.ID, "topic", base),
	}
	trigger := preparationHistoryMessage("d-trigger", g.ID, "topic", base)
	insertPreparationHistoryMessages(t, s, want[3],
		preparationHistoryMessage("0-later", g.ID, "topic", base.Add(time.Nanosecond)),
		want[0], trigger, want[2], preparationHistoryMessage("e-tied", g.ID, "topic", base), want[1])

	for _, tc := range []struct {
		name  string
		limit int
		want  []domain.Message
	}{
		{"all strictly earlier", 200, want},
		{"newest suffix chronological", 2, want[2:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListPreparationHistory(ctx, g.ID, trigger.ID, tc.limit)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("exclusive tuple or deterministic suffix order: %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestListPreparationHistoryLimits(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	history := make([]domain.Message, 201)
	for n := range history {
		history[n] = preparationHistoryMessage(fmt.Sprintf("history-%03d", n), g.ID, "", base.Add(time.Duration(n)*time.Nanosecond))
	}
	trigger := preparationHistoryMessage("trigger", g.ID, "", base.Add(time.Second))
	insertPreparationHistoryMessages(t, s, append(history, trigger)...)

	for _, tc := range []struct {
		name  string
		limit int
		want  []domain.Message
	}{
		{"zero defaults to fifty", 0, history[151:]},
		{"explicit fifty", 50, history[151:]},
		{"minimum", 1, history[200:]},
		{"maximum", 200, history[1:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListPreparationHistory(ctx, g.ID, trigger.ID, tc.limit)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("limit %d: got %d messages, %v; want %d", tc.limit, len(got), err, len(tc.want))
			}
		})
	}
	for _, limit := range []int{-1, 201} {
		got, err := s.ListPreparationHistory(ctx, g.ID, trigger.ID, limit)
		if got != nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid limit %d: %+v, %v", limit, got, err)
		}
	}
}

func TestListPreparationHistoryEmptyAndCancellation(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	trigger := preparationHistoryMessage("trigger", g.ID, "", base)
	insertPreparationHistoryMessages(t, s, trigger,
		preparationHistoryMessage("other-topic", g.ID, "thread", base.Add(-time.Second)))
	t.Run("empty", func(t *testing.T) {
		got, err := s.ListPreparationHistory(ctx, g.ID, trigger.ID, 0)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("valid main-topic trigger must return a nonnil empty slice: %+v, %v", got, err)
		}
	})

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	expired, expire := context.WithDeadline(ctx, time.Unix(1, 0))
	defer expire()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"cancelled", cancelled, context.Canceled},
		{"expired", expired, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListPreparationHistory(tc.ctx, g.ID, trigger.ID, 50)
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("context error category lost: %+v, %v", got, err)
			}
		})
	}
}

func TestListPreparationHistoryDoesNotReadDeliveries(t *testing.T) {
	f := setupMessages(t)
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	history := preparationHistoryMessage("history", f.groupID, "topic", base)
	trigger := preparationHistoryMessage("trigger", f.groupID, "topic", base.Add(time.Second))
	insertPreparationHistoryMessages(t, f.s, history, trigger)
	c, err := f.s.GetOrCreateConversation(ctx, f.groupID, f.a, "topic")
	if err != nil {
		t.Fatal(err)
	}
	// Malformed delivery metadata makes any accidental delivery load fail.
	for _, m := range []domain.Message{history, trigger} {
		if _, err := f.s.db.Exec(`INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
		 VALUES(?,?,?,?,?,?,'PRIVATE-DELIVERY-STAMP')`, "d-"+m.ID, m.ID, c.ID, f.a, f.groupID, m.TopicID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.s.ListPreparationHistory(ctx, f.groupID, trigger.ID, 50)
	if err != nil || !reflect.DeepEqual(got, []domain.Message{history}) {
		t.Fatalf("history must return message rows with empty deliveries: %+v, %v", got, err)
	}
}

func TestListPreparationHistoryErrorsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		args        []any
	}{
		{"trigger decoding", "UPDATE messages SET created_at=? WHERE id='trigger'", []any{"0000-PRIVATE-TRIGGER-PAYLOAD"}},
		{"history decoding", "UPDATE messages SET created_at=? WHERE id='history'", []any{"0000-PRIVATE-HISTORY-PAYLOAD"}},
		{"SQL failure", "ALTER TABLE messages RENAME COLUMN content TO private_history_payload", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			g := group(t, s)
			base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			insertPreparationHistoryMessages(t, s,
				preparationHistoryMessage("history", g.ID, "topic", base),
				preparationHistoryMessage("valid-newer", g.ID, "topic", base.Add(time.Nanosecond)),
				preparationHistoryMessage("trigger", g.ID, "topic", base.Add(time.Second)))
			if _, err := s.db.Exec(tc.query, tc.args...); err != nil {
				t.Fatal(err)
			}
			got, err := s.ListPreparationHistory(ctx, g.ID, "trigger", 50)
			if got != nil || err == nil || err.Error() != "读取准备历史失败" {
				t.Fatalf("unexpected errors must discard partial results and hide SQL/payloads: %+v, %v", got, err)
			}
		})
	}
}
