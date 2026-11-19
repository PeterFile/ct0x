package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// ErrThreadDecisionRequestUnknown does not imply rollback. Preserve the exact
// call identity and content; a later native terminal is not write evidence.
var ErrThreadDecisionRequestUnknown = errors.New("待决请求保存结果未确认；请保留原调用标识和原请求内容用于对账")

type threadDecisionIntakeGrant struct{ uncertain atomic.Bool }

func (s *MemberTools) offersThreadDecision(entry memberToolRegistration) bool {
	return entry.intake.threadDecision != nil && s.isChatIntake(entry) && entry.candidate.SourceKind == "local_user_chat" &&
		entry.candidate.SourceSenderID == "user" && entry.candidate.SourceID != ""
}

// The existing send_message grant, claimed intake purpose and explicitly
// issued schema are all required. The candidate/scope never come from JSON.
// F owns the exact direct-recipient, current claim and replay checks.
func (s *MemberTools) AssessChatDecisionClarification(ctx context.Context, token string, input domain.ChatAssessment, details domain.DecisionContext, content, key string) (domain.DecisionRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	if !s.offersThreadDecision(entry) {
		return domain.DecisionRequest{}, store.ErrMemberToolDenied
	}
	request, err := s.chatEngine.AssessChatDecisionClarification(ctx, entry.scope, entry.candidate.ID, input, details, content, key)
	if err == nil {
		return request, nil
	}
	// F exposes stable business rejections but no request-commit certainty
	// marker here. Opaque, joined and context errors may not become a denial
	// or cancellation that the caller could mistake for confirmed rollback.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if marker, ok := cause.(interface{ CommitOutcomeUnknown() bool }); ok && marker.CommitOutcomeUnknown() {
			break
		}
		if marker, ok := cause.(interface{ RollbackConfirmed() bool }); ok && !marker.RollbackConfirmed() {
			break
		}
		switch cause {
		case store.ErrMemberToolDenied, store.ErrInvalid, store.ErrConflict, store.ErrDecisionRequestsUnavailable:
			return domain.DecisionRequest{}, err
		}
	}
	entry.intake.threadDecision.uncertain.Store(true)
	return domain.DecisionRequest{}, errors.Join(ErrThreadDecisionRequestUnknown, err)
}
