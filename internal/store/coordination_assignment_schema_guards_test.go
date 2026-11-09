package store

import (
	"crypto/rand"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCoordinationAssignmentSchemaCounterfactualEveryUniqueVictim(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprint(recursive), func(t *testing.T) {
			s, _ := assignmentV13Store(t)
			g, c, w := dispatchConfiguredGroup(t, s)
			a := assignmentCandidate(t, s, g, c, w, "victims")
			b := assignmentCandidate(t, s, g, c, member(t, s, g).ID, "victims")
			assignmentCounterfactual(t, s)
			coordinatorMustCommit(t, s, assignmentPair(a)...)
			a.state = assignmentReceipt(t, s, a)
			coordinatorMustCommit(t, s, assignmentStateUpdate(a.state))
			dispatchRecursive(t, s, recursive)
			check := func(table, columns, insert, guard string, original, candidate []any, prefix []coordinatorSQL, wantKeys []string) {
				t.Helper()
				var keys []string
				for _, index := range messageSequenceRows(t, s.db, fmt.Sprintf(`SELECT name FROM pragma_index_list('%s') WHERE "unique"=1`, table)) {
					var names []string
					values := slices.Clone(candidate)
					for _, row := range messageSequenceRows(t, s.db, fmt.Sprintf("SELECT name FROM pragma_index_info('%s') ORDER BY seqno", index[0])) {
						column := row[0].(string)
						names = append(names, column)
						i := slices.Index(strings.Split(columns, ","), column)
						values[i] = original[i]
					}
					key := strings.Join(names, ",")
					keys = append(keys, key)
					for _, form := range []string{"INSERT INTO", "INSERT OR IGNORE INTO", "INSERT OR REPLACE INTO"} {
						t.Run(table+"/"+key+"/"+form, func(t *testing.T) {
							statements := append(slices.Clone(prefix), coordinatorSQL{strings.Replace(insert, "INSERT INTO", form, 1), values})
							dispatchReject(t, s, guard, statements...)
						})
					}
				}
				slices.Sort(keys)
				slices.Sort(wantKeys)
				if !slices.Equal(keys, wantKeys) {
					t.Fatalf("uncovered UNIQUE keys on %s: %v; want %v", table, keys, wantKeys)
				}
			}
			check("coordination_assignments", assignmentColumns, assignmentInsert, "分配不可替换", a.assignment, b.assignment, nil,
				[]string{"id", "request_id", "decision_id", "task_id", "id,task_id,group_id,worker_member_id"})
			check("coordination_assignment_dispatch", assignmentStateColumns, assignmentStateInsert, "状态不可替换", a.state, b.state,
				[]coordinatorSQL{{assignmentInsert, b.assignment}}, []string{"assignment_id", "message_id", "delivery_id", "assignment_id,task_id,group_id,worker_member_id"})
			coordinatorMustCommit(t, s, assignmentPair(b)...)
			bReceipt := assignmentReceipt(t, s, b)
			// Each mutable UNIQUE has a different existing victim; the old receipt must survive.
			for _, column := range []string{"assignment_id", "message_id", "delivery_id"} {
				values := dispatchSet(assignmentStateColumns, bReceipt, column, a.state[slices.Index(strings.Split(assignmentStateColumns, ","), column)])
				update := assignmentStateUpdate(values)
				update.query = strings.Replace(update.query, "UPDATE ", "UPDATE OR REPLACE ", 1)
				update.args[len(update.args)-1] = b.assignment[0]
				dispatchReject(t, s, "状态不可替换", update)
			}
			for _, column := range []string{"id", "request_id", "decision_id", "task_id"} {
				dispatchReject(t, s, "分配不可修改", coordinatorSQL{"UPDATE OR REPLACE coordination_assignments SET " + column + "=? WHERE id=?", []any{a.assignment[slices.Index(strings.Split(assignmentColumns, ","), column)], b.assignment[0]}})
			}
			coordinatorMustCommit(t, s, assignmentStateUpdate(bReceipt))
			b.state = bReceipt
			ab, bb := assignmentBinding(t, s, a), assignmentBinding(t, s, b)
			coordinatorMustCommit(t, s, coordinatorSQL{assignmentBindingInsert, ab})
			check("coordination_assignment_attempts", assignmentBindingColumns, assignmentBindingInsert, "绑定不可替换", ab, bb, nil,
				[]string{"assignment_id", "turn_id", "attempt_id"})
			coordinatorMustCommit(t, s, coordinatorSQL{assignmentBindingInsert, bb})
			for _, column := range []string{"assignment_id", "turn_id", "attempt_id"} {
				dispatchReject(t, s, "绑定不可修改", coordinatorSQL{"UPDATE OR REPLACE coordination_assignment_attempts SET " + column + "=? WHERE assignment_id=?", []any{ab[slices.Index(strings.Split(assignmentBindingColumns, ","), column)], bb[0]}})
			}
			for _, table := range []string{"coordination_assignments", "coordination_assignment_dispatch", "coordination_assignment_attempts"} {
				dispatchReject(t, s, "不可删除", coordinatorSQL{"DELETE FROM " + table, nil})
			}
			dispatchAssertFK(t, s)
		})
	}
}

