package coordination

import (
	"context"
	"fmt"

	"agentworkspace/internal/store"
)

// NewTrustedIntentPass binds one process-local pass to the Engine's Store.
// The caller must invoke the callback serially; report runs synchronously.
// Construction neither establishes runtime readiness nor opens public gates.
func NewTrustedIntentPass(engine *Engine, report func(error)) (func(context.Context), error) {
	if report == nil {
		return nil, fmt.Errorf("%w：物化扫描需要有效的错误报告器", store.ErrInvalid)
	}
	pass, err := newIntentPass(engine)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) {
		if err := pass.run(ctx); err != nil {
			report(err)
		}
	}, nil
}
