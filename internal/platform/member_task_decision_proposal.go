package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const ProposeTaskDecisionOperation = provider.ProposeTaskDecisionTool

var (
	ErrTaskDecisionProposalUnknown = errors.New("待决问题保存结果未确认；请保留原调用标识和内容用于对账")
	errTaskDecisionProposalSchema  = errors.New("Pi 任务待决问题需要已安装的待决事项记录结构")
)

// The distinct proof never leaves the digest-bound private native config except
// in the loopback header. A generic bearer or a startup check is not a grant.
type taskDecisionGrant struct {
	proof     string
	admitted  atomic.Bool
	mu        sync.Mutex // Serialize all writes for this capability, not reads.
	uncertain bool
}

func newTaskDecisionGrant() *taskDecisionGrant {
	var secret [32]byte
	_, _ = rand.Read(secret[:])
	return &taskDecisionGrant{proof: hex.EncodeToString(secret[:])}
}

func (s *MemberTools) TaskDecisionProposalsEnabled() bool {
	return s != nil && s.taskDecisions
}

func (s *MemberTools) checkTaskDecisionSchema(ctx context.Context) error {
	// F owns the installed-schema predicate. Use its bounded trusted read,
	// never a migration number or probe write. No rows leave this check.
	if _, err := s.chatEngine.ListPendingDecisionRequests(ctx, store.ListOptions{Limit: 1}); err != nil {
		return errors.Join(errTaskDecisionProposalSchema, err)
	}
	return nil
}

func taskDecisionCallMatches(token, proof, id, key string) bool {
	if len(id) == 0 || len(id) > 256 || strings.Contains(id, token) || strings.Contains(id, proof) {
		return false
	}
	for i := range len(id) {
		if id[i] < '!' || id[i] > '~' {
			return false
		}
	}
	digest := sha256.Sum256([]byte(ProposeTaskDecisionOperation + id))
	return key == hex.EncodeToString(digest[:])
}

// ProposeTaskDecision uses the exact fresh text owner's credential and holds
// revocation exclusion through F. Model data cannot choose any scope field.
func (s *MemberTools) ProposeTaskDecision(ctx context.Context, token, proof, callID, key string, input domain.TaskDecisionRequestInput) (domain.DecisionRequest, error) {
	var zero domain.DecisionRequest
	if !s.TaskDecisionProposalsEnabled() || len(proof) != 64 || !taskDecisionCallMatches(token, proof, callID, key) {
		return zero, store.ErrMemberToolDenied
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	if !entry.taskWorkProfileValid() || !slices.Contains(entry.grants.Operations, ProposeTaskDecisionOperation) {
		return zero, store.ErrMemberToolDenied
	}
	binding, scope := entry.taskWork, entry.scope
	grant := binding.decision
	if grant == nil || subtle.ConstantTimeCompare([]byte(grant.proof), []byte(proof)) != 1 || !grant.admitted.Load() {
		return zero, store.ErrMemberToolDenied
	}
	grant.mu.Lock()
	defer grant.mu.Unlock()
	if grant.uncertain {
		return zero, ErrTaskDecisionProposalUnknown
	}
	turn, err := s.db.GetTurn(ctx, scope.Claim.GroupID, scope.Claim.TurnID)
	if err != nil || !memberToolRoleFenceMatches(turn, scope.Claim) || turn.State != domain.TurnStateRunning ||
		turn.NativeSessionID != scope.Claim.TurnID || turn.AcceptedAt == nil || turn.CancelRequestedAt != nil || turn.CandidateTerminal != "" ||
		turn.DeliveryID != binding.delivery.DeliveryID || turn.MemberID != scope.MemberID || turn.ConversationID != scope.ConversationID {
		return zero, store.ErrMemberToolDenied // Definitively no Engine invocation.
	}
	// Acceptance cannot regress on the same live claim. F still serializes and
	// re-derives the current owner, Task/authority generation and version in-tx.
	out, err := s.chatEngine.ProposeTaskDecisionRequest(ctx, scope, input, key)
	if err != nil {
		// F #221 has no rollback evidence on this command. Even a conflict or
		// denied cause can be opaque here; never downgrade it to a safe retry.
		grant.uncertain = true
		return zero, ErrTaskDecisionProposalUnknown
	}
	return out, nil
}
