package provider

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestCodexRPCIDPrecisionAndType(t *testing.T) {
	// Raw wire IDs retain both their type and exact integer precision.
	for _, id := range []string{
		`0`, `-0`, `-1`, `-9223372036854775808`, `9223372036854775807`, `9007199254740993`,
		`"0"`, `"9007199254740993"`, `"-9223372036854775808"`, `""`,
	} {
		t.Run(id, func(t *testing.T) {
			got := codexRPCDecode(t, `{"id":`+id+`,"method":"m"}`)
			if string(got.ID) != id {
				t.Fatalf("ID precision or type lost: %s", got.ID)
			}
			if id[0] != '"' {
				var value int64
				if err := json.Unmarshal(got.ID, &value); err != nil {
					t.Fatalf("numeric ID is not signed int64: %v", err)
				}
			}
			if again := codexRPCDecode(t, string(codexRPCEncode(t, got))); string(again.ID) != id {
				t.Fatalf("ID changed on encode: %s", again.ID)
			}
		})
	}
	for _, id := range []string{`null`, `true`, `[]`, `{}`, `1.5`, `1.0`, `1e0`, `9223372036854775808`, `-9223372036854775809`} {
		t.Run("invalid/"+id, func(t *testing.T) {
			if _, err := decodeCodexRPCFrame([]byte(`{"id":` + id + `,"method":"m"}`)); err == nil {
				t.Fatal("invalid ID accepted")
			}
			method := "m"
			codexRPCRejectEncode(t, codexRPCMessage{ID: json.RawMessage(id), Method: &method})
		})
	}
}

func TestCodexRPCOmittedAndNull(t *testing.T) {
	method := "m"
	for _, tc := range []struct {
		name, wire string
		want       codexRPCMessage
	}{
		{"notification-omitted", `{"method":"m"}`, codexRPCMessage{Method: &method}},
		{"notification-null-params", `{"method":"m","params":null}`, codexRPCMessage{Method: &method, Params: json.RawMessage(`null`)}},
		{"request-omitted", `{"id":0,"method":"m"}`, codexRPCMessage{ID: json.RawMessage(`0`), Method: &method}},
		{"request-null-params", `{"id":0,"method":"m","params":null}`, codexRPCMessage{ID: json.RawMessage(`0`), Method: &method, Params: json.RawMessage(`null`)}},
		{"result-null", `{"id":0,"result":null}`, codexRPCMessage{ID: json.RawMessage(`0`), Result: json.RawMessage(`null`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := codexRPCDecode(t, tc.wire)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("omitted and null conflated: got %+v, want %+v", got, tc.want)
			}
			if again := codexRPCDecode(t, string(codexRPCEncode(t, got))); !reflect.DeepEqual(again, got) {
				t.Fatal("omitted/null presence changed on encode")
			}
		})
	}
}

func TestCodexRPCErrorStructure(t *testing.T) {
	for _, raw := range []string{
		`{"code":0,"message":""}`,
		`{"code":-9223372036854775808,"message":"错误","data":null}`,
		`{"code":9223372036854775807,"message":"error","data":{"nested":[9007199254740993,null]},"extension":true}`,
	} {
		t.Run("valid/"+raw, func(t *testing.T) {
			message := codexRPCDecode(t, `{"id":1,"error":`+raw+`}`)
			if string(message.Error) != raw {
				t.Fatal("raw error or data presence changed")
			}
			if again := codexRPCDecode(t, string(codexRPCEncode(t, message))); string(again.Error) != raw {
				t.Fatal("error changed on encode")
			}
		})
	}
	for _, raw := range []string{
		`null`, `true`, `1`, `"error"`, `[]`, `{}`,
		`{"code":0}`, `{"message":"e"}`, `{"code":null,"message":"e"}`,
		`{"code":true,"message":"e"}`, `{"code":"0","message":"e"}`, `{"code":1.5,"message":"e"}`,
		`{"code":1e0,"message":"e"}`, `{"code":9223372036854775808,"message":"e"}`, `{"code":-9223372036854775809,"message":"e"}`,
		`{"code":0,"message":null}`, `{"code":0,"message":1}`, `{"code":0,"message":false}`,
		`{"code":0,"code":1,"message":"e"}`, `{"code":0,"message":"e","message":"other"}`,
		`{"code":0,"message":"e","data":null,"data":{}}`, `{"code":0,"c\u006fde":1,"message":"e"}`,
	} {
		t.Run("invalid/"+raw, func(t *testing.T) {
			if _, err := decodeCodexRPCFrame([]byte(`{"id":1,"error":` + raw + `}`)); err == nil {
				t.Fatal("invalid error structure accepted")
			}
			codexRPCRejectEncode(t, codexRPCMessage{ID: json.RawMessage(`1`), Error: json.RawMessage(raw)})
		})
	}
}

