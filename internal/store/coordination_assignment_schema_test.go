package store

import (
	"crypto/rand"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

const (
	assignmentColumns        = "id,request_id,decision_id,task_id,group_id,worker_member_id,created_at"
	assignmentInsert         = "INSERT INTO coordination_assignments(" + assignmentColumns + ") VALUES(?,?,?,?,?,?,?)"
	assignmentStateColumns   = "assignment_id,task_id,group_id,worker_member_id,generation,status,attempt_count,next_attempt_at,updated_at,last_error_code,held_reason,message_id,delivery_id,conversation_id,delivered_member_id,materialized_from_generation,materialized_at"
	assignmentStateInsert    = "INSERT INTO coordination_assignment_dispatch(" + assignmentStateColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	assignmentBindingColumns = "assignment_id,group_id,task_id,worker_member_id,turn_id,attempt_id,task_revision,created_at"
	assignmentBindingInsert  = "INSERT INTO coordination_assignment_attempts(" + assignmentBindingColumns + ") VALUES(?,?,?,?,?,?,?,?)"
)

type assignmentSQLRows struct {
	request           dispatchSQLRows
	assignment, state []any
}

func assignmentV13Store(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "assignment-v13 ?#&_pragma=foreign_keys(0).sqlite")
	return assignmentV13Open(t, path), path
}

// Version-scoped schema tests use real Store.Open, but never apply later migrations.
// Like the existing migration fixtures, these helpers must not run in parallel.
func assignmentV13Open(t *testing.T, path string) *Store {
	t.Helper()
	original := migrations
	migrations = original[:13]
	defer func() { migrations = original }()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	messageSequenceLedger(t, s, 13)
	return s
}

func TestCoordinationAssignmentV13FixtureVersionBoundary(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	complete := append(slices.Clone(original), `CREATE TABLE assignment_v13_boundary_future(id TEXT);
	 INSERT INTO assignment_v13_boundary_missing VALUES(1);`)
	migrations = complete
	s, path := assignmentV13Store(t)
	second := assignmentV13Open(t, path)
	before := messageSequenceSnapshot(t, s)
	objects := messageSequenceRows(t, s.db, assignmentSchemaQuery)
	latest, err := Open(path)
	if err == nil {
		_ = latest.Close()
		t.Fatal("latest Store.Open swallowed the saved future migration failure")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("迁移版本 %d", len(complete))) || !strings.Contains(err.Error(), "assignment_v13_boundary_missing") {
		t.Fatalf("latest Store.Open did not run the complete future chain: %v", err)
	}
	for _, handle := range []*Store{s, second} {
		messageSequenceLedger(t, handle, 13)
		messageSequencePreserved(t, handle, before)
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
	}
	reopened := assignmentV13Open(t, path)
	messageSequencePreserved(t, reopened, before)
	if got := messageSequenceRows(t, reopened.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
		t.Fatal("failed future migration leaked through the v13 fixture boundary")
	}
	if !slices.Equal(migrations, complete) {
		t.Fatal("version-scoped handles did not restore the complete migration list")
	}
}

func assignmentCandidate(t *testing.T, s *Store, groupID, coordinator, worker, topic string) assignmentSQLRows {
	t.Helper()
	r := dispatchCandidate(t, s, groupID, coordinator, topic)
	coordinatorMustCommit(t, s, dispatchPair(r)...)
	id := rand.Text()
	return assignmentSQLRows{
		request:    r,
		assignment: []any{id, r.request[0], rand.Text(), r.request[3], groupID, worker, dispatchStamp},
		state:      []any{id, r.request[3], groupID, worker, int64(1), "pending", int64(0), dispatchStamp, dispatchStamp, "", "", nil, nil, nil, nil, nil, nil},
	}
}

func assignmentPair(r assignmentSQLRows) []coordinatorSQL {
	return []coordinatorSQL{{assignmentInsert, r.assignment}, {assignmentStateInsert, r.state}}
}

