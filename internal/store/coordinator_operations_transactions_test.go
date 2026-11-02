package store

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCoordinatorOperationsConcurrentCASAcrossHandles(t *testing.T) {
	for _, policy := range []bool{false, true} {
		for _, expected := range []int64{0, 1} {
			t.Run(fmt.Sprintf("policy=%t/revision=%d", policy, expected), func(t *testing.T) {
				f := setupMessages(t)
				second := queueHandle(t, f.path, false)
				if expected == 1 {
					if _, err := coordinatorOperationWrite(f.s, f.groupID, f.a, 0, "seed", policy); err != nil {
						t.Fatal(err)
					}
				}
				type result struct {
					index int
					value any
					err   error
				}
				start, results := make(chan struct{}), make(chan result, 2)
				ids := []string{f.a, f.b}
				for i, handle := range []*Store{f.s, second} {
					go func() {
						<-start
						value, err := coordinatorOperationWrite(handle, f.groupID, ids[i], expected, "race-"+strconv.Itoa(i), policy)
						results <- result{i, value, err}
					}()
				}
				close(start)
				outcomes := []result{<-results, <-results}
				winner, loser := -1, -1
				var saved any
				for _, r := range outcomes {
					if r.err != nil {
						coordinatorOperationRequireZero(t, r.value, r.err, ErrConflict)
						loser = r.index
					} else {
						if winner != -1 {
							t.Fatal("two CAS writers committed")
						}
						winner, saved = r.index, r.value
					}
				}
				if winner == -1 || loser == -1 {
					t.Fatalf("expected exactly one eligible writer: %+v", outcomes)
				}
				bindings, policies := int(expected+1), 0
				if policy {
					bindings, policies = 0, bindings
				}
				coordinatorOperationCounts(t, second, bindings, policies, int(expected+1))
				if got, err := coordinatorOperationRead(second, f.groupID, policy); err != nil || !reflect.DeepEqual(got, saved) {
					t.Fatalf("winner did not become the exact current projection: %+v, %v", got, err)
				}
				updated, err := coordinatorOperationWrite(second, f.groupID, ids[loser], expected+1, "race-"+strconv.Itoa(loser), policy)
				if err != nil {
					t.Fatalf("losing CAS burned its key: %v", err)
				}
				if got, err := coordinatorOperationRead(second, f.groupID, policy); err != nil || !reflect.DeepEqual(got, updated) {
					t.Fatalf("successful retry did not advance current projection: %+v, %v", got, err)
				}
				if got, err := coordinatorOperationWrite(second, f.groupID, ids[winner], expected, "race-"+strconv.Itoa(winner), policy); err != nil || !reflect.DeepEqual(got, saved) {
					t.Fatalf("concurrent winner lost historical receipt: %+v, %v", got, err)
				}
				if rows := messageSequenceRows(t, second.db, "PRAGMA foreign_key_check"); len(rows) != 0 {
					t.Fatalf("concurrent CAS broke references: %v", rows)
				}
			})
		}
	}
}

func TestCoordinatorOperationsConcurrentIdenticalReplay(t *testing.T) {
	for _, policy := range []bool{false, true} {
		t.Run(strconv.FormatBool(policy), func(t *testing.T) {
			f := setupMessages(t)
			second := queueHandle(t, f.path, false)
			type result struct {
				value any
				err   error
			}
			start, results := make(chan struct{}), make(chan result, 2)
			for _, handle := range []*Store{f.s, second} {
				go func() {
					<-start
					value, err := coordinatorOperationWrite(handle, f.groupID, f.a, 0, "same-key", policy)
					results <- result{value, err}
				}()
			}
			close(start)
			a, b := <-results, <-results
			if a.err != nil || b.err != nil || !reflect.DeepEqual(a.value, b.value) {
				t.Fatalf("identical concurrent retries diverged: %+v vs %+v", a, b)
			}
			bindings, policies := 1, 0
			if policy {
				bindings, policies = 0, 1
			}
			coordinatorOperationCounts(t, second, bindings, policies, 1)
		})
	}
}

