package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestRequestDispatchValidationAndChangedPayload(t *testing.T) {
	f := requestDispatchSetup(t)
	api := requestDispatchStore(t, f.s)
	before := messageSequenceSnapshot(t, f.s)
	for _, bad := range []string{"", " ", "\u2003", " id", "id\u2003", "id\x00", "id\r", "id\n", string([]byte{0xff}), strings.Repeat("x", 4097)} {
		got, err := api.RequestDispatch(ctx, bad, f.task.ID, 1, 1, "安排", "key")
		requestDispatchZero(t, got, err, ErrInvalid)
		got, err = api.RequestDispatch(ctx, f.groupID, bad, 1, 1, "安排", "key")
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	for _, bad := range []string{"", " \r\n\t", "\u2003"} {
		got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, bad, "key")
		requestDispatchZero(t, got, err, ErrInvalid)
		got, err = api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", bad)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	for _, bad := range []int64{0, -1, math.MinInt64} {
		got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, bad, 1, "安排", "key")
		requestDispatchZero(t, got, err, ErrInvalid)
		got, err = api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, bad, "安排", "key")
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err := api.RequestDispatch(cancelled, f.groupID, f.task.ID, 1, 1, "安排", "key")
	requestDispatchZero(t, got, err, context.Canceled)
	messageSequencePreserved(t, f.s, before)
	original, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "key")
	if err != nil {
		t.Fatalf("invalid request burned key: %v", err)
	}
	before = messageSequenceSnapshot(t, f.s)
	for _, changed := range []struct {
		taskID        string
		revision, gen int64
		reason        string
	}{
		{f.task.ID, 1, 1, "安排 "}, {f.task.ID, 2, 1, "安排"}, {f.task.ID, 1, 2, "安排"},
		{"missing-task", 1, 1, "安排"}, {f.task.ID, math.MaxInt64, math.MaxInt64, "安排"},
	} {
		got, err := api.RequestDispatch(ctx, f.groupID, changed.taskID, changed.revision, changed.gen, changed.reason, "key")
		requestDispatchZero(t, got, err, ErrConflict)
	}
	messageSequencePreserved(t, f.s, before)
	if got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "key"); err != nil || got != original {
		t.Fatalf("changed payload damaged original receipt: %+v, %v", got, err)
	}
}

func TestRequestDispatchOpaqueIDsKeysAndPositiveInt64Maximum(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	groupID := "group:opaque|群\tinside"
	if _, err := s.db.Exec("UPDATE groups SET id=? WHERE id=?", groupID, g.ID); err != nil {
		t.Fatal(err)
	}
	coord, worker := member(t, s, groupID).ID, member(t, s, groupID).ID
	key := " \topaque\x00\r\n" + strings.Repeat("k", 5000) + " "
	if _, err := s.SetCoordinator(ctx, groupID, coord, 0, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, groupID, []string{worker}, 0, key); err != nil {
		t.Fatal(err)
	}
	task := createTestTask(t, s, groupID)
	taskID := "task:opaque|任务\tinside"
	if _, err := s.db.Exec("UPDATE tasks SET id=?,revision=? WHERE id=?", taskID, int64(math.MaxInt64), task.ID); err != nil {
		t.Fatal(err)
	}
	task.ID, task.Revision = taskID, math.MaxInt64
	thread := domain.ThreadRef{GroupID: groupID, TopicID: " "}
	admission := coordinationAdmissionRows(task, coordinationAdmissionSource(t, s, thread))
	admission.current[4] = int64(math.MaxInt64)
	if err := coordinationAdmissionCommit(s, admission); err != nil {
		t.Fatal(err)
	}
	api := requestDispatchStore(t, s)
	reason := string([]byte{0xff, 0, 'x'})
	original, err := api.RequestDispatch(ctx, groupID, taskID, math.MaxInt64, math.MaxInt64, reason, key)
	if err != nil || original.TaskRevision != math.MaxInt64 || original.AuthorityGeneration != math.MaxInt64 || original.Reason != reason {
		t.Fatalf("snapshot versions were treated as increments/bytes restricted: %+v, %v", original, err)
	}
	if got, err := api.RequestDispatch(ctx, groupID, taskID, math.MaxInt64, math.MaxInt64, reason, key); err != nil || got != original {
		t.Fatalf("opaque key replay: %+v, %v", got, err)
	}
	var savedKey string
	var payload []byte
	if err := s.db.QueryRow("SELECT idempotency_key,payload FROM mutation_idempotency WHERE operation='request_dispatch'").Scan(&savedKey, &payload); err != nil || savedKey != key || !reflect.DeepEqual(payload, mutationPayload(groupID, taskID, fmt.Sprint(int64(math.MaxInt64)), fmt.Sprint(int64(math.MaxInt64)), reason)) {
		t.Fatalf("payload/key bytes changed: %q %v, %v", savedKey, payload, err)
	}
	got, err := api.RequestDispatch(ctx, groupID, taskID, math.MaxInt64, math.MaxInt64, reason, strings.TrimSpace(key))
	requestDispatchZero(t, got, err, ErrConflict)
}

