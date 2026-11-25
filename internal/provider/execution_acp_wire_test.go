package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestACPWireStrictJSONAndTypeSensitiveIDs(t *testing.T) {
	valid := []string{
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":"agent-1","method":"session/request_permission","params":{}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"text":"\ud83d\ude00","literal":"\\uD800"}}`,
	}
	for _, raw := range valid {
		if _, err := decodeACPMessage([]byte(raw)); err != nil {
			t.Fatalf("valid v1 frame rejected: %s", raw)
		}
	}
	invalid := []string{
		`{"id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"id":2,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"ignored":{"x":1,"x":2}}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"\u0078":1,"x":2}}`,
		`{"jsonrpc":"2.0","id":1.0,"result":{}}`,
		`{"jsonrpc":"2.0","id":1e0,"result":{}}`,
		`{"jsonrpc":"2.0","id":9007199254740992,"result":{}}`,
		`{"jsonrpc":"2.0","id":null,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"bad"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"text":"\ud800"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"text":"\udfff"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{}} {}`,
		`[{"jsonrpc":"2.0","id":1,"result":{}}]`,
		`{"jsonrpc":"2.0","method":"x","result":{}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":"1","message":"bad"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"deep":` + strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65) + `}}`,
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"bad\":\"\xff\"}}",
	}
	for _, raw := range invalid {
		if _, err := decodeACPMessage([]byte(raw)); err == nil {
			t.Fatalf("lossy/ambiguous frame accepted: %.100s", raw)
		}
	}
	numeric, _ := acpID(json.RawMessage(`1`))
	text, _ := acpID(json.RawMessage(`"1"`))
	if numeric == text {
		t.Fatal("numeric and string request IDs alias")
	}
}

func TestACPWireFramingRequiresLFAndDrainsAfterDamage(t *testing.T) {
	for _, raw := range []string{`{"jsonrpc":"2.0","id":1,"result":{}}`, strings.Repeat("x", acpFrameLimit+1) + "\n" + strings.Repeat("tail", 1<<19)} {
		output := make(chan acpRead, 3)
		readACPOutput(strings.NewReader(raw), output)
		first, last := <-output, <-output
		if !first.damaged || !last.done || !last.eof {
			t.Fatal("truncated/oversized stream became a valid frame")
		}
	}
}