func TestCoordinationAssignmentSchemaCounterfactualBindingExactJoins(t *testing.T) {
	s, _ := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	other, oc, ow := dispatchConfiguredGroup(t, s)
	a := assignmentCandidate(t, s, g, c, w, "binding")
	b := assignmentCandidate(t, s, other, oc, ow, "binding")
	assignmentCounterfactual(t, s)
	for _, r := range []*assignmentSQLRows{&a, &b} {
		coordinatorMustCommit(t, s, assignmentPair(*r)...)
		r.state = assignmentReceipt(t, s, *r)
		coordinatorMustCommit(t, s, assignmentStateUpdate(r.state))
	}
	ab, bb := assignmentBinding(t, s, a), assignmentBinding(t, s, b)
	for i, column := range strings.Split(assignmentBindingColumns, ",") {
		if column == "created_at" {
			continue
		}
		value := bb[i]
		if column == "task_revision" {
			value = int64(2)
		}
		dispatchReject(t, s, "", coordinatorSQL{assignmentBindingInsert, dispatchSet(assignmentBindingColumns, ab, column, value)})
	}
	// A matching actor/revision is insufficient: require the exact Attempt's Turn.
	otherDelivery := queueDelivery(t, s, g, w, "binding")
	const turnInsert = `INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,created_at,updated_at)
	 VALUES(?,?,?,?,?,?,'not_started',?,?)`
	otherTurn, otherAttempt := rand.Text(), rand.Text()
	coordinatorMustCommit(t, s,
		coordinatorSQL{turnInsert, []any{otherTurn, g, otherDelivery.ID, otherDelivery.ConversationID, w, rand.Text(), dispatchStamp, dispatchStamp}},
		coordinatorSQL{`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at) VALUES(?,?,?,1,?,?,?)`, []any{otherAttempt, a.assignment[3], g, w, otherTurn, dispatchStamp}},
	)
	dispatchReject(t, s, "绑定关联无效", coordinatorSQL{assignmentBindingInsert, dispatchSet(assignmentBindingColumns, ab, "attempt_id", otherAttempt)})
	dispatchReject(t, s, "绑定关联无效", coordinatorSQL{assignmentBindingInsert, dispatchSet(assignmentBindingColumns, ab, "turn_id", otherTurn)})
	// These parent changes are not yet fenced globally; insertion must still check both pointers.
	for _, mutation := range []coordinatorSQL{
		{"UPDATE turns SET delivery_id=? WHERE id=?", []any{otherDelivery.ID, ab[4]}},
		{"UPDATE deliveries SET turn_id=? WHERE id=?", []any{otherTurn, a.state[12]}},
	} {
		dispatchReject(t, s, "绑定关联无效", mutation, coordinatorSQL{assignmentBindingInsert, ab})
	}
	// Capture revision 1 cannot be rebound to an otherwise valid revision-2 Attempt.
	newAttempt := rand.Text()
	dispatchReject(t, s, "绑定关联无效",
		coordinatorSQL{"UPDATE tasks SET active_attempt_id=NULL,revision=2 WHERE id=?", []any{a.assignment[3]}},
		coordinatorSQL{`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at) VALUES(?,?,?,2,?,?,?)`, []any{newAttempt, a.assignment[3], g, w, ab[4], dispatchStamp}},
		coordinatorSQL{assignmentBindingInsert, dispatchSet(assignmentBindingColumns, dispatchSet(assignmentBindingColumns, ab, "attempt_id", newAttempt), "task_revision", int64(2))},
	)
	coordinatorMustCommit(t, s, coordinatorSQL{assignmentBindingInsert, ab})
	dispatchAssertFK(t, s)
}

