package store

import (
	"crypto/rand"
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
	dispatchRequestColumns = "id,group_id,topic_id,task_id,task_revision,authority_generation,mandate_id,coordinator_member_id,binding_revision,policy_revision,requested_by,reason,created_at"
	dispatchStateColumns   = "request_id,task_id,group_id,generation,status,attempt_count,next_attempt_at,updated_at,last_error_code,held_reason,message_id,delivery_id,conversation_id,delivered_member_id,materialized_from_generation,materialized_at"
	dispatchRequestInsert  = "INSERT INTO coordination_dispatch_requests(" + dispatchRequestColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)"
	dispatchStateInsert    = "INSERT INTO coordination_dispatch_state(" + dispatchStateColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	dispatchStamp          = "2026-01-02T03:04:05.123456789Z"
)

type dispatchSQLRows struct{ request, state []any }

func dispatchConfiguredGroup(t *testing.T, s *Store) (string, string, string) {
	t.Helper()
	g := group(t, s)
	coordinator, worker := member(t, s, g.ID), member(t, s, g.ID)
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 1, coordinator.ID}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, fmt.Sprintf("[%q]", worker.ID)}},
		coordinatorSQL{coordinatorBindingInsert, []any{g.ID, 1}},
		coordinatorSQL{coordinatorPolicyInsert, []any{g.ID, 1}},
	)
	return g.ID, coordinator.ID, worker.ID
}

func dispatchCandidate(t *testing.T, s *Store, groupID, coordinatorID, topic string) dispatchSQLRows {
	t.Helper()
	admission := coordinationAdmissionFixture(t, s, domain.ThreadRef{GroupID: groupID, TopicID: topic})
	if err := coordinationAdmissionCommit(s, admission); err != nil {
		t.Fatal(err)
	}
	id := rand.Text()
	return dispatchSQLRows{
		request: []any{id, groupID, topic, admission.current[0], int64(1), int64(1), admission.mandate[0], coordinatorID, int64(1), int64(1), "user", " 明确安排协调工作 ", dispatchStamp},
		state:   []any{id, admission.current[0], groupID, int64(1), "pending", int64(0), dispatchStamp, dispatchStamp, "", "", nil, nil, nil, nil, nil, nil},
	}
}

func dispatchPair(rows dispatchSQLRows) []coordinatorSQL {
	return []coordinatorSQL{{dispatchRequestInsert, rows.request}, {dispatchStateInsert, rows.state}}
}

func dispatchSet(columns string, values []any, column string, value any) []any {
	out := slices.Clone(values)
	i := slices.Index(strings.Split(columns, ","), column)
	if i < 0 {
		panic("unknown fixture column: " + column)
	}
	out[i] = value
	return out
}

func dispatchUpdate(values []any) coordinatorSQL {
	var assignments []string
	for _, column := range strings.Split(dispatchStateColumns, ",") {
		assignments = append(assignments, column+"=?")
	}
	return coordinatorSQL{"UPDATE coordination_dispatch_state SET " + strings.Join(assignments, ",") + " WHERE request_id=?", append(slices.Clone(values), values[0])}
}

func dispatchReject(t *testing.T, s *Store, guard string, statements ...coordinatorSQL) {
	t.Helper()
	before := messageSequenceSnapshot(t, s)
	err := coordinatorCommit(s, statements...)
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_CONSTRAINT || guard != "" && !strings.Contains(err.Error(), guard) {
		t.Fatalf("expected rejection %q, got %v", guard, err)
	}
	messageSequencePreserved(t, s, before)
}

func dispatchReceipt(t *testing.T, s *Store, rows dispatchSQLRows) []any {
	t.Helper()
	delivery := queueDelivery(t, s, rows.request[1].(string), rows.request[7].(string), rows.request[2].(string))
	values := slices.Clone(rows.state)
	values[3], values[4], values[8], values[9] = values[3].(int64)+1, "enqueued", "", ""
	values[10], values[11], values[12], values[13] = delivery.MessageID, delivery.ID, delivery.ConversationID, delivery.RecipientID
	values[14], values[15] = rows.state[3], values[7]
	return values
}

