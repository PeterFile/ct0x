package provider

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode"
)

type piSessionToolExecution struct {
	call                   piSessionCall
	started, ended, result bool
	resultSHA              string
}

// This latch is invocation-local but permanent: a later successful assistant
// cannot erase an errored/unpaired platform write when a new bridge is launched.
// All tools are checked conservatively, including the four platform write tools.
type piSessionTracker struct {
	history             *piSessionHistory
	executions          map[string]*piSessionToolExecution
	expectedID, prompt  string
	priorMessages       []string
	nativeID, headerSHA string
	anchorEligible      bool
	uncertain, settled  bool
	starts              int
	openRole            string
	userStartMS, userMS int64
	answerUncertain     bool
}

func newPiSessionTracker(profile piSessionProfile, expectedID, prompt string) *piSessionTracker {
	history := newPiSessionHistory(profile)
	// Resume may emit no new system patch. The persisted history independently
	// proves the effective loadout; argv always supplies this exact current list.
	for _, name := range profile.tools {
		history.activeTools[name] = true
	}
	// Pi 1.4 main.js applies JavaScript String.trim() to piped stdin.
	prompt = strings.TrimFunc(prompt, func(r rune) bool { return r == '\uFEFF' || r != '\u0085' && unicode.IsSpace(r) })
	return &piSessionTracker{history: history, expectedID: expectedID, prompt: prompt, anchorEligible: true, executions: make(map[string]*piSessionToolExecution)}
}

func piSessionToolResultDigest(result map[string]any) string {
	return piSessionDigest(map[string]any{"content": result["content"], "details": result["details"], "usage": result["usage"]})
}

// false means the record cannot provide unambiguous native stream evidence.
// Anchor eligibility and execution uncertainty never gate later tool observation.
func (t *piSessionTracker) observe(line []byte) bool {
	value, err := nativeSessionJSON(line, 64, false)
	event, ok := value.(map[string]any)
	if err != nil || !ok {
		t.anchorEligible = false
		return false
	}
	kind := piSessionString(event, "type")
	if kind == "session" {
		if t.nativeID != "" || !validPiSessionHeader(event, t.history.profile.cwd, "") {
			t.anchorEligible = false
			return false
		}
		t.nativeID, t.headerSHA = piSessionString(event, "id"), piSessionDigest(event)
		if t.expectedID != "" && t.expectedID != t.nativeID {
			// Observe the actual identity/lifecycle, but never adopt this session.
			t.loseExecutionCertainty()
		}
		return true
	}
	if t.nativeID == "" || t.settled || kind == "" {
		t.anchorEligible = false
		return false
	}
	// Reject aliases before the case-folding Go decoder sees the frame. The
	// strict JSON parse above has already rejected duplicates at every depth.
	if piSessionEventHasAliases(event, kind) {
		t.loseAnswerCertainty()
		return false
	}
	switch kind {
	case "agent_start":
		t.starts++
		if t.starts != 1 {
			t.loseAnswerCertainty()
			return false
		}
	case "queue_update":
		for _, key := range []string{"steering", "followUp"} {
			queue, ok := event[key].([]any)
			if !ok || len(queue) != 0 {
				t.anchorEligible = false
				return false
			}
		}
	case "agent_settled":
		aborted, ok := event["aborted"].(bool)
		if !ok {
			t.anchorEligible = false
			return false
		}
		t.settled = true
		if t.userMS == 0 || t.openRole != "" {
			t.loseAnswerCertainty()
		}
		if aborted {
			t.anchorEligible = false
		}
	case "message_start", "message_end":
		message, _ := event["message"].(map[string]any)
		t.observeMessage(message, kind == "message_start")
	case "tool_execution_start", "tool_execution_update", "tool_execution_end":
		t.observeToolExecution(event, kind)
	case "message_update", "turn_start", "turn_end", "agent_end":
		// No new persistence or authorization is inferred from these events.
	default:
		// Compaction/retry/custom/session changes are outside this first slice.
		t.anchorEligible = false
	}
	return true
}

func piSessionEventHasAliases(event map[string]any, kind string) bool {
	// The common decoder visits these top-level fields even on other events.
	if piSessionHasFieldAlias(event, "type", "id", "cwd", "version", "aborted", "message") {
		return true
	}
	if message, ok := event["message"].(map[string]any); ok {
		if piSessionHasFieldAlias(message, "role", "content", "stopReason", "timestamp", "provider", "model", "thinkingLevel",
			"toolsAdded", "toolsRemoved", "toolCallId", "toolName", "isError", "details", "usage", "nestedCalls") || piSessionContentHasAliases(message["content"]) {
			return true
		}
		for _, field := range []string{"toolsAdded", "toolsRemoved"} {
			tools, _ := message[field].([]any)
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				if piSessionHasFieldAlias(tool, "name") {
					return true
				}
			}
		}
	}
	switch kind {
	case "queue_update":
		return piSessionHasFieldAlias(event, "steering", "followUp")
	case "tool_execution_start", "tool_execution_update", "tool_execution_end":
		result, _ := event["result"].(map[string]any)
		return piSessionHasFieldAlias(event, "toolCallId", "toolName", "args", "result", "isError") ||
			piSessionHasFieldAlias(result, "content", "details", "usage", "isError", "nestedCalls") || piSessionContentHasAliases(result["content"])
	}
	return false
}

func piSessionContentHasAliases(content any) bool {
	blocks, _ := content.([]any)
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if piSessionHasFieldAlias(block, "type", "text", "thinking", "redacted", "id", "name", "arguments", "namespace") {
			return true
		}
	}
	return false
}

func (t *piSessionTracker) loseExecutionCertainty() {
	t.anchorEligible, t.uncertain = false, true
}