func TestCoordinatorOperationsConcurrentCrossSetterExclusion(t *testing.T) {
	for _, expected := range []int64{0, 1} {
		t.Run(strconv.FormatInt(expected, 10), func(t *testing.T) {
			f := setupMessages(t)
			second := queueHandle(t, f.path, false)
			target := member(t, f.s, f.groupID).ID
			if expected == 1 {
				if _, err := f.s.SetCoordinator(ctx, f.groupID, f.a, 0, "seed-binding"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 0, "seed-policy"); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				policy bool
				value  any
				err    error
			}
			start, results := make(chan struct{}), make(chan result, 2)
			for i, handle := range []*Store{f.s, second} {
				go func() {
					<-start
					value, err := coordinatorOperationWrite(handle, f.groupID, target, expected, "cross", i == 1)
					results <- result{i == 1, value, err}
				}()
			}
			close(start)
			outcomes := []result{<-results, <-results}
			successes, conflicts := 0, 0
			bindings, policies := int(expected), int(expected)
			for _, r := range outcomes {
				if r.err != nil {
					coordinatorOperationRequireZero(t, r.value, r.err, ErrConflict)
					conflicts++
					operation := "set_coordinator"
					if r.policy {
						operation = "set_dispatch_policy"
					}
					var count int
					if err := second.db.QueryRow("SELECT count(*) FROM mutation_idempotency WHERE operation=? AND idempotency_key='cross'", operation).Scan(&count); err != nil || count != 0 {
						t.Fatalf("losing cross setter left a receipt: %d, %v", count, err)
					}
				} else {
					successes++
					if r.policy {
						policies++
					} else {
						bindings++
					}
				}
			}
			if successes != 1 || conflicts != 1 {
				t.Fatalf("cross setters were not serialized: %+v", outcomes)
			}
			coordinatorOperationCounts(t, second, bindings, policies, int(2*expected+1))
			binding, err := second.GetCoordinator(ctx, f.groupID)
			if bindings == 0 {
				coordinatorOperationRequireZero(t, binding, err, ErrNotFound)
			} else if err != nil || binding.Revision != int64(bindings) {
				t.Fatalf("binding pointer does not name committed version: %+v, %v", binding, err)
			}
			currentPolicy, err := second.GetDispatchPolicy(ctx, f.groupID)
			if err != nil || currentPolicy.Revision != int64(policies) {
				t.Fatalf("policy pointer does not name committed version: %+v, %v", currentPolicy, err)
			}
			rows := messageSequenceRows(t, second.db, `SELECT b.group_id FROM group_coordinator_bindings b
			 JOIN group_coordinator_binding_revisions r USING(group_id,revision)
			 JOIN group_dispatch_policies p ON p.group_id=b.group_id
			 JOIN group_dispatch_policy_revisions d ON d.group_id=p.group_id AND d.revision=p.revision
			 JOIN json_each(d.allowed_worker_ids) w ON w.value=r.member_id`)
			if len(rows) != 0 {
				t.Fatalf("committed overlapping current configurations: %v", rows)
			}
		})
	}
}

func TestCoordinatorOperationsStatementIgnoreAndDeferredCommitRollback(t *testing.T) {
	for _, policy := range []bool{false, true} {
		for _, expected := range []int64{0, 1} {
			for _, phase := range []string{"version", "pointer", "receipt", "ignore-version", "ignore-pointer", "ignore-receipt", "commit"} {
				t.Run(fmt.Sprintf("policy=%t/revision=%d/%s", policy, expected, phase), func(t *testing.T) {
					f := setupMessages(t)
					observer := queueHandle(t, f.path, false)
					if expected == 1 {
						if _, err := coordinatorOperationWrite(f.s, f.groupID, f.a, 0, "seed", policy); err != nil {
							t.Fatal(err)
						}
					}
					versions, pointer := "group_coordinator_binding_revisions", "group_coordinator_bindings"
					if policy {
						versions, pointer = "group_dispatch_policy_revisions", "group_dispatch_policies"
					}
					part := strings.TrimPrefix(phase, "ignore-")
					table := map[string]string{"version": versions, "pointer": pointer, "receipt": "mutation_idempotency", "commit": "mutation_idempotency"}[part]
					event, timing := "INSERT", "AFTER"
					if part == "pointer" && expected == 1 {
						event = "UPDATE"
					}
					body := "SELECT RAISE(ABORT,'注入配置语句错误');"
					if strings.HasPrefix(phase, "ignore-") {
						timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
					} else if phase == "commit" {
						if _, err := f.s.db.Exec("CREATE TABLE coordinator_operation_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
							t.Fatal(err)
						}
						body = "INSERT INTO coordinator_operation_commit_guard VALUES('missing');"
					}
					if _, err := f.s.db.Exec("CREATE TRIGGER coordinator_operation_fault " + timing + " " + event + " ON " + table + " BEGIN " + body + " END"); err != nil {
						t.Fatal(err)
					}
					before := messageSequenceSnapshot(t, f.s)
					got, err := coordinatorOperationWrite(f.s, f.groupID, f.b, expected, "retryable", policy)
					coordinatorOperationRequireZero(t, got, err, nil)
					if strings.HasPrefix(phase, "ignore-") && !errors.Is(err, ErrConflict) {
						t.Fatalf("ignored required write did not report conflict: %v", err)
					}
					if phase == "commit" {
						coordinationAdmissionRequireFK(t, err)
						if !strings.Contains(err.Error(), "保存协调配置失败") {
							t.Fatalf("deferred constraint failed before COMMIT: %v", err)
						}
					}
					messageSequencePreserved(t, f.s, before)
					messageSequencePreserved(t, observer, before)
					if _, err := f.s.db.Exec("DROP TRIGGER coordinator_operation_fault"); err != nil {
						t.Fatal(err)
					}
					// Change the payload as well as retrying the key to detect leaked receipts.
					if _, err := coordinatorOperationWrite(observer, f.groupID, f.a, expected, "retryable", policy); err != nil {
						t.Fatalf("failed write left state or burned key: %v", err)
					}
					bindings, policies := int(expected+1), 0
					if policy {
						bindings, policies = 0, bindings
					}
					coordinatorOperationCounts(t, observer, bindings, policies, int(expected+1))
				})
			}
		}
	}
}

