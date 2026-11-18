package provider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const piSessionTestUUID = "01a1ea7e-68d1-77cf-8c6c-76d9fd8dc803"
const piSessionTestTime = "2026-11-17T17:00:00.000Z"

func piSessionTestProfile() piSessionProfile {
	return piSessionProfile{cwd: "/fixture", model: "synthetic/model", thinking: "max", tools: []string{"read_channel", "send_message"}}
}

func piSessionTestLines() []map[string]any {
	return []map[string]any{
		{"type": "session", "version": 3, "id": piSessionTestUUID, "timestamp": piSessionTestTime, "cwd": "/fixture"},
		{"type": "model_change", "id": "model1", "parentId": nil, "timestamp": piSessionTestTime, "provider": "synthetic", "modelId": "model"},
		{"type": "thinking_level_change", "id": "think1", "parentId": "model1", "timestamp": piSessionTestTime, "thinkingLevel": "max"},
		{"type": "message", "id": "system1", "parentId": "think1", "timestamp": piSessionTestTime, "message": map[string]any{"role": "system", "content": "restricted", "toolsAdded": []any{map[string]any{"name": "read_channel"}, map[string]any{"name": "send_message"}}, "timestamp": 1}},
		{"type": "message", "id": "user1", "parentId": "system1", "timestamp": piSessionTestTime, "message": map[string]any{"role": "user", "content": "hello", "timestamp": 2}},
		{"type": "message", "id": "assistant1", "parentId": "user1", "timestamp": piSessionTestTime, "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "done"}}, "provider": "synthetic", "model": "model", "thinkingLevel": "max", "stopReason": "stop", "timestamp": 3}},
		{"type": "usage", "id": "usage1", "parentId": "assistant1", "timestamp": piSessionTestTime, "kind": "cache_warm", "provider": "synthetic", "model": "model", "usage": map[string]any{}},
	}
}

func piSessionTestBytes(lines []map[string]any) []byte {
	var data []byte
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			panic(err)
		}
		data = append(append(data, encoded...), '\n')
	}
	return data
}

func TestPiSessionFileContentIntegrity(t *testing.T) {
	valid := piSessionTestBytes(piSessionTestLines())
	if got, err := parsePiSession(valid, piSessionTestProfile()); err != nil || got.nativeID != piSessionTestUUID || got.leafID != "usage1" || got.assistantID != "assistant1" {
		t.Fatalf("complete native endpoint rejected: %+v %v", got, err)
	}
	for _, name := range []string{"bad-line", "no-lf", "invalid-utf8", "duplicate-key", "nested-duplicate-key", "second-header", "old-version", "custom-id", "wrong-cwd", "orphan", "duplicate-id", "second-root", "branch", "user-only", "pending-assistant", "wrong-model", "wrong-thinking", "foreign-tools", "missing-tools", "unknown-entry", "trailing-value"} {
		t.Run(name, func(t *testing.T) {
			lines := piSessionTestLines()
			data := valid
			switch name {
			case "bad-line":
				data = append(bytes.Clone(valid), []byte("{broken}\n")...)
			case "no-lf":
				data = valid[:len(valid)-1]
			case "invalid-utf8":
				data = bytes.Replace(valid, []byte("done"), []byte{0xff}, 1)
			case "duplicate-key":
				data = bytes.Replace(valid, []byte(`"version":3`), []byte(`"version":3,"version":3`), 1)
			case "nested-duplicate-key":
				data = bytes.Replace(valid, []byte(`"text":"done"`), []byte(`"text":"done","text":"changed"`), 1)
			case "second-header":
				lines = append(lines, lines[0])
			case "old-version":
				lines[0]["version"] = 2
			case "custom-id":
				lines[0]["id"] = "legacy-run1"
			case "wrong-cwd":
				lines[0]["cwd"] = "/elsewhere"
			case "orphan":
				lines[5]["parentId"] = "missing"
			case "duplicate-id":
				lines[5]["id"] = "user1"
			case "second-root":
				lines[5]["parentId"] = nil
			case "branch":
				lines[5]["parentId"] = "system1"
			case "user-only":
				lines = lines[:5]
			case "pending-assistant":
				lines[5]["message"].(map[string]any)["stopReason"] = "pending"
			case "wrong-model":
				lines[5]["message"].(map[string]any)["model"] = "other"
			case "wrong-thinking":
				lines[5]["message"].(map[string]any)["thinkingLevel"] = "off"
			case "foreign-tools":
				lines[3]["message"].(map[string]any)["toolsAdded"] = []any{map[string]any{"name": "bash"}}
			case "missing-tools":
				delete(lines[3]["message"].(map[string]any), "toolsAdded")
			case "unknown-entry":
				lines[6]["type"] = "context_edit"
			case "trailing-value":
				data = bytes.Replace(valid, []byte("\n"), []byte(" {}\n"), 1)
			}
			if !strings.Contains("bad-line,no-lf,invalid-utf8,duplicate-key,nested-duplicate-key,trailing-value", name) {
				data = piSessionTestBytes(lines)
			}
			if _, err := parsePiSession(data, piSessionTestProfile()); err == nil {
				t.Fatal("unsafe history accepted")
			}
		})
	}
}