func TestRequestDispatchScopeBeforeReceiptAndNamespaceIsolation(t *testing.T) {
	f := requestDispatchSetup(t)
	api := requestDispatchStore(t, f.s)
	for _, payload := range [][]byte{mutationPayload("missing-group", f.task.ID, "1", "1", "安排"), []byte("private-payload")} {
		if _, err := f.s.db.Exec("INSERT OR REPLACE INTO mutation_idempotency VALUES('request_dispatch','user','missing-group','forged',?,?)", payload, "private-id"); err != nil {
			t.Fatal(err)
		}
		before := messageSequenceSnapshot(t, f.s)
		got, err := api.RequestDispatch(ctx, "missing-group", f.task.ID, 1, 1, "安排", "forged")
		requestDispatchZero(t, got, err, ErrNotFound)
		messageSequencePreserved(t, f.s, before)
	}
	if _, err := f.s.db.Exec("INSERT INTO mutation_idempotency VALUES('request_dispatch',?,?,'shared',?,'private')", f.worker, f.groupID, []byte("member-private")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetCoordinator(ctx, f.groupID, f.coord, 1, "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.worker}, 1, "shared"); err != nil {
		t.Fatal(err)
	}
	own, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "shared")
	if err != nil || own.RequestedBy != "user" {
		t.Fatalf("actor/operation namespace collision: %+v, %v", own, err)
	}
	other, coordinator, _ := dispatchConfiguredGroup(t, f.s)
	candidate := dispatchCandidate(t, f.s, other, coordinator, "topic")
	foreignTask := candidate.request[3].(string)
	before := messageSequenceSnapshot(t, f.s)
	got, err := api.RequestDispatch(ctx, other, f.task.ID, 1, 1, "安排", "shared")
	requestDispatchZero(t, got, err, ErrNotFound)
	got, err = api.RequestDispatch(ctx, f.groupID, foreignTask, 1, 1, "安排", "other-key")
	requestDispatchZero(t, got, err, ErrNotFound)
	messageSequencePreserved(t, f.s, before)
	foreign, err := api.RequestDispatch(ctx, other, foreignTask, 1, 1, "安排", "shared")
	if err != nil || foreign.ID == own.ID {
		t.Fatalf("key crossed group: %+v, %v", foreign, err)
	}
	if _, err := f.s.db.Exec("INSERT INTO mutation_idempotency VALUES('request_dispatch','user',?,'foreign-history',?,?)", f.groupID, mutationPayload(f.groupID, f.task.ID, "1", "1", "安排"), foreign.ID); err != nil {
		t.Fatal(err)
	}
	before = messageSequenceSnapshot(t, f.s)
	got, err = api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "foreign-history")
	requestDispatchZero(t, got, err, ErrNotFound)
	messageSequencePreserved(t, f.s, before)
}