func dispatchAssertFK(t *testing.T, s *Store) {
	t.Helper()
	if violations := messageSequenceRows(t, s.db, "PRAGMA foreign_key_check"); len(violations) != 0 {
		t.Fatalf("foreign key violations: %v", violations)
	}
}

func TestCoordinationDispatchSchemaPairOrderAndCommit(t *testing.T) {
	s, path := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "topic")
	before := messageSequenceSnapshot(t, s)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(dispatchStateInsert, rows.state...); err == nil {
		t.Fatal("state-first must fail immediately, not at COMMIT")
	} else {
		coordinationAdmissionRequireFK(t, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	messageSequencePreserved(t, s, before)

	// The request is accepted as a statement but cannot commit without its state.
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(dispatchRequestInsert, rows.request...); err != nil {
		t.Fatalf("request-first must be deferred: %v", err)
	}
	if _, err := tx.Exec("INSERT INTO mutation_idempotency VALUES('dispatch-fixture','user',?,'retry',?,?)", g, []byte("pair"), rows.request[0]); err != nil {
		t.Fatal(err)
	}
	coordinationAdmissionRequireFK(t, tx.Commit())
	_ = tx.Rollback()
	messageSequencePreserved(t, s, before)
	coordinatorMustCommit(t, s, append(dispatchPair(rows), coordinatorSQL{"INSERT INTO mutation_idempotency VALUES('dispatch-fixture','user',?,'retry',?,?)", []any{g, []byte("pair"), rows.request[0]}})...)
	committed := messageSequenceSnapshot(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	messageSequencePreserved(t, reopened, committed)
	dispatchAssertFK(t, reopened)
}

func TestCoordinationDispatchSchemaExactRelationships(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, worker := dispatchConfiguredGroup(t, s)
	other, foreign, _ := dispatchConfiguredGroup(t, s)
	a := dispatchCandidate(t, s, g, coordinator, "exact topic")
	b := dispatchCandidate(t, s, g, coordinator, "other topic")
	c := dispatchCandidate(t, s, other, foreign, "exact topic")
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g, 2, worker}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g, 2, "[]"}},
	)
	for _, test := range []struct {
		column string
		values []any
	}{
		{"group_id", []any{"missing", other}}, {"task_id", []any{"missing", b.request[3], c.request[3]}},
		{"topic_id", []any{"", "exact topic ", "other topic"}},
		{"mandate_id", []any{"missing", b.request[6], c.request[6]}},
		{"task_revision", []any{int64(2)}}, {"authority_generation", []any{int64(2)}},
		{"binding_revision", []any{int64(2), int64(3)}}, {"policy_revision", []any{int64(2), int64(3)}},
		{"coordinator_member_id", []any{"missing", worker, foreign}},
	} {
		for _, value := range test.values {
			t.Run(fmt.Sprintf("request/%s/%v", test.column, value), func(t *testing.T) {
				bad := a
				bad.request = dispatchSet(dispatchRequestColumns, a.request, test.column, value)
				dispatchReject(t, s, "", dispatchPair(bad)...)
			})
		}
	}
	for _, test := range []struct {
		column string
		value  any
	}{
		{"request_id", b.request[0]}, {"task_id", b.request[3]}, {"group_id", other},
	} {
		t.Run("state/"+test.column, func(t *testing.T) {
			bad := a
			bad.state = dispatchSet(dispatchStateColumns, a.state, test.column, test.value)
			dispatchReject(t, s, "FOREIGN KEY", dispatchPair(bad)...)
		})
	}
	for _, edge := range []struct {
		table, target string
		columns       [][]any
	}{
		{"coordination_dispatch_requests", "tasks", [][]any{{"task_id", "id"}, {"group_id", "group_id"}}},
		{"coordination_dispatch_requests", "mandates", [][]any{{"mandate_id", "id"}, {"task_id", "task_id"}, {"group_id", "group_id"}, {"topic_id", "topic_id"}}},
		{"coordination_dispatch_requests", "members", [][]any{{"group_id", "group_id"}, {"coordinator_member_id", "id"}}},
		{"coordination_dispatch_requests", "group_coordinator_binding_revisions", [][]any{{"group_id", "group_id"}, {"binding_revision", "revision"}}},
		{"coordination_dispatch_requests", "group_dispatch_policy_revisions", [][]any{{"group_id", "group_id"}, {"policy_revision", "revision"}}},
		{"coordination_dispatch_requests", "coordination_dispatch_state", [][]any{{"id", "request_id"}, {"task_id", "task_id"}, {"group_id", "group_id"}}},
		{"coordination_dispatch_state", "coordination_dispatch_requests", [][]any{{"request_id", "id"}, {"task_id", "task_id"}, {"group_id", "group_id"}}},
		{"coordination_dispatch_state", "deliveries", [][]any{{"delivery_id", "id"}, {"group_id", "group_id"}, {"conversation_id", "conversation_id"}, {"delivered_member_id", "recipient_id"}}},
	} {
		got := messageSequenceRows(t, s.db, fmt.Sprintf(`SELECT "from","to" FROM pragma_foreign_key_list('%s') WHERE "table"='%s' ORDER BY seq`, edge.table, edge.target))
		if !reflect.DeepEqual(got, edge.columns) {
			t.Fatalf("exact %s -> %s: %v; want %v", edge.table, edge.target, got, edge.columns)
		}
	}
	coordinatorMustCommit(t, s, dispatchPair(a)...)
	dispatchAssertFK(t, s)
}

