package coordination

import (
	"context"
	"errors"
	"time"

	"agentworkspace/internal/domain"
)

var ErrDispatchRuntimeNotReady = errors.New("协调运行时尚未就绪，分派物化未启用")

// MaterializeDispatchRequest is deliberately unavailable, even with a working Store.
// N4 must review role tools, provider readiness, postclaim fencing and forwarding;
// replacing this gate with an unconditional Store call is not sufficient.
func (e *Engine) MaterializeDispatchRequest(ctx context.Context, requestID string, expectedGeneration int64, at time.Time) (domain.DispatchMaterialization, error) {
	return domain.DispatchMaterialization{}, ErrDispatchRuntimeNotReady
}
