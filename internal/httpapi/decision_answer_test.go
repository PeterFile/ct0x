package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestDecisionRoutesV30DoNotProjectOrWriteUnavailableAnswers(t *testing.T) {
	f := openGroupAPI(t, "")
	if got := f.request(http.MethodGet, "/api/v1/me/pending-decisions", ""); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("v30 pending route status=%d: %s", got.Code, got.Body)
	}
	const body = `{"group_id":"local-group","topic_id":"","version":1,"text":"这次只看工作日"}`
	for _, path := range []string{"/api/v1/me/decisions/item/answer", "/api/v1/groups/local-group/decisions/item/answer"} {
		if got := f.request(http.MethodPost, path, body); got.Code != http.StatusServiceUnavailable {
			t.Fatalf("v30 answer route %s: %d %s", path, got.Code, got.Body)
		}
		if got := f.request(http.MethodPost, path, strings.TrimSuffix(body, "}")+`,"actor_id":"user"}`); got.Code != http.StatusBadRequest {
			t.Fatalf("route accepted caller identity in JSON: %s %d", path, got.Code)
		}
	}
	if got := f.request(http.MethodPost, "/api/v1/groups/other/decisions/item/answer", body); got.Code != http.StatusNotFound {
		t.Fatalf("group alias ignored exact equality: %d", got.Code)
	}
}
