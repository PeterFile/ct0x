package provider

import "slices"

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
	expectedID          string
	nativeID, headerSHA string
	anchorEligible      bool
	uncertain, settled  bool
	starts              int
	openRole            string
}

func newPiSessionTracker(profile piSessionProfile, expectedID string) *piSessionTracker {
	history := newPiSessionHistory(profile)
	// Resume may emit no new system patch. The persisted history independently
	// proves the effective loadout; argv always supplies this exact current list.
	for _, name := range profile.tools {
		history.activeTools[name] = true
	}
	return &piSessionTracker{history: history, expectedID: expectedID, anchorEligible: true, executions: make(map[string]*piSessionToolExecution)}
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
	// A folded discriminator can hide a second session header among ordinary
	// events. Treat that as stream damage, not just loss of the file anchor.
	if t.nativeID == "" || t.settled || kind == "" || piSessionHasFieldAlias(event, "type") {
		t.anchorEligible = false
		return false
	}
	switch kind {
	case "agent_start":
		t.starts++
		if t.starts != 1 {
			t.anchorEligible = false
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
		t.settled = true
		if event["aborted"] != false || t.openRole != "" {
			t.anchorEligible = false
		}
	case "message_start", "message_end":
		message, ok := event["message"].(map[string]any)
		role := piSessionString(message, "role")
		if !ok || !slices.Contains([]string{"system", "user", "assistant", "toolResult"}, role) {
			t.anchorEligible = false
			break
		}
		if kind == "message_start" {
			if t.openRole != "" {
				t.anchorEligible = false
			}
			t.openRole = role
			break
		}
		if t.openRole != role {
			t.anchorEligible = false
		}
		t.openRole = ""
		// History can reject a model/loadout/format without proving that a tool
		// did not run. Track tool facts independently, even after that rejection.
		t.observeToolMessage(message)
		if t.anchorEligible && !t.history.message(message) {
			t.anchorEligible = false
		}
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

func (t *piSessionTracker) loseExecutionCertainty() {
	t.anchorEligible, t.uncertain = false, true
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