func assignmentStateUpdate(values []any) coordinatorSQL {
	var fields []string
	for _, column := range strings.Split(assignmentStateColumns, ",") {
		fields = append(fields, column+"=?")
	}
	return coordinatorSQL{"UPDATE coordination_assignment_dispatch SET " + strings.Join(fields, ",") + " WHERE assignment_id=?", append(slices.Clone(values), values[0])}
}

// This isolated fixture does not enable N3 runtime or supply N3a2 decision provenance.
func assignmentCounterfactual(t *testing.T, s *Store) {
	t.Helper()
	coordinatorMustCommit(t, s, coordinatorSQL{"DROP TRIGGER coordination_assignments_disabled", nil})
	t.Log("Counterfactual foundation only: removed the temporary Assignment INSERT block; all N2 guards remain intact")
}

func assignmentReceipt(t *testing.T, s *Store, r assignmentSQLRows) []any {
	t.Helper()
	m, err := s.CreateMessage(ctx, MessageRequest{GroupID: r.assignment[4].(string), ActorID: r.request.request[7].(string), TopicID: r.request.request[2].(string), Content: "明确的工作分配"}, []string{r.assignment[5].(string)})
	if err != nil {
		t.Fatal(err)
	}
	d := m.Deliveries[0]
	v := slices.Clone(r.state)
	v[4], v[5], v[9], v[10] = v[4].(int64)+1, "enqueued", "", ""
	v[11], v[12], v[13], v[14], v[15], v[16] = d.MessageID, d.ID, d.ConversationID, d.RecipientID, r.state[4], v[8]
	return v
}

// Historical v13 foundation sample: queue APIs and guard-checked historical
// Attempt insertion create parents, not current N3 business activation.
func assignmentBinding(t *testing.T, s *Store, r assignmentSQLRows) []any {
	t.Helper()
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.assignment[4].(string), DeliveryID: r.state[12].(string), MaxActive: 20})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	a, err := dispatchHistoricalAttempt(t, s, ActivateTaskAttemptInput{GroupID: turn.GroupID, TaskID: r.assignment[3].(string), ExpectedRevision: 1, ActorID: turn.MemberID, MemberScope: &scope})
	if err != nil {
		t.Fatal(err)
	}
	return []any{r.assignment[0], turn.GroupID, a.TaskID, turn.MemberID, turn.ID, a.ID, a.TaskRevision, dispatchStamp}
}

func assignmentEmpty(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"coordination_assignments", "coordination_assignment_dispatch", "coordination_assignment_attempts"} {
		if got := messageSequenceTableRows(t, s, table); len(got) != 0 {
			t.Fatalf("blocked production table retained business rows: %s: %v", table, got)
		}
	}
	dispatchAssertFK(t, s)
}

