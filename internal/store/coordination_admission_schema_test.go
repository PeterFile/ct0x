package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	coordinationAdmissionDecisionColumns = "id,trigger_id,group_id,topic_id,kind,actor_id,policy_revision,reason,task_id,task_revision_at_admission,mandate_id,created_at"
	coordinationAdmissionMandateColumns  = "id,decision_id,origin_trigger_id,group_id,topic_id,task_id,task_revision_at_admission,authority_kind,authorized_by,strength,policy_revision,objective,description,acceptance,assignee_id,dependency_ids,created_at"
	coordinationAdmissionCurrentColumns  = "task_id,group_id,topic_id,current_mandate_id,authority_generation"
	coordinationAdmissionDecisionInsert  = "INSERT INTO routing_decisions(" + coordinationAdmissionDecisionColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?)"
	coordinationAdmissionMandateInsert   = "INSERT INTO mandates(" + coordinationAdmissionMandateColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	coordinationAdmissionCurrentInsert   = "INSERT INTO task_coordination(" + coordinationAdmissionCurrentColumns + ") VALUES(?,?,?,?,?)"
)

type coordinationAdmissionSQLRows struct {
	decision, mandate, current []any
}

func coordinationAdmissionRows(task domain.Task, trigger domain.Trigger) coordinationAdmissionSQLRows {
	decisionID, mandateID := rand.Text(), rand.Text()
	dependencies, _ := json.Marshal(task.DependencyIDs)
	stamp := time.Now().UTC().Format(timestampLayout)
	return coordinationAdmissionSQLRows{
		decision: []any{decisionID, trigger.ID, task.GroupID, trigger.TopicID, "create_new", "user", "local-user-explicit/v1", "用户明确要求开展此工作", task.ID, task.Revision, mandateID, stamp},
		mandate:  []any{mandateID, decisionID, trigger.ID, task.GroupID, trigger.TopicID, task.ID, task.Revision, "local_user_explicit", "user", "explicit", "local-user-explicit/v1", task.Title, task.Description, task.Acceptance, task.AssigneeID, string(dependencies), stamp},
		current:  []any{task.ID, task.GroupID, trigger.TopicID, mandateID, int64(1)},
	}
}

func coordinationAdmissionSource(t *testing.T, s *Store, thread domain.ThreadRef) domain.Trigger {
	t.Helper()
	message := coordinationTriggerMessage(t, s, thread)
	trigger, err := s.RecordLocalMessageTrigger(ctx, thread, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	return trigger
}

func coordinationAdmissionFixture(t *testing.T, s *Store, thread domain.ThreadRef) coordinationAdmissionSQLRows {
	t.Helper()
	return coordinationAdmissionRows(createTestTask(t, s, thread.GroupID), coordinationAdmissionSource(t, s, thread))
}

// These helpers intentionally exercise schema SQL, not a production admission writer.
func coordinationAdmissionInsert(tx *sql.Tx, rows coordinationAdmissionSQLRows) error {
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{coordinationAdmissionDecisionInsert, rows.decision},
		{coordinationAdmissionMandateInsert, rows.mandate},
		{coordinationAdmissionCurrentInsert, rows.current},
	} {
		if statement.args == nil {
			continue
		}
		if _, err := tx.Exec(statement.query, statement.args...); err != nil {
			return err
		}
	}
	return nil
}

func coordinationAdmissionCommit(s *Store, rows ...coordinationAdmissionSQLRows) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range rows {
		if err := coordinationAdmissionInsert(tx, row); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func coordinationAdmissionRequireFK(t *testing.T, err error) {
	t.Helper()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
		t.Fatalf("expected exact-relationship FK rejection, got %v", err)
	}
}

