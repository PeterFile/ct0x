package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestDecisionRoutesV30DoNotProjectOrWriteUnavailableAnswers(t *testing.T) {
	f := openGroupAPI(t, "")
	// Inspect the migrated fixture independently of the HTTP responses.
	var answersInstalled bool
	if err := messageSQL(t, f).QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='decision_answers')`).Scan(&answersInstalled); err != nil {
		t.Fatal(err)
	}

	const unavailable = "decision_answers_unavailable"
	w := f.request(http.MethodGet, "/api/v1/me/pending-decisions", "")
	if answersInstalled {
		page := decoded[listResult[domain.DecisionRequest]](t, w, http.StatusOK)
		if page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
			t.Fatalf("expected an empty pending list: %+v", page)
		}
	} else {
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusServiceUnavailable, unavailable)
	}

	readStatus, readCode := http.StatusServiceUnavailable, unavailable
	writeStatus, writeCode := readStatus, readCode
	if answersInstalled {
		readStatus, readCode = http.StatusNotFound, "not_found"
		writeStatus, writeCode = http.StatusConflict, "decision_conflict"
	}
	const answerPath = "/api/v1/me/decisions/item/answer"
	// An explicitly empty topic_id is valid for an exact main-topic lookup.
	w = f.request(http.MethodGet, answerPath+"?group_id=local-group&topic_id=&version=1", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), readStatus, readCode)

	const body = `{"group_id":"local-group","topic_id":"","version":1,"text":"这次只看工作日"}`
	for _, path := range []string{answerPath, "/api/v1/groups/local-group/decisions/item/answer"} {
		w = f.request(http.MethodPost, path, body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), writeStatus, writeCode)
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
