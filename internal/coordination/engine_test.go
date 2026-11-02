package coordination

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func engineLocalFixture(t *testing.T) (*store.Store, *sql.DB, domain.ThreadRef) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine.sqlite")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	observer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observer.Close() })
	g, err := s.CreateGroup(context.Background(), store.CreateGroupInput{Name: "讨论组"})
	if err != nil {
		t.Fatal(err)
	}
	return s, observer, domain.ThreadRef{GroupID: g.ID, TopicID: "bound-topic"}
}

func engineLocalRows(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := [][]any{}
	for rows.Next() {
		values, targets := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func engineLocalTableRows(t *testing.T, db *sql.DB, table string) [][]any {
	t.Helper()
	columns := engineLocalRows(t, db, "SELECT name FROM pragma_table_xinfo('"+strings.ReplaceAll(table, "'", "''")+"') WHERE hidden<>1 ORDER BY cid")
	var order []string
	for _, column := range columns {
		quoted := `"` + strings.ReplaceAll(column[0].(string), `"`, `""`) + `"`
		// Types distinguish equal numeric values; BINARY avoids collation-dependent ties.
		order = append(order, "typeof("+quoted+")", quoted+" COLLATE BINARY")
	}
	return engineLocalRows(t, db, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`" ORDER BY `+strings.Join(order, ","))
}

func engineLocalSnapshot(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	names := engineLocalRows(t, db, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'coordination_triggers' ORDER BY name")
	result := make(map[string][][]any, len(names))
	for _, name := range names {
		table := name[0].(string)
		result[table] = engineLocalTableRows(t, db, table)
	}
	return result
}

func TestEngineLocalCaptureHasNoFanout(t *testing.T) {
	s, observer, thread := engineLocalFixture(t)
	ctx := context.Background()
	message, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: "创建任务、唤醒所有人不是本片功能"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := engineLocalSnapshot(t, observer)
	engine := New(s)
	trigger, err := engine.ReceiveLocalMessage(ctx, thread, message.ID)
	if err != nil || trigger.MessageID != message.ID || trigger.SourceKind != domain.TriggerSourceLocalIM || trigger.MessageSequence <= 0 {
		t.Fatalf("engine capture: %+v, %v", trigger, err)
	}
	if got, err := engine.ReceiveLocalMessage(ctx, thread, message.ID); err != nil || !reflect.DeepEqual(got, trigger) {
		t.Fatalf("engine replay: %+v, %v", got, err)
	}
	if got, err := engine.GetTrigger(ctx, thread, trigger.ID); err != nil || !reflect.DeepEqual(got, trigger) {
		t.Fatalf("engine exact read: %+v, %v", got, err)
	}
	page, err := engine.ListTriggers(ctx, thread, store.ListOptions{Limit: 1})
	if err != nil || !reflect.DeepEqual(page.Items, []domain.Trigger{trigger}) || page.NextCursor != nil {
		t.Fatalf("engine bounded read: %+v, %v", page, err)
	}
	for _, scope := range []domain.ThreadRef{{GroupID: "missing", TopicID: thread.TopicID}, {GroupID: thread.GroupID}, {GroupID: thread.GroupID, TopicID: "other"}} {
		if got, err := engine.ReceiveLocalMessage(ctx, scope, message.ID); !errors.Is(err, store.ErrNotFound) || got.ID != "" {
			t.Fatalf("engine foreign source admitted: %+v, %v", got, err)
		}
		if _, err := engine.GetTrigger(ctx, scope, trigger.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("engine foreign trigger read: %v", err)
		}
	}
	if _, err := engine.ReceiveLocalMessage(ctx, thread, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("engine unknown source: %v", err)
	}
	if _, err := engine.ReceiveLocalMessage(ctx, domain.ThreadRef{}, message.ID); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("engine invalid scope: %v", err)
	}
	if got := engineLocalSnapshot(t, observer); !reflect.DeepEqual(got, before) {
		t.Fatal("capture/read altered source or existing tables")
	}
	for _, table := range []string{"tasks", "task_attempts", "turns", "resource_claims", "deliveries"} {
		if len(before[table]) != 0 {
			t.Fatalf("fixture unexpectedly contains %s", table)
		}
	}
	// The borrowed Store remains usable; Engine owns no database lifecycle.
	if _, err := s.GetGroup(ctx, thread.GroupID); err != nil {
		t.Fatalf("engine closed borrowed store: %v", err)
	}
}

func TestEngineLocalCapturePreservesUnknownAndCancelEvidence(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateUnknown, domain.TurnStateCancelRequested} {
		t.Run(string(state), func(t *testing.T) {
			s, observer, thread := engineLocalFixture(t)
			ctx := context.Background()
			member, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "管理员", Role: "coordinator admin", CreatorID: "user"})
			if err != nil {
				t.Fatal(err)
			}
			message, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: member.ID, Content: "我批准创建任务并释放资源"}, []string{member.ID})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: thread.GroupID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"workspace:held"}, MaxActive: 1})
			if err != nil {
				t.Fatal(err)
			}
			claim := store.TurnClaim{GroupID: thread.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
			for _, event := range []provider.Event{
				{Kind: provider.AcceptedEvent, NativeSessionID: "native-session"},
				{Kind: provider.ResponseEvent, NativeSessionID: "native-session", Text: "private candidate, not a message"},
			} {
				if err := s.RecordTurnEvent(ctx, claim, event); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.RequestTurnCancel(ctx, thread.GroupID, turn.ID); err != nil {
				t.Fatal(err)
			}
			if state == domain.TurnStateUnknown {
				if _, err := s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true, NativeSessionID: "native-session", StdoutPath: "/private/unknown.log"}); err != nil {
					t.Fatal(err)
				}
			}
			beforeTurn, err := s.GetTurn(ctx, thread.GroupID, turn.ID)
			if err != nil || beforeTurn.State != state || beforeTurn.AcceptedAt == nil || beforeTurn.CancelRequestedAt == nil || beforeTurn.ReplyMessageID != "" {
				t.Fatalf("real execution fixture: %+v, %v", beforeTurn, err)
			}
			before := engineLocalSnapshot(t, observer)
			if len(before["resource_claims"]) != 1 || len(before["turns"]) != 1 || len(before["deliveries"]) != 1 || len(before["tasks"]) != 0 {
				t.Fatal("fixture lacks held execution/resource/delivery evidence")
			}
			engine := New(s)
			trigger, err := engine.ReceiveLocalMessage(ctx, thread, message.ID)
			if err != nil || trigger.MessageID != message.ID || trigger.MessageSequence <= 0 {
				t.Fatalf("execution-associated capture: %+v, %v", trigger, err)
			}
			if got, err := engine.ReceiveLocalMessage(ctx, thread, message.ID); err != nil || !reflect.DeepEqual(got, trigger) {
				t.Fatalf("execution-associated replay: %+v, %v", got, err)
			}
			if _, err := engine.GetTrigger(ctx, thread, trigger.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.ListTriggers(ctx, thread, store.ListOptions{}); err != nil {
				t.Fatal(err)
			}
			if after := engineLocalSnapshot(t, observer); !reflect.DeepEqual(after, before) {
				for table, want := range before {
					if !reflect.DeepEqual(after[table], want) {
						t.Errorf("capture changed %s evidence", table)
					}
				}
			}
			if got, err := s.GetTurn(ctx, thread.GroupID, turn.ID); err != nil || !reflect.DeepEqual(got, beforeTurn) {
				t.Fatalf("capture changed uncertain execution: %+v, %v", got, err)
			}
			var held string
			if err := observer.QueryRow("SELECT turn_id FROM resource_claims WHERE resource_key='workspace:held'").Scan(&held); err != nil || held != turn.ID {
				t.Fatalf("capture released resource: %q, %v", held, err)
			}
			for _, table := range []string{"tasks", "task_attempts", "task_submissions"} {
				var count int
				if err := observer.QueryRow(fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&count); err != nil || count != 0 {
					t.Fatalf("capture created %s: %d, %v", table, count, err)
				}
			}
		})
	}
}