func TestCoordinationAssignmentSchemaCounterfactualScopedDueIndex(t *testing.T) {
	s, _ := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	other, oc, ow := dispatchConfiguredGroup(t, s)
	assignmentCounterfactual(t, s)
	var candidates []assignmentSQLRows
	for i := range 4 {
		groupID, coordinator, worker := g, c, w
		if i == 3 {
			groupID, coordinator, worker = other, oc, ow
		}
		r := assignmentCandidate(t, s, groupID, coordinator, worker, "due")
		r.assignment[0], r.state[0] = fmt.Sprintf("assignment-due-%d", i), fmt.Sprintf("assignment-due-%d", i)
		coordinatorMustCommit(t, s, assignmentPair(r)...)
		candidates = append(candidates, r)
	}
	held := slices.Clone(candidates[2].state)
	held[4], held[5], held[10] = int64(2), "held", "worker_inactive"
	coordinatorMustCommit(t, s, assignmentStateUpdate(held))
	for _, query := range []string{
		`SELECT assignment_id FROM coordination_assignment_dispatch WHERE status='pending' AND next_attempt_at<=? ORDER BY next_attempt_at,assignment_id LIMIT 2`,
		`SELECT assignment_id FROM coordination_assignment_dispatch WHERE status='pending' AND next_attempt_at<=? AND (next_attempt_at,assignment_id)>(?,'assignment-due-1') ORDER BY next_attempt_at,assignment_id LIMIT 2`,
	} {
		args := []any{dispatchStamp}
		if strings.Contains(query, ")>(?") {
			args = append(args, dispatchStamp)
		}
		plan, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var details []string
		for plan.Next() {
			var id, parent, unused int
			var detail string
			if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		if err := plan.Err(); err != nil {
			t.Fatal(err)
		}
		plan.Close()
		detail := strings.Join(details, "\n")
		t.Log(detail)
		if !strings.Contains(detail, "SEARCH ") || !strings.Contains(detail, "INDEX coordination_assignment_pending_due") || strings.Contains(detail, "SCAN ") || strings.Contains(detail, "TEMP B-TREE") {
			t.Fatalf("due query must use the new-table range index without sorting: %s", detail)
		}
		rows, err := s.db.Query(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		want := []string{"assignment-due-0", "assignment-due-1"}
		if len(args) == 2 {
			want = []string{"assignment-due-3"}
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("SQL-only LIMIT/keyset and held exclusion: %v; want %v", ids, want)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM coordination_assignments a JOIN coordination_assignment_dispatch s
	 ON (s.assignment_id,s.task_id,s.group_id,s.worker_member_id)=(a.id,a.task_id,a.group_id,a.worker_member_id)
	 WHERE a.id=? AND a.group_id=?`, candidates[0].assignment[0], other).Scan(&count); err != nil || count != 0 {
		t.Fatalf("exact cross-group read leaked: %d, %v", count, err)
	}
	dispatchAssertFK(t, s)
}

func TestCoordinationAssignmentSchemaCounterfactualTwoHandleIdentityConcurrency(t *testing.T) {
	s, path := assignmentV13Store(t)
	observer := assignmentV13Open(t, path)
	g, c, w := dispatchConfiguredGroup(t, s)
	a := assignmentCandidate(t, s, g, c, w, "race")
	b := a
	b.assignment, b.state = slices.Clone(a.assignment), slices.Clone(a.state)
	b.assignment[0], b.assignment[2] = rand.Text(), rand.Text()
	b.state[0] = b.assignment[0]
	assignmentCounterfactual(t, s)
	before := messageSequenceSnapshot(t, s)
	start, results := make(chan struct{}), make(chan error, 2)
	for i, handle := range []*Store{s, observer} {
		r := []assignmentSQLRows{a, b}[i]
		go func() {
			<-start
			results <- coordinatorCommit(handle, assignmentPair(r)...)
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-results; err == nil {
			winners++
		} else if !strings.Contains(err.Error(), "分配不可替换") {
			t.Errorf("loser must hit durable identity guard, not unrelated contention: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("same request/Task admitted %d Assignments", winners)
	}
	for _, table := range []string{"coordination_assignments", "coordination_assignment_dispatch"} {
		delete(before, table)
	}
	messageSequencePreserved(t, observer, before)
	got := messageSequenceRows(t, observer.db, `SELECT a.id,s.assignment_id,s.generation FROM coordination_assignments a
	 JOIN coordination_assignment_dispatch s ON (s.assignment_id,s.task_id,s.group_id,s.worker_member_id)=(a.id,a.task_id,a.group_id,a.worker_member_id)`)
	if len(got) != 1 || got[0][0] != got[0][1] || got[0][2] != int64(1) {
		t.Fatalf("loser leaked a half or replaced winner: %v", got)
	}
	dispatchAssertFK(t, observer)
}

func TestCoordinationAssignmentSchemaCounterfactualTwoHandleReceiptConcurrency(t *testing.T) {
	s, path := assignmentV13Store(t)
	observer := assignmentV13Open(t, path)
	g, c, w := dispatchConfiguredGroup(t, s)
	r := assignmentCandidate(t, s, g, c, w, "receipt-race")
	assignmentCounterfactual(t, s)
	coordinatorMustCommit(t, s, assignmentPair(r)...)
	receipts := [][]any{assignmentReceipt(t, s, r), assignmentReceipt(t, s, r)}
	type result struct {
		rows int64
		err  error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for i, handle := range []*Store{s, observer} {
		statement := assignmentStateUpdate(receipts[i])
		statement.query += " AND generation=1"
		go func() {
			<-start
			tx, err := handle.db.BeginTx(ctx, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer tx.Rollback()
			r, err := tx.Exec(statement.query, statement.args...)
			if err != nil {
				results <- result{err: err}
				return
			}
			n, err := r.RowsAffected()
			if err == nil {
				err = tx.Commit()
			}
			results <- result{n, err}
		}()
	}
	close(start)
	var counts []int64
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		counts = append(counts, r.rows)
	}
	slices.Sort(counts)
	if !slices.Equal(counts, []int64{0, 1}) {
		t.Fatalf("receipt CAS winners: %v", counts)
	}
	got := messageSequenceTableRows(t, observer, "coordination_assignment_dispatch")
	if !reflect.DeepEqual(got, [][]any{receipts[0]}) && !reflect.DeepEqual(got, [][]any{receipts[1]}) {
		t.Fatalf("race did not preserve one exact six-field receipt: %v", got)
	}
	dispatchAssertFK(t, observer)
}
