package store

import (
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestCoordinationTriggerSchemaBindingsAndIndex(t *testing.T) {
	if len(migrations) < 9 || migrations[8] != coordinationTriggerSchemaV9 {
		t.Fatal("v9 trigger migration is missing or misplaced")
	}
	s, _ := openTestStore(t)
	rows, err := s.db.Query("PRAGMA foreign_key_list(coordination_triggers)")
	if err != nil {
		t.Fatal(err)
	}
	var bindings []string
	for rows.Next() {
		var id, seq int
		var table, from, to, update, delete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &update, &delete, &match); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		bindings = append(bindings, fmt.Sprintf("%d:%s:%s:%s", seq, table, from, to))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	slices.Sort(bindings)
	want := []string{"0:message_log:message_sequence:sequence", "0:messages:message_id:id", "1:messages:group_id:group_id", "2:messages:topic_id:topic_id"}
	if !slices.Equal(bindings, want) {
		t.Fatalf("trigger foreign keys: %v; want %v", bindings, want)
	}
	rows, err = s.db.Query("PRAGMA index_xinfo(coordination_triggers_thread_created_id)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var seq, cid, desc, key int
		var name, collation sql.NullString
		if err := rows.Scan(&seq, &cid, &name, &desc, &collation, &key); err != nil {
			t.Fatal(err)
		}
		if key == 1 {
			columns = append(columns, name.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(columns, []string{"group_id", "topic_id", "created_at", "id"}) {
		t.Fatalf("bounded scope index: %v", columns)
	}
}

func TestCoordinationTriggerImmutableSQLGuards(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursive), func(t *testing.T) {
			s, _ := openTestStore(t)
			thread := domain.ThreadRef{GroupID: group(t, s).ID, TopicID: "bound"}
			first := coordinationTriggerMessage(t, s, thread)
			second := coordinationTriggerMessage(t, s, thread)
			third := coordinationTriggerMessage(t, s, thread)
			trigger, err := s.RecordLocalMessageTrigger(ctx, thread, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			var secondSequence int64
			if err := s.db.QueryRow("SELECT sequence FROM message_log WHERE message_id=?", second.ID).Scan(&secondSequence); err != nil {
				t.Fatal(err)
			}
			var thirdSequence int64
			if err := s.db.QueryRow("SELECT sequence FROM message_log WHERE message_id=?", third.ID).Scan(&thirdSequence); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(fmt.Sprintf("PRAGMA recursive_triggers=%d", recursive)); err != nil {
				t.Fatal(err)
			}
			var enabled int
			if err := s.db.QueryRow("PRAGMA recursive_triggers").Scan(&enabled); err != nil || enabled != recursive {
				t.Fatalf("recursive trigger fixture: %d, %v", enabled, err)
			}
			before := messageSequenceSnapshot(t, s)
			reject := func(query string, args ...any) {
				t.Helper()
				if _, err := s.db.Exec(query, args...); err == nil {
					t.Fatalf("immutable trigger accepted SQL: %s", query)
				}
			}
			for _, change := range []string{
				"id='renamed'", "source_kind='remote'", "group_id='foreign'", "topic_id=''", "message_id='missing'",
				"message_sequence=message_sequence+1", "created_at='2000-01-01'", "id=id",
			} {
				reject("UPDATE coordination_triggers SET " + change + " WHERE id='" + trigger.ID + "'")
			}
			reject("DELETE FROM coordination_triggers WHERE id=?", trigger.ID)
			insert := `INSERT INTO coordination_triggers(id,source_kind,group_id,topic_id,message_id,message_sequence,created_at) VALUES(?,?,?,?,?,?,?)`
			stamp := time.Now().UTC().Format(timestampLayout)
			// Both FKs individually resolve, but the sequence points to another valid source row.
			if _, err := s.db.Exec(insert, "forged", "local_im", thread.GroupID, thread.TopicID, second.ID, thirdSequence, stamp); err == nil || !strings.Contains(err.Error(), "消息序号与来源不匹配") {
				t.Fatalf("independent FKs admitted a forged mapping: %v", err)
			}
			for _, args := range [][]any{
				{"unknown-source", "remote", thread.GroupID, thread.TopicID, second.ID, secondSequence, stamp},
				{"missing-message", "local_im", thread.GroupID, thread.TopicID, "missing", secondSequence, stamp},
				{"wrong-group", "local_im", "missing", thread.TopicID, second.ID, secondSequence, stamp},
				{"wrong-topic", "local_im", thread.GroupID, "", second.ID, secondSequence, stamp},
				{"zero", "local_im", thread.GroupID, thread.TopicID, second.ID, 0, stamp},
				{"negative", "local_im", thread.GroupID, thread.TopicID, second.ID, -1, stamp},
				{"fractional", "local_im", thread.GroupID, thread.TopicID, second.ID, 1.5, stamp},
				{"", "local_im", thread.GroupID, thread.TopicID, second.ID, secondSequence, stamp},
				{nil, "local_im", thread.GroupID, thread.TopicID, second.ID, secondSequence, stamp},
				{"null-source", nil, thread.GroupID, thread.TopicID, second.ID, secondSequence, stamp},
				{"null-topic", "local_im", thread.GroupID, nil, second.ID, secondSequence, stamp},
				{"null-created", "local_im", thread.GroupID, thread.TopicID, second.ID, secondSequence, nil},
			} {
				reject(insert, args...)
			}
			replace := strings.Replace(insert, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
			for _, args := range [][]any{
				{trigger.ID, "local_im", thread.GroupID, thread.TopicID, second.ID, secondSequence, stamp}, // PK only.
				{"new-id", "local_im", thread.GroupID, thread.TopicID, first.ID, trigger.MessageSequence, stamp},
				{"new-sequence-id", "local_im", thread.GroupID, thread.TopicID, second.ID, trigger.MessageSequence, stamp},
			} {
				if _, err := s.db.Exec(replace, args...); err == nil || !strings.Contains(err.Error(), "协调触发记录不可替换") {
					t.Fatalf("BEFORE INSERT guard did not block REPLACE: %v", err)
				}
			}
			// An exact duplicate and a no-op UPSERT must also be rejected, not silently rewritten.
			reject(insert, trigger.ID, "local_im", thread.GroupID, thread.TopicID, first.ID, trigger.MessageSequence, stamp)
			reject(insert+" ON CONFLICT(id) DO UPDATE SET id=id", trigger.ID, "local_im", thread.GroupID, thread.TopicID, first.ID, trigger.MessageSequence, stamp)
			messageSequencePreserved(t, s, before)
			if got, err := s.GetTrigger(ctx, thread, trigger.ID); err != nil || !reflect.DeepEqual(got, trigger) {
				t.Fatalf("rejected mutation changed capture: %+v, %v", got, err)
			}
			coordinationTriggerCount(t, s, 1)
		})
	}
}
