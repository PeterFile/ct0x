package provider

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

type codexExecutor struct{ executionCommand }

// NewCodexExecutor binds a fresh, read-only noninteractive turn. Native user
// configuration is ignored, but credentials remain with the original CLI.
// A nonpositive timeout defaults to ten minutes; the caller's deadline also applies.
func NewCodexExecutor(path string, timeout time.Duration) (Executor, error) {
	e, err := newExecutionCommand(path, timeout)
	if err != nil {
		return nil, err
	}
	return codexExecutor{e}, nil
}

func (e codexExecutor) Execute(ctx context.Context, request ExecuteRequest, onEvent func(Event) error) ExecutionReceipt {
	return e.executionCommand.execute(ctx, request, onEvent, true)
}

// Codex exec has one thread and one turn, but no native turn ID. Only completed
// agent messages are public snapshots; errors and tool items remain diagnostics.
func (o *executionOutput) recordCodex(line []byte) {
	var event struct {
		Type     string
		ThreadID string `json:"thread_id"`
		Item     struct {
			ID, Type string
			Text     *string
		}
		Usage *struct {
			Input  *uint64 `json:"input_tokens"`
			Cached *uint64 `json:"cached_input_tokens"`
			Output *uint64 `json:"output_tokens"`
		}
		Error *struct{ Message *string }
	}
	if !utf8.Valid(line) || json.Unmarshal(line, &event) != nil || event.Type == "" || o.settled {
		o.damaged = true
		return
	}
	r := o.receipt
	switch event.Type {
	case "thread.started":
		if event.ThreadID == "" || strings.TrimSpace(event.ThreadID) != event.ThreadID || strings.ContainsAny(event.ThreadID, "\x00\r\n") || r.NativeSessionID != "" {
			o.damaged = true
			return
		}
		r.NativeSessionID = event.ThreadID
		o.emit(Event{Kind: SessionEvent})
	case "turn.started":
		if r.NativeSessionID == "" || r.Accepted {
			o.damaged = true
			return
		}
		r.Accepted = true
		o.emit(Event{Kind: AcceptedEvent})
	case "item.completed":
		if event.Item.Type == "" {
			o.damaged = true
			return
		}
		if event.Item.Type != "agent_message" {
			return
		}
		if r.NativeSessionID == "" || !r.Accepted || event.Item.ID == "" || event.Item.Text == nil {
			o.damaged = true
			return
		}
		o.emit(Event{Kind: ResponseEvent, Text: *event.Item.Text})
	case "turn.completed", "turn.failed":
		if r.NativeSessionID == "" || !r.Accepted {
			o.damaged = true
			return
		}
		terminal := OutcomeSucceeded
		if event.Type == "turn.completed" {
			if event.Usage == nil || event.Usage.Input == nil || event.Usage.Cached == nil || event.Usage.Output == nil {
				o.damaged = true
				return
			}
		} else {
			if event.Error == nil || event.Error.Message == nil || strings.TrimSpace(*event.Error.Message) == "" {
				o.damaged = true
				return
			}
			terminal = OutcomeFailed
		}
		o.settled, r.Terminal = true, terminal
		o.emit(Event{Kind: TerminalEvent, Terminal: terminal})
	}
}
