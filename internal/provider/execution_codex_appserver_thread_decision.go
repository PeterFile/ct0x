package provider

import (
	"context"
	"time"
)

const CodexThreadDecisionAckTool = "ack_thread_decision"

// NewCodexAppServerThreadDecisionExecutor reuses the restricted member MCP
// transport on a fresh thread, never the ordinary grant or resume profiles.
// The trusted caller issues the in-memory bearer after the typed launch fence.
// bindLifetime binds the registration to the adapter's actual cancellation and
// timeout context before the child can connect; false refuses the launch.
// observeLifecycle receives only validated acceptance and terminal observations,
// synchronously before event delivery. It must not wait for persistence.
func NewCodexAppServerThreadDecisionExecutor(selected Provider, endpoint, token string, bindLifetime func(context.Context) bool, observeLifecycle func(EventKind), timeout time.Duration) (Executor, error) {
	if selected.Name != Codex || selected.Version != "0.166.0" || bindLifetime == nil || observeLifecycle == nil {
		return nil, errExecutorConfiguration
	}
	if err := ValidateExecutor(selected, timeout); err != nil {
		return nil, err
	}
	origin := memberToolConfigOrigin(endpoint, token)
	if origin == "" {
		return nil, errExecutorConfiguration
	}
	command, err := newExecutionCommand(selected.Path, timeout)
	if err != nil {
		return nil, err
	}
	return codexAppServerThreadDecisionExecutor{codexAppServerExecutor: codexAppServerExecutor{command},
		config: memberToolConfig{endpoint: endpoint, origin: origin, token: token},
		bindLifetime: bindLifetime, observeLifecycle: observeLifecycle}, nil
}

type codexAppServerThreadDecisionExecutor struct {
	codexAppServerExecutor
	config           memberToolConfig
	bindLifetime     func(context.Context) bool
	observeLifecycle func(EventKind)
}

type codexAppServerThreadDecisionSession struct {
	bindLifetime     func(context.Context) bool
	observeLifecycle func(EventKind)
	bearer           string // In-memory only; reject reflection in native lifecycle IDs.
}

func (e codexAppServerThreadDecisionExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	return e.executeMember(ctx, request, onEvent, e.config, &codexAppServerMemberSession{
		names: []string{CodexThreadDecisionAckTool}, threadDecision: &codexAppServerThreadDecisionSession{
			bindLifetime: e.bindLifetime, observeLifecycle: e.observeLifecycle, bearer: e.config.token,
		},
	})
}
