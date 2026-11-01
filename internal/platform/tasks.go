package platform

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type TaskInput struct {
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Acceptance    string   `json:"acceptance"`
	AssigneeID    string   `json:"assignee_id"`
	DependencyIDs []string `json:"dependency_ids"`
}

func (s *LocalUser) CreateTask(ctx context.Context, groupID string, input TaskInput, key string) (domain.Task, error) {
	return s.db.CreateTask(ctx, store.CreateTaskInput{
		GroupID: groupID, ActorID: "user", Title: input.Title, Description: input.Description,
		Acceptance: input.Acceptance, AssigneeID: input.AssigneeID, DependencyIDs: input.DependencyIDs, IdempotencyKey: key,
	})
}

func (s *LocalUser) GetTask(ctx context.Context, groupID, taskID string) (domain.Task, error) {
	return s.db.GetTask(ctx, groupID, taskID)
}

func (s *LocalUser) ListTasks(ctx context.Context, groupID string, options store.ListOptions) (store.Page[domain.Task], error) {
	return s.db.ListTasks(ctx, groupID, options)
}
