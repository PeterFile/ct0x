package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecisionRoutesV30DoNotProjectOrWriteUnavailableAnswers(t *testing.T) {
	f := openGroupAPI(t, "")
	if got := f.request(http.MethodGet, "/api/v1/me/pending-decisions", ""); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("v30 pending route status=%d: %s", got.Code, got.Body)
	}
	// An explicitly empty topic_id reaches the schema check for the main topic.
	w := f.request(http.MethodGet, "/api/v1/me/decisions/item/answer?group_id=local-group&topic_id=&version=1", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusServiceUnavailable, "decision_answers_unavailable")

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

func TestDecisionAnswerReadRequiresExactLocalQuery(t *testing.T) {
	f := openGroupAPI(t, "")
	const path = "/api/v1/me/decisions/item/answer"
	const base = "?group_id=local-group&topic_id=&version="
	for _, query := range []string{
		"", "?topic_id=&version=1", "?group_id=local-group&version=1", "?group_id=local-group&topic_id=",
		"?group_id=&topic_id=&version=1", "?group_id=local-group&topic_id=%zz&version=1",
		base, base + "0", base + "-1", base + "1.0", base + "1e0", base + "9223372036854775808",
		base + "1&group_id=other", base + "1&topic_id=other", base + "1&version=1", base + "1&actor_id=user",
	} {
		w := f.request(http.MethodGet, path+query, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("invalid query %q returned %d: %s", query, w.Code, w.Body)
		}
	}
	w := f.request(http.MethodGet, path+base+"1", `{"actor_id":"user"}`)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_input")

	r := httptest.NewRequest(http.MethodGet, "http://not-local.invalid"+path+base+"1", nil)
	r.Header.Set("X-Forwarded-Host", DefaultAddress)
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusForbidden, "invalid_host")
}
