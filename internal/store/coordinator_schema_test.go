package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

const (
	coordinatorBindingRevisionInsert = "INSERT INTO group_coordinator_binding_revisions(group_id,revision,member_id) VALUES(?,?,?)"
	coordinatorPolicyRevisionInsert  = "INSERT INTO group_dispatch_policy_revisions(group_id,revision,allowed_worker_ids) VALUES(?,?,?)"
	coordinatorBindingInsert         = "INSERT INTO group_coordinator_bindings(group_id,revision) VALUES(?,?)"
	coordinatorPolicyInsert          = "INSERT INTO group_dispatch_policies(group_id,revision) VALUES(?,?)"
)

type coordinatorSQL struct {
	query string
	args  []any
}

// Exercise schema statements directly, not future Store mutation semantics.
func coordinatorCommit(s *Store, statements ...coordinatorSQL) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, err := tx.Exec(statement.query, statement.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func coordinatorMustCommit(t *testing.T, s *Store, statements ...coordinatorSQL) {
	t.Helper()
	if err := coordinatorCommit(s, statements...); err != nil {
		t.Fatal(err)
	}
}

func coordinatorReject(t *testing.T, s *Store, statements ...coordinatorSQL) {
	t.Helper()
	before := messageSequenceSnapshot(t, s)
	if err := coordinatorCommit(s, statements...); err == nil {
		t.Fatal("invalid configuration committed")
	}
	messageSequencePreserved(t, s, before)
}

func coordinatorOtherRows(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	rows := messageSequenceSnapshot(t, s)
	for _, table := range []string{"group_coordinator_binding_revisions", "group_dispatch_policy_revisions", "group_coordinator_bindings", "group_dispatch_policies"} {
		delete(rows, table)
	}
	return rows
}

func coordinatorEmpty(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"group_coordinator_binding_revisions", "group_dispatch_policy_revisions", "group_coordinator_bindings", "group_dispatch_policies"} {
		if rows := messageSequenceRows(t, s.db, "SELECT * FROM "+table); len(rows) != 0 {
			t.Fatalf("configuration must not be auto-created: %s: %v", table, rows)
		}
	}
}

func TestCoordinatorSchemaTypedTables(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	m := member(t, s, g.ID)
	coordinatorEmpty(t, s)
	before := coordinatorOtherRows(t, s)
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 1, m.ID}},
		coordinatorSQL{coordinatorBindingInsert, []any{g.ID, 1}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, "[]"}},
		coordinatorSQL{coordinatorPolicyInsert, []any{g.ID, 1}},
	)
	var binding domain.GroupCoordinatorBinding
	if err := s.db.QueryRow(`SELECT b.group_id,r.member_id,b.revision FROM group_coordinator_bindings b
	 JOIN group_coordinator_binding_revisions r USING(group_id,revision)`).Scan(&binding.GroupID, &binding.MemberID, &binding.Revision); err != nil {
		t.Fatal(err)
	}
	var policy domain.GroupDispatchPolicy
	var workers string
	if err := s.db.QueryRow(`SELECT p.group_id,p.revision,r.allowed_worker_ids FROM group_dispatch_policies p
	 JOIN group_dispatch_policy_revisions r USING(group_id,revision)`).Scan(&policy.GroupID, &policy.Revision, &workers); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(workers), &policy.AllowedWorkerIDs); err != nil {
		t.Fatal(err)
	}
	for _, value := range []struct {
		value any
		want  string
	}{
		{binding, fmt.Sprintf(`{"group_id":%q,"member_id":%q,"revision":1}`, g.ID, m.ID)},
		{policy, fmt.Sprintf(`{"group_id":%q,"revision":1,"allowed_worker_ids":[]}`, g.ID)},
	} {
		got, err := json.Marshal(value.value)
		if err != nil || string(got) != value.want {
			t.Fatalf("typed public value: %s, %v; want %s", got, err, value.want)
		}
	}
	messageSequencePreserved(t, s, before)
}

