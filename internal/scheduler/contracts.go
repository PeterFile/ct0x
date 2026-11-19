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
}

type PrepareFunc func(context.Context, store.PendingDelivery) (PreparedExecution, error)

// ErrNotReady leaves the delivery queued while scheduling continues elsewhere.
var ErrNotReady = errors.New("执行配置尚未就绪，投递继续排队")

type Options struct {
	MaxActive    int
	PollInterval time.Duration
	RuntimeDir   string
	// IntentPass synchronously observes intents once per scheduling cycle using the run context.
	IntentPass func(context.Context)
	// Recovered declares singleton ownership and successful explicit startup recovery.
	Recovered bool
}
