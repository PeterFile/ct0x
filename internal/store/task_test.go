package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func taskInput(groupID string) CreateTaskInput {
	return CreateTaskInput{GroupID: groupID, ActorID: "user", Title: "任务", Description: "说明", Acceptance: "提供可验证结果"}
}

func createTestTask(t *testing.T, s *Store, groupID string) domain.Task {
	t.Helper()
	task, err := s.CreateTask(ctx, taskInput(groupID))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func assertTaskCounts(t *testing.T, s *Store, tasks, deps, keys int) {
	t.Helper()
	for table, want := range map[string]int{"tasks": tasks, "task_dependencies": deps, "mutation_idempotency": keys} {
		var got int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", table, got, err, want)
		}
	}
}

func TestTaskCreateGetReplayAndReopen(t *testing.T) {
	s, path := openTestStore(t)
	g, other := group(t, s), group(t, s)
	assignee := member(t, s, g.ID)
	a, b := createTestTask(t, s, g.ID), createTestTask(t, s, g.ID)
	input := taskInput(g.ID)
	input.AssigneeID, input.IdempotencyKey = assignee.ID, "create"
	input.DependencyIDs = []string{b.ID, a.ID, b.ID}
	before := slices.Clone(input.DependencyIDs)
	task, err := s.CreateTask(ctx, input)
	if err != nil || task.ID == "" || task.GroupID != g.ID || task.Title != input.Title || task.Description != input.Description ||
		task.Acceptance != input.Acceptance || task.AssigneeID != assignee.ID || task.Status != domain.TaskStatusOpen || task.Revision != 1 ||
		task.ActiveAttemptID != "" || task.CurrentSubmissionID != "" || task.CreatedAt.IsZero() || task.CreatedAt.Location() != time.UTC || len(task.DependencyIDs) != 2 {
		t.Fatalf("created task: %+v, %v", task, err)
	}
	if !slices.Equal(before, input.DependencyIDs) || !slices.IsSorted(task.DependencyIDs) {
		t.Fatal("dependency normalization mutated input or was not canonical")
	}
	// Existing unaccepted dependencies are valid at creation; readiness is F2.
	if a.Status != domain.TaskStatusOpen || b.Status != domain.TaskStatusOpen {
		t.Fatal("fixture does not exercise unaccepted dependencies")
	}
	check := func(s *Store) {
		t.Helper()
		got, err := s.GetTask(ctx, g.ID, task.ID)
		if err != nil || !reflect.DeepEqual(got, task) {
			t.Fatalf("task read changed: %+v, %v", got, err)
		}
		retry := input
		retry.DependencyIDs = []string{a.ID, b.ID}
		got, err = s.CreateTask(ctx, retry)
		if err != nil || !reflect.DeepEqual(got, task) {
			t.Fatalf("canonical replay changed: %+v, %v", got, err)
		}
		for _, pair := range [][2]string{{other.ID, task.ID}, {g.ID, "missing"}, {"missing", task.ID}} {
			if _, err := s.GetTask(ctx, pair[0], pair[1]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("scoped read %v: %v", pair, err)
			}
		}
		assertTaskCounts(t, s, 3, 2, 1)
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
	// A replay does not revalidate a now-left assignee.
	if _, err := reopened.db.Exec("UPDATE members SET status='left' WHERE id=?", assignee.ID); err != nil {
		t.Fatal(err)
	}
	check(reopened)
}

func TestTaskCreationAccessAndInvalidDependencies(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	active, left, foreign := member(t, s, g.ID), member(t, s, g.ID), member(t, s, other.ID)
	if _, err := s.db.Exec("UPDATE members SET status='left' WHERE id=?", left.ID); err != nil {
		t.Fatal(err)
	}
	foreignTask := createTestTask(t, s, other.ID)
	for _, actor := range []string{"", " ", " user", "missing", left.ID, foreign.ID} {
		input := taskInput(g.ID)
		input.ActorID, input.IdempotencyKey = actor, "access"
		if _, err := s.CreateTask(ctx, input); !errors.Is(err, ErrNotFound) {
			t.Errorf("unauthorized actor %q: %v", actor, err)
		}
	}
	for _, assignee := range []string{"missing", left.ID, foreign.ID} {
		input := taskInput(g.ID)
		input.AssigneeID, input.IdempotencyKey = assignee, "assignee"
		if _, err := s.CreateTask(ctx, input); !errors.Is(err, ErrNotFound) {
			t.Errorf("invalid assignee %q: %v", assignee, err)
		}
	}
	for _, deps := range [][]string{{"missing"}, {foreignTask.ID}} {
		input := taskInput(g.ID)
		input.DependencyIDs, input.IdempotencyKey = deps, "dependency"
		if _, err := s.CreateTask(ctx, input); !errors.Is(err, ErrNotFound) {
			t.Errorf("inaccessible dependency %v: %v", deps, err)
		}
	}
	for _, change := range []func(*CreateTaskInput){
		func(i *CreateTaskInput) { i.Title = " \t" }, func(i *CreateTaskInput) { i.Acceptance = "" },
		func(i *CreateTaskInput) { i.AssigneeID = " " }, func(i *CreateTaskInput) { i.DependencyIDs = []string{" "} },
	} {
		input := taskInput(g.ID)
		input.IdempotencyKey = "invalid"
		change(&input)
		if _, err := s.CreateTask(ctx, input); !errors.Is(err, ErrInvalid) || !errors.Is(err, domain.ErrTaskInvalid) {
			t.Fatalf("domain invalid category lost: %v", err)
		}
	}
	input := taskInput("missing")
	if _, err := s.CreateTask(ctx, input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing group: %v", err)
	}
	assertTaskCounts(t, s, 1, 0, 0)
	input = taskInput(g.ID)
	input.ActorID, input.AssigneeID = active.ID, active.ID
	if _, err := s.CreateTask(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"ActorID":"forged","actor_id":"forged"}`), &input); err != nil || input.ActorID != active.ID {
		t.Fatalf("transport replaced actor: %+v, %v", input, err)
	}
}

func TestTaskCreatePayloadConflictsAndScopes(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	actor := member(t, s, g.ID)
	input := taskInput(g.ID)
	input.IdempotencyKey = "key"
	original, err := s.CreateTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*CreateTaskInput){
		func(i *CreateTaskInput) { i.Title += " " }, func(i *CreateTaskInput) { i.Title = "" },
		func(i *CreateTaskInput) { i.Description += " " }, func(i *CreateTaskInput) { i.Acceptance = "" },
		func(i *CreateTaskInput) { i.AssigneeID = "missing" }, func(i *CreateTaskInput) { i.DependencyIDs = []string{"missing"} },
	} {
		changed := input
		change(&changed)
		if got, err := s.CreateTask(ctx, changed); !errors.Is(err, ErrConflict) || got.ID != "" {
			t.Errorf("changed original payload: %+v, %v", got, err)
		}
	}
	input.DependencyIDs = []string{}
	if got, err := s.CreateTask(ctx, input); err != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("nil/empty dependency sets differ: %+v, %v", got, err)
	}
	input.ActorID = actor.ID
	if got, err := s.CreateTask(ctx, input); err != nil || got.ID == original.ID {
		t.Fatalf("actor scope collision: %+v, %v", got, err)
	}
	if _, err := s.db.Exec("UPDATE members SET status='left' WHERE id=?", actor.ID); err != nil {
		t.Fatal(err)
	}
	input.Title = "changed"
	if _, err := s.CreateTask(ctx, input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("left actor probed a receipt/conflict: %v", err)
	}
	input = taskInput(other.ID)
	input.IdempotencyKey = "key"
	if got, err := s.CreateTask(ctx, input); err != nil || got.ID == original.ID {
		t.Fatalf("group scope collision: %+v, %v", got, err)
	}
	assertTaskCounts(t, s, 3, 0, 3)
	for _, pair := range []struct{ source, target error }{{domain.ErrTaskInvalid, ErrInvalid}, {domain.ErrTaskConflict, ErrConflict}} {
		if err := taskError("任务操作失败", fmt.Errorf("wrapped: %w", pair.source)); !errors.Is(err, pair.target) || !errors.Is(err, pair.source) {
			t.Fatalf("domain mapping requires string matching: %v", err)
		}
	}
}

func TestTaskCreationRollsBackAndKeyCanRetry(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	a, b := createTestTask(t, s, g.ID), createTestTask(t, s, g.ID)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_second_task_dependency BEFORE INSERT ON task_dependencies
WHEN (SELECT count(*) FROM task_dependencies WHERE task_id=NEW.task_id)=1
BEGIN SELECT RAISE(ABORT,'later dependency failure'); END`); err != nil {
		t.Fatal(err)
	}
	input := taskInput(g.ID)
	input.IdempotencyKey, input.DependencyIDs = "retry", []string{a.ID, b.ID}
	if got, err := s.CreateTask(ctx, input); err == nil || got.ID != "" {
		t.Fatalf("partial task returned success: %+v, %v", got, err)
	}
	assertTaskCounts(t, s, 2, 0, 0)
	if _, err := s.db.Exec("DROP TRIGGER fail_second_task_dependency"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.CreateTask(cancelled, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	input.Title = "fixed"
	if _, err := s.CreateTask(ctx, input); err != nil {
		t.Fatal(err)
	}
	assertTaskCounts(t, s, 3, 2, 1)
	plain := taskInput(g.ID)
	first, err := s.CreateTask(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateTask(ctx, plain)
	if err != nil || first.ID == second.ID || first.DependencyIDs == nil {
		t.Fatalf("no-key creation acquired idempotency: %+v, %v", second, err)
	}
	data, err := json.Marshal(first)
	if err != nil || !strings.Contains(string(data), `"dependency_ids":[]`) {
		t.Fatalf("nil dependency array: %s, %v", data, err)
	}
}

func TestTaskConcurrentCreateAcrossStores(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			s, path := openTestStore(t)
			g := group(t, s)
			dep := createTestTask(t, s, g.ID)
			second, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			type result struct {
				task domain.Task
				err  error
			}
			results, start := make(chan result, 12), make(chan struct{})
			for i := range 12 {
				go func() {
					<-start
					input := taskInput(g.ID)
					input.IdempotencyKey, input.DependencyIDs = "concurrent", []string{dep.ID, dep.ID}
					if conflict && i%2 == 1 {
						input.Title = "other"
					}
					task, err := []*Store{s, second}[i%2].CreateTask(ctx, input)
					results <- result{task, err}
				}()
			}
			close(start)
			var winner domain.Task
			failures := 0
			for range 12 {
				r := <-results
				if errors.Is(r.err, ErrConflict) {
					failures++
					continue
				}
				if r.err != nil || (winner.ID != "" && !reflect.DeepEqual(winner, r.task)) {
					t.Errorf("concurrent create diverged: %+v, %v", r.task, r.err)
				}
				winner = r.task
			}
			if winner.ID == "" || (!conflict && failures != 0) || (conflict && failures != 6) {
				t.Fatalf("unexpected winner/conflicts: %+v, %d", winner, failures)
			}
			assertTaskCounts(t, s, 2, 1, 1)
		})
	}
}
