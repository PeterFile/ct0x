package platform

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// LocalUser is an explicitly trusted local-human entry, never a member tool
// service. A future member entry must bind its own run identity and grants.
type LocalUser struct{ db *store.Store }

type GroupInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Provider, model and workspace are preferences, not capabilities or access grants.
type MemberInput struct {
	Name       string `json:"name"`
	Role       string `json:"role"`
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
	Workspace  string `json:"workspace"`
}

func NewLocalUser(db *store.Store) *LocalUser { return &LocalUser{db} }

func (s *LocalUser) CreateGroup(ctx context.Context, input GroupInput, key string) (domain.Group, error) {
	return s.db.CreateGroup(ctx, store.CreateGroupInput{
		Name: input.Name, Description: input.Description, ActorID: "user", IdempotencyKey: key,
	})
}

func (s *LocalUser) GetGroup(ctx context.Context, id string) (domain.Group, error) {
	return s.db.GetGroup(ctx, id)
}

func (s *LocalUser) ListGroups(ctx context.Context, options store.ListOptions) (store.Page[domain.Group], error) {
	return s.db.ListGroups(ctx, options)
}

func (s *LocalUser) CreateMember(ctx context.Context, groupID string, input MemberInput, key string) (domain.Member, error) {
	return s.db.CreateMember(ctx, store.CreateMemberInput{
		GroupID: groupID, Name: input.Name, Role: input.Role, ProviderID: input.ProviderID,
		Model: input.Model, Workspace: input.Workspace, CreatorID: "user", OwnerID: "user",
		ActorID: "user", IdempotencyKey: key,
	})
}

func (s *LocalUser) GetMember(ctx context.Context, groupID, memberID string) (domain.Member, error) {
	return s.db.GetMember(ctx, groupID, memberID)
}

func (s *LocalUser) ListMembers(ctx context.Context, groupID string, options store.ListOptions) (store.Page[domain.Member], error) {
	return s.db.ListMembers(ctx, groupID, options)
}
