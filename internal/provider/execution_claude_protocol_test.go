package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeSDKStrictJSONRejectsDuplicatesAndSurrogates(t *testing.T) {
	for _, raw := range []string{`{"x":1,"x":2}`, `{"x":{"a":1,"\u0061":2}}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":NaN}`, strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34), "{\"x\":\"" + string([]byte{255}) + "\"}"} {
		if claudeStrictJSON([]byte(raw)) == nil {
			t.Fatalf("invalid JSON accepted: %q", raw)
		}
	}
	for _, raw := range []string{`{"x":"\ud83d\ude00"}`, `{"x":"\\ud800"}`, `{"x":{"y":[true,0,null]}}`} {
		if err := claudeStrictJSON([]byte(raw)); err != nil {
			t.Fatalf("valid JSON rejected: %s", raw)
		}
	}
	var body struct {
		IsError bool `json:"is_error"`
	}
	for _, raw := range []string{`{}`, `{"is_error":null}`, `{"is_error":false,"extra":1}`} {
		if claudeDecode(json.RawMessage(raw), &body, "is_error") == nil {
			t.Fatalf("non-strict body accepted: %s", raw)
		}
	}
}

func TestClaudeSDKEnvironmentDropsStaleCapabilitiesWithoutCopyingCredentials(t *testing.T) {
	env := claudeEnvironment([]string{"HOME=/original", "ANTHROPIC_API_KEY=original-key", "CLAUDE_CONFIG_DIR=/original/.claude", "NODE_OPTIONS=--import /evil", "ANTHROPIC_MODEL=foreign", "CLAUDE_CODE_REMOTE=1", "CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1", "AGENT_WORKSPACE_MCP_TOKEN_OLD=old-secret", "PI_OFFLINE=1", "PATH=/trusted"})
	joined := strings.Join(env, "\n")
	for _, expected := range []string{"HOME=/original", "ANTHROPIC_API_KEY=original-key", "CLAUDE_CONFIG_DIR=/original/.claude", "PATH=/trusted"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("credential ownership lost: %s", expected)
		}
	}
	for _, forbidden := range []string{"NODE_OPTIONS", "ANTHROPIC_MODEL", "CLAUDE_CODE_REMOTE", "RESUME_INTERRUPTED", "old-secret", "PI_OFFLINE"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("stale capability retained: %s", forbidden)
		}
	}
}