func TestCoordinatorSchemaRevisionsAndJSON(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	m := member(t, s, g.ID)
	for _, revision := range []any{nil, 0, -1, 1.5, "bad", []byte{1}} {
		for _, statement := range []coordinatorSQL{
			{coordinatorBindingRevisionInsert, []any{g.ID, revision, m.ID}},
			{coordinatorPolicyRevisionInsert, []any{g.ID, revision, "[]"}},
		} {
			t.Run(fmt.Sprintf("%s/%v", strings.Fields(statement.query)[2], revision), func(t *testing.T) {
				coordinatorReject(t, s, statement)
			})
		}
	}
	for _, workers := range []any{nil, "", "not-json", "null", "{}", `"id"`, "1", "true", `[null]`, `[1]`, `[1.5]`, `[true]`, `[{}]`, `[[]]`, `["valid",null]`, `[""]`, `[" "]`, `["same","same"]`, `["\u0061","a"]`, []byte("[]")} {
		t.Run(fmt.Sprintf("workers/%v", workers), func(t *testing.T) {
			coordinatorReject(t, s, coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, workers}})
		})
	}
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 1, m.ID}},
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 2, m.ID}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, "[]"}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 2, "[]"}},
	)
	for _, insert := range []string{coordinatorBindingInsert, coordinatorPolicyInsert} {
		for _, revision := range []any{nil, 0, -1, 1.5, "bad", []byte{1}, 2} {
			coordinatorReject(t, s, coordinatorSQL{insert, []any{g.ID, revision}})
		}
	}
}

func TestCoordinatorSchemaExactReferencesAndActiveMembers(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	active, left, foreign := member(t, s, g.ID), member(t, s, g.ID), member(t, s, other.ID)
	coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE members SET status='left' WHERE id=?", []any{left.ID}})
	for _, pair := range [][2]string{{g.ID, foreign.ID}, {other.ID, active.ID}, {"missing", active.ID}, {g.ID, "missing"}} {
		before := messageSequenceSnapshot(t, s)
		coordinationAdmissionRequireFK(t, coordinatorCommit(s, coordinatorSQL{coordinatorBindingRevisionInsert, []any{pair[0], 1, pair[1]}}))
		messageSequencePreserved(t, s, before)
	}
	coordinationAdmissionRequireFK(t, coordinatorCommit(s, coordinatorSQL{coordinatorPolicyRevisionInsert, []any{"missing", 1, "[]"}}))
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{other.ID, 1, foreign.ID}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{other.ID, 1, "[]"}},
	)
	// Only the other group has revision 1; a revision number alone is never a reference.
	coordinatorReject(t, s, coordinatorSQL{coordinatorBindingInsert, []any{g.ID, 1}})
	coordinatorReject(t, s, coordinatorSQL{coordinatorPolicyInsert, []any{g.ID, 1}})
	coordinatorReject(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 1, left.ID}},
		coordinatorSQL{coordinatorBindingInsert, []any{g.ID, 1}},
	)
	for _, worker := range []string{left.ID, foreign.ID, "missing", "*", "coordinator", "worker"} {
		coordinatorReject(t, s,
			coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, fmt.Sprintf("[%q]", worker)}},
			coordinatorSQL{coordinatorPolicyInsert, []any{g.ID, 1}},
		)
	}
	for _, reference := range []struct {
		table, target string
		columns       [][]any
	}{
		{"group_coordinator_binding_revisions", "members", [][]any{{"group_id", "group_id"}, {"member_id", "id"}}},
		{"group_dispatch_policy_revisions", "groups", [][]any{{"group_id", "id"}}},
		{"group_coordinator_bindings", "group_coordinator_binding_revisions", [][]any{{"group_id", "group_id"}, {"revision", "revision"}}},
		{"group_dispatch_policies", "group_dispatch_policy_revisions", [][]any{{"group_id", "group_id"}, {"revision", "revision"}}},
	} {
		query := fmt.Sprintf(`SELECT "from","to" FROM pragma_foreign_key_list('%s') WHERE "table"='%s' ORDER BY seq`, reference.table, reference.target)
		if got := messageSequenceRows(t, s.db, query); !reflect.DeepEqual(got, reference.columns) {
			t.Fatalf("%s must reference the exact identity: %v; want %v", reference.table, got, reference.columns)
		}
	}
}

