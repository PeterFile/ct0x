package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMemberDiscussionWireGuards(t *testing.T) {
	f := chatHTTPSetup(t) // A live intake token without the Pi relay schema.
	invite, _ := json.Marshal(map[string]any{"kind": "invite", "recipient_id": f.peer.ID})
	valid := `{"content":"请核对。","discussion":` + string(invite)
	before := f.counts(t)
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{valid + `}`, "forbidden", http.StatusForbidden},
		{valid + `,"recipient_ids":[]}`, "invalid_input", http.StatusBadRequest},
		{valid + `,"intent_assessment":{"disposition":"discuss","reason":"先讨论。"}}`, "invalid_input", http.StatusBadRequest},
		{valid + `,"recipient_ids":null}`, "invalid_json", http.StatusBadRequest},
		{`{"content":"请核对。","discussion":null}`, "invalid_json", http.StatusBadRequest},
		{`{"content":"请核对。","discussion":{"kind":"invite","recipient_id":["peer"]}}`, "invalid_json", http.StatusBadRequest},
		{`{"content":"请核对。","discussion":{"kind":"invite","recipient_id":"peer","expected_reply":null}}`, "invalid_json", http.StatusBadRequest},
		{`{"content":"请核对。","discussion":{"kind":"invite","kind":"resolve","recipient_id":"peer"}}`, "invalid_json", http.StatusBadRequest},
		{`{"content":"\ud800","discussion":{"kind":"resolve"}}`, "invalid_json", http.StatusBadRequest},
		{`{"content":"已核对。","discussion":{"kind":"reply","recipient_id":""}}`, "invalid_input", http.StatusBadRequest},
		{`{"content":"已核对。","discussion":{"kind":"resolve","expected_reply":""}}`, "invalid_input", http.StatusBadRequest},
		{`{"content":"已核对。","discussion":{"kind":"reply","origin_message_id":"forged"}}`, "invalid_input", http.StatusBadRequest},
	} {
		w := f.request(t, "send_message", tc.body, "not-consumed")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), tc.status, tc.code)
	}
	if f.counts(t) != before {
		t.Fatal("invalid or unissued relay changed durable state")
	}
}

func TestMemberDiscussionMCPRejectsBeforeContinuationBinding(t *testing.T) {
	for _, invalidFirst := range []bool{true, false} {
		order := "valid-first"
		if invalidFirst {
			order = "invalid-first"
		}
		t.Run(order, func(t *testing.T) {
			f := chatHTTPSetup(t)
			f.handler = mustServer(t, DefaultAddress, append(f.routes, MemberMCPRoutes(f.tools)...)...).Handler
			const proposal = `{"content":"记录续办。","intent_assessment":{"disposition":"continue_work","reason":"用户明确要求续办"}`
			const rpcID = `"continuation"`
			call := func(id, arguments string) *httptest.ResponseRecorder {
				return memberMCPHTTP(t, f, memberMCPCall(id, "send_message", arguments))
			}
			reject := func() {
				for _, field := range []string{`,"discussion":{"kind":"resolve"}`, `,"D\u0069Scussion":null`} {
					w := call(rpcID, proposal+field+`}`)
					if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":-32602`) {
						t.Fatalf("relay argument reached MCP binding or native execution: %d %s", w.Code, w.Body)
					}
				}
			}
			before := f.counts(t)
			if invalidFirst {
				reject()
			}
			// Continuation is disabled in this fixture. A clean request must still
			// bind normally and reach the native denial, not a binding conflict.
			first := call(rpcID, proposal+`}`)
			if text := memberMCPText(t, first, true); !strings.Contains(text, `"code":"forbidden"`) {
				t.Fatalf("clean continuation did not reach native authorization: %s", text)
			}
			if !invalidFirst {
				reject()
			}
			omitted := proposal[:len(proposal)-1] + `,"followup":{"constraint":""}}}`
			if replay := call(rpcID, omitted); first.Body.String() != replay.Body.String() {
				t.Fatalf("MCP continuation lost existing omission replay semantics: %s", replay.Body)
			}
			if text := memberMCPText(t, call(rpcID, strings.ReplaceAll(proposal, "用户明确要求续办", "其他理由")+`}`), true); !strings.Contains(text, `"code":"conflict"`) {
				t.Fatalf("a genuinely changed same-ID continuation did not conflict: %s", text)
			}
			w := call(`"\ud800"`, proposal+`,"discussion":null}`)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"id":null`) {
				t.Fatalf("broken Unicode ID was echoed as a replacement-character identity: %s", w.Body)
			}
			if f.counts(t) != before {
				t.Fatal("rejected requests changed durable state")
			}
		})
	}
}

func TestMemberDiscussionContinuationProjectionKeepsUnknownArguments(t *testing.T) {
	const proposal = `{"content":"ok","intent_assessment":{"disposition":"continue_work","reason":"ok"}`
	for _, raw := range []string{
		proposal + `,"discussion":null}`,
		proposal + `,"discussion":{"kind":"reply"}}`,
		proposal + `,"unexpected":{"nested":true}}`,
		proposal[:len(proposal)-1] + `,"followup":{"unexpected":null}}}`,
	} {
		if got := string(memberMCPContinueWorkArguments(json.RawMessage(raw))); got != raw {
			t.Fatalf("invalid continuation was projected onto a different request: %s -> %s", raw, got)
		}
	}
}