func TestCoordinationDispatchSchemaCurrentInsertionFence(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "topic")
	other := dispatchCandidate(t, s, g, coordinator, "topic")
	task, err := s.GetTask(ctx, g, rows.request[3].(string))
	if err != nil {
		t.Fatal(err)
	}
	alternative := coordinationAdmissionRows(task, coordinationAdmissionSource(t, s, domain.ThreadRef{GroupID: g, TopicID: "topic"}))
	alternative.current = nil
	changedTopic := coordinationAdmissionRows(task, coordinationAdmissionSource(t, s, domain.ThreadRef{GroupID: g, TopicID: "changed-topic"}))
	changedTopic.current = nil
	if err := coordinationAdmissionCommit(s, alternative, changedTopic); err != nil {
		t.Fatal(err)
	}
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g, 2, coordinator}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g, 2, "[]"}},
	)
	for _, change := range []coordinatorSQL{
		{"UPDATE task_coordination SET authority_generation=2 WHERE task_id=?", []any{rows.request[3]}},
		{"DELETE FROM task_coordination WHERE task_id=?", []any{rows.request[3]}},
		{"UPDATE task_coordination SET current_mandate_id=? WHERE task_id=?", []any{alternative.mandate[0], task.ID}},
		{"UPDATE task_coordination SET current_mandate_id=?,topic_id='changed-topic' WHERE task_id=?", []any{changedTopic.mandate[0], task.ID}},
		{"UPDATE tasks SET revision=2 WHERE id=?", []any{rows.request[3]}},
		{"UPDATE group_coordinator_bindings SET revision=2 WHERE group_id=?", []any{g}},
		{"UPDATE group_dispatch_policies SET revision=2 WHERE group_id=?", []any{g}},
	} {
		dispatchReject(t, s, "当前", append([]coordinatorSQL{change}, dispatchPair(rows)...)...)
	}
	// Current revision 2 cannot authorize Mandate revision 1, even when request/current match.
	bad := rows
	bad.request = dispatchSet(dispatchRequestColumns, rows.request, "task_revision", int64(2))
	dispatchReject(t, s, "授权", append([]coordinatorSQL{{"UPDATE tasks SET revision=2 WHERE id=?", []any{rows.request[3]}}}, dispatchPair(bad)...)...)

	attempt, err := s.ActivateTaskAttempt(ctx, ActivateTaskAttemptInput{GroupID: g, TaskID: rows.request[3].(string), ActorID: "user", ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	dispatchReject(t, s, "当前", dispatchPair(rows)...)
	submission, err := s.SubmitTask(ctx, taskSubmissionInput(attempt))
	if err != nil {
		t.Fatal(err)
	}
	dispatchReject(t, s, "当前", dispatchPair(rows)...)
	if _, err := s.ReviewTask(ctx, taskReviewInput(submission)); err != nil {
		t.Fatal(err)
	}
	dispatchReject(t, s, "当前", dispatchPair(rows)...)
	coordinatorMustCommit(t, s, dispatchPair(other)...)
}

func TestCoordinationDispatchSchemaHistoricalSnapshotsSurviveChanges(t *testing.T) {
	s, path := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "topic")
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
	before := map[string][][]any{}
	for _, table := range []string{"coordination_dispatch_requests", "coordination_dispatch_state", "mandates", "routing_decisions"} {
		before[table] = messageSequenceTableRows(t, s, table)
	}
	task, err := s.GetTask(ctx, g, rows.request[3].(string))
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
	coordinatorMustCommit(t, s,
		coordinatorSQL{"UPDATE task_coordination SET authority_generation=2 WHERE task_id=?", []any{task.ID}},
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g, 2, coordinator}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g, 2, "[]"}},
		coordinatorSQL{"UPDATE group_coordinator_bindings SET revision=2 WHERE group_id=?", []any{g}},
		coordinatorSQL{"UPDATE group_dispatch_policies SET revision=2 WHERE group_id=?", []any{g}},
		coordinatorSQL{"UPDATE members SET status='left' WHERE id=?", []any{coordinator}},
	)
	current, err := s.GetTask(ctx, g, task.ID)
	if err != nil || current.Revision != 2 || current.Status != domain.TaskStatusOpen {
		t.Fatalf("real rejection did not advance Task: %+v, %v", current, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	messageSequencePreserved(t, reopened, before)
	dispatchAssertFK(t, reopened)
}

func TestCoordinationDispatchSchemaOpaqueTopicsAndSQLValueBoundary(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	for _, topic := range []string{"", " \n\t ", "line\ntopic", "nul\x00topic", string([]byte{0xff})} {
		t.Run(fmt.Sprintf("topic/%q", topic), func(t *testing.T) {
			rows := dispatchCandidate(t, s, g, coordinator, topic)
			coordinatorMustCommit(t, s, dispatchPair(rows)...)
			var got string
			if err := s.db.QueryRow("SELECT topic_id FROM coordination_dispatch_requests WHERE id=?", rows.request[0]).Scan(&got); err != nil || got != topic {
				t.Fatalf("opaque topic changed: %q, %v; want %q", got, err, topic)
			}
		})
	}
	rows := dispatchCandidate(t, s, g, coordinator, "values")
	for i, column := range strings.Split(dispatchRequestColumns, ",") {
		invalid := []any{nil, []byte("blob")}
		switch column {
		case "topic_id":
			invalid = []any{nil}
		case "task_revision", "authority_generation", "binding_revision", "policy_revision":
			invalid = append(invalid, 0, -1, 1.5, "bad", "9223372036854775808")
		case "reason":
			invalid = append(invalid, "", " ")
		case "created_at":
			invalid = append(invalid, "")
		default:
			invalid = append(invalid, "", " ", " padded", "padded ")
		}
		for _, value := range invalid {
			t.Run(fmt.Sprintf("request/%s/%#v", column, value), func(t *testing.T) {
				bad := rows
				bad.request = slices.Clone(rows.request)
				bad.request[i] = value
				dispatchReject(t, s, "", dispatchPair(bad)...)
			})
		}
	}
	// SQL only specifies ASCII U+0020 trim; Unicode validation belongs to N2b.
	rows.request[0], rows.state[0], rows.request[11] = "\trequest\t", "\trequest\t", "\u3000"
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
}

func TestCoordinationDispatchSchemaStateInitialShape(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "initial")
	receipt := dispatchReceipt(t, s, rows)
	for i, column := range strings.Split(dispatchStateColumns, ",") {
		var invalid []any
		if i < 10 {
			invalid = []any{nil, []byte("blob")}
		}
		switch column {
		case "generation":
			invalid = append(invalid, 0, -1, 2, 1.5, "bad", "9223372036854775808")
		case "attempt_count":
			invalid = append(invalid, -1, 1, 1.5, "bad", "9223372036854775808")
		case "status":
			invalid = append(invalid, "held", "enqueued", "decided", "")
		case "next_attempt_at", "updated_at":
			invalid = append(invalid, "2026-01-02T03:04:06.123456789Z")
		case "last_error_code":
			invalid = append(invalid, "sqlite_busy", "sqlite_locked", "unknown")
		case "held_reason":
			invalid = append(invalid, "task_stale")
		default:
			if i >= 10 {
				invalid = append(invalid, receipt[i])
			} else {
				invalid = append(invalid, "", " ")
			}
		}
		for _, value := range invalid {
			t.Run(fmt.Sprintf("%s/%#v", column, value), func(t *testing.T) {
				bad := rows
				bad.state = slices.Clone(rows.state)
				bad.state[i] = value
				dispatchReject(t, s, "", dispatchPair(bad)...)
			})
		}
	}
	for _, status := range []string{"held", "enqueued"} {
		bad := rows
		bad.state = slices.Clone(receipt)
		bad.state[4] = status
		if status == "held" {
			bad.state[9] = "task_stale"
		}
		dispatchReject(t, s, "", dispatchPair(bad)...)
	}
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
}