func TestCoordinationAdmissionSchemaValidShapesAndOpaqueTopics(t *testing.T) {
	for _, topic := range []string{"", "topic", " \n\t ", "line\ntopic", "nul\x00topic", string([]byte{0xff})} {
		for _, kind := range []string{"create_new", "link_existing"} {
			t.Run(fmt.Sprintf("%s/%q", kind, topic), func(t *testing.T) {
				s, _ := openTestStore(t)
				g := group(t, s)
				assignee := member(t, s, g.ID)
				dependency := createTestTask(t, s, g.ID) // An unaccepted dependency is valid admission evidence.
				input := taskInput(g.ID)
				input.AssigneeID, input.DependencyIDs = assignee.ID, []string{dependency.ID}
				task, err := s.CreateTask(ctx, input)
				if err != nil {
					t.Fatal(err)
				}
				trigger := coordinationAdmissionSource(t, s, domain.ThreadRef{GroupID: g.ID, TopicID: topic})
				rows := coordinationAdmissionRows(task, trigger)
				rows.decision[4] = kind
				before := messageSequenceSnapshot(t, s)
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				// Either insertion order must work inside the deferred identity cycle.
				queries, args := []string{coordinationAdmissionDecisionInsert, coordinationAdmissionMandateInsert}, [][]any{rows.decision, rows.mandate}
				if kind == "link_existing" {
					slices.Reverse(queries)
					slices.Reverse(args)
				}
				for i, query := range queries {
					if _, err := tx.Exec(query, args[i]...); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tx.Exec(coordinationAdmissionCurrentInsert, rows.current...); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				for _, table := range []string{"routing_decisions", "mandates", "task_coordination"} {
					delete(before, table)
				}
				messageSequencePreserved(t, s, before)
				var objective, description, acceptance, assigneeID, dependencyJSON, persistedTopic string
				var revision, generation int64
				if err := s.db.QueryRow(`SELECT m.objective,m.description,m.acceptance,m.assignee_id,m.dependency_ids,
				 c.topic_id,m.task_revision_at_admission,c.authority_generation FROM mandates m
				 JOIN task_coordination c ON c.current_mandate_id=m.id WHERE m.id=?`, rows.mandate[0]).Scan(
					&objective, &description, &acceptance, &assigneeID, &dependencyJSON, &persistedTopic, &revision, &generation); err != nil {
					t.Fatal(err)
				}
				var dependencies []string
				if err := json.Unmarshal([]byte(dependencyJSON), &dependencies); err != nil || !slices.Equal(dependencies, task.DependencyIDs) ||
					objective != task.Title || description != task.Description || acceptance != task.Acceptance || assigneeID != task.AssigneeID ||
					persistedTopic != topic || revision != task.Revision || generation != 1 {
					t.Fatalf("snapshot/projection did not retain exact authority: deps=%v revision=%d generation=%d topic=%q, %v", dependencies, revision, generation, persistedTopic, err)
				}
				if violations := messageSequenceRows(t, s.db, "PRAGMA foreign_key_check"); len(violations) != 0 {
					t.Fatalf("valid admission has FK violations: %v", violations)
				}
			})
		}
	}
}

func TestCoordinationAdmissionExactRelationships(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	thread := domain.ThreadRef{GroupID: g.ID, TopicID: " exact\ntopic "}
	foreign := coordinationAdmissionFixture(t, s, domain.ThreadRef{GroupID: other.ID, TopicID: thread.TopicID})
	otherTopic := coordinationAdmissionSource(t, s, domain.ThreadRef{GroupID: g.ID, TopicID: "exact topic"})
	for _, test := range []struct {
		name   string
		change func(*coordinationAdmissionSQLRows, *coordinationAdmissionSQLRows)
	}{
		{"decision_missing_trigger", func(a, b *coordinationAdmissionSQLRows) { a.decision[1], a.mandate[2] = "missing", "missing" }},
		{"decision_trigger_group", func(a, b *coordinationAdmissionSQLRows) {
			a.decision[1], a.mandate[2] = foreign.decision[1], foreign.decision[1]
		}},
		{"decision_trigger_topic", func(a, b *coordinationAdmissionSQLRows) { a.decision[1], a.mandate[2] = otherTopic.ID, otherTopic.ID }},
		{"decision_group", func(a, b *coordinationAdmissionSQLRows) { a.decision[2] = other.ID }},
		{"decision_topic", func(a, b *coordinationAdmissionSQLRows) { a.decision[3] = "" }},
		{"decision_task_group", func(a, b *coordinationAdmissionSQLRows) {
			a.decision[8], a.mandate[5], a.current[0] = foreign.decision[8], foreign.decision[8], foreign.decision[8]
		}},
		{"decision_missing_task", func(a, b *coordinationAdmissionSQLRows) { a.decision[8] = "missing" }},
		{"mandate_origin", func(a, b *coordinationAdmissionSQLRows) { a.mandate[2] = b.decision[1] }},
		{"mandate_group", func(a, b *coordinationAdmissionSQLRows) { a.mandate[3] = other.ID }},
		{"mandate_topic", func(a, b *coordinationAdmissionSQLRows) { a.mandate[4] = "" }},
		{"mandate_task", func(a, b *coordinationAdmissionSQLRows) { a.mandate[5] = b.decision[8] }},
		{"mandate_revision", func(a, b *coordinationAdmissionSQLRows) { a.mandate[6] = int64(2) }},
		{"decision_revision", func(a, b *coordinationAdmissionSQLRows) { a.decision[9] = int64(2) }},
		{"mandate_missing_decision", func(a, b *coordinationAdmissionSQLRows) { a.mandate[1] = "missing" }},
		{"crossed_decision_pointers", func(a, b *coordinationAdmissionSQLRows) {
			a.decision[10], b.decision[10] = b.decision[10], a.decision[10]
		}},
		{"crossed_mandate_pointers", func(a, b *coordinationAdmissionSQLRows) { a.mandate[1], b.mandate[1] = b.mandate[1], a.mandate[1] }},
		{"current_task", func(a, b *coordinationAdmissionSQLRows) { a.current[0], b.current[0] = b.current[0], a.current[0] }},
		{"current_group", func(a, b *coordinationAdmissionSQLRows) { a.current[1] = other.ID }},
		{"current_topic", func(a, b *coordinationAdmissionSQLRows) { a.current[2] = "" }},
		{"current_missing_mandate", func(a, b *coordinationAdmissionSQLRows) { a.current[3] = "missing" }},
		{"current_unrelated_mandate", func(a, b *coordinationAdmissionSQLRows) { a.current[3], b.current[3] = b.current[3], a.current[3] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, b := coordinationAdmissionFixture(t, s, thread), coordinationAdmissionFixture(t, s, thread)
			before := messageSequenceSnapshot(t, s)
			test.change(&a, &b)
			if !strings.HasPrefix(test.name, "current_") {
				// Test the Decision/Mandate edge itself, without projection FKs masking it.
				a.current, b.current = nil, nil
			}
			coordinationAdmissionRequireFK(t, coordinationAdmissionCommit(s, a, b))
			messageSequencePreserved(t, s, before)
		})
	}
}

func TestCoordinationAdmissionRequiredValuesAndTypedSnapshot(t *testing.T) {
	s, _ := openTestStore(t)
	rows := coordinationAdmissionFixture(t, s, domain.ThreadRef{GroupID: group(t, s).ID})
	before := messageSequenceSnapshot(t, s)
	for _, table := range []struct {
		name, columns, insert string
		values                []any
		invalid               map[string][]any
	}{
		{"routing_decisions", coordinationAdmissionDecisionColumns, coordinationAdmissionDecisionInsert, rows.decision, map[string][]any{
			"kind": {"attach", "", "CREATE_NEW"}, "actor_id": {"agent", "USER", ""}, "policy_revision": {"", "local-user-explicit/v2"},
			"task_revision_at_admission": {0, -1, 1.5, "bad", []byte{1}},
		}},
		{"mandates", coordinationAdmissionMandateColumns, coordinationAdmissionMandateInsert, rows.mandate, map[string][]any{
			"authority_kind": {"inferred", ""}, "authorized_by": {"agent", ""}, "strength": {"implicit", ""}, "policy_revision": {"", "local-user-explicit/v2"},
			"task_revision_at_admission": {0, -1, 1.5, "bad", []byte{1}}, "objective": {"", " "}, "acceptance": {"", " "}, "assignee_id": {" "},
			"dependency_ids": {"", "not-json", "null", "{}", `"id"`, "1", `[null]`, `[1]`, `[true]`, `[{}]`, `[[]]`, `[""]`, `[" "]`, `["duplicate","duplicate"]`, fmt.Sprintf("[%q]", rows.current[0])},
		}},
		{"task_coordination", coordinationAdmissionCurrentColumns, coordinationAdmissionCurrentInsert, rows.current, map[string][]any{
			"authority_generation": {0, -1, 1.5, "bad", []byte{1}},
		}},
	} {
		for i, column := range strings.Split(table.columns, ",") {
			invalid := append([]any{nil}, table.invalid[column]...)
			if column == "id" || strings.HasSuffix(column, "_id") && column != "topic_id" && column != "assignee_id" {
				invalid = append(invalid, "")
			}
			for _, value := range invalid {
				t.Run(fmt.Sprintf("%s/%s/%v", table.name, column, value), func(t *testing.T) {
					tx, err := s.db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					// Resolve parent references so a missing FK cannot mask a missing CHECK.
					if table.name != "routing_decisions" {
						if _, err := tx.Exec(coordinationAdmissionDecisionInsert, rows.decision...); err != nil {
							t.Fatal(err)
						}
					}
					if table.name == "task_coordination" {
						if _, err := tx.Exec(coordinationAdmissionMandateInsert, rows.mandate...); err != nil {
							t.Fatal(err)
						}
					}
					args := slices.Clone(table.values)
					args[i] = value
					if _, err := tx.Exec(table.insert, args...); err == nil {
						t.Fatalf("malformed persisted %s.%s accepted: %#v", table.name, column, value)
					}
				})
			}
		}
	}
	messageSequencePreserved(t, s, before)
	if err := coordinationAdmissionCommit(s, rows); err != nil {
		t.Fatalf("valid empty-assignee/empty-dependency snapshot rejected: %v", err)
	}
}

func TestCoordinationAdmissionOneInitialAdmissionPerOrigin(t *testing.T) {
	s, _ := openTestStore(t)
	thread := domain.ThreadRef{GroupID: group(t, s).ID}
	first := coordinationAdmissionFixture(t, s, thread)
	if err := coordinationAdmissionCommit(s, first); err != nil {
		t.Fatal(err)
	}
	second := coordinationAdmissionFixture(t, s, thread)
	second.decision[1], second.mandate[2] = first.decision[1], first.decision[1]
	before := messageSequenceSnapshot(t, s)
	if err := coordinationAdmissionCommit(s, second); err == nil {
		t.Fatal("one origin produced two successful initial admissions")
	}
	messageSequencePreserved(t, s, before)
}

func TestCoordinationAdmissionImmutableEveryUniqueConflict(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprintf("recursive_triggers=%d", recursive), func(t *testing.T) {
			s, _ := openTestStore(t)
			thread := domain.ThreadRef{GroupID: group(t, s).ID, TopicID: "topic"}
			first, candidate := coordinationAdmissionFixture(t, s, thread), coordinationAdmissionFixture(t, s, thread)
			if err := coordinationAdmissionCommit(s, first); err != nil {
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
			for _, table := range []struct {
				name, columns, insert, guard string
				original, candidate          []any
				unique                       []string
			}{
				{"routing_decisions", coordinationAdmissionDecisionColumns, coordinationAdmissionDecisionInsert, "路由决定不可替换", first.decision, candidate.decision,
					[]string{"id", "trigger_id", "mandate_id", "id,mandate_id,trigger_id,task_id,group_id,topic_id,task_revision_at_admission"}},
				{"mandates", coordinationAdmissionMandateColumns, coordinationAdmissionMandateInsert, "授权记录不可替换", first.mandate, candidate.mandate,
					[]string{"id", "decision_id", "id,decision_id,origin_trigger_id,task_id,group_id,topic_id,task_revision_at_admission", "id,task_id,group_id,topic_id"}},
			} {
				for _, query := range []string{"UPDATE " + table.name + " SET id=id", "UPDATE " + table.name + " SET created_at='changed'", "DELETE FROM " + table.name} {
					if _, err := s.db.Exec(query); err == nil {
						t.Fatalf("immutable SQL accepted: %s", query)
					}
				}
				indexes := messageSequenceRows(t, s.db, fmt.Sprintf(`SELECT name FROM pragma_index_list('%s') WHERE "unique"=1`, table.name))
				var unique []string
				for _, index := range indexes {
					keys := messageSequenceRows(t, s.db, fmt.Sprintf("SELECT name FROM pragma_index_info('%s') ORDER BY seqno", index[0]))
					var names []string
					args := slices.Clone(table.candidate)
					for _, key := range keys {
						name := key[0].(string)
						names = append(names, name)
						i := slices.Index(strings.Split(table.columns, ","), name)
						args[i] = table.original[i]
					}
					unique = append(unique, strings.Join(names, ","))
					query := strings.Replace(table.insert, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
					if _, err := s.db.Exec(query, args...); err == nil || !strings.Contains(err.Error(), table.guard) {
						t.Fatalf("BEFORE INSERT guard missed %s UNIQUE(%s), recursive=%d: %v", table.name, strings.Join(names, ","), recursive, err)
					}
				}
				slices.Sort(unique)
				slices.Sort(table.unique)
				if !slices.Equal(unique, table.unique) {
					t.Fatalf("%s UNIQUE keys changed: %v; want %v", table.name, unique, table.unique)
				}
				for _, suffix := range []string{"", " ON CONFLICT(id) DO UPDATE SET id=id"} {
					if _, err := s.db.Exec(table.insert+suffix, table.original...); err == nil || !strings.Contains(err.Error(), table.guard) {
						t.Fatalf("duplicate/upsert bypassed immutable guard: %v", err)
					}
				}
			}
			messageSequencePreserved(t, s, before)
		})
	}
}

func TestCoordinationAdmissionHistoricalRevisionSurvivesTaskReject(t *testing.T) {
	s, path := openTestStore(t)
	thread := domain.ThreadRef{GroupID: group(t, s).ID}
	task := createTestTask(t, s, thread.GroupID)
	rows := coordinationAdmissionRows(task, coordinationAdmissionSource(t, s, thread))
	if err := coordinationAdmissionCommit(s, rows); err != nil {
		t.Fatal(err)
	}
	authority := make(map[string][][]any)
	for _, table := range []string{"routing_decisions", "mandates", "task_coordination"} {
		authority[table] = messageSequenceRows(t, s.db, "SELECT * FROM "+table+" ORDER BY rowid")
	}
	attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
	if err != nil {
		t.Fatal(err)
	}
	submission, err := s.SubmitTask(ctx, taskSubmissionInput(attempt))
	if err != nil {
		t.Fatal(err)
	}
	review := taskReviewInput(submission)
	review.Outcome = domain.ReviewOutcomeRejected
	if _, err := s.ReviewTask(ctx, review); err != nil {
		t.Fatalf("admission history blocked legitimate rejection: %v", err)
	}
	current, err := s.GetTask(ctx, thread.GroupID, task.ID)
	if err != nil || current.Revision != 2 || current.Status != domain.TaskStatusOpen {
		t.Fatalf("task reject did not advance revision: %+v, %v", current, err)
	}
	if _, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(current)); err != nil {
		t.Fatalf("historical authority blocked the next task generation: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	messageSequencePreserved(t, reopened, authority)
	if violations := messageSequenceRows(t, reopened.db, "PRAGMA foreign_key_check"); len(violations) != 0 {
		t.Fatalf("historical revision became a current-revision FK: %v", violations)
	}
}

func TestCoordinationAdmissionDeferredCommitFailureRollsBackWholeCycle(t *testing.T) {
	for _, missingMandate := range []bool{true, false} {
		t.Run(fmt.Sprintf("missing_mandate=%t", missingMandate), func(t *testing.T) {
			s, path := openTestStore(t)
			observer := queueHandle(t, path, false)
			thread := domain.ThreadRef{GroupID: group(t, s).ID, TopicID: "topic"}
			message := coordinationTriggerMessage(t, s, thread)
			stamp := time.Now().UTC()
			task := domain.Task{ID: rand.Text(), GroupID: thread.GroupID, Title: "目标", Acceptance: "验收", DependencyIDs: []string{}, Revision: 1, CreatedAt: stamp}
			trigger := domain.Trigger{ID: rand.Text(), GroupID: thread.GroupID, TopicID: thread.TopicID}
			rows := coordinationAdmissionRows(task, trigger)
			before := messageSequenceSnapshot(t, s)
			write := func(broken bool) error {
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				if _, err := tx.Exec(`INSERT INTO tasks(id,group_id,title,acceptance,created_at) VALUES(?,?,?,?,?)`, task.ID, task.GroupID, task.Title, task.Acceptance, stamp.Format(timestampLayout)); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(`INSERT INTO coordination_triggers(id,source_kind,group_id,topic_id,message_id,message_sequence,created_at)
				 SELECT ?,'local_im',group_id,topic_id,message_id,sequence,? FROM message_log WHERE message_id=?`, trigger.ID, stamp.Format(timestampLayout), message.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(`INSERT INTO mutation_idempotency VALUES('request_work','user',?,'retryable',?,?)`, task.GroupID, []byte("schema fixture"), rows.decision[0]); err != nil {
					t.Fatal(err)
				}
				decision := slices.Clone(rows.decision)
				if broken && !missingMandate {
					decision[10] = "unrelated-mandate"
				}
				if _, err := tx.Exec(coordinationAdmissionDecisionInsert, decision...); err != nil {
					t.Fatalf("deferred decision rejected before COMMIT: %v", err)
				}
				if !broken || !missingMandate {
					if _, err := tx.Exec(coordinationAdmissionMandateInsert, rows.mandate...); err != nil {
						t.Fatalf("deferred mandate rejected before COMMIT: %v", err)
					}
					if _, err := tx.Exec(coordinationAdmissionCurrentInsert, rows.current...); err != nil {
						t.Fatal(err)
					}
				}
				messageSequencePreserved(t, observer, before)
				return tx.Commit()
			}
			coordinationAdmissionRequireFK(t, write(true))
			messageSequencePreserved(t, s, before)
			if err := write(false); err != nil {
				t.Fatalf("failed COMMIT leaked rows or burned the retry key: %v", err)
			}
			if violations := messageSequenceRows(t, s.db, "PRAGMA foreign_key_check"); !reflect.DeepEqual(violations, [][]any{}) {
				t.Fatalf("retry did not commit one exact cycle: %v", violations)
			}
		})
	}
}
