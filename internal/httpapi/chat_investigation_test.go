package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
)

func TestChatInvestigationPolicyHTTPOptIn(t *testing.T) {
	f := newMemberToolHTTP(t)
	f.handler = mustServer(t, DefaultAddress, append(f.routes, CoordinationRoutes(platform.NewCoordinations(coordination.New(f.db)))...)...).Handler
	url := "http://" + DefaultAddress + "/api/v1/groups/" + f.groupID + "/coordination/chat-policy?topic_id=investigation"
	request := func(method, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodGet, "", ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"investigation":`) ||
		!strings.Contains(w.Body.String(), `"investigation_supported":true`) || !strings.Contains(w.Body.String(), `"investigation_execution_enabled":false`) {
		t.Fatalf("empty policy must advertise support without scope or readiness: %d %s", w.Code, w.Body)
	}
	body := `{"enabled":true,"action":"text_submission","max_admissions":1,"expected_revision":0,"investigation":{"objective":"核对讨论中未决事项","allow_topic_messages":true,"max_delegations":0,"clue_terms":["导出","只有表头"]}}`
	for _, terms := range []string{`null`, `[]`, `[""]`} {
		bad := strings.Replace(body, `["导出","只有表头"]`, terms, 1)
		if w := request(http.MethodPut, bad, "configure-investigation"); w.Code != http.StatusBadRequest {
			t.Fatalf("malformed clue terms %s were not rejected: %d %s", terms, w.Code, w.Body)
		}
	}
	w := request(http.MethodPut, body, "configure-investigation")
	var policy domain.ChatPolicy
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &policy) != nil || policy.Revision != 1 || policy.Investigation == nil ||
		!policy.Investigation.AllowTopicMessages || !policy.Investigation.MatchesClue("导出偶尔只有表头") || policy.Investigation.MatchesClue("导出") ||
		policy.ConfigurationSourceID == "" || !policy.InvestigationSupported || policy.InvestigationExecutionEnabled {
		t.Fatalf("configuration before runtime opt-in failed: %d %s", w.Code, w.Body)
	}
	for _, other := range []*httptest.ResponseRecorder{request(http.MethodGet, "", ""), request(http.MethodPut, body, "configure-investigation")} {
		if other.Code != http.StatusOK || other.Body.String() != w.Body.String() {
			t.Fatalf("GET/replay changed the stored policy: %d %s", other.Code, other.Body)
		}
	}
	if stale := request(http.MethodPut, body, "another-key"); stale.Code != http.StatusConflict {
		t.Fatalf("stale revision escaped CAS: %d %s", stale.Code, stale.Body)
	}
	for _, field := range []string{"investigation_supported", "investigation_execution_enabled"} {
		if bad := request(http.MethodPut, `{"`+field+`":true,`+body[1:], "forged-capability"); bad.Code != http.StatusBadRequest {
			t.Fatalf("response-only %s was accepted in PUT: %d %s", field, bad.Code, bad.Body)
		}
	}
	f.db.EnableInvestigationExecution() // Trusted process assertion, never accepted in a request.
	disabled := request(http.MethodPut, `{"enabled":false,"action":"text_submission","max_admissions":1,"expected_revision":1}`, "disable-investigation")
	var current domain.ChatPolicy
	if disabled.Code != http.StatusOK || json.Unmarshal(disabled.Body.Bytes(), &current) != nil || current.Enabled || current.Investigation != nil ||
		!current.InvestigationSupported || !current.InvestigationExecutionEnabled {
		t.Fatalf("process readiness must be independent of configured enabled: %d %s", disabled.Code, disabled.Body)
	}
}
