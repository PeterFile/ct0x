package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"unicode/utf8"
)

const executionFrameLimit = 64 << 10
const executionLogLimit = 1 << 20

// A full log or a write error never stops pipe draining. No stderr is retained
// in memory or promoted to product messages. Log failures cancel the invocation.
type executionLog struct {
	file              *os.File
	cancel            context.CancelFunc
	size              int
	truncated, failed bool
}

func (l *executionLog) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > executionLogLimit-l.size {
		p, l.truncated = p[:executionLogLimit-l.size], true
	}
	if !l.failed && len(p) != 0 {
		written, err := l.file.Write(p)
		l.size += written
		if err != nil || written != len(p) {
			l.failed = true
			l.cancel()
		}
	}
	return n, nil
}

type executionOutput struct {
	log                        *executionLog
	receipt                    *ExecutionReceipt
	callback                   func(Event) error
	cancel                     context.CancelFunc
	ctx                        context.Context
	codex                      bool
	runID                      string
	line                       []byte
	dropping, damaged, settled bool
	last                       Outcome
}

func (o *executionOutput) Write(p []byte) (int, error) {
	n, _ := o.log.Write(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if len(o.line)+len(part) > executionFrameLimit {
			o.dropping, o.damaged, o.last = true, true, ""
		}
		if !o.dropping {
			o.line = append(o.line, part...)
		}
		if i < 0 {
			break
		}
		if !o.dropping {
			line := bytes.TrimSuffix(o.line, []byte{'\r'})
			if o.codex {
				o.recordCodex(line)
			} else {
				o.recordPi(line)
			}
		}
		o.line, o.dropping = o.line[:0], false
		p = p[i+1:]
	}
	return n, nil
}

func (o *executionOutput) emit(event Event) {
	if o.callback != nil && !o.receipt.DeliveryFailed && o.ctx.Err() == nil {
		event.NativeSessionID = o.receipt.NativeSessionID
		if o.callback(event) != nil {
			o.receipt.DeliveryFailed = true
			o.cancel()
		}
	}
}

// Pi 1.4.0 print-mode.js emits the header before prompt(). agent_start is
// therefore the adapter's acceptance rule, not a native acknowledgement.
// agent-session.js settles only after retries/recovery/queued work finish.
func (o *executionOutput) recordPi(line []byte) {
	var event struct {
		Type, ID, CWD string
		Version       int
		Aborted       *bool
		Message       struct {
			Role, StopReason string
			Content          json.RawMessage
		}
	}
	if !utf8.Valid(line) || json.Unmarshal(line, &event) != nil || event.Type == "" {
		o.damaged, o.last = true, ""
		return
	}
	r := o.receipt
	if event.Type != "session" && r.NativeSessionID == "" {
		o.damaged, o.last = true, ""
		return
	}
	if o.settled {
		o.damaged = true
	}
	switch event.Type {
	case "session":
		if event.Version != 3 || event.ID != o.runID || !filepath.IsAbs(event.CWD) || r.NativeSessionID != "" {
			o.damaged = true
			return
		}
		r.NativeSessionID = event.ID
		o.emit(Event{Kind: SessionEvent})
	case "agent_start":
		o.last = ""
		if !r.Accepted {
			r.Accepted = true
			o.emit(Event{Kind: AcceptedEvent})
		}
	case "message_start", "message_end":
		if event.Message.Role == "" {
			o.damaged, o.last = true, ""
			return
		}
		if event.Message.Role != "assistant" {
			return
		}
		o.last = ""
		if !r.Accepted {
			o.damaged = true
			return
		}
		if event.Type == "message_start" {
			return
		}
		var blocks []struct {
			Type string
			Text *string
		}
		if len(event.Message.Content) == 0 || event.Message.Content[0] != '[' || json.Unmarshal(event.Message.Content, &blocks) != nil {
			o.damaged = true
			return
		}
		var text bytes.Buffer
		for _, block := range blocks {
			if block.Type == "" || (block.Type == "text" && block.Text == nil) {
				o.damaged = true
				return
			}
			if block.Type == "text" {
				text.WriteString(*block.Text)
			}
		}
		// Only a complete assistant's stop reason decides the final result.
		// Earlier error messages must not poison a later successful retry.
		o.last = map[string]Outcome{"stop": OutcomeSucceeded, "error": OutcomeFailed, "aborted": OutcomeAborted, "length": OutcomeTruncated}[event.Message.StopReason]
		o.emit(Event{Kind: ResponseEvent, Text: text.String()})
	case "agent_settled":
		if event.Aborted == nil || !r.Accepted || r.NativeSessionID == "" || o.settled {
			o.damaged = true
			return
		}
		o.settled = true
		terminal := o.last
		if *event.Aborted {
			terminal = OutcomeAborted
		}
		if terminal != "" {
			r.Terminal = terminal
			o.emit(Event{Kind: TerminalEvent, Terminal: terminal})
		}
	}
}