func TestCodexRPCStrictUnicode(t *testing.T) {
	// Surrogate rejection is a host policy, not an upstream Rust parser claim.
	for _, tc := range []struct {
		name, token, text string
		valid             bool
	}{
		{"lone-high", `"\uD800"`, "", false},
		{"lone-low", `"\uDC00"`, "", false},
		{"high-then-high", `"\uD800\uDBFF"`, "", false},
		{"high-then-nonsurrogate", `"\uD800\u0041"`, "", false},
		{"high-then-text", `"\uD800x"`, "", false},
		{"high-then-literal-escape", `"\uD800\\uDC00"`, "", false},
		{"low-then-high", `"\uDC00\uD800"`, "", false},
		{"invalid-UTF8", "\"PRIVATE-PAYLOAD\xff\"", "", false},
		{"pair", `"\uD83D\uDE00"`, "😀", true},
		{"minimum-pair", `"\uD800\uDC00"`, "\U00010000", true},
		{"maximum-pair", `"\uDBFF\uDFFF"`, "\U0010FFFF", true},
		{"literal-replacement", `"�"`, "�", true},
		{"escaped-replacement", `"\uFFFD"`, "�", true},
		{"escaped-backslash", `"\\uD800"`, `\uD800`, true},
		{"escaped-backslash-codepoint", `"\u005cuD800"`, `\uD800`, true},
		{"escaped-quote", `"\"\\uD800"`, `"\uD800`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, template := range []string{
				`{"method":%s}`, `{"id":%s,"method":"m"}`,
				`{"method":"m","params":{"text":%s}}`, `{"id":1,"result":{"text":%s}}`,
				`{"id":1,"error":{"code":0,"message":%s}}`,
				`{"id":1,"error":{"code":0,"message":"e","data":%s}}`,
				`{"method":"m","trace":{%s:null}}`,
			} {
				message, err := decodeCodexRPCFrame([]byte(fmt.Sprintf(template, tc.token)))
				if !tc.valid {
					if err == nil || strings.Contains(err.Error(), "PRIVATE-PAYLOAD") {
						t.Errorf("invalid Unicode accepted or exposed: template=%s, error=%v", template, err)
					}
					continue
				}
				if err != nil || message.Method == nil && message.Result == nil && message.Error == nil {
					t.Fatalf("valid Unicode rejected or lost: %v", err)
				}
				if template == `{"method":%s}` && *message.Method != tc.text {
					t.Fatalf("method string changed: %q", *message.Method)
				}
				codexRPCEncode(t, message)
			}
			method := "m"
			for _, message := range []codexRPCMessage{
				{ID: json.RawMessage(tc.token), Method: &method},
				{Method: &method, Params: json.RawMessage(`{"text":` + tc.token + `}`)},
				{ID: json.RawMessage(`1`), Result: json.RawMessage(tc.token)},
				{ID: json.RawMessage(`1`), Error: json.RawMessage(`{"code":0,"message":` + tc.token + `}`)},
				{ID: json.RawMessage(`1`), Error: json.RawMessage(`{"code":0,"message":"e","data":` + tc.token + `}`)},
			} {
				if tc.valid {
					codexRPCEncode(t, message)
				} else {
					codexRPCRejectEncode(t, message)
				}
			}
		})
	}
}

func TestCodexRPCEncodeRejectsInvalidInputWithoutOutput(t *testing.T) {
	method, invalidMethod := "m", "PRIVATE-PAYLOAD\xff"
	for i, message := range []codexRPCMessage{
		{}, {Method: &invalidMethod}, {ID: json.RawMessage(`1`)},
		{ID: json.RawMessage(`1`), Method: &method, Result: json.RawMessage(`null`)},
		{ID: json.RawMessage(`1`), Result: json.RawMessage(`null`), Error: json.RawMessage(`{"code":0,"message":"e"}`)},
		{ID: json.RawMessage(`1`), Result: json.RawMessage(`null`), Params: json.RawMessage(`null`)},
		{Method: &method, ID: json.RawMessage{}},
		{Method: &method, Params: json.RawMessage{}},
		{Method: &method, Params: json.RawMessage(`{"secret":"PRIVATE-PAYLOAD"`)},
		{ID: json.RawMessage(`1`), Result: json.RawMessage(`null true`)},
		{ID: json.RawMessage(`1`), Result: json.RawMessage(`"` + strings.Repeat("a", 1<<20) + `"`)},
	} {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			frame, err := encodeCodexRPCFrame(message)
			if err == nil || frame != nil || strings.Contains(err.Error(), "PRIVATE-PAYLOAD") {
				t.Fatalf("invalid input produced bytes or exposed payload: length=%d, error=%v", len(frame), err)
			}
		})
	}
}