func TestCoordinationDispatchSchemaPendingTransitionShape(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "backoff")
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
	at, err := time.Parse(timestampLayout, dispatchStamp)
	if err != nil {
		t.Fatal(err)
	}
	for i, seconds := range []int{2, 4, 8, 16, 32, 60, 60} {
		values := slices.Clone(rows.state)
		// A backwards wall clock is legal; exact arithmetic is still required.
		at = at.Add(-time.Minute)
		values[3], values[5] = int64(i+2), int64(i+1)
		values[6], values[7], values[8] = at.Add(time.Duration(seconds)*time.Second).Format(timestampLayout), at.Format(timestampLayout), "sqlite_busy"
		if i%2 != 0 {
			values[8] = "sqlite_locked"
		}
		for column, invalid := range map[string][]any{
			"generation":      {int64(i + 1), int64(i + 3), 1.5, "9223372036854775808"},
			"attempt_count":   {int64(i), int64(i + 2), -1, 1.5},
			"last_error_code": {"", "timeout", "unknown", "SQLITE_BUSY", nil, []byte("sqlite_busy")},
			"held_reason":     {"task_stale"},
			"next_attempt_at": {at.Add(time.Duration(seconds+1) * time.Second).Format(timestampLayout), at.Add(time.Duration(seconds)*time.Second + time.Nanosecond).Format(timestampLayout)},
		} {
			for _, value := range invalid {
				t.Run(fmt.Sprintf("attempt=%d/%s/%v", i, column, value), func(t *testing.T) {
					dispatchReject(t, s, "", dispatchUpdate(dispatchSet(dispatchStateColumns, values, column, value)))
				})
			}
		}
		coordinatorMustCommit(t, s, dispatchUpdate(values))
		rows.state = values
	}
	for _, code := range []string{"task_stale", "authority_stale", "binding_stale", "policy_stale", "coordinator_inactive", "no_eligible_worker", "payload_too_large", "payload_not_encodable"} {
		t.Run("held/"+code, func(t *testing.T) {
			values := slices.Clone(rows.state)
			values[3], values[4], values[8], values[9] = values[3].(int64)+1, "held", "", code
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			update := dispatchUpdate(values)
			if result, err := tx.Exec(update.query, update.args...); err != nil {
				t.Fatal(err)
			} else if count, err := result.RowsAffected(); err != nil || count != 1 {
				t.Fatalf("held update: count=%d, %v", count, err)
			}
		})
	}
	for _, status := range []string{"held", "enqueued"} {
		values := slices.Clone(rows.state)
		if status == "enqueued" {
			values = dispatchReceipt(t, s, rows)
		} else {
			values[3], values[4], values[8], values[9] = values[3].(int64)+1, status, "", "task_stale"
		}
		for column, invalid := range map[string][]any{
			"attempt_count":   {values[5].(int64) + 1, int64(0)},
			"next_attempt_at": {dispatchStamp}, "last_error_code": {"sqlite_busy"},
			"held_reason": {"unknown", " "},
		} {
			for _, value := range invalid {
				dispatchReject(t, s, "", dispatchUpdate(dispatchSet(dispatchStateColumns, values, column, value)))
			}
		}
	}
}

