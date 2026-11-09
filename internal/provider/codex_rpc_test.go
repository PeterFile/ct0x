package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func codexRPCDecode(t *testing.T, wire string) codexRPCMessage {
	t.Helper()
	message, err := decodeCodexRPCFrame([]byte(wire))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	return message
}

func codexRPCEncode(t *testing.T, message codexRPCMessage) []byte {
	t.Helper()
	frame, err := encodeCodexRPCFrame(message)
	if err != nil || len(frame) == 0 {
		t.Fatalf("encode returned %d bytes: %v", len(frame), err)
	}
	return frame
}

func codexRPCRejectEncode(t *testing.T, message codexRPCMessage) {
	t.Helper()
	frame, err := encodeCodexRPCFrame(message)
	if err == nil || frame != nil {
		t.Fatalf("invalid encode returned sendable bytes: length=%d, error=%v", len(frame), err)
	}
}

func TestCodexRPCMessageCategories(t *testing.T) {
	// Handwritten examples follow the pinned 0.166 JSONRPCMessage and
	// DynamicToolCallParams schemas, not captured native execution traffic.
	params := `{"arguments":{"nested":[null,{"secret":"PRIVATE-PAYLOAD"}]},"callId":"business-call","namespace":null,"threadId":"thread-1","tool":"workspace.read","turnId":"turn-1"}`
	request, notification := "item/tool/call", "initialized"
	for _, tc := range []struct {
		name, wire string
		want       codexRPCMessage
	}{
		{"request", `{"id":"rpc-request","method":"item/tool/call","params":` + params + `}`, codexRPCMessage{ID: json.RawMessage(`"rpc-request"`), Method: &request, Params: json.RawMessage(params)}},
		{"notification", `{"method":"initialized"}`, codexRPCMessage{Method: &notification}},
		{"result", `{"id":17,"result":{"nested":[null,{"ok":true}]}}`, codexRPCMessage{ID: json.RawMessage(`17`), Result: json.RawMessage(`{"nested":[null,{"ok":true}]}`)}},
		{"error", `{"id":17,"error":{"code":-32601,"message":"未知方法","data":{"nested":[null,9007199254740993]}}}`, codexRPCMessage{ID: json.RawMessage(`17`), Error: json.RawMessage(`{"code":-32601,"message":"未知方法","data":{"nested":[null,9007199254740993]}}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := codexRPCDecode(t, tc.wire)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("wrong message category or payload: got %+v, want %+v", got, tc.want)
			}
			if roundtrip := codexRPCDecode(t, string(codexRPCEncode(t, got))); !reflect.DeepEqual(roundtrip, got) {
				t.Fatalf("envelope changed on roundtrip: %+v", roundtrip)
			}
			if tc.name == "request" {
				var payload struct {
					CallID string `json:"callId"`
				}
				if err := json.Unmarshal(got.Params, &payload); err != nil || payload.CallID != "business-call" || string(got.ID) != `"rpc-request"` {
					t.Fatalf("RPC id and business callId were conflated: %v", err)
				}
			}
		})
	}
}

func TestCodexRPCMetadataAndUnknownMethod(t *testing.T) {
	for _, version := range []string{"", `"jsonrpc":"2.0",`, `"jsonrpc":"2\u002e0",`} {
		wire := `{` + version + `"id":7,"method":"future/unknown","params":{"caller":"untrusted"},"trace":{"traceparent":"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01","tracestate":null},"extension":{"method":"ignored"},"extension":null}`
		got := codexRPCDecode(t, wire)
		if got.Method == nil || *got.Method != "future/unknown" || string(got.ID) != "7" || string(got.Params) != `{"caller":"untrusted"}` {
			t.Fatalf("unknown method was not retained as data: %+v", got)
		}
		var encoded map[string]json.RawMessage
		if err := json.Unmarshal(codexRPCEncode(t, got), &encoded); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"jsonrpc", "trace", "extension"} {
			if _, present := encoded[key]; present {
				t.Fatalf("ignored metadata %q was emitted", key)
			}
		}
	}
	// Open upstream schemas permit extra properties; they are not authority.
	got := codexRPCDecode(t, `{"Method":false,"method":"","trace":null,"code":0,"code":1}`)
	if got.Method == nil || *got.Method != "" {
		t.Fatal("case-sensitive method or empty string was changed")
	}
}

func TestCodexRPCRejectEnvelopeShapes(t *testing.T) {
	// Mixed-shape rejection is a host policy beyond the upstream open anyOf.
	for i, wire := range []string{
		``, `{}`, `null`, `true`, `1`, `"scalar"`, `[]`, `[{"method":"m"}]`,
		`{"params":null}`, `{"id":1}`, `{"result":null}`, `{"error":{"code":0,"message":"e"}}`,
		`{"method":null}`, `{"method":1}`, `{"method":{ "secret":"PRIVATE-PAYLOAD"}}`,
		`{"id":null,"method":"m"}`, `{"id":null,"result":null}`, `{"id":null,"error":{"code":0,"message":"e"}}`,
		`{"id":1,"method":"m","result":null}`, `{"method":"m","error":{"code":0,"message":"e"}}`,
		`{"id":1,"result":null,"error":{"code":0,"message":"e"}}`,
		`{"id":1,"result":null,"params":null}`, `{"id":1,"error":{"code":0,"message":"e"},"params":{}}`,
		`{"method":"m"}{"method":"n"}`, `{"method":"m"} null`, `{"method":"m"} garbage`,
		`{"method":"m",}`, `{"method":`, `{"method":"m"`,
		`{"jsonrpc":"1.0","method":"m"}`, `{"jsonrpc":2,"method":"m"}`, `{"jsonrpc":null,"method":"m"}`,
	} {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			if _, err := decodeCodexRPCFrame([]byte(wire)); err == nil || strings.Contains(err.Error(), "PRIVATE-PAYLOAD") {
				t.Fatalf("invalid envelope accepted or payload exposed: %v", err)
			}
		})
	}
}

func TestCodexRPCRejectDuplicateReservedKeys(t *testing.T) {
	for _, wire := range []string{
		`{"id":1,"id":2,"method":"m"}`, `{"method":"m","method":"n"}`,
		`{"method":"m","params":null,"params":{}}`, `{"id":1,"result":null,"result":1}`,
		`{"id":1,"error":{"code":0,"message":"e"},"error":{"code":1,"message":"e"}}`,
		`{"method":"m","jsonrpc":"2.0","jsonrpc":"2.0"}`, `{"method":"m","trace":null,"trace":{}}`,
		`{"i\u0064":1,"id":1,"method":"m"}`, `{"\u006dethod":"m","method":"m"}`,
	} {
		t.Run(wire, func(t *testing.T) {
			if _, err := decodeCodexRPCFrame([]byte(wire)); err == nil {
				t.Fatal("duplicate reserved key accepted")
			}
		})
	}
	// Nested business DTO duplicates are intentionally deferred to C2b/C3.
	got := codexRPCDecode(t, `{"method":"m","params":{"id":1,"id":2}}`)
	if string(got.Params) != `{"id":1,"id":2}` {
		t.Fatal("nested business JSON was interpreted by the envelope codec")
	}
}

func TestCodexRPCFrameBoundary(t *testing.T) {
	const limit = 1 << 20
	frame := func(size int) string {
		return `{"method":"` + strings.Repeat("a", size-len(`{"method":""}`)) + `"}`
	}
	for _, tc := range []struct {
		name, wire string
		valid      bool
	}{
		{"limit", frame(limit), true},
		{"plus-one", frame(limit + 1), false},
		{"CR-at-limit", frame(limit-1) + "\r", true},
		{"CR-over-limit", frame(limit) + "\r", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCodexRPCFrame([]byte(tc.wire))
			if !tc.valid {
				if err == nil {
					t.Fatal("oversize frame accepted")
				}
				return
			}
			if err != nil || got.Method == nil || len(*got.Method) != len(strings.TrimSuffix(tc.wire, "\r"))-len(`{"method":""}`) {
				t.Fatalf("bounded frame rejected or changed: %v", err)
			}
		})
	}
	method := strings.Repeat("a", limit-len(`{"method":""}`))
	if got := codexRPCEncode(t, codexRPCMessage{Method: &method}); len(got) != limit || bytes.HasSuffix(got, []byte("\n")) {
		t.Fatalf("encoded frame length or LF changed: %d", len(got))
	}
	method += "a"
	codexRPCRejectEncode(t, codexRPCMessage{Method: &method})
	method = strings.Repeat("a", limit+1)
	codexRPCRejectEncode(t, codexRPCMessage{Method: &method})
	// A legal Go string may expand sixfold only when marshaled.
	method = strings.Repeat("\x00", limit/6+1)
	codexRPCRejectEncode(t, codexRPCMessage{Method: &method})
}

func TestCodexRPCWorstEscapeToolResult(t *testing.T) {
	text := strings.Repeat("\x00", 64<<10)
	result, err := json.Marshal(map[string]any{"contentItems": []map[string]string{{"type": "text", "text": text}}})
	if err != nil {
		t.Fatal(err)
	}
	frame := codexRPCEncode(t, codexRPCMessage{ID: json.RawMessage(`"rpc-result"`), Result: result})
	wantSize := 6*len(text) + len(`{"id":"rpc-result","result":{"contentItems":[{"text":"","type":"text"}]}}`)
	if len(frame) != wantSize || len(frame) > 1<<20 || bytes.Count(frame, []byte(`\u0000`)) != len(text) {
		t.Fatalf("worst-escape fixture was not encoded as expected: length=%d, want=%d", len(frame), wantSize)
	}
	message := codexRPCDecode(t, string(frame))
	var payload struct {
		ContentItems []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"contentItems"`
	}
	if err := json.Unmarshal(message.Result, &payload); err != nil || len(payload.ContentItems) != 1 || payload.ContentItems[0].Type != "text" || !bytes.Equal([]byte(payload.ContentItems[0].Text), []byte(text)) {
		t.Fatalf("64 KiB text bytes changed after encode/decode: %v", err)
	}
}