func TestCoordinatorOperationsFinalPointerGuardsRollbackInjectedChanges(t *testing.T) {
	for _, policy := range []bool{false, true} {
		for _, expected := range []int64{0, 1} {
			for _, change := range []string{"member-left", "opposite-config", "pointer-moved"} {
				t.Run(fmt.Sprintf("policy=%t/revision=%d/%s", policy, expected, change), func(t *testing.T) {
					f := setupMessages(t)
					observer := queueHandle(t, f.path, false)
					if expected == 1 {
						if _, err := coordinatorOperationWrite(f.s, f.groupID, f.a, 0, "seed", policy); err != nil {
							t.Fatal(err)
						}
					}
					versions, pointer := "group_coordinator_binding_revisions", "group_coordinator_bindings"
					if policy {
						versions, pointer = "group_dispatch_policy_revisions", "group_dispatch_policies"
					}
					body := "UPDATE members SET status='left' WHERE group_id=NEW.group_id;"
					if change == "opposite-config" {
						body = `INSERT INTO group_dispatch_policy_revisions(group_id,revision,allowed_worker_ids)
						 VALUES(NEW.group_id,1,json_array(NEW.member_id));
						 INSERT INTO group_dispatch_policies VALUES(NEW.group_id,1);`
						if policy {
							body = `INSERT INTO group_coordinator_binding_revisions(group_id,revision,member_id)
							 VALUES(NEW.group_id,1,json_extract(NEW.allowed_worker_ids,'$[0]'));
							 INSERT INTO group_coordinator_bindings VALUES(NEW.group_id,1);`
						}
					} else if change == "pointer-moved" {
						body = "INSERT INTO " + pointer + " VALUES(NEW.group_id,NEW.revision);"
						if expected == 1 {
							body = "UPDATE " + pointer + " SET revision=NEW.revision WHERE group_id=NEW.group_id;"
						}
					}
					if _, err := f.s.db.Exec("CREATE TRIGGER coordinator_operation_final_guard AFTER INSERT ON " + versions + " BEGIN " + body + " END"); err != nil {
						t.Fatal(err)
					}
					before := messageSequenceSnapshot(t, f.s)
					got, err := coordinatorOperationWrite(f.s, f.groupID, f.b, expected, "final-key", policy)
					coordinatorOperationRequireZero(t, got, err, nil)
					messageSequencePreserved(t, f.s, before)
					messageSequencePreserved(t, observer, before)
					if _, err := f.s.db.Exec("DROP TRIGGER coordinator_operation_final_guard"); err != nil {
						t.Fatal(err)
					}
					if _, err := coordinatorOperationWrite(observer, f.groupID, f.b, expected, "final-key", policy); err != nil {
						t.Fatalf("schema final-write failure burned key: %v", err)
					}
				})
			}
		}
	}
}

func TestCoordinatorOperationsPreserveRealUnknownCancelAndResources(t *testing.T) {
	old, path, groupID, a, b, check := coordinatorV10Fixture(t)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, path, false)
	check(s)
	before := coordinatorOperationOtherRows(t, s)
	if _, err := s.SetCoordinator(ctx, groupID, a, 0, "binding"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, groupID, []string{b}, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCoordinator(ctx, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDispatchPolicy(ctx, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, groupID, nil, 1, "clear"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCoordinator(ctx, groupID, b, 1, "replace"); err != nil {
		t.Fatal(err)
	}
	check(s)
	if after := coordinatorOperationOtherRows(t, s); !reflect.DeepEqual(after, before) {
		for table, want := range before {
			if !reflect.DeepEqual(after[table], want) {
				t.Errorf("configuration changed unrelated/execution table %s", table)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	check(reopened)
	if after := coordinatorOperationOtherRows(t, reopened); !reflect.DeepEqual(after, before) {
		t.Fatal("configuration reopen changed original execution evidence")
	}
	binding, err := reopened.SetCoordinator(ctx, groupID, a, 0, "binding")
	if err != nil || binding.MemberID != a || binding.Revision != 1 {
		t.Fatalf("configuration reopen lost original receipt: %+v, %v", binding, err)
	}
	binding, err = reopened.GetCoordinator(ctx, groupID)
	if err != nil || binding.MemberID != b || binding.Revision != 2 {
		t.Fatalf("configuration reopen lost current pointer: %+v, %v", binding, err)
	}
}
