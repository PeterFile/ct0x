package coordination

import (
	"context"
	"fmt"
	"strings"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// RequestWork is the explicitly local-user command, never a model/tool entry point.
func (e *Engine) RequestWork(ctx context.Context, groupID string, request domain.RequestWorkRequest, key string) (domain.AdmissionReceipt, error) {
	if strings.TrimSpace(key) == "" {
		return domain.AdmissionReceipt{}, fmt.Errorf("%w：工作请求必须提供幂等键", store.ErrInvalid)
	}
	return e.store.RequestWork(ctx, groupID, request, key)
}