func TestCoordinationDispatchSchemaReceiptAllNullOrComplete(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "receipt")
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
	valid := dispatchReceipt(t, s, rows)
	for mask := 0; mask < 64; mask++ {
		if mask == 63 {
			continue
		}
		t.Run(fmt.Sprintf("receipt-mask=%06b", mask), func(t *testing.T) {
			values := slices.Clone(valid)
			for i := range 6 {
				if mask&(1<<i) == 0 {
					values[10+i] = nil
				}
			}
			dispatchReject(t, s, "", dispatchUpdate(values))
		})
	}
	for _, column := range []string{"materialized_from_generation", "materialized_at"} {
		for _, value := range map[string][]any{
			"materialized_from_generation": {0, -1, int64(2), 1.5, "9223372036854775808", []byte("1")},
			"materialized_at":              {"2026-01-02T03:04:06.123456789Z", "", []byte(dispatchStamp)},
		}[column] {
			dispatchReject(t, s, "", dispatchUpdate(dispatchSet(dispatchStateColumns, valid, column, value)))
		}
	}
	for i := 10; i < 14; i++ {
		for _, value := range []any{"", " ", []byte(valid[i].(string))} {
			dispatchReject(t, s, "", dispatchUpdate(dispatchSet(dispatchStateColumns, valid, strings.Split(dispatchStateColumns, ",")[i], value)))
		}
	}
	// A full receipt on pending or held has no N2 transition path.
	for _, status := range []string{"pending", "held"} {
		values := dispatchSet(dispatchStateColumns, valid, "status", status)
		if status == "held" {
			values[9] = "task_stale"
		}
		dispatchReject(t, s, "", dispatchUpdate(values))
	}
	coordinatorMustCommit(t, s, dispatchUpdate(valid))
	dispatchAssertFK(t, s)
}

