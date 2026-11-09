package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func resolutionStateUpdate(r resolutionRows, values []any, decision any) coordinatorSQL {
	update := dispatchUpdate(values)
	update.query = strings.Replace(update.query, " WHERE request_id=?", ",decision_id=? WHERE request_id=?", 1)
	update.args = append(slices.Clone(values), decision, r.decision[1])
	return update
}

func TestCoordinationAssignmentResolutionV14ResolutionKeepsExactState(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := resolutionStore(t)
			r := resolutionCandidate(t, s, kind)
			statements := resolutionStatements(r)
			prefix := statements[:len(statements)-1]
			valid := slices.Clone(r.request.state)
			valid[3] = valid[3].(int64) + 1
			valid[4] = map[string]string{"assign": "assigned", "decline": "declined"}[kind]
			for i, column := range strings.Split(dispatchStateColumns, ",") {
				if column == "updated_at" {
					continue // Canonical resolution time is new evidence, not part of the original receipt.
				}
				var value any = "changed"
				switch column {
				case "generation", "attempt_count":
					value = valid[i].(int64) + 1
				case "status":
					value = map[string]string{"assign": "declined", "decline": "assigned"}[kind]
				case "next_attempt_at", "materialized_at":
					value = "2026-01-02T03:04:06.123456789Z"
				case "materialized_from_generation":
					value = int64(2)
				}
				t.Run(column, func(t *testing.T) {
					bad := slices.Clone(valid)
					bad[i] = value
					dispatchReject(t, s, "", append(slices.Clone(prefix), resolutionStateUpdate(r, bad, r.decision[0]))...)
				})
			}
			for _, pointer := range []any{nil, "", rand.Text(), []byte("blob")} {
				dispatchReject(t, s, "", append(slices.Clone(prefix), resolutionStateUpdate(r, valid, pointer))...)
			}
			if kind == "assign" {
				// The resolution needs the initial pending intent, not an already deferred/materialized one.
				deferred := slices.Clone(r.state)
				deferred[4], deferred[5], deferred[10] = int64(2), "held", "worker_inactive"
				bad := append(slices.Clone(prefix), assignmentStateUpdate(deferred), statements[len(statements)-1])
				dispatchReject(t, s, "决定关联", bad...)
			}
			coordinatorMustCommit(t, s, statements...)
			for _, status := range []string{"pending", "enqueued", "held", "assigned", "declined"} {
				dispatchReject(t, s, "冻结状态", coordinatorSQL{"UPDATE coordination_dispatch_state SET generation=generation+1,status=? WHERE request_id=?", []any{status, r.decision[1]}})
			}
			dispatchReject(t, s, "不可删除", coordinatorSQL{"DELETE FROM coordination_dispatch_state", nil})
		})
	}
}

func TestCoordinationAssignmentResolutionV14ResolutionGenerationOverflow(t *testing.T) {
	for _, generation := range []int64{9223372036854775806, 9223372036854775807} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			s, _ := resolutionStore(t)
			r := resolutionCandidate(t, s, "decline")
			t.Log("Counterfactual unreachable generation boundary, with every production guard restored before resolution")
			dispatchSeedCounters(t, s, r.decision[1], generation, 0)
			if generation == 9223372036854775807 {
				dispatchReject(t, s, "世代", resolutionStatements(r)...)
			} else {
				coordinatorMustCommit(t, s, resolutionStatements(r)...)
			}
		})
	}
}

func TestCoordinationAssignmentResolutionV14SameMemberOrdinaryTurnCannotDecide(t *testing.T) {
	s, _ := resolutionStore(t)
	r := resolutionCandidate(t, s, "decline")
	d := queueDelivery(t, s, r.turn.GroupID, r.turn.MemberID, "ordinary")
	id := rand.Text()
	coordinatorMustCommit(t, s, resolutionOrdinaryTurn(r.turn.GroupID, d, id, "not_started"), coordinatorSQL{"UPDATE deliveries SET turn_id=? WHERE id=?", []any{id, d.ID}})
	r.decision = dispatchSet(resolutionDecisionColumns, r.decision, "coordinator_turn_id", id)
	dispatchReject(t, s, "决定来源", resolutionStatements(r)...)
}

func resolutionCommitContext(ctx context.Context, s *Store, statements []coordinatorSQL) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func TestCoordinationAssignmentResolutionV14TwoHandleTaskExclusion(t *testing.T) {
	for _, mode := range []string{"second-decision", "new-request"} {
		t.Run(mode, func(t *testing.T) {
			s, path := resolutionStore(t)
			other := resolutionOpen(t, path)
			r := resolutionCandidate(t, s, "assign")
			first := resolutionStatements(r)
			var second []coordinatorSQL
			if mode == "second-decision" {
				alternate := r
				alternate.decision = slices.Clone(r.decision)
				alternate.assignment = slices.Clone(r.assignment)
				alternate.state = slices.Clone(r.state)
				alternate.decision[0], alternate.decision[8] = rand.Text(), rand.Text()
				alternate.assignment[0], alternate.assignment[2], alternate.state[0] = alternate.decision[8], alternate.decision[0], alternate.decision[8]
				second = resolutionStatements(alternate)
			} else {
				request := dispatchSQLRows{request: slices.Clone(r.request.request), state: slices.Clone(r.request.state)}
				request.request[0] = rand.Text()
				request.state = []any{request.request[0], request.request[3], request.request[1], int64(1), "pending", int64(0), dispatchStamp, dispatchStamp, "", "", nil, nil, nil, nil, nil, nil}
				second = dispatchPair(request)
			}
			bounded, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			start, results := make(chan struct{}), make(chan error, 2)
			for i, handle := range []*Store{s, other} {
				statements := [][]coordinatorSQL{first, second}[i]
				go func() {
					<-start
					results <- resolutionCommitContext(bounded, handle, statements)
				}()
			}
			close(start)
			winners := 0
			for range 2 {
				select {
				case err := <-results:
					if err == nil {
						winners++
					} else if !strings.Contains(err.Error(), "不可替换") && !strings.Contains(err.Error(), "所有权") {
						t.Errorf("unexpected race failure, not a durable ownership rejection: %v", err)
					}
				case <-bounded.Done():
					t.Fatalf("%s Task exclusion exceeded 30s: %v", mode, bounded.Err())
				}
			}
			if winners != 1 {
				t.Fatalf("%s produced %d winners", mode, winners)
			}
			rows := messageSequenceRows(t, s.db, `SELECT a.id,s.status,w.status FROM coordination_assignments a
			 JOIN coordination_dispatch_state s ON s.request_id=a.request_id
			 JOIN coordination_assignment_dispatch w ON w.assignment_id=a.id`)
			if len(rows) != 1 || rows[0][1] != "assigned" || rows[0][2] != "pending" {
				t.Fatalf("Task exclusion left partial ownership: %v", rows)
			}
			dispatchAssertFK(t, s)
		})
	}
}
