package platform

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// Member tool DTOs deliberately have no caller/group/topic or creation policy fields.
type MemberMessageInput = store.MemberToolMessageInput
type MemberCreateInput = store.MemberToolCreateInput
type MemberCreateResult = store.MemberToolCreateResult

func (s *MemberTools) ListMembers(ctx context.Context, token string, options store.ListOptions) (store.Page[domain.Member], error) {
	var result store.Page[domain.Member]
	err := s.withAuthorized(ctx, token, "list_members", func(scope store.MemberToolScope, _ MemberToolGrants) error {
		var err error
		result, err = s.db.ListMembersForTool(ctx, scope, options)
		return err
	})
	if err != nil {
		return store.Page[domain.Member]{}, err
	}
	return result, nil
}

func (s *MemberTools) ReadChannel(ctx context.Context, token string, options store.ListOptions) (store.Page[domain.Message], error) {
	var result store.Page[domain.Message]
	err := s.withAuthorized(ctx, token, "read_channel", func(scope store.MemberToolScope, _ MemberToolGrants) error {
		var err error
		result, err = s.db.ReadChannelForTool(ctx, scope, options)
		return err
	})
	if err != nil {
		return store.Page[domain.Message]{}, err
	}
	return result, nil
}

func (s *MemberTools) SendMessage(ctx context.Context, token string, input MemberMessageInput, key string) (domain.Message, error) {
	var result domain.Message
	err := s.withAuthorized(ctx, token, "send_message", func(scope store.MemberToolScope, _ MemberToolGrants) error {
		var err error
		result, err = s.db.SendMessageForTool(ctx, scope, input, key)
		return err
	})
	if err != nil {
		return domain.Message{}, err
	}
	return result, nil
}

// CreateMember applies exact-match automatic reuse and trusted capacity, not LocalUser policy.
func (s *MemberTools) CreateMember(ctx context.Context, token string, input MemberCreateInput, key string) (MemberCreateResult, error) {
	var result MemberCreateResult
	err := s.withAuthorized(ctx, token, "create_member", func(scope store.MemberToolScope, grants MemberToolGrants) error {
		var err error
		result, err = s.db.CreateMemberForTool(ctx, scope, input, store.MemberToolCreatePolicy{
			ProviderID: grants.ProviderID, Model: grants.Model, MaxMembers: grants.MaxGroupMembers,
		}, key)
		return err
	})
	if err != nil {
		return MemberCreateResult{}, err
	}
	return result, nil
}
