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
	if !entry.investigationIntake() {
		candidate.Policy.Investigation = nil
		candidate.Policy.InvestigationExecutionEnabled = false
	}
	return &candidate, nil
}

type memberToolIntakeSchema struct {
	piDiscussionExcerpt bool // The paired Pi bridge can advertise this optional field.
	investigation       bool // The exact opted-in Pi, fresh root only.
	investigationBlobs  bool // The runtime owns a separate artifact reader.
}

func (schema memberToolIntakeSchema) offersInvestigation(policy domain.ChatPolicy) bool {
	return schema.investigation && policy.Enabled && policy.InvestigationExecutionEnabled && policy.Investigation != nil &&
		(len(policy.Investigation.ArtifactVersions) == 0 || schema.investigationBlobs)
}

func (entry memberToolRegistration) investigationIntake() bool {
	return entry.purpose == domain.ChatIntakePurpose && entry.candidate != nil && entry.intake.offersInvestigation(entry.candidate.Policy)
}

// AssessChatIntent shares send_message's transport, not the local-user actor.
// Only the server-captured candidate from this exact execution can be assessed.
func (s *MemberTools) AssessChatIntent(ctx context.Context, token string, input domain.ChatAssessment, content, key string) (domain.ChatAssessmentReceipt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	if !s.isChatIntake(entry) || input.Action == domain.ChatActionReadOnlyInvestigation && !entry.investigationIntake() {
		return domain.ChatAssessmentReceipt{}, store.ErrMemberToolDenied
	}
	return s.chatEngine.AssessChatIntent(ctx, entry.scope, entry.candidate.ID, input, content, key)
}

func (s *MemberTools) withChatIntake(token string, use func(store.MemberToolScope, string) (domain.ChatAssessmentReceipt, error)) (domain.ChatAssessmentReceipt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	if !s.isChatIntake(entry) {
		return domain.ChatAssessmentReceipt{}, store.ErrMemberToolDenied
	}
	return use(entry.scope, entry.candidate.ID)
}

// Caller holds the registry lock; an absent registration cannot match.
func (s *MemberTools) isChatIntake(entry memberToolRegistration) bool {
	return entry.purpose == domain.ChatIntakePurpose && s.chatEngine != nil && entry.candidate != nil && slices.Contains(entry.grants.Operations, "send_message")
}
