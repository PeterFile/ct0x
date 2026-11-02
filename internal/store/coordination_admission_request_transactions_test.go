package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestRequestWorkConcurrentHandles(t *testing.T) {
	for _, contention := range []string{"same-key", "changed-payload", "same-trigger", "same-task"} {
		t.Run(contention, func(t *testing.T) {
			s, path, thread, request := requestWorkFixture(t)
			second := queueHandle(t, path, false)
			const workers = 8
			requests := make([]domain.RequestWorkRequest, workers)
			keys := make([]string, workers)
			taskCount := 1
			var target domain.Task
			if contention == "same-task" {
				target = createTestTask(t, s, thread.GroupID)
			}
			for i := range workers {
				requests[i], keys[i] = request, "key"
				if contention == "changed-payload" && i%2 == 1 {
					requests[i].Reason += "changed"
				}
				if contention == "same-trigger" || contention == "same-task" {
					keys[i] = fmt.Sprintf("key-%d", i)
				}
				if contention == "same-task" {
					requests[i] = requestWorkLink(requests[i], target)
					requests[i].MessageID = coordinationTriggerMessage(t, s, thread).ID
				}
			}
			type result struct {
				receipt domain.AdmissionReceipt
				err     error
			}
			start, results := make(chan struct{}), make(chan result, workers)
			for i := range workers {
				go func() {
					<-start
					receipt, err := []*Store{s, second}[i%2].RequestWork(ctx, thread.GroupID, requests[i], keys[i])
					results <- result{receipt, err}
				}()
			}
			close(start)
			var winner domain.AdmissionReceipt
			conflicts := 0
			for range workers {
				r := <-results
				if r.err != nil {
					requestWorkRequireZero(t, r.receipt, r.err, ErrConflict)
					conflicts++
					continue
				}
				if winner.Decision.ID != "" && !reflect.DeepEqual(winner, r.receipt) {
					t.Fatalf("concurrent receipts diverged: %+v vs %+v", winner, r.receipt)
				}
				winner = r.receipt
			}
			wantConflicts := map[string]int{"same-key": 0, "changed-payload": workers / 2, "same-trigger": workers - 1, "same-task": workers - 1}[contention]
			if conflicts != wantConflicts || winner.Decision.ID == "" {
				t.Fatalf("conflicts=%d, want %d; winner=%+v", conflicts, wantConflicts, winner)
			}
			requestWorkCounts(t, s, 1, taskCount, 0, 1)
			if violations := messageSequenceRows(t, s.db, "PRAGMA foreign_key_check"); len(violations) != 0 {
				t.Fatalf("concurrent operation committed a broken cycle: %v", violations)
			}
		})
	}
}

func TestRequestWorkStatementAndDeferredCommitFailuresRollback(t *testing.T) {
	for _, mode := range []domain.RoutingDecisionKind{domain.RoutingDecisionCreateNew, domain.RoutingDecisionLinkExisting} {
		for _, phase := range []string{"trigger", "receipt", "decision", "mandate", "coordination", "ignore-receipt", "ignore-decision", "ignore-mandate", "ignore-coordination", "commit", "unique"} {
			t.Run(string(mode)+"/"+phase, func(t *testing.T) {
				s, path, thread, request := requestWorkFixture(t)
				observer := queueHandle(t, path, false)
				baseTasks := 0
				if mode == domain.RoutingDecisionLinkExisting {
					request = requestWorkLink(request, createTestTask(t, s, thread.GroupID))
					baseTasks = 1
				} else {
					request.NewTask.DependencyIDs = []string{createTestTask(t, s, thread.GroupID).ID}
					baseTasks = 1
				}
				table := map[string]string{
					"trigger": "coordination_triggers", "receipt": "mutation_idempotency", "decision": "routing_decisions", "mandate": "mandates", "coordination": "task_coordination",
					"ignore-receipt": "mutation_idempotency", "ignore-decision": "routing_decisions", "ignore-mandate": "mandates", "ignore-coordination": "task_coordination", "commit": "task_coordination", "unique": "task_coordination",
				}[phase]
				body, timing := "SELECT RAISE(ABORT,'注入 admission 语句错误');", "AFTER"
				if strings.HasPrefix(phase, "ignore-") {
					body, timing = "SELECT RAISE(IGNORE);", "BEFORE"
				} else if phase == "commit" {
					if _, err := s.db.Exec("CREATE TABLE request_work_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
						t.Fatal(err)
					}
					body = "INSERT INTO request_work_commit_guard VALUES('missing');"
				} else if phase == "unique" {
					if _, err := s.db.Exec("CREATE TABLE request_work_unique_guard(id TEXT PRIMARY KEY); INSERT INTO request_work_unique_guard VALUES('occupied')"); err != nil {
						t.Fatal(err)
					}
					body = "INSERT INTO request_work_unique_guard VALUES('occupied');"
				}
				if _, err := s.db.Exec("CREATE TRIGGER request_work_fail " + timing + " INSERT ON " + table + " BEGIN " + body + " END"); err != nil {
					t.Fatal(err)
				}
				before := messageSequenceSnapshot(t, s)
				got, err := s.RequestWork(ctx, thread.GroupID, request, "retryable")
				requestWorkRequireZero(t, got, err, nil)
				if phase == "commit" {
					coordinationAdmissionRequireFK(t, err)
					if !strings.Contains(err.Error(), "保存工作请求失败") {
						t.Fatalf("deferred failure happened before COMMIT: %v", err)
					}
				} else if phase == "unique" || strings.HasPrefix(phase, "ignore-") {
					if !errors.Is(err, ErrConflict) {
						t.Fatalf("constraint/CAS failure lost conflict category: %v", err)
					}
				}
				messageSequencePreserved(t, s, before)
				messageSequencePreserved(t, observer, before)
				requestWorkCounts(t, s, 0, baseTasks, 0, 0)
				if _, err := s.db.Exec("DROP TRIGGER request_work_fail"); err != nil {
					t.Fatal(err)
				}
				request.Reason = "修复后重试，证明失败未烧 key"
				if _, err := s.RequestWork(ctx, thread.GroupID, request, "retryable"); err != nil {
					t.Fatalf("failed operation leaked state or burned key: %v", err)
				}
				wantTasks, wantDeps := baseTasks, 0
				if mode == domain.RoutingDecisionCreateNew {
					wantTasks++
					wantDeps = 1
				}
				requestWorkCounts(t, s, 1, wantTasks, wantDeps, 1)
			})
		}
	}
}
