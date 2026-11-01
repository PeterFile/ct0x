package platform

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestTaskLocalUserCreateGetListAndBoundActor(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	empty, err := f.local.ListTasks(ctx, f.groupID, store.ListOptions{})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("empty task list: %+v, %v", empty, err)
	}
	assignee := f.member(t, f.groupID)
	dependency, err := f.local.CreateTask(ctx, f.groupID, TaskInput{Title: "dependency", Acceptance: "criteria"}, "")
	if err != nil {
		t.Fatal(err)
	}
	input := TaskInput{
		Title: " exact title ", Description: " exact description ", Acceptance: " criteria ",
		AssigneeID: assignee.ID, DependencyIDs: []string{dependency.ID},
	}
	task, err := f.local.CreateTask(ctx, f.groupID, input, "task-key")
	if err != nil || task.ID == "" || task.GroupID != f.groupID || task.Title != input.Title || task.Description != input.Description ||
		task.Acceptance != input.Acceptance || task.AssigneeID != input.AssigneeID || !reflect.DeepEqual(task.DependencyIDs, input.DependencyIDs) ||
		task.Status != domain.TaskStatusOpen || task.Revision != 1 || task.ActiveAttemptID != "" || task.CurrentSubmissionID != "" ||
		task.CreatedAt.IsZero() || task.CreatedAt.Location() != time.UTC {
		t.Fatalf("local task contract: %+v, %v", task, err)
	}
	// A store replay under the server identity proves both actor binding and exact input forwarding.
	replay, err := f.db.CreateTask(ctx, store.CreateTaskInput{
		GroupID: f.groupID, ActorID: "user", Title: input.Title, Description: input.Description,
		Acceptance: input.Acceptance, AssigneeID: input.AssigneeID, DependencyIDs: input.DependencyIDs, IdempotencyKey: "task-key",
	})
	if err != nil || !reflect.DeepEqual(replay, task) {
		t.Fatalf("task actor or input was rewritten: %+v, %v", replay, err)
	}
	got, err := f.local.GetTask(ctx, f.groupID, task.ID)
	if err != nil || !reflect.DeepEqual(got, task) {
		t.Fatalf("task detail: %+v, %v", got, err)
	}
	page, err := f.local.ListTasks(ctx, f.groupID, store.ListOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], task) || page.NextCursor == nil {
		t.Fatalf("task first page: %+v, %v", page, err)
	}
	tail, err := f.local.ListTasks(ctx, f.groupID, store.ListOptions{Limit: 1, Before: page.NextCursor})
	if err != nil || len(tail.Items) != 1 || !reflect.DeepEqual(tail.Items[0], dependency) || tail.NextCursor != nil {
		t.Fatalf("task continuation: %+v, %v", tail, err)
	}
	replay, err = f.local.CreateTask(ctx, f.groupID, input, "task-key")
	if err != nil || !reflect.DeepEqual(replay, task) {
		t.Fatalf("local replay: %+v, %v", replay, err)
	}
	input.Title += "changed"
	if _, err := f.local.CreateTask(ctx, f.groupID, input, "task-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed task payload: %v", err)
	}
	isolated, err := f.local.ListTasks(ctx, f.otherID, store.ListOptions{})
	if err != nil || isolated.Items == nil || len(isolated.Items) != 0 || isolated.NextCursor != nil {
		t.Fatalf("cross-group list leaked: %+v, %v", isolated, err)
	}
}

func TestTaskLocalUserDelegatesValidationAndIsolation(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	foreignMember := f.member(t, f.otherID)
	foreignTask, err := f.local.CreateTask(ctx, f.otherID, TaskInput{Title: "foreign", Acceptance: "criteria"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, groupID string
		input         TaskInput
		want          error
	}{
		{"title", f.groupID, TaskInput{Title: " \t", Acceptance: "criteria"}, store.ErrInvalid},
		{"acceptance", f.groupID, TaskInput{Title: "task"}, store.ErrInvalid},
		{"group", "missing", TaskInput{Title: "task", Acceptance: "criteria"}, store.ErrNotFound},
		{"assignee", f.groupID, TaskInput{Title: "task", Acceptance: "criteria", AssigneeID: foreignMember.ID}, store.ErrNotFound},
		{"dependency", f.groupID, TaskInput{Title: "task", Acceptance: "criteria", DependencyIDs: []string{foreignTask.ID}}, store.ErrNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.local.CreateTask(ctx, tt.groupID, tt.input, "retry"); !errors.Is(err, tt.want) {
				t.Fatalf("task error = %v, want %v", err, tt.want)
			}
		})
	}
	for _, pair := range [][2]string{{f.groupID, foreignTask.ID}, {f.otherID, "missing"}, {"missing", foreignTask.ID}} {
		if _, err := f.local.GetTask(ctx, pair[0], pair[1]); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("inaccessible task %v: %v", pair, err)
		}
	}
	if _, err := f.local.ListTasks(ctx, "missing", store.ListOptions{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task group: %v", err)
	}
	page, err := f.local.ListTasks(ctx, f.groupID, store.ListOptions{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("rejected task became visible: %+v, %v", page, err)
	}
	if _, err := f.local.CreateTask(ctx, f.groupID, TaskInput{Title: "fixed", Acceptance: "criteria"}, "retry"); err != nil {
		t.Fatalf("rejected request poisoned its key: %v", err)
	}
}
