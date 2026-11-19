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
func NewCodexAppServerThreadDecisionExecutor(selected Provider, endpoint, token string, bindLifetime func(context.Context) bool, timeout time.Duration) (Executor, error) {
	if selected.Name != Codex || selected.Version != "0.166.0" || bindLifetime == nil {
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
		config: memberToolConfig{endpoint: endpoint, origin: origin, token: token}, bindLifetime: bindLifetime}, nil
}

type codexAppServerThreadDecisionExecutor struct {
	codexAppServerExecutor
	config       memberToolConfig
	bindLifetime func(context.Context) bool
}

func (e codexAppServerThreadDecisionExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	return e.executeMember(ctx, request, onEvent, e.config, &codexAppServerMemberSession{
		names: []string{CodexThreadDecisionAckTool}, bindThreadDecisionContext: e.bindLifetime})
}
