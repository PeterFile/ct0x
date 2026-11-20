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
	size, tailReserve int
	truncated, failed bool
}

func (l *executionLog) Write(p []byte) (int, error) {
	n := len(p)
	if room := executionLogLimit - l.tailReserve - l.size; len(p) > room {
		p, l.truncated = p[:room], true
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
	delivery                   *executionDelivery
	codex                      bool
	frameLimit                 int
	runID                      string
	line                       []byte
	dropping, damaged, settled bool
	last                       Outcome
	piSession                  *piSessionTracker
	piTypedWork                *piTypedWorkOutput
	piInvestigation            *piInvestigationStartup
	piRunExplanation           bool
}

func (o *executionOutput) Write(p []byte) (int, error) {
	n, _ := o.log.Write(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if len(o.line)+len(part) > o.frameLimit {
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
	event.NativeSessionID = o.receipt.NativeSessionID
	if s := o.piInvestigation; s != nil {
		switch event.Kind {
		case ResponseEvent:
			s.response = event
			return
		case TerminalEvent:
			return // Only deliver after checking the entire framed stream.
		}
	}
	o.delivery.enqueue(event)
}

func (o *executionOutput) damageFrame() {
	o.damaged, o.last = true, ""
	if o.piInvestigation != nil {
		// A lost or malformed frame could hide a conflicting startup proof.
		o.piInvestigation.invalid = true
	}
}

// Pi 1.4.0 print-mode.js emits the header before prompt(). The ordinary
// adapter treats agent_start as acceptance. Native-session mode also needs the
// current prompt's user pair. Settlement follows retries/recovery/queued work.
func (o *executionOutput) recordPi(line []byte) {
	t := o.piSession
	if t != nil {
		if !t.observe(line) {
			o.damaged, o.last = true, ""
			return
		}
		if o.receipt.NativeSessionID == "" {
			// Only a validated header can be accepted before native identity is
			// established. Publish that ID, not a case-folded struct field.
			o.receipt.NativeSessionID = t.nativeID
			o.emit(Event{Kind: SessionEvent})
			return
		}
	}
	var event struct {
		Type, ID, CWD, ToolName string
		Version                 int
		Aborted                 *bool
		Message                 struct {
			Role, StopReason string
			Content          json.RawMessage
		}
	}
	if !utf8.Valid(line) || json.Unmarshal(line, &event) != nil || event.Type == "" ||
		o.piTypedWork != nil && !piTypedWorkHasExactType(line) {
		o.damageFrame()
		return
	}
	if s := o.piInvestigation; s != nil && !s.allow(o, event.Type, line) {
		if s.invalid && !o.receipt.Accepted {
			s.observeUnverifiedWrite(event.Type, line, o.runID)
		}
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
	if !r.Accepted && (t == nil && event.Type == "agent_start" || t != nil && !o.damaged && t.hasCurrentPrompt()) {
		r.Accepted = true
		o.emit(Event{Kind: AcceptedEvent})
	}
	// Decode genuine native evidence even if the opt-in stream never proved
	// that this invocation's prompt was accepted.
	agentStarted := r.Accepted || t != nil && t.starts > 0
	switch event.Type {
	case "session":
		if o.piSession != nil || event.Version != 3 || event.ID != o.runID || !filepath.IsAbs(event.CWD) || r.NativeSessionID != "" {
			o.damaged = true
			return
		}
		r.NativeSessionID = event.ID
		o.emit(Event{Kind: SessionEvent})
	case "agent_start":
		o.last = ""
	case "tool_execution_start":
		if o.piTypedWork != nil && !o.piTypedWork.observeStart(line, r.Accepted && !o.settled) {
			o.damaged = true
		}
	case "tool_execution_end":
		if o.piRunExplanation && event.ToolName == PublishRunExplanationTool && r.Accepted && !o.settled {
			if key := piRunExplanationAckKey(line, o.runID); key != "" {
				if r.RunExplanationAckKey == "" {
					r.RunExplanationAckKey = key
				} else if r.RunExplanationAckKey != key {
					// Conflicting acknowledgements cannot validate the attempt.
					r.RunExplanationAckKey, o.piRunExplanation, o.damaged = "", false, true
				}
			}
		}
		if o.piTypedWork != nil && !o.piTypedWork.observeEnd(line, o.runID, r.Accepted && !o.settled) {
			// A bad pair denies settlement but does not erase native terminal evidence.
			o.damaged = true
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
		if !agentStarted {
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
		// An empty candidate still replaces earlier text. Native-session success
		// later requires the shutdown-verified file to confirm the candidate.
		if r.Accepted && (t == nil || !t.answerUncertain) {
			o.emit(Event{Kind: ResponseEvent, Text: text.String()})
		}
	case "agent_settled":
		if event.Aborted == nil || !agentStarted || r.NativeSessionID == "" || o.settled {
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
			// The turn event sink requires acceptance before terminal callbacks.
			if r.Accepted {
				o.emit(Event{Kind: TerminalEvent, Terminal: terminal})
			}
		}
	}
}