func TestCoordinatorSchemaCrossPolicyTransactions(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, policyFirst := range []bool{false, true} {
			for _, replace := range []bool{false, true} {
				t.Run(fmt.Sprintf("existing=%t/policy_first=%t/replace=%t", existing, policyFirst, replace), func(t *testing.T) {
					s, _ := openTestStore(t)
					g := group(t, s)
					a, b, candidate := member(t, s, g.ID), member(t, s, g.ID), member(t, s, g.ID)
					revision := 1
					if existing {
						coordinatorMustCommit(t, s,
							coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 1, a.ID}},
							coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, fmt.Sprintf("[%q]", b.ID)}},
							coordinatorSQL{coordinatorBindingInsert, []any{g.ID, 1}},
							coordinatorSQL{coordinatorPolicyInsert, []any{g.ID, 1}},
						)
						revision++
					}
					binding, policy := coordinatorBindingInsert, coordinatorPolicyInsert
					if existing && !replace {
						binding = "UPDATE group_coordinator_bindings SET revision=?2 WHERE group_id=?1"
						policy = "UPDATE group_dispatch_policies SET revision=?2 WHERE group_id=?1"
					} else if replace {
						binding = strings.Replace(binding, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
						policy = strings.Replace(policy, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
					}
					pointers := []coordinatorSQL{{binding, []any{g.ID, revision}}, {policy, []any{g.ID, revision}}}
					if policyFirst {
						pointers[0], pointers[1] = pointers[1], pointers[0]
					}
					statements := []coordinatorSQL{
						{coordinatorBindingRevisionInsert, []any{g.ID, revision, candidate.ID}},
						{coordinatorPolicyRevisionInsert, []any{g.ID, revision, fmt.Sprintf("[%q]", candidate.ID)}},
					}
					coordinatorReject(t, s, append(statements, pointers...)...)
				})
			}
		}
	}
}

func TestCoordinatorSchemaConcurrentCrossPolicyWrites(t *testing.T) {
	s, path := openTestStore(t)
	other := queueHandle(t, path, false)
	g := group(t, s)
	m := member(t, s, g.ID)
	before := coordinatorOtherRows(t, s)
	statements := [][]coordinatorSQL{
		{{coordinatorBindingRevisionInsert, []any{g.ID, 1, m.ID}}, {coordinatorBindingInsert, []any{g.ID, 1}}},
		{{coordinatorPolicyRevisionInsert, []any{g.ID, 1, fmt.Sprintf("[%q]", m.ID)}}, {coordinatorPolicyInsert, []any{g.ID, 1}}},
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for i, handle := range []*Store{s, other} {
		go func() {
			<-start
			results <- coordinatorCommit(handle, statements[i]...)
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-results; err == nil {
			winners++
		} else if !strings.Contains(err.Error(), "当前") {
			t.Errorf("loser must fail the current configuration gate, not database contention: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("overlapping configurations had %d winners; want 1", winners)
	}
	for _, query := range []string{
		"SELECT (SELECT count(*) FROM group_coordinator_bindings)+(SELECT count(*) FROM group_dispatch_policies)",
		"SELECT (SELECT count(*) FROM group_coordinator_binding_revisions)+(SELECT count(*) FROM group_dispatch_policy_revisions)",
	} {
		var count int
		if err := s.db.QueryRow(query).Scan(&count); err != nil || count != 1 {
			t.Fatalf("losing transaction leaked a pointer or history: %d, %v", count, err)
		}
	}
	messageSequencePreserved(t, s, before)
}

func TestCoordinatorSchemaSwitchViaEmptyPolicy(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	a, b := member(t, s, g.ID), member(t, s, g.ID)
	before := coordinatorOtherRows(t, s)
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 1, a.ID}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, fmt.Sprintf("[%q]", b.ID)}},
		coordinatorSQL{coordinatorBindingInsert, []any{g.ID, 1}},
		coordinatorSQL{coordinatorPolicyInsert, []any{g.ID, 1}},
	)
	coordinatorReject(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 2, b.ID}},
		coordinatorSQL{"UPDATE group_coordinator_bindings SET revision=2 WHERE group_id=?", []any{g.ID}},
	)
	coordinatorReject(t, s,
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 2, fmt.Sprintf("[%q]", a.ID)}},
		coordinatorSQL{"UPDATE group_dispatch_policies SET revision=2 WHERE group_id=?", []any{g.ID}},
	)
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 2, b.ID}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 2, "[]"}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 3, fmt.Sprintf("[%q]", a.ID)}},
		coordinatorSQL{"UPDATE group_dispatch_policies SET revision=2 WHERE group_id=?", []any{g.ID}},
		coordinatorSQL{"UPDATE group_coordinator_bindings SET revision=2 WHERE group_id=?", []any{g.ID}},
		coordinatorSQL{"UPDATE group_dispatch_policies SET revision=3 WHERE group_id=?", []any{g.ID}},
	)
	if got := messageSequenceRows(t, s.db, "SELECT revision,member_id FROM group_coordinator_binding_revisions ORDER BY revision"); !reflect.DeepEqual(got, [][]any{{int64(1), a.ID}, {int64(2), b.ID}}) {
		t.Fatalf("monotonic switch lost history: %v", got)
	}
	if rows := messageSequenceRows(t, s.db, "PRAGMA foreign_key_check"); len(rows) != 0 {
		t.Fatalf("configuration references are invalid: %v", rows)
	}
	messageSequencePreserved(t, s, before)
}