func TestCoordinationDispatchSchemaReceiptExactParents(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, worker := dispatchConfiguredGroup(t, s)
	other, foreign, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "exact")
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
	valid := dispatchReceipt(t, s, rows)
	otherMessage := queueDelivery(t, s, g, coordinator, "exact")
	otherTopic := queueDelivery(t, s, g, coordinator, "different")
	otherMember := queueDelivery(t, s, g, worker, "exact")
	otherGroup := queueDelivery(t, s, other, foreign, "exact")
	memberMessage, err := s.CreateMessage(ctx, MessageRequest{GroupID: g, ActorID: worker, TopicID: "exact", Content: "not the user command"}, []string{coordinator})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []domain.Delivery{otherTopic, otherMember, otherGroup, memberMessage.Deliveries[0]} {
		values := slices.Clone(valid)
		values[10], values[11], values[12], values[13] = d.MessageID, d.ID, d.ConversationID, d.RecipientID
		dispatchReject(t, s, "", dispatchUpdate(values))
	}
	for _, pair := range []struct {
		column string
		value  any
	}{
		{"message_id", otherMessage.MessageID}, {"message_id", "missing"},
		{"delivery_id", otherMessage.ID}, {"delivery_id", "missing"},
		{"conversation_id", otherTopic.ConversationID}, {"conversation_id", otherMember.ConversationID},
		{"delivered_member_id", worker}, {"delivered_member_id", foreign},
	} {
		dispatchReject(t, s, "", dispatchUpdate(dispatchSet(dispatchStateColumns, valid, pair.column, pair.value)))
	}
	coordinatorMustCommit(t, s, dispatchUpdate(valid))
	dispatchAssertFK(t, s)
}

