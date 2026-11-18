package provider

import (
	"encoding/json"
	"testing"
)

func piSessionToolHistory(name string) ([]map[string]any, piSessionProfile) {
	profile := piSessionTestProfile()
	profile.tools = []string{name}
	lines := piSessionTestLines()
	lines[3]["message"].(map[string]any)["toolsAdded"] = []any{map[string]any{"name": name}}
	call := map[string]any{"role": "assistant", "provider": "synthetic", "model": "model", "thinkingLevel": "max", "stopReason": "toolUse", "content": []any{map[string]any{"type": "toolCall", "id": "call1", "name": name, "arguments": map[string]any{"text": "offline"}}}}
	result := map[string]any{"role": "toolResult", "toolCallId": "call1", "toolName": name, "isError": false, "content": []any{map[string]any{"type": "text", "text": "ok"}}}
	makeEntry := func(id, parent string, message map[string]any) map[string]any {
		return map[string]any{"type": "message", "id": id, "parentId": parent, "timestamp": piSessionTestTime, "message": message}
	}
	final := lines[5]
	final["parentId"] = "result1"
	out := append([]map[string]any{}, lines[:5]...)
	out = append(out, makeEntry("call-entry", "user1", call), makeEntry("result1", "call-entry", result), final, lines[6])
	return out, profile
}

func TestPiSessionPersistedToolCompletion(t *testing.T) {
	for _, name := range []string{"valid", "pending", "missing-result", "error", "wrong-id", "wrong-name", "duplicate-result"} {
		t.Run(name, func(t *testing.T) {
			lines, profile := piSessionToolHistory("send_message")
			switch name {
			case "pending":
				lines = lines[:6]
			case "missing-result":
				lines[7]["parentId"] = "call-entry"
				lines = append(lines[:6], lines[7:]...)
			case "error":
				lines[6]["message"].(map[string]any)["isError"] = true
			case "wrong-id":
				lines[6]["message"].(map[string]any)["toolCallId"] = "other"
			case "wrong-name":
				lines[6]["message"].(map[string]any)["toolName"] = "create_member"
			case "duplicate-result":
				duplicate := map[string]any{"type": "message", "id": "duplicate", "parentId": "result1", "timestamp": piSessionTestTime, "message": lines[6]["message"]}
				lines[7]["parentId"] = "duplicate"
				lines = append(lines[:7], append([]map[string]any{duplicate}, lines[7:]...)...)
			}
			_, err := parsePiSession(piSessionTestBytes(lines), profile)
			if (err == nil) != (name == "valid") {
				t.Fatalf("persisted pending/error evidence accepted=%t for %s", err == nil, name)
			}
		})
	}
}

func TestPiSessionPlatformWriteUnknownLatch(t *testing.T) {
	for _, tool := range []string{"send_message", "create_member", "decide_dispatch", "submit_result"} {
		for _, mode := range []string{"valid", "error-end", "error-result", "missing-end", "missing-start", "wrong-id", "wrong-name", "wrong-args", "missing-is-error", "duplicate-end", "duplicate-start", "missing-result", "different-result"} {
			t.Run(tool+"/"+mode, func(t *testing.T) {
				lines, profile := piSessionToolHistory(tool)
				tracker := newPiSessionTracker(profile, "")
				observe := func(event map[string]any) {
					data, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					if !tracker.observe(data) {
						t.Fatal("fixture unexpectedly damaged native stream")
					}
				}
				message := func(entry map[string]any) {
					observe(map[string]any{"type": "message_start", "message": entry["message"]})
					observe(map[string]any{"type": "message_end", "message": entry["message"]})
				}
				observe(lines[0])
				observe(map[string]any{"type": "agent_start"})
				message(lines[3])
				message(lines[4])
				message(lines[5])
				start := map[string]any{"type": "tool_execution_start", "toolCallId": "call1", "toolName": tool, "args": map[string]any{"text": "offline"}}
				end := map[string]any{"type": "tool_execution_end", "toolCallId": "call1", "toolName": tool, "isError": false, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}}
				switch mode {
				case "error-end":
					end["isError"] = true
				case "error-result":
					lines[6]["message"].(map[string]any)["isError"] = true
				case "wrong-id":
					end["toolCallId"] = "other"
				case "wrong-name":
					end["toolName"] = "not-issued"
				case "wrong-args":
					start["args"] = map[string]any{"text": "different"}
				case "missing-is-error":
					delete(end, "isError")
				case "different-result":
					end["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "different"}}}
				}
				if mode != "missing-start" {
					observe(start)
				}
				if mode == "duplicate-start" {
					observe(start)
				}
				if mode != "missing-end" {
					observe(end)
				}
				if mode == "duplicate-end" {
					observe(end)
				}
				if mode != "missing-result" {
					message(lines[6])
				}
				message(lines[7]) // A normal final stop must never clear a prior unknown.
				observe(map[string]any{"type": "agent_settled", "aborted": false})
				if tracker.complete() != (mode == "valid") {
					t.Fatal("normal final assistant erased incomplete/unknown platform write")
				}
			})
		}
	}
}
