package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func messageSequenceV7Fixture(t *testing.T) (*Store, string, func(*Store)) {
	t.Helper()
	s, path := taskArtifactV6Fixture(t)
	if _, err := s.db.Exec(migrations[6] + "INSERT INTO schema_migrations VALUES(7);"); err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(ctx, keyedGroup())
	if err != nil {
		t.Fatal(err)
	}
	m := member(t, s, g.ID)
	request := MessageRequest{GroupID: g.ID, ActorID: "user", TopicID: "legacy", Content: "legacy input", IdempotencyKey: "legacy-message"}
	message, err := s.CreateMessage(ctx, request, []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: g.ID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"v7-held"}, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	claim := TurnClaim{GroupID: g.ID, TurnID: turn.ID, Token: turn.ClaimToken}
	queueCandidate(t, s, claim, "private legacy candidate", provider.OutcomeSucceeded)
	if _, err := s.RequestTurnCancel(ctx, g.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	receipt := queueReceipt(provider.OutcomeSucceeded)
	receipt.Outcome, receipt.OutputComplete = provider.OutcomeUnknown, false
	if _, err := s.ApplyTurnReceipt(ctx, claim, receipt); err != nil {
		t.Fatal(err)
	}
	beforeTurn := queueTurn(t, s, claim)
	beforeReceipt := queueReceiptJSON(t, s, claim)
	beforeMessage, err := s.GetMessage(ctx, g.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := createTestTask(t, s, g.ID)
	activate := taskAttemptInput(task)
	activate.IdempotencyKey = "legacy-attempt"
	// Historical v7 construction; keep the later public keyed replay unchanged.
	attempt, err := dispatchHistoricalAttempt(t, s, activate)
	if err != nil {
		t.Fatal(err)
	}
	publish := artifactPublishInput(g.ID)
	publish.SourceTaskID, publish.IdempotencyKey = task.ID, "legacy-artifact"
	version := mustPublishArtifact(t, s, publish)
	submit := taskSubmissionInput(attempt)
	submit.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}
	submission, err := s.SubmitTask(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	reviewInput := taskReviewInput(submission)
	reviewInput.IdempotencyKey = "legacy-review"
	review, err := s.ReviewTask(ctx, reviewInput)
	if err != nil {
		t.Fatal(err)
	}
	beforeTask, err := s.GetTask(ctx, g.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	check := func(current *Store) {
		t.Helper()
		for _, read := range []struct {
			name string
			want any
			call func() (any, error)
		}{
			{"group receipt", g, func() (any, error) { return current.CreateGroup(ctx, keyedGroup()) }},
			{"message", beforeMessage, func() (any, error) { return current.GetMessage(ctx, g.ID, message.ID) }},
			{"message replay", beforeMessage, func() (any, error) { return current.CreateMessage(ctx, request, nil) }},
			{"unknown native turn", beforeTurn, func() (any, error) { return current.GetTurn(ctx, g.ID, turn.ID) }},
			{"task state", beforeTask, func() (any, error) { return current.GetTask(ctx, g.ID, task.ID) }},
			{"attempt replay", attempt, func() (any, error) { return current.ActivateTaskAttempt(ctx, activate) }},
			{"v7 submission refs/replay", submission, func() (any, error) { return current.SubmitTask(ctx, submit) }},
			{"v7 review refs/replay", review, func() (any, error) { return current.ReviewTask(ctx, reviewInput) }},
			{"artifact replay", version, func() (any, error) { return current.PublishArtifact(ctx, publish) }},
		} {
			if got, err := read.call(); err != nil || !reflect.DeepEqual(got, read.want) {
				t.Fatalf("migration changed %s: %+v, %v; want %+v", read.name, got, err, read.want)
			}
		}
		if got, found, err := current.LookupMessageReceipt(ctx, request); err != nil || !found || !reflect.DeepEqual(got, beforeMessage) {
			t.Fatalf("migration changed message receipt: %+v, %t, %v", got, found, err)
		}
		var held string
		if err := current.db.QueryRow("SELECT turn_id FROM resource_claims WHERE resource_key='v7-held'").Scan(&held); err != nil || held != turn.ID {
			t.Fatalf("migration released unknown execution's resource: %q, %v", held, err)
		}
		if got := queueReceiptJSON(t, current, claim); got != beforeReceipt {
			t.Fatalf("migration changed native receipt evidence: %s; want %s", got, beforeReceipt)
		}
	}
	return s, path, check
}

func messageSequenceRows(t *testing.T, db *sql.DB, query string) [][]any {
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
	values := [][]any{}
	for rows.Next() {
		row, dest := make([]any, len(columns)), make([]any, len(columns))
		for i := range row {
			dest[i] = &row[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		values = append(values, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func messageSequenceTableRows(t *testing.T, s *Store, table string) [][]any {
	t.Helper()
	columns := messageSequenceRows(t, s.db, "SELECT name FROM pragma_table_xinfo('"+strings.ReplaceAll(table, "'", "''")+"') WHERE hidden<>1 ORDER BY cid")
	var order []string
	for _, column := range columns {
		quoted := `"` + strings.ReplaceAll(column[0].(string), `"`, `""`) + `"`
		// Types distinguish equal numeric values; BINARY avoids collation-dependent ties.
		order = append(order, "typeof("+quoted+")", quoted+" COLLATE BINARY")
	}
	return messageSequenceRows(t, s.db, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`" ORDER BY `+strings.Join(order, ","))
}

func messageSequenceSnapshot(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	names := messageSequenceRows(t, s.db, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'schema_migrations' ORDER BY name")
	snapshot := make(map[string][][]any, len(names))
	for _, name := range names {
		table := name[0].(string)
		snapshot[table] = messageSequenceTableRows(t, s, table)
	}
	return snapshot
}

func messageSequencePreserved(t *testing.T, s *Store, snapshot map[string][][]any) {
	t.Helper()
	for table, want := range snapshot {
		got := messageSequenceTableRows(t, s, table)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("migration changed original %s rows: %+v; want %+v", table, got, want)
		}
	}
}

func messageSequenceLedger(t *testing.T, s *Store, want int) {
	t.Helper()
	var latest, count, v8 int
	err := s.db.QueryRow("SELECT max(version),count(*),count(CASE WHEN version=8 THEN 1 END) FROM schema_migrations").Scan(&latest, &count, &v8)
	wantV8 := 0
	if want >= 8 {
		wantV8 = 1
	}
	if err != nil || latest != want || count != want || v8 != wantV8 {
		t.Fatalf("migration ledger: latest=%d count=%d v8=%d, %v; want %d/%d/%d", latest, count, v8, err, want, want, wantV8)
	}
}

func TestMessageSequenceV7BackfillAndReopen(t *testing.T) {
	old, path, check := messageSequenceV7Fixture(t)
	var originalID string
	if err := old.db.QueryRow("SELECT id FROM messages").Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	g := group(t, old)
	tied := make([]string, 6)
	for i := range tied {
		tied[i] = rand.Text()
	}
	slices.Sort(tied)
	stamp := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	// Reverse sorted random IDs ensure insertion order disagrees with the bootstrap tie-breaker.
	for i := len(tied) - 1; i >= 0; i-- {
		if _, err := old.db.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at)
		 VALUES(?,?,'tie','user','legacy tie',?)`, tied[i], g.ID, stamp.Format(timestampLayout)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := messageSequenceSnapshot(t, old)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	var committed []messageSequenceEntry
	for reopen := range 2 {
		s := queueHandle(t, path, false)
		messageSequenceLedger(t, s, len(migrations))
		check(s)
		entries := messageSequenceLog(t, s.db)
		if reopen == 0 {
			messageSequencePreserved(t, s, snapshot)
			want := append(slices.Clone(tied), originalID)
			var got []string
			for _, entry := range entries {
				got = append(got, entry.MessageID)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("bootstrap must order by created_at,id: %v; want %v", got, want)
			}
			m := member(t, s, g.ID)
			// This frozen v7 has no member-join write path; valid direct SQL proves generic trigger coverage.
			if _, err := s.db.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at)
			 VALUES('!later-earlier',?,'join',?,'成员加入',?)`, g.ID, m.ID, stamp.Add(-time.Hour).Format(timestampLayout)); err != nil {
				t.Fatal(err)
			}
			committed = messageSequenceLog(t, s.db)
			last := committed[len(committed)-1]
			if len(committed) != len(entries)+1 || !reflect.DeepEqual(committed[:len(entries)], entries) || last.MessageID != "!later-earlier" || last.GroupID != g.ID || last.TopicID != "join" || last.Sequence <= entries[len(entries)-1].Sequence {
				t.Fatalf("later insert followed its display timestamp/ID instead of append order: %+v", committed)
			}
		} else if !reflect.DeepEqual(entries, committed) {
			t.Fatalf("reopen rewrote backfill/append positions: %+v; want %+v", entries, committed)
		}
		cursor := &Cursor{CreatedAt: stamp, ID: tied[0]}
		page, err := s.ListMessages(ctx, g.ID, MessageListOptions{After: cursor})
		var ids []string
		for _, message := range page.Items {
			ids = append(ids, message.ID)
		}
		if err != nil || !slices.Equal(ids, tied[1:]) {
			t.Fatalf("existing timestamp/ID after cursor changed: %v, %v", ids, err)
		}
		page, err = s.ListMessages(ctx, g.ID, MessageListOptions{Before: cursor})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != "!later-earlier" {
			t.Fatalf("existing timestamp/ID before cursor changed: %+v, %v", page, err)
		}
		messageSequenceCoverage(t, s)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMessageSequenceV8MigrationFailureRollsBackAndRetries(t *testing.T) {
	for _, failure := range []string{"late-ddl", "backfill"} {
		t.Run(failure, func(t *testing.T) {
			old, path, check := messageSequenceV7Fixture(t)
			g := group(t, old)
			if _, err := old.CreateMessage(ctx, MessageRequest{GroupID: g.ID, ActorID: "user", Content: "second backfill row"}, nil); err != nil {
				t.Fatal(err)
			}
			if failure == "late-ddl" {
				// The final v8 index runs after the table, populated backfill and all guards.
				if _, err := old.db.Exec("CREATE TABLE message_log_topic_sequence(collision TEXT); INSERT INTO message_log_topic_sequence VALUES('preserved');"); err != nil {
					t.Fatal(err)
				}
			} else {
				// Permit one backfill insert, then exhaust AUTOINCREMENT on the second.
				// This injects a real backfill failure without rewriting the parent schema or global migrations.
				if _, err := old.db.Exec(`CREATE TABLE sequence_exhaustion_fixture(id INTEGER PRIMARY KEY AUTOINCREMENT);
				 INSERT INTO sqlite_sequence(name,seq) VALUES('message_log',9223372036854775806);`); err != nil {
					t.Fatal(err)
				}
			}
			schema := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
			snapshot := messageSequenceSnapshot(t, old)
			if err := migrate(old.db); err == nil || !strings.Contains(err.Error(), "迁移版本 8") {
				t.Fatalf("v8 %s failure was not exercised: %v", failure, err)
			} else {
				t.Logf("injected %s migration failure: %v", failure, err)
				if failure == "backfill" {
					var sqliteErr *sqlite.Error
					if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_FULL {
						t.Fatalf("failure did not reach AUTOINCREMENT backfill: %v", err)
					}
				}
			}
			messageSequenceLedger(t, old, 7)
			if got := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name"); !reflect.DeepEqual(got, schema) {
				t.Fatalf("failed v8 left DDL/backfill guards or changed original objects: %+v; want %+v", got, schema)
			}
			messageSequencePreserved(t, old, snapshot)
			check(old)
			if failure == "late-ddl" {
				if _, err := old.db.Exec("DROP TABLE message_log_topic_sequence"); err != nil {
					t.Fatal(err)
				}
			} else {
				var seed int64
				if err := old.db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='message_log'").Scan(&seed); err != nil || seed != 9223372036854775806 {
					t.Fatalf("failed backfill changed original sequence metadata: %d, %v", seed, err)
				}
				if _, err := old.db.Exec("DELETE FROM sqlite_sequence WHERE name='message_log'; DROP TABLE sequence_exhaustion_fixture;"); err != nil {
					t.Fatal(err)
				}
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			s := queueHandle(t, path, false)
			messageSequenceLedger(t, s, len(migrations))
			check(s)
			if entries := messageSequenceLog(t, s.db); len(entries) != 2 {
				t.Fatalf("clean retry did not backfill exactly once: %+v", entries)
			}
			messageSequenceCoverage(t, s)
		})
	}
}