func TestCoordinationDispatchSchemaFrozenStatesKeepTaskSlot(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		for _, status := range []string{"pending", "held", "enqueued"} {
			t.Run(fmt.Sprintf("recursive=%d/%s", recursive, status), func(t *testing.T) {
				s, path := openTestStore(t)
				g, coordinator, _ := dispatchConfiguredGroup(t, s)
				rows := dispatchCandidate(t, s, g, coordinator, "frozen")
				coordinatorMustCommit(t, s, dispatchPair(rows)...)
				coordinatorMustCommit(t, s, coordinatorSQL{fmt.Sprintf("PRAGMA recursive_triggers=%d", recursive), nil})
				if status == "held" {
					rows.state[3], rows.state[4], rows.state[9] = int64(2), status, "task_stale"
					coordinatorMustCommit(t, s, dispatchUpdate(rows.state))
				} else if status == "enqueued" {
					rows.state = dispatchReceipt(t, s, rows)
					coordinatorMustCommit(t, s, dispatchUpdate(rows.state))
				}
				for _, target := range []string{"pending", "held", "enqueued", "decided"} {
					values := slices.Clone(rows.state)
					values[3], values[4] = values[3].(int64)+1, target
					dispatchReject(t, s, "", dispatchUpdate(values))
				}
				if status != "pending" {
					for i, column := range strings.Split(dispatchStateColumns, ",") {
						if i < 3 {
							continue
						}
						values := slices.Clone(rows.state)
						values[3] = values[3].(int64) + 1
						for _, value := range []any{nil, "changed", rows.state[i]} {
							dispatchReject(t, s, "冻结状态", dispatchUpdate(dispatchSet(dispatchStateColumns, values, column, value)))
						}
					}
				}
				for _, query := range []string{
					"UPDATE coordination_dispatch_state SET generation=generation",
					"UPDATE coordination_dispatch_state SET generation=generation+1",
					"UPDATE coordination_dispatch_state SET last_error_code='sqlite_busy'",
					"UPDATE OR REPLACE coordination_dispatch_state SET request_id=request_id",
					"DELETE FROM coordination_dispatch_state",
					"DELETE FROM coordination_dispatch_requests",
					"UPDATE coordination_dispatch_requests SET reason=reason",
					"UPDATE OR REPLACE coordination_dispatch_requests SET id=id",
				} {
					dispatchReject(t, s, "", coordinatorSQL{query, nil})
				}
				newRows := dispatchSQLRows{slices.Clone(rows.request), slices.Clone(rows.state)}
				newRows.request[0], newRows.state[0] = rand.Text(), ""
				newRows.state[0], newRows.state[3], newRows.state[4], newRows.state[5] = newRows.request[0], int64(1), "pending", int64(0)
				newRows.state[6], newRows.state[7], newRows.state[8], newRows.state[9] = dispatchStamp, dispatchStamp, "", ""
				for i := 10; i < 16; i++ {
					newRows.state[i] = nil
				}
				for _, conflict := range []string{"INSERT INTO", "INSERT OR IGNORE INTO", "INSERT OR REPLACE INTO"} {
					statements := dispatchPair(newRows)
					statements[1].query = strings.Replace(statements[1].query, "INSERT INTO", conflict, 1)
					dispatchReject(t, s, "状态不可替换", statements...)
				}
				for _, table := range []struct {
					insert string
					args   []any
				}{{dispatchRequestInsert, rows.request}, {dispatchStateInsert, newRows.state}} {
					if table.insert == dispatchStateInsert {
						table.args = slices.Clone(newRows.state)
						table.args[0] = rows.request[0]
					}
					for _, form := range []string{"INSERT INTO", "INSERT OR IGNORE INTO", "INSERT OR REPLACE INTO"} {
						dispatchReject(t, s, "不可替换", coordinatorSQL{strings.Replace(table.insert, "INSERT INTO", form, 1), table.args})
					}
					dispatchReject(t, s, "不可替换", coordinatorSQL{table.insert + " ON CONFLICT DO UPDATE SET " + strings.Split(strings.Split(table.insert, "(")[1], ",")[0] + "=" + strings.Split(strings.Split(table.insert, "(")[1], ",")[0], table.args})
				}
				before := messageSequenceSnapshot(t, s)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				reopened := queueHandle(t, path, false)
				messageSequencePreserved(t, reopened, before)
				dispatchAssertFK(t, reopened)
			})
		}
	}
}

