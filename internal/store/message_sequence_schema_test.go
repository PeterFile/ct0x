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

type messageSequenceEntry struct {
	Sequence                    int64
	MessageID, GroupID, TopicID string
}

type messageSequenceQuerier interface {
	Query(string, ...any) (*sql.Rows, error)
}

func messageSequenceLog(t *testing.T, db messageSequenceQuerier) []messageSequenceEntry {
	t.Helper()
	rows, err := db.Query("SELECT sequence,message_id,group_id,topic_id FROM message_log ORDER BY sequence")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	entries := []messageSequenceEntry{}
	var previous int64
	for rows.Next() {
		var entry messageSequenceEntry
		if err := rows.Scan(&entry.Sequence, &entry.MessageID, &entry.GroupID, &entry.TopicID); err != nil {
			t.Fatal(err)
		}
		if entry.Sequence <= previous {
			t.Fatalf("positions must be positive and strictly increasing: %d after %d", entry.Sequence, previous)
		}
		previous = entry.Sequence
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

func messageSequenceCoverage(t *testing.T, s *Store) {
	t.Helper()
	var missing, extra int
	err := s.db.QueryRow(`SELECT
	 (SELECT count(*) FROM messages m LEFT JOIN message_log l
	  ON l.message_id=m.id AND l.group_id=m.group_id AND l.topic_id=m.topic_id WHERE l.sequence IS NULL),
	 (SELECT count(*) FROM message_log l LEFT JOIN messages m
	  ON m.id=l.message_id AND m.group_id=l.group_id AND m.topic_id=l.topic_id WHERE m.id IS NULL)`).Scan(&missing, &extra)
	if err != nil || missing != 0 || extra != 0 {
		t.Fatalf("message/log coverage: missing=%d extra=%d, %v", missing, extra, err)
	}
	taskArtifactIntegrity(t, s)
}

func TestMessageSequenceSchemaAndIndexes(t *testing.T) {
	if len(migrations) < 8 || !strings.Contains(migrations[7], "CREATE TABLE message_log") {
		t.Fatal("v8 message append log migration is missing or misplaced")
	}
	s, _ := openTestStore(t)
	var tableSQL string
	if err := s.db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='message_log'").Scan(&tableSQL); err != nil || !strings.Contains(tableSQL, "INTEGER PRIMARY KEY AUTOINCREMENT") {
		t.Fatalf("durable sequence must use AUTOINCREMENT: %q, %v", tableSQL, err)
	}
	for index, want := range map[string][]string{
		"message_log_group_sequence": {"group_id", "sequence"},
		"message_log_topic_sequence": {"group_id", "topic_id", "sequence"},
	} {
		rows, err := s.db.Query("PRAGMA index_xinfo(" + index + ")")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var seq, cid, desc, key int
			var name, collation sql.NullString
			if err := rows.Scan(&seq, &cid, &name, &desc, &collation, &key); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if key == 1 {
				if desc != 0 {
					t.Errorf("%s must be ascending", index)
				}
				got = append(got, name.String)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		if !slices.Equal(got, want) {
			t.Fatalf("index %s: %v; want %v", index, got, want)
		}
	}
	rows, err := s.db.Query("PRAGMA foreign_key_list(message_log)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var bindings []string
	for rows.Next() {
		var id, seq int
		var table, from, to, update, delete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &update, &delete, &match); err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, fmt.Sprintf("%d:%d:%s:%s:%s", id, seq, table, from, to))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bindings, []string{"0:0:messages:message_id:id", "0:1:messages:group_id:group_id", "0:2:messages:topic_id:topic_id"}) {
		t.Fatalf("composite message binding: %v", bindings)
	}
}

func TestMessageSequenceDatabaseGuards(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursive), func(t *testing.T) {
			f := setupMessages(t)
			if _, err := f.s.db.Exec(fmt.Sprintf("PRAGMA recursive_triggers=%d", recursive)); err != nil {
				t.Fatal(err)
			}
			var enabled int
			if err := f.s.db.QueryRow("PRAGMA recursive_triggers").Scan(&enabled); err != nil || enabled != recursive {
				t.Fatalf("recursive trigger fixture: %d, %v", enabled, err)
			}
			message, err := f.s.CreateMessage(ctx, f.request(), nil)
			if err != nil {
				t.Fatal(err)
			}
			before := messageSequenceLog(t, f.s.db)
			reject := func(query string, args ...any) {
				t.Helper()
				if _, err := f.s.db.Exec(query, args...); err == nil {
					t.Fatalf("immutable history accepted: %s", query)
				}
			}
			for _, change := range []struct{ column, value string }{
				{"id", "renamed"}, {"group_id", f.otherID}, {"topic_id", "different"},
				{"sender_id", f.b}, {"content", "replacement"},
			} {
				reject("UPDATE messages SET "+change.column+"=? WHERE id=?", change.value, message.ID)
			}
			reject("DELETE FROM messages WHERE id=?", message.ID)
			// Check the explicit BEFORE guard, not a later FK/log failure after REPLACE's implicit delete.
			if _, err := f.s.db.Exec(`INSERT OR REPLACE INTO messages(`+messageColumns+`)
			 SELECT id,group_id,topic_id,sender_id,'replacement',created_at FROM messages WHERE id=?`, message.ID); err == nil || !strings.Contains(err.Error(), "消息身份不可替换") {
				t.Fatalf("message duplicate BEFORE guard did not reject REPLACE: %v", err)
			}
			for _, change := range []string{
				"sequence=sequence+100", "message_id='missing'", "group_id='different'", "topic_id='different'", "sequence=sequence",
			} {
				reject("UPDATE message_log SET " + change)
			}
			reject("DELETE FROM message_log")
			reject("INSERT INTO message_log(message_id,group_id,topic_id) SELECT message_id,group_id,topic_id FROM message_log")
			reject("INSERT OR REPLACE INTO message_log SELECT * FROM message_log")
			reject("INSERT OR REPLACE INTO message_log(sequence,message_id,group_id,topic_id) SELECT sequence+100,message_id,group_id,topic_id FROM message_log")
			if got, err := f.s.GetMessage(ctx, f.groupID, message.ID); err != nil || !reflect.DeepEqual(got, message) {
				t.Fatalf("rejected mutation changed the source: %+v, %v", got, err)
			}
			stamp := message.CreatedAt.Add(-time.Hour)
			if _, err := f.s.db.Exec("UPDATE messages SET created_at=? WHERE id=?", stamp.Format(timestampLayout), message.ID); err != nil {
				t.Fatalf("display timestamp must remain mutable: %v", err)
			}
			if got, err := f.s.GetMessage(ctx, f.groupID, message.ID); err != nil || !got.CreatedAt.Equal(stamp) {
				t.Fatalf("display timestamp did not change: %+v, %v", got, err)
			}
			if got := messageSequenceLog(t, f.s.db); !reflect.DeepEqual(got, before) {
				t.Fatalf("mutation/timestamp edit rewrote positions: %+v; want %+v", got, before)
			}

			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			// Isolate the FK/CHECK constraints using an unlogged parent only inside this rolled-back fixture.
			if _, err := tx.Exec("DROP TRIGGER messages_append_log"); err != nil {
				t.Fatal(err)
			}
			raw := domain.Message{ID: "unlogged-parent", GroupID: f.groupID, TopicID: "bound", SenderID: f.a, Content: "fixture", CreatedAt: stamp}
			if err := insertMessageRow(ctx, tx, raw); err != nil {
				t.Fatal(err)
			}
			insert := "INSERT INTO message_log(sequence,message_id,group_id,topic_id) VALUES(?,?,?,?)"
			for _, args := range [][]any{
				{0, raw.ID, raw.GroupID, raw.TopicID}, {-1, raw.ID, raw.GroupID, raw.TopicID},
				{1.5, raw.ID, raw.GroupID, raw.TopicID}, {nil, "missing", raw.GroupID, raw.TopicID},
				{nil, raw.ID, f.otherID, raw.TopicID}, {nil, raw.ID, raw.GroupID, "wrong-topic"},
				{nil, nil, raw.GroupID, raw.TopicID}, {nil, raw.ID, nil, raw.TopicID}, {nil, raw.ID, raw.GroupID, nil},
			} {
				if _, err := tx.Exec(insert, args...); err == nil {
					t.Fatalf("invalid sequence/message/scope accepted: %v", args)
				}
			}
			if _, err := tx.Exec(strings.Replace(insert, "INSERT INTO", "INSERT OR REPLACE INTO", 1), before[0].Sequence, raw.ID, raw.GroupID, raw.TopicID); err == nil || !strings.Contains(err.Error(), "消息序号映射不可替换") {
				t.Fatalf("sequence-only duplicate BEFORE guard did not reject REPLACE: %v", err)
			}
			if _, err := tx.Exec(insert, before[0].Sequence+100, raw.ID, raw.GroupID, raw.TopicID); err != nil {
				t.Fatalf("valid positive, nonconsecutive position rejected: %v", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if got := messageSequenceLog(t, f.s.db); !reflect.DeepEqual(got, before) {
				t.Fatalf("rolled-back constraint fixture changed positions: %+v", got)
			}
			messageSequenceCoverage(t, f.s)
		})
	}
}
