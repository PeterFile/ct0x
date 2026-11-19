package platform

import (
	"context"

	"agentworkspace/internal/store"
)

type ProjectInput struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Directory   *string `json:"directory"`
}

type ChannelInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *LocalUser) CreateProject(ctx context.Context, input ProjectInput, key string) (store.ProjectView, error) {
	return s.db.CreateProject(ctx, store.CreateProjectInput{
		Name: input.Name, Description: input.Description, Directory: input.Directory, IdempotencyKey: key,
	})
}

func (s *LocalUser) GetProject(ctx context.Context, projectID string) (store.ProjectView, error) {
	return s.db.GetProject(ctx, projectID)
}

func (s *LocalUser) ListProjects(ctx context.Context, options store.ListOptions) (store.Page[store.ProjectView], error) {
	return s.db.ListProjects(ctx, options)
}

func (s *LocalUser) CreateChannel(ctx context.Context, projectID string, input ChannelInput, key string) (store.ChannelView, error) {
	return s.db.CreateChannel(ctx, store.CreateChannelInput{
		ProjectID: projectID, Name: input.Name, Description: input.Description, IdempotencyKey: key,
	})
}

func (s *LocalUser) GetChannel(ctx context.Context, projectID, channelID string) (store.ChannelView, error) {
	return s.db.GetChannel(ctx, projectID, channelID)
}

func (s *LocalUser) ListChannels(ctx context.Context, projectID string, options store.ListOptions) (store.Page[store.ChannelView], error) {
	return s.db.ListChannels(ctx, projectID, options)
}

// GetGroupOwnership is a trusted local-user lookup, not a member authorization check.
func (s *LocalUser) GetGroupOwnership(ctx context.Context, groupID string) (store.GroupOwnership, error) {
	return s.db.GetGroupOwnership(ctx, groupID)
}