// Seed only unreachable boundary counters; restore the exact production trigger before testing writes.
func dispatchSeedCounters(t *testing.T, s *Store, id any, generation, attempts int64) {
	t.Helper()
	var ddl string
	if err := s.db.QueryRow("SELECT sql FROM sqlite_master WHERE type='trigger' AND name='coordination_dispatch_state_update'").Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	coordinatorMustCommit(t, s,
		coordinatorSQL{"DROP TRIGGER coordination_dispatch_state_update", nil},
		coordinatorSQL{"UPDATE coordination_dispatch_state SET generation=?,attempt_count=?,last_error_code=CASE WHEN ?=0 THEN '' ELSE 'sqlite_busy' END WHERE request_id=?", []any{generation, attempts, attempts, id}},
		coordinatorSQL{ddl, nil},
	)
}

func TestCoordinationDispatchSchemaCounterOverflow(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "overflow")
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
	for _, status := range []string{"pending", "held", "enqueued"} {
		dispatchSeedCounters(t, s, rows.request[0], 9223372036854775806, 9223372036854775805)
		values := slices.Clone(rows.state)
		values[3], values[4], values[5], values[8] = int64(9223372036854775807), status, int64(9223372036854775805), ""
		switch status {
		case "pending":
			values[5], values[6], values[8] = int64(9223372036854775806), "2026-01-02T03:05:05.123456789Z", "sqlite_busy"
		case "held":
			values[9] = "task_stale"
		case "enqueued":
			values = dispatchReceipt(t, s, rows)
			values[3], values[5], values[14] = int64(9223372036854775807), int64(9223372036854775805), int64(9223372036854775806)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		update := dispatchUpdate(values)
		if _, err := tx.Exec(update.query, update.args...); err != nil {
			t.Fatalf("last representable generation must succeed for %s: %v", status, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	for _, counters := range [][2]int64{{9223372036854775807, 9223372036854775806}, {1, 9223372036854775807}} {
		dispatchSeedCounters(t, s, rows.request[0], counters[0], counters[1])
		dispatchReject(t, s, "", coordinatorSQL{`UPDATE coordination_dispatch_state SET generation=generation+1,
		 attempt_count=attempt_count+1,last_error_code='sqlite_busy',next_attempt_at='2026-01-02T03:05:05.123456789Z' WHERE request_id=?`, []any{rows.request[0]}})
		if counters[0] == 9223372036854775807 {
			for _, target := range []string{"held", "enqueued"} {
				values := slices.Clone(rows.state)
				values[3], values[4], values[5], values[9] = "9223372036854775808", target, counters[1], "task_stale"
				if target == "enqueued" {
					values = dispatchReceipt(t, s, rows)
					values[3], values[5], values[14] = "9223372036854775808", counters[1], counters[0]
				}
				dispatchReject(t, s, "", dispatchUpdate(values))
			}
		}
	}
	dispatchAssertFK(t, s)
}
