package scheduler

import (
	"context"
	"errors"
	"time"

	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// PreparedExecution contains trusted configuration only; preparation never starts execution.
type PreparedExecution struct {
	Executor          provider.Executor
	Request           provider.ExecuteRequest
	ResourceKeys      []string
	BackgroundProfile store.BackgroundDispatchProfile // Nonzero only for an actual A1 tool-free run.
	// BindTurnClaim optionally receives the original committed claim before Execute.
	// The claim stays in trusted Go code, outside the provider request.
	BindTurnClaim func(store.TurnClaim) `json:"-"`
}

type PrepareFunc func(context.Context, store.PendingDelivery) (PreparedExecution, error)

// PreparedThreadDecision may only be used in the typed lane. Execute receives
// the exact generation returned by ClaimTurn, never a refreshed token.
type PreparedThreadDecision struct {
	Execute      func(context.Context, store.TurnClaim, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt
	Request      provider.ExecuteRequest
	ResourceKeys []string
}

type PrepareThreadDecisionFunc func(context.Context, store.PendingDelivery, store.ThreadDecisionWake) (PreparedThreadDecision, error)

// ErrNotReady leaves the delivery queued while scheduling continues elsewhere.
var ErrNotReady = errors.New("执行配置尚未就绪，投递继续排队")

type Options struct {
	MaxActive    int
	PollInterval time.Duration
	RuntimeDir   string
	// IntentPass synchronously observes intents once per scheduling cycle using the run context.
	IntentPass func(context.Context)
	// PrepareThreadDecision opts into the typed lane. Trusted composition must
	// enable F's consumer on this Store only after the adapter is ready.
	PrepareThreadDecision PrepareThreadDecisionFunc
	// Recovered declares singleton ownership and successful explicit startup recovery.
	Recovered bool
}