func TestCoordinationAssignmentSchemaDefaultBlockedTables(t *testing.T) {
	s, _ := assignmentV13Store(t)
	got := messageSequenceRows(t, s.db, `SELECT name FROM sqlite_master
	 WHERE type='table' AND name LIKE 'coordination_assignment%' ORDER BY name`)
	want := [][]any{{"coordination_assignment_attempts"}, {"coordination_assignment_dispatch"}, {"coordination_assignments"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated N3a1 must contain exactly three foundation tables: %v; want %v", got, want)
	}
	var gate string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger'
	 AND name='coordination_assignments_disabled' AND tbl_name='coordination_assignments'`).Scan(&gate); err != nil {
		t.Fatalf("explicit default INSERT block missing: %v", err)
	}
	if !strings.Contains(gate, "BEFORE INSERT") || !strings.Contains(gate, "RAISE(ABORT") {
		t.Fatalf("default block must abort INSERT, not rely on absent Go methods: %s", gate)
	}
	assignmentEmpty(t, s)
}

func TestCoordinationAssignmentSchemaDefaultExactForeignKeys(t *testing.T) {
	s, _ := assignmentV13Store(t)
	for _, edge := range []struct {
		table, target string
		columns       [][]any
	}{
		{"coordination_assignments", "coordination_dispatch_requests", [][]any{{"request_id", "id"}, {"task_id", "task_id"}, {"group_id", "group_id"}}},
		{"coordination_assignments", "members", [][]any{{"group_id", "group_id"}, {"worker_member_id", "id"}}},
		{"coordination_assignments", "coordination_assignment_dispatch", [][]any{{"id", "assignment_id"}, {"task_id", "task_id"}, {"group_id", "group_id"}, {"worker_member_id", "worker_member_id"}}},
		{"coordination_assignment_dispatch", "coordination_assignments", [][]any{{"assignment_id", "id"}, {"task_id", "task_id"}, {"group_id", "group_id"}, {"worker_member_id", "worker_member_id"}}},
		{"coordination_assignment_dispatch", "deliveries", [][]any{{"delivery_id", "id"}, {"group_id", "group_id"}, {"conversation_id", "conversation_id"}, {"delivered_member_id", "recipient_id"}}},
		{"coordination_assignment_attempts", "coordination_assignments", [][]any{{"assignment_id", "id"}, {"task_id", "task_id"}, {"group_id", "group_id"}, {"worker_member_id", "worker_member_id"}}},
		{"coordination_assignment_attempts", "turns", [][]any{{"turn_id", "id"}, {"group_id", "group_id"}, {"worker_member_id", "member_id"}}},
		{"coordination_assignment_attempts", "task_attempts", [][]any{{"attempt_id", "id"}, {"task_id", "task_id"}, {"group_id", "group_id"}, {"task_revision", "task_revision"}, {"worker_member_id", "actor_id"}}},
	} {
		got := messageSequenceRows(t, s.db, fmt.Sprintf(`SELECT "from","to" FROM pragma_foreign_key_list('%s') WHERE "table"='%s' ORDER BY seq`, edge.table, edge.target))
		if !reflect.DeepEqual(got, edge.columns) {
			t.Fatalf("exact %s -> %s: %v; want %v", edge.table, edge.target, got, edge.columns)
		}
	}
	for table, count := range map[string]int{"coordination_assignments": 3, "coordination_assignment_dispatch": 2, "coordination_assignment_attempts": 3} {
		got := messageSequenceRows(t, s.db, fmt.Sprintf(`SELECT DISTINCT id FROM pragma_foreign_key_list('%s')`, table))
		if len(got) != count {
			t.Fatalf("unexpected FK (including nonexistent decision provenance) on %s: %v", table, got)
		}
	}
	assignmentEmpty(t, s)
}

func TestCoordinationAssignmentSchemaDefaultBlockedWritesWithValidParents(t *testing.T) {
	s, path := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	r := assignmentCandidate(t, s, g, c, w, "blocked")
	// Even the orphan binding has real, exact v12 Turn/Attempt parents.
	r.state = assignmentReceipt(t, s, r)
	binding := assignmentBinding(t, s, r)
	for _, form := range []string{"INSERT INTO", "INSERT OR IGNORE INTO", "INSERT OR REPLACE INTO"} {
		t.Run(form, func(t *testing.T) {
			pair := assignmentPair(r)
			pair[0].query = strings.Replace(pair[0].query, "INSERT INTO", form, 1)
			dispatchReject(t, s, "工作分配尚未启用", pair...)
			// Use valid initial shape to prove the immediate parent FK, not a shape failure.
			state := slices.Clone(r.state)
			state[4], state[5] = int64(1), "pending"
			for i := 11; i < 17; i++ {
				state[i] = nil
			}
			dispatchReject(t, s, "FOREIGN KEY", coordinatorSQL{strings.Replace(assignmentStateInsert, "INSERT INTO", form, 1), state})
			dispatchReject(t, s, "", coordinatorSQL{strings.Replace(assignmentBindingInsert, "INSERT INTO", form, 1), binding})
		})
	}
	assignmentEmpty(t, s)
	before := messageSequenceSnapshot(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := assignmentV13Open(t, path)
	messageSequencePreserved(t, reopened, before)
	assignmentEmpty(t, reopened)
}

func TestCoordinationAssignmentSchemaCounterfactualPairRollback(t *testing.T) {
	s, _ := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	r := assignmentCandidate(t, s, g, c, w, "pair")
	assignmentCounterfactual(t, s)
	before := messageSequenceSnapshot(t, s)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(assignmentStateInsert, r.state...); err == nil {
		t.Fatal("child-first must fail immediately")
	} else {
		coordinationAdmissionRequireFK(t, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(assignmentInsert, r.assignment...); err != nil {
		t.Fatalf("parent-first must defer the exact intent FK: %v", err)
	}
	coordinationAdmissionRequireFK(t, tx.Commit())
	_ = tx.Rollback()
	messageSequencePreserved(t, s, before)
	coordinatorMustCommit(t, s, assignmentPair(r)...)
	dispatchAssertFK(t, s)
}

func TestCoordinationAssignmentSchemaCounterfactualExactIdentityAndSnapshots(t *testing.T) {
	s, path := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	other, oc, ow := dispatchConfiguredGroup(t, s)
	a := assignmentCandidate(t, s, g, c, w, "exact")
	b := assignmentCandidate(t, s, g, c, w, "other")
	foreign := assignmentCandidate(t, s, other, oc, ow, "exact")
	assignmentCounterfactual(t, s)
	for column, values := range map[string][]any{
		"request_id": {"missing", b.assignment[1], foreign.assignment[1]},
		"task_id":    {b.assignment[3], foreign.assignment[3]},
		"group_id":   {other}, "worker_member_id": {ow, "missing"},
	} {
		for _, value := range values {
			t.Run(column+"/"+fmt.Sprint(value), func(t *testing.T) {
				bad := a
				bad.assignment = dispatchSet(assignmentColumns, a.assignment, column, value)
				dispatchReject(t, s, "FOREIGN KEY", assignmentPair(bad)...)
			})
		}
	}
	for column, value := range map[string]any{"assignment_id": b.state[0], "task_id": b.state[1], "group_id": other, "worker_member_id": c} {
		bad := a
		bad.state = dispatchSet(assignmentStateColumns, a.state, column, value)
		dispatchReject(t, s, "FOREIGN KEY", assignmentPair(bad)...)
	}
	// DecisionID is opaque reserved provenance; no decision table exists at this stage.
	a.assignment[2] = "opaque-not-a-decision-row"
	coordinatorMustCommit(t, s, assignmentPair(a)...)
	before := map[string][][]any{}
	for _, table := range []string{"coordination_assignments", "coordination_assignment_dispatch", "coordination_dispatch_requests", "mandates"} {
		before[table] = messageSequenceTableRows(t, s, table)
	}
	task, err := s.GetTask(ctx, g, a.assignment[3].(string))
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := dispatchHistoricalAttempt(t, s, taskAttemptInput(task))
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
	coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE members SET status='left' WHERE id=?", []any{w}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := assignmentV13Open(t, path)
	messageSequencePreserved(t, reopened, before)
	got := messageSequenceRows(t, reopened.db, `SELECT r.task_revision,t.revision FROM coordination_assignments a
	 JOIN coordination_dispatch_requests r ON (r.id,r.task_id,r.group_id)=(a.request_id,a.task_id,a.group_id)
	 JOIN tasks t ON t.id=a.task_id`)
	if !reflect.DeepEqual(got, [][]any{{int64(1), int64(2)}}) {
		t.Fatalf("Assignment must derive immutable request snapshots, not current Task revision: %v", got)
	}
	if got := messageSequenceRows(t, reopened.db, fmt.Sprintf("SELECT id FROM coordination_assignments WHERE group_id='%s' AND id='%s'", other, a.assignment[0])); len(got) != 0 {
		t.Fatalf("exact scoped read leaked Assignment: %v", got)
	}
	dispatchAssertFK(t, reopened)
}
