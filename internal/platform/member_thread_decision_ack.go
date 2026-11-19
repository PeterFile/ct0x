package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const AckThreadDecisionOperation = provider.CodexThreadDecisionAckTool

// This registration never enters the ordinary member capability map: there is
// no MemberToolScope, inherited grant, or native resume reference.
type threadDecisionAckRegistration struct {
	claim           store.TurnClaim
	answerID        string
	native          context.Context // Bound once, under MemberTools.mu, before child startup.
	acceptanceSaved chan struct{}   // Closed only after the scheduler persists native acceptance.
	revoked         chan struct{}

	observedAcceptance atomic.Pointer[time.Time]
	observedTerminal   atomic.Bool

	callOnce  sync.Once
	callKey   string
	uncertain atomic.Bool
}

func (s *MemberTools) commitThreadDecisionAckLaunch(ctx context.Context, owner *memberToolRoleFenceOwner, launch store.ThreadDecisionLaunch) (string, *threadDecisionAckRegistration, error) {
	// Distinct from ordinary rand.Text() capabilities. The prefix grants nothing.
	token := "thread-ack-" + rand.Text()
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.chatEngine.UsesStore(s.db) || token == owner.claim.Token || s.threadDecisionAcks[key] != nil || len(s.threadDecisionAcks) >= maxMemberToolRegistrations {
		return "", nil, store.ErrMemberToolDenied
	}
	// Registry -> Store. The only callback in F's transaction spends the
	// existing owner. Publish the registration only after a successful commit.
	if err := s.db.CommitThreadDecisionLaunch(ctx, owner.claim, launch, owner.beginInvoke); err != nil {
		return "", nil, err
	}
	ack := &threadDecisionAckRegistration{claim: owner.claim, answerID: launch.Wake.Receipt.ID,
		acceptanceSaved: make(chan struct{}), revoked: make(chan struct{})}
	if s.threadDecisionAcks == nil {
		s.threadDecisionAcks = make(map[[sha256.Size]byte]*threadDecisionAckRegistration)
	}
	s.threadDecisionAcks[key] = ack
	return token, ack, nil
}

func (s *MemberTools) bindThreadDecisionAckContext(token string, ack *threadDecisionAckRegistration, ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threadDecisionAcks[sha256.Sum256([]byte(token))] != ack || ack.native != nil || ctx.Err() != nil {
		return false
	}
	ack.native = ctx
	return true
}

func (s *MemberTools) revokeThreadDecisionAckLocked(key [sha256.Size]byte) bool {
	ack := s.threadDecisionAcks[key]
	if ack == nil {
		return false
	}
	delete(s.threadDecisionAcks, key)
	close(ack.revoked)
	return true
}

// Called by the parser, before logging or asynchronous delivery. Never take
// the registry lock here: it can be held through a Store transaction.
func (a *threadDecisionAckRegistration) observeLifecycle(kind provider.EventKind) {
	switch kind {
	case provider.AcceptedEvent:
		at := time.Now() // Keep the monotonic clock for comparison with HTTP ingress.
		a.observedAcceptance.Store(&at)
	case provider.TerminalEvent:
		a.observedTerminal.Store(true)
	}
}

// Caller holds the registry lock. Discovery can happen before acceptance.
func (a *threadDecisionAckRegistration) live() bool {
	return a.native != nil && a.native.Err() == nil && !a.observedTerminal.Load()
}

// AcknowledgeThreadDecision serves only the dedicated MCP operation. The
// transport supplies its stable key and server-side ingress time, but cannot
// select a claim or answer.
func (s *MemberTools) AcknowledgeThreadDecision(ctx context.Context, token, key string, entered time.Time) (domain.DecisionAnswerView, error) {
	var zero domain.DecisionAnswerView
	digest := sha256.Sum256([]byte(token))
	s.mu.RLock()
	ack := s.threadDecisionAcks[digest]
	if ack == nil || ctx.Err() != nil {
		s.mu.RUnlock()
		return zero, store.ErrMemberToolDenied
	}
	observed := ack.observedAcceptance.Load()
	// The terminal load in live is the admission fence, ordered against the
	// parser's terminal store. Body parsing or lock contention cannot make a
	// request that entered before native acceptance eligible.
	if observed == nil || !observed.Before(entered) || !ack.live() {
		s.mu.RUnlock()
		return zero, store.ErrMemberToolDenied
	}
	native := ack.native
	s.mu.RUnlock()

	// Only admitted calls may wait for the asynchronous scheduler write.
	// Do not hold the registry lock or use a Store write as a readiness probe.
	callCtx, cancel := context.WithTimeout(native, memberToolRuntimeVerificationTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	select {
	case <-ack.acceptanceSaved:
	case <-ack.revoked:
		return zero, store.ErrMemberToolDenied
	case <-callCtx.Done():
		return zero, store.ErrMemberToolDenied
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.threadDecisionAcks[digest] != ack || native.Err() != nil || ctx.Err() != nil || callCtx.Err() != nil {
		return zero, store.ErrMemberToolDenied
	}
	// Admission already preceded native terminal observation. The Store now
	// arbitrates against any durable terminal, and revocation still drains.
	// Freeze the first attempted identity even if Store cannot confirm its
	// outcome. Revoke waits through the F transaction and uncertainty latch.
	ack.callOnce.Do(func() { ack.callKey = key })
	if ack.callKey != key {
		return zero, store.ErrConflict
	}
	view, err := s.chatEngine.AcknowledgeThreadDecision(callCtx, ack.claim, ack.answerID, key)
	if err == nil && (view.Receipt.ID != ack.answerID || view.Handling == nil || view.Handling.TurnID != ack.claim.TurnID ||
		view.Handling.AttemptID != "" || view.Handling.ConsumedAt.IsZero()) {
		err = store.ErrTaskWorkMutationUnknown
	}
	if errors.Is(err, store.ErrTaskWorkMutationUnknown) {
		ack.uncertain.Store(true)
	}
	if err != nil {
		return zero, err
	}
	return view, nil
}
