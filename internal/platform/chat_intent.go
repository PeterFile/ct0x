package platform

import (
	"context"
	"crypto/sha256"
	"slices"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// EnableChatIntake is trusted startup composition only. It cannot rotate policy
// or upgrade an issued capability, and the Engine must borrow this exact Store.
func (s *MemberTools) EnableChatIntake(engine *coordination.Engine) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !engine.UsesStore(s.db) || s.chatEngine != nil || len(s.capabilities) != 0 {
		return store.ErrMemberToolDenied
	}
	s.chatEngine = engine
	return nil
}

// ChatCandidate exposes captured source/policy metadata, never a claim token.
// A legacy capability has no candidate and retains the old response shape.
func (s *MemberTools) ChatCandidate(ctx context.Context, token string) (*domain.ChatCandidate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found || !slices.Contains(entry.grants.Operations, "read_channel") {
		return nil, store.ErrMemberToolDenied
	}
	if entry.candidate == nil {
		return nil, nil
	}
	if err := s.db.ValidateMemberTool(ctx, entry.scope); err != nil {
		return nil, memberRoleError(ctx, err)
	}
	candidate := *entry.candidate
	return &candidate, nil
}

// AssessChatIntent shares send_message's transport, not the local-user actor.
// Only the server-captured candidate from this exact execution can be assessed.
func (s *MemberTools) AssessChatIntent(ctx context.Context, token string, input domain.ChatAssessment, content, key string) (domain.ChatAssessmentReceipt, error) {
	return s.withChatIntake(token, func(scope store.MemberToolScope, candidateID string) (domain.ChatAssessmentReceipt, error) {
		return s.chatEngine.AssessChatIntent(ctx, scope, candidateID, input, content, key)
	})
}

func (s *MemberTools) withChatIntake(token string, use func(store.MemberToolScope, string) (domain.ChatAssessmentReceipt, error)) (domain.ChatAssessmentReceipt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found || entry.purpose != domain.ChatIntakePurpose || s.chatEngine == nil || entry.candidate == nil || !slices.Contains(entry.grants.Operations, "send_message") {
		return domain.ChatAssessmentReceipt{}, store.ErrMemberToolDenied
	}
	return use(entry.scope, entry.candidate.ID)
}
