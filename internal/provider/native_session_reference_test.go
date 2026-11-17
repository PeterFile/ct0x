package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNativeSessionBindingCanonicalAndBounded(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"a":1,"b":[true,null,"text"]}`)} {
		if !validNativeSessionBinding(raw) {
			t.Fatal("canonical server binding rejected")
		}
	}
	for _, raw := range []string{
		``, `null`, `[]`, `{"b":1,"a":2}`, `{"a":1,"a":1}`, `{"a":{"x":1,"x":1}}`, `{"a":1.0}`, `{"a":-0}`, `{"a":1e0}`, `{"a":9223372036854775808}`,
		`{"a":"\ud800"}`, `{} {}`, ` {}`, `{"a":` + strings.Repeat(`[`, 17) + `0` + strings.Repeat(`]`, 17) + `}`,
		`{"a":"` + strings.Repeat("x", NativeSessionBindingLimit) + `"}`,
	} {
		if validNativeSessionBinding(json.RawMessage(raw)) {
			t.Fatal("ambiguous, noncanonical or unbounded server binding accepted")
		}
	}
	original := &NativeSessionReference{Version: 1, Binding: json.RawMessage(`{"version":1}`), Codex: &CodexNativeSessionReference{NativeThreadID: "original"}}
	copy := cloneNativeSessionReference(original)
	copy.Binding[0], copy.Codex.NativeThreadID = ' ', "changed"
	if string(original.Binding) != `{"version":1}` || original.Codex.NativeThreadID != "original" {
		t.Fatal("native receipt payloads alias across deep copies")
	}
	if cloneNativeSessionReference(nil) != nil {
		t.Fatal("nil clone changed meaning")
	}
}

func TestNativeSessionReferencePiPayloadIsSeparateAndValueCloned(t *testing.T) {
	ref := &NativeSessionReference{Version: 1, Binding: json.RawMessage(`{"version":1}`), Pi: &PiNativeSessionReference{NativeSessionID: "pi-session", SessionFile: "/owned/exact-session.jsonl", Inode: 12, FileSHA256: strings.Repeat("a", 64)}}
	copy := cloneNativeSessionReference(ref)
	copy.Pi.NativeSessionID, copy.Pi.Inode = "different-session", 13
	if ref.Pi.NativeSessionID != "pi-session" || ref.Pi.Inode != 12 || validCodexNativeReference(ref) {
		t.Fatal("Pi reference aliases its clone or qualifies for the Codex adapter")
	}
	codex := &NativeSessionReference{Version: 1, Codex: &CodexNativeSessionReference{Adapter: codexSessionAdapter, Protocol: codexSessionProtocol,
		CLIPath: "/synthetic/codex", CLIVersion: "0.166.0", Provider: "synthetic", Model: "model", CWD: "/synthetic/cwd", NativeHome: "/synthetic/home",
		NativeThreadID: "thread", NativeTurnID: "turn", ToolsProfileSHA256: strings.Repeat("b", 64), HistorySHA256: strings.Repeat("c", 64), HistoryTurns: 1, HistoryItems: 2}}
	if !validCodexNativeReference(codex) {
		t.Fatal("valid Codex reference rejected")
	}
	before, _ := json.Marshal(codex)
	if strings.Contains(string(before), `"pi"`) {
		t.Fatal("absent Pi payload changed legacy Codex JSON")
	}
	codex.Pi = ref.Pi
	if validCodexNativeReference(codex) {
		t.Fatal("mixed Codex/Pi reference qualified for resume")
	}
	codex.Pi = nil
	after, _ := json.Marshal(codex)
	if !bytes.Equal(before, after) {
		t.Fatal("Pi opt-in changed existing Codex reference bytes")
	}
}

func TestNativeSessionReferenceDoesNotChangeCanonicalReceiptJSON(t *testing.T) {
	r := ExecutionReceipt{}
	old, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	const legacy = `{"started":false,"accepted":false,"outcome":"","delivery_failed":false,"native_session_id":"","direct_child_reaped":false,"output_complete":false,"exit_code":0,"stdout_path":"","stderr_path":"","logs_truncated":false,"reason":""}`
	if string(old) != legacy {
		t.Fatal("legacy canonical receipt bytes changed")
	}
	r.NativeRef = &NativeSessionReference{Version: 1, Binding: json.RawMessage(`{"purpose":"generic"}`), Codex: &CodexNativeSessionReference{NativeThreadID: "server-only"}}
	current, err := json.Marshal(r)
	if err != nil || !bytes.Equal(old, current) {
		t.Fatal("server-only reference entered canonical receipt JSON")
	}
}

func TestCodexSessionToolsProfileMatchesStoreAlgorithm(t *testing.T) {
	names := []string{"read_channel", "list_members", "read_channel"}
	before := append([]string{}, names...)
	digest := sha256.Sum256([]byte(`["list_members","read_channel"]`))
	if codexSessionToolsProfile(names) != hex.EncodeToString(digest[:]) || !reflect.DeepEqual(names, before) {
		t.Fatal("tools digest differs from sorted unique canonical array or mutates issuance order")
	}
}

func codexSessionHistoryFixture() (*codexAppServerProtocol, map[string]any) {
	p := &codexAppServerProtocol{request: ExecuteRequest{CWD: "/synthetic/cwd"}, threadID: "thread",
		session: &codexAppServerSession{provider: "synthetic"}, member: &codexAppServerMemberSession{names: []string{"read_channel"}}}
	items := []any{
		map[string]any{"id": "user", "type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "actual user text", "text_elements": []any{}}}},
		map[string]any{"id": "tool", "type": "mcpToolCall", "server": "run-server", "tool": "read_channel", "status": "completed", "arguments": map[string]any{"limit": 1}, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "actual tool text"}}, "_meta": nil, "structuredContent": nil}, "error": nil, "durationMs": 10},
		map[string]any{"id": "answer", "type": "agentMessage", "text": "actual answer", "phase": "final_answer", "startedAtMs": 10, "completedAtMs": 20},
	}
	thread := map[string]any{"id": "thread", "cliVersion": "0.166.0", "source": "appServer", "cwd": "/synthetic/cwd", "ephemeral": false,
		"status": map[string]any{"type": "idle"}, "modelProvider": "synthetic", "updatedAt": 1,
		"turns": []any{map[string]any{"id": "turn", "status": "completed", "items": items, "error": nil, "startedAt": 1, "completedAt": 2, "durationMs": 10}}}
	return p, map[string]any{"thread": thread, "readState": map[string]any{"revision": "not-a-history-revision"}}
}

func TestCodexSessionPublicHistoryCanonicalContentNotTiming(t *testing.T) {
	p, response := codexSessionHistoryFixture()
	baseline, ok := p.sessionHistory(mustCodexAppServerJSON(response))
	if !ok || baseline.items != 3 || len(baseline.turns) != 1 {
		t.Fatal("full public history rejected")
	}
	thread := response["thread"].(map[string]any)
	turn := thread["turns"].([]any)[0].(map[string]any)
	items := turn["items"].([]any)
	thread["updatedAt"], response["readState"] = 99, map[string]any{"revision": "changed-read-state"}
	turn["durationMs"], turn["startedAt"], turn["completedAt"] = 99, 99, 99
	items[1].(map[string]any)["durationMs"] = 99
	items[2].(map[string]any)["completedAtMs"] = 99
	stable, ok := p.sessionHistory(mustCodexAppServerJSON(response))
	if !ok || stable.digest() != baseline.digest() {
		t.Fatal("unstable timestamps or read-state revision changed public anchor")
	}
	for _, part := range []string{"answer", "arguments", "result", "user"} {
		p, response := codexSessionHistoryFixture()
		items := response["thread"].(map[string]any)["turns"].([]any)[0].(map[string]any)["items"].([]any)
		switch part {
		case "answer":
			items[2].(map[string]any)["text"] = "different answer"
		case "arguments":
			items[1].(map[string]any)["arguments"] = map[string]any{"limit": 2}
		case "result":
			items[1].(map[string]any)["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "different result"}}}
		case "user":
			items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "different input"
		}
		changed, ok := p.sessionHistory(mustCodexAppServerJSON(response))
		if !ok || changed.digest() == baseline.digest() || changed.items != baseline.items {
			t.Fatal("public content drift was hidden behind identical IDs/counts")
		}
	}
}

func TestCodexSessionPublicHistoryIncludesStableRootAndToolHint(t *testing.T) {
	for _, field := range []string{"rootTurnId", "readOnlyHint"} {
		t.Run(field, func(t *testing.T) {
			p, response := codexSessionHistoryFixture()
			before, ok := p.sessionHistory(mustCodexAppServerJSON(response))
			if !ok {
				t.Fatal("baseline history rejected")
			}
			turn := response["thread"].(map[string]any)["turns"].([]any)[0].(map[string]any)
			if field == "rootTurnId" {
				turn[field] = "changed-root"
			} else {
				turn["items"].([]any)[1].(map[string]any)[field] = true
			}
			after, ok := p.sessionHistory(mustCodexAppServerJSON(response))
			if !ok || before.digest() == after.digest() {
				t.Fatal("stable public ancestry/tool metadata drift was omitted from canonical history")
			}
		})
	}
}

func TestCodexSessionPublicHistoryRejectsAmbiguityAndBounds(t *testing.T) {
	for _, mode := range []string{"duplicate", "nested-duplicate", "too-many-turns", "too-many-items", "too-large", "unfinished", "duplicate-id", "file", "dynamic", "summary", "nontext", "deep"} {
		t.Run(mode, func(t *testing.T) {
			p, response := codexSessionHistoryFixture()
			thread := response["thread"].(map[string]any)
			turn := thread["turns"].([]any)[0].(map[string]any)
			items := turn["items"].([]any)
			var raw []byte
			switch mode {
			case "duplicate":
				raw = []byte(strings.Replace(string(mustCodexAppServerJSON(response)), `"updatedAt":1`, `"updatedAt":1,"updatedAt":1`, 1))
			case "nested-duplicate":
				raw = []byte(strings.Replace(string(mustCodexAppServerJSON(response)), `"limit":1`, `"limit":1,"limit":2`, 1))
			case "too-many-turns":
				turns := []any{}
				for i := 0; i <= codexSessionHistoryTurnLimit; i++ {
					turns = append(turns, map[string]any{"id": fmt.Sprintf("turn-%d", i), "status": "completed", "items": []any{}, "error": nil})
				}
				thread["turns"] = turns
			case "too-many-items":
				items := []any{}
				for i := 0; i <= codexSessionHistoryItemLimit; i++ {
					items = append(items, map[string]any{"id": fmt.Sprintf("item-%d", i), "type": "agentMessage", "text": "bounded"})
				}
				turn["items"] = items
			case "too-large":
				raw = bytes.Repeat([]byte(" "), codexRPCFrameLimit+1)
			case "unfinished":
				turn["status"] = "interrupted"
			case "duplicate-id":
				items[2].(map[string]any)["id"] = "user"
			case "file":
				items[2].(map[string]any)["type"] = "fileChange"
			case "dynamic":
				items[1].(map[string]any)["type"] = "dynamicToolCall"
			case "summary":
				turn["itemsView"] = "summary"
			case "nontext":
				items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["type"] = "image"
			case "deep":
				items[1].(map[string]any)["arguments"] = json.RawMessage(`{"nested":` + strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65) + `}`)
			}
			if raw == nil {
				raw = mustCodexAppServerJSON(response)
			}
			if _, ok := p.sessionHistory(raw); ok {
				t.Fatal("ambiguous, unbounded or unsupported public history accepted")
			}
		})
	}
}