func (t *piSessionTracker) loseAnswerCertainty() {
	t.answerUncertain = true
	t.loseExecutionCertainty()
}

func (t *piSessionTracker) matchesPrompt(content any) bool {
	if t.prompt == "" {
		return false
	}
	if text, ok := content.(string); ok {
		return text == t.prompt
	}
	// agent-session.js creates one text block for the plain stdin prompt.
	blocks, ok := content.([]any)
	if !ok || len(blocks) != 1 {
		return false
	}
	block, _ := blocks[0].(map[string]any)
	return piSessionString(block, "type") == "text" && piSessionString(block, "text") == t.prompt
}

func piSessionMessageMillis(message map[string]any) int64 {
	stamp, _ := message["timestamp"].(json.Number)
	ms, err := stamp.Int64()
	if err != nil {
		return 0
	}
	return ms
}

func (t *piSessionTracker) hasCurrentPrompt() bool {
	return t.starts == 1 && t.userMS > 0 && !t.answerUncertain
}

func (t *piSessionTracker) isPriorMessage(message map[string]any) bool {
	return len(t.priorMessages) != 0 && slices.Contains(t.priorMessages, piSessionDigest(message))
}

func (t *piSessionTracker) observeMessage(message map[string]any, starting bool) {
	role := piSessionString(message, "role")
	if !slices.Contains([]string{"system", "user", "assistant", "toolResult"}, role) {
		t.loseAnswerCertainty()
		return
	}
	// Pi 1.4 agent-loop.js emits the current user pair after agent_start and
	// before the assistant. Assistant starts may be partial; the matching end
	// is authoritative. Do not equate their content or require different ms.
	ms := piSessionMessageMillis(message)
	if starting {
		if t.openRole != "" {
			t.loseAnswerCertainty()
		}
		t.openRole = role
	} else {
		if t.openRole != role {
			t.loseAnswerCertainty()
		}
		t.openRole = ""
	}
	switch role {
	case "user":
		if t.starts != 1 || t.userMS != 0 || ms <= 0 || !t.matchesPrompt(message["content"]) {
			t.loseAnswerCertainty()
		} else if starting {
			t.userStartMS = ms
		} else if t.userStartMS != ms || t.isPriorMessage(message) {
			t.loseAnswerCertainty()
		} else {
			t.userMS = ms
		}
	case "assistant":
		if t.userMS == 0 || ms < t.userMS || !starting && t.isPriorMessage(message) {
			t.loseAnswerCertainty()
		}
	}
	if starting {
		return
	}
	// History can reject a model/loadout/format without proving that a tool
	// did not run. Track tool facts independently, even after that rejection.
	t.observeToolMessage(message)
	if t.anchorEligible && !t.history.message(message) {
		t.anchorEligible = false
	}
}

func (t *piSessionTracker) observeToolMessage(message map[string]any) {
	switch piSessionString(message, "role") {
	case "assistant":
		blocks, _ := message["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			if piSessionString(block, "type") != "toolCall" {
				continue
			}
			id, name := piSessionString(block, "id"), piSessionString(block, "name")
			args, ok := block["arguments"].(map[string]any)
			if len(id) == 0 || len(id) > 256 || !slices.Contains(t.history.profile.tools, name) || block["namespace"] != nil || !ok || t.executions[id] != nil || len(t.executions) >= piSessionEntryLimit {
				t.loseExecutionCertainty()
				continue
			}
			t.executions[id] = &piSessionToolExecution{call: piSessionCall{name: name, argsSHA: piSessionDigest(args)}}
		}
	case "toolResult":
		execution := t.executions[piSessionString(message, "toolCallId")]
		if execution == nil || execution.call.name != piSessionString(message, "toolName") || !execution.ended || execution.result ||
			message["isError"] != false || !piSessionText(message["content"]) || message["nestedCalls"] != nil || execution.resultSHA != piSessionToolResultDigest(message) {
			t.loseExecutionCertainty()
			return
		}
		execution.result = true
	}
}

func (t *piSessionTracker) observeToolExecution(event map[string]any, kind string) {
	execution := t.executions[piSessionString(event, "toolCallId")]
	if execution == nil || execution.call.name != piSessionString(event, "toolName") || execution.ended {
		t.loseExecutionCertainty()
		return
	}
	if kind == "tool_execution_start" || kind == "tool_execution_update" {
		args, ok := event["args"].(map[string]any)
		if !ok || piSessionDigest(args) != execution.call.argsSHA ||
			kind == "tool_execution_start" && execution.started || kind == "tool_execution_update" && !execution.started {
			t.loseExecutionCertainty()
			return
		}
		execution.started = true
		return
	}
	result, ok := event["result"].(map[string]any)
	if !execution.started || !ok || event["isError"] != false || result["isError"] != nil && result["isError"] != false ||
		!piSessionText(result["content"]) || result["nestedCalls"] != nil {
		t.loseExecutionCertainty()
		return
	}
	execution.ended, execution.resultSHA = true, piSessionToolResultDigest(result)
}

// Called after parser drain. Native settlement cannot prove an unpaired tool or
// an errored external write completed. A later successful result cannot reset it.
func (t *piSessionTracker) hasUncertainExecution() bool {
	if t.uncertain {
		return true
	}
	for _, execution := range t.executions {
		if !execution.started || !execution.ended || !execution.result {
			return true
		}
	}
	return false
}

func (t *piSessionTracker) complete() bool {
	return t.anchorEligible && t.settled && t.starts == 1 && t.openRole == "" && t.history.users == 1 && t.history.complete() &&
		len(t.executions) == len(t.history.calls) && !t.hasUncertainExecution()
}
