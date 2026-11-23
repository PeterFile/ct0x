package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
)

func chatInvestigationPolicyHTTPRequest(t *testing.T, f *memberToolHTTPFixture) func(method, topic, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	handler := mustServer(t, DefaultAddress, CoordinationRoutes(platform.NewCoordinations(coordination.New(f.db)))...).Handler
	url := "http://" + DefaultAddress + "/api/v1/groups/" + f.groupID + "/coordination/chat-policy?topic_id="
	return func(method, topic, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, url+topic, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
}

func TestChatInvestigationPolicyHTTPOptIn(t *testing.T) {
	f := newMemberToolHTTP(t)
	call := chatInvestigationPolicyHTTPRequest(t, f)
	request := func(method, body, key string) *httptest.ResponseRecorder {
		return call(method, "investigation", body, key)
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

func TestChatInvestigationPolicyHTTPClueUnicode(t *testing.T) {
	request := chatInvestigationPolicyHTTPRequest(t, newMemberToolHTTP(t))
	const prefix = `{"enabled":true,"action":"text_submission","max_admissions":1,"expected_revision":0,"investigation":{"objective":"核对未决事项","allow_topic_messages":true,`
	for _, tc := range []struct {
		name, fields string
		want         []string
	}{
		{"lone-high", `"clue_terms":["\uD800"]`, nil},
		{"lone-low", `"clue_terms":["\udc00"]`, nil},
		{"duplicate", `"clue_terms":["原词"],"CLUE_\u0054ERMS":["其他"]`, nil},
		{"unknown", `"clue_terms":["原词"],"extra":true`, nil},
		{"valid", `"clue_terms":["\uD83D\uDE00","原�","\ufffd","\\ud800"]`, []string{"😀", "原�", "�", `\ud800`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Independent topics and keys let both malformed PUTs reach revision zero on RED.
			body := prefix + tc.fields + `}}`
			w := request(http.MethodPut, tc.name, body, tc.name)
			if tc.want == nil {
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_json")
				return
			}
			var policy domain.ChatPolicy
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &policy) != nil || policy.Investigation == nil ||
				!slices.Equal(policy.Investigation.ClueTerms, tc.want) {
				t.Fatalf("valid clue terms did not round-trip exactly: %d %s", w.Code, w.Body)
			}
			for _, other := range []*httptest.ResponseRecorder{request(http.MethodGet, tc.name, "", ""), request(http.MethodPut, tc.name, body, tc.name)} {
				if other.Code != http.StatusOK || other.Body.String() != w.Body.String() {
					t.Fatalf("GET/replay changed the stored policy: %d %s", other.Code, other.Body)
				}
			}
		})
	}
}
