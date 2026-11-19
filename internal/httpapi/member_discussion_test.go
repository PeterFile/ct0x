package httpapi

import (
	"encoding/json"
	"net/http"
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
