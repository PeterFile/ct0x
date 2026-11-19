package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// ErrThreadDecisionRequestUnknown does not imply rollback. Preserve the exact
// call identity and content; a later native terminal is not write evidence.
var ErrThreadDecisionRequestUnknown = errors.New("待决请求保存结果未确认；请保留原调用标识和原请求内容用于对账")

// This fence is local to the issued intake. Cross-restart proposal identity,
// rollback evidence, and the Thread retry ledger are still F follow-ups.
type threadDecisionIntakeGrant struct {
	writes    sync.Mutex
	uncertain atomic.Bool
}

// Hold the registry read lock through the mutation and the uncertainty latch
// so revocation drains both. Reads and intakes without this grant do not take
// the write lock. There is no replay exception, even for the original key.
func (g *threadDecisionIntakeGrant) lockMutation() (unlock func(), ok bool) {
	if g == nil {
		return func() {}, true
	}
	g.writes.Lock()
	if g.uncertain.Load() {
		g.writes.Unlock()
		return nil, false
	}
	return g.writes.Unlock, true
}

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
	decision := entry.intake.threadDecision
	unlock, ok := decision.lockMutation()
	if !ok {
		return domain.DecisionRequest{}, store.ErrMemberToolDenied
	}
	defer unlock()
	request, err := s.chatEngine.AssessChatDecisionClarification(ctx, entry.scope, entry.candidate.ID, input, details, content, key)
	if err == nil {
		return request, nil
	}
	if threadDecisionRejectionConfirmed(err) {
		return domain.DecisionRequest{}, err
	}
	decision.uncertain.Store(true)
	return domain.DecisionRequest{}, errors.Join(ErrThreadDecisionRequestUnknown, err)
}

func threadDecisionRejectionConfirmed(err error) bool {
	// F's returned error must attest to rollback or no mutation. A business
	// category or a marker found only in an inner cause cannot prove that.
	if _, ok := err.(interface{ RollbackConfirmed() bool }); !ok {
		return false
	}
	// Only a single chain to a business category is accepted; negative
	// outcome evidence anywhere in that chain takes precedence.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if marker, ok := cause.(interface{ CommitOutcomeUnknown() bool }); ok && marker.CommitOutcomeUnknown() {
			return false
		}
		if marker, ok := cause.(interface{ RollbackConfirmed() bool }); ok && !marker.RollbackConfirmed() {
			return false
		}
		switch cause {
		case store.ErrMemberToolDenied, store.ErrInvalid, store.ErrConflict, store.ErrDecisionRequestsUnavailable:
			return true
		}
	}
	return false
}