func TestRequestDispatchMembersIndependentEligibility(t *testing.T) {
	for _, test := range []struct {
		name                              string
		coordinatorActive, workerEligible bool
	}{
		{"both-active", true, true},
		{"coordinator-left", false, true},
		{"worker-left", true, false},
		{"both-left", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := requestDispatchSetup(t)
			// The supplied historical policy, not the current pointer, selects workers.
			if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, nil, 1, "current-empty"); err != nil {
				t.Fatal(err)
			}
			for id, active := range map[string]bool{f.coord: test.coordinatorActive, f.worker: test.workerEligible} {
				if !active {
					if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", id); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := messageSequenceSnapshot(t, f.s)
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			coordinatorActive, workerEligible, err := coordinationDispatchMembersInTx(ctx, tx, f.groupID, f.coord, 1, "", "")
			if err != nil || coordinatorActive != test.coordinatorActive || workerEligible != test.workerEligible {
				t.Fatalf("independent membership: (%t,%t), %v; want (%t,%t)", coordinatorActive, workerEligible, err, test.coordinatorActive, test.workerEligible)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestRequestDispatchCurrentEligibilityAndRecoverableKey(t *testing.T) {
	for _, change := range []string{"stale-task", "stale-authority", "authority-moved", "revision-moved", "active-attempt", "submitted", "accepted", "missing-binding", "missing-policy", "empty-policy", "coordinator-left", "worker-left", "task-assignee", "ordinary-task"} {
		t.Run(change, func(t *testing.T) {
			f := requestDispatchSetup(t)
			api := requestDispatchStore(t, f.s)
			taskID, revision, gen := f.task.ID, int64(1), int64(1)
			want := ErrConflict
			switch change {
			case "stale-task":
				revision = 2
			case "stale-authority":
				gen = 2
			case "authority-moved":
				_, err := f.s.db.Exec("UPDATE task_coordination SET authority_generation=2 WHERE task_id=?", f.task.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "revision-moved":
				if _, err := f.s.db.Exec("UPDATE tasks SET revision=2 WHERE id=?", f.task.ID); err != nil {
					t.Fatal(err)
				}
				revision = 2 // Current Task matches, but its immutable admission no longer does.
			case "active-attempt", "submitted", "accepted":
				attempt, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(f.task))
				if err != nil {
					t.Fatal(err)
				}
				if change != "active-attempt" {
					submission, err := f.s.SubmitTask(ctx, taskSubmissionInput(attempt))
					if err != nil {
						t.Fatal(err)
					}
					if change == "accepted" {
						if _, err := f.s.ReviewTask(ctx, taskReviewInput(submission)); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "missing-binding", "missing-policy":
				// Use a fresh group with an authorized Task but absent current config.
				g, c, w := group(t, f.s).ID, "", ""
				c, w = member(t, f.s, g).ID, member(t, f.s, g).ID
				if change == "missing-binding" {
					if _, err := f.s.SetDispatchPolicy(ctx, g, []string{w}, 0, "policy"); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := f.s.SetCoordinator(ctx, g, c, 0, "binding"); err != nil {
						t.Fatal(err)
					}
				}
				rows := coordinationAdmissionFixture(t, f.s, domain.ThreadRef{GroupID: g, TopicID: "topic"})
				if err := coordinationAdmissionCommit(f.s, rows); err != nil {
					t.Fatal(err)
				}
				f.groupID, f.coord, f.worker, taskID = g, c, w, rows.current[0].(string)
				if change == "missing-binding" {
					want = ErrNotFound
				}
			case "empty-policy":
				if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, nil, 1, "clear"); err != nil {
					t.Fatal(err)
				}
			case "coordinator-left", "worker-left":
				id := f.coord
				if change == "worker-left" {
					id = f.worker
				}
				if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", id); err != nil {
					t.Fatal(err)
				}
			case "task-assignee":
				if _, err := f.s.db.Exec("UPDATE tasks SET assignee_id=? WHERE id=?", member(t, f.s, f.groupID).ID, f.task.ID); err != nil {
					t.Fatal(err)
				}
			case "ordinary-task":
				taskID, want = createTestTask(t, f.s, f.groupID).ID, ErrNotFound
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err := api.RequestDispatch(ctx, f.groupID, taskID, revision, gen, "安排", "recoverable")
			requestDispatchZero(t, got, err, want)
			messageSequencePreserved(t, f.s, before)
			// Restore eligible configuration and use a fresh authorized Task with the failed key.
			if _, err := f.s.db.Exec("UPDATE members SET status='active' WHERE group_id=?", f.groupID); err != nil {
				t.Fatal(err)
			}
			binding, err := f.s.GetCoordinator(ctx, f.groupID)
			if err != nil {
				if _, err := f.s.SetCoordinator(ctx, f.groupID, f.coord, 0, "restore-binding"); err != nil {
					t.Fatal(err)
				}
			} else if binding.MemberID != f.coord {
				t.Fatal("unexpected binding")
			}
			policy, err := f.s.GetDispatchPolicy(ctx, f.groupID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.worker}, policy.Revision, "restore-policy"); err != nil {
				t.Fatal(err)
			}
			rows := coordinationAdmissionFixture(t, f.s, domain.ThreadRef{GroupID: f.groupID})
			if err := coordinationAdmissionCommit(f.s, rows); err != nil {
				t.Fatal(err)
			}
			if _, err := api.RequestDispatch(ctx, f.groupID, rows.current[0].(string), 1, 1, "恢复后安排", "recoverable"); err != nil {
				t.Fatalf("failed eligibility burned key: %v", err)
			}
		})
	}
}

func TestRequestDispatchBothAssigneesAndUnacceptedDependencies(t *testing.T) {
	for _, assignee := range []string{"none", "worker", "coordinator", "disallowed", "left", "task-disagrees", "mandate-only", "mandate-disallowed", "task-only"} {
		t.Run(assignee, func(t *testing.T) {
			f := requestDispatchSetup(t)
			api := requestDispatchStore(t, f.s)
			dependency := createTestTask(t, f.s, f.groupID)
			task := createTestTask(t, f.s, f.groupID)
			fixed := ""
			switch assignee {
			case "worker", "left", "task-disagrees", "mandate-only", "mandate-disallowed":
				fixed = f.worker
			case "coordinator":
				fixed = f.coord
			case "disallowed":
				fixed = member(t, f.s, f.groupID).ID
			}
			task.AssigneeID, task.DependencyIDs = fixed, []string{dependency.ID}
			if _, err := f.s.db.Exec("UPDATE tasks SET assignee_id=nullif(?, '') WHERE id=?", fixed, task.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec("INSERT INTO task_dependencies VALUES(?,?,?)", f.groupID, task.ID, dependency.ID); err != nil {
				t.Fatal(err)
			}
			rows := coordinationAdmissionRows(task, coordinationAdmissionSource(t, f.s, domain.ThreadRef{GroupID: f.groupID}))
			if err := coordinationAdmissionCommit(f.s, rows); err != nil {
				t.Fatal(err)
			}
			if assignee == "task-disagrees" || assignee == "mandate-only" || assignee == "mandate-disallowed" || assignee == "task-only" {
				now := ""
				if assignee == "task-disagrees" {
					now = member(t, f.s, f.groupID).ID
					if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.worker, now}, 1, "both-allowed"); err != nil {
						t.Fatal(err)
					}
				} else if assignee == "mandate-disallowed" {
					if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{member(t, f.s, f.groupID).ID}, 1, "other-only"); err != nil {
						t.Fatal(err)
					}
				} else if assignee == "task-only" {
					now = f.worker
				}
				if _, err := f.s.db.Exec("UPDATE tasks SET assignee_id=nullif(?, '') WHERE id=?", now, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if assignee == "left" {
				if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.worker); err != nil {
					t.Fatal(err)
				}
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err := api.RequestDispatch(ctx, f.groupID, task.ID, 1, 1, "依赖未验收也可先安排", "dependency")
			if assignee == "coordinator" || assignee == "disallowed" || assignee == "left" || assignee == "task-disagrees" || assignee == "mandate-disallowed" {
				requestDispatchZero(t, got, err, ErrConflict)
				messageSequencePreserved(t, f.s, before)
			} else {
				if err != nil {
					t.Fatalf("eligible worker/unaccepted dependency rejected: %v", err)
				}
				requestDispatchOnlyRows(t, before, messageSequenceSnapshot(t, f.s))
				if _, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(task)); !errors.Is(err, ErrConflict) {
					t.Fatalf("dispatch bypassed existing dependency execution gate: %v", err)
				}
			}
		})
	}
}
