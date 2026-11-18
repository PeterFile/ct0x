package httpapi

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestChatDiscussionPolicyHTTP(t *testing.T) {
	a := openMessageAPI(t, "")
	group, other := newGroup(t, a, "讨论"), newGroup(t, a, "隔离")
	const topic = " 精确\n话题+ "
	base := coordinationHTTPBase(group.ID) + "/discussion-policy"
	endpoint := base + "?topic_id=" + url.QueryEscape(topic)
	raw := messageSQL(t, a)
	var installed bool
	if err := raw.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='chat_discussion_policy_revisions')").Scan(&installed); err != nil {
		t.Fatal(err)
	}
	const disabled = `{"expected_revision":0,"max_invite_hops":0}`
	if !installed {
		for _, response := range []*httptest.ResponseRecorder{
			a.request("GET", endpoint, ""),
			a.request("PUT", endpoint, disabled, "key"),
			a.request("PUT", endpoint, ""),
		} {
			assertError(t, response.Code, response.Header(), response.Body.Bytes(), 404, "not_found")
		}
		return
	}
	ref := domain.ThreadRef{GroupID: group.ID, TopicID: topic}
	if got := decoded[domain.ChatDiscussionPolicy](t, a.request("GET", endpoint, ""), 200); got != (domain.ChatDiscussionPolicy{ThreadRef: ref, MaxInviteHops: 2, MaxTargetedWakes: 4}) {
		t.Fatalf("expected scoped default: %+v", got)
	}
	for _, test := range []struct{ path, body, key string }{
		{endpoint, disabled, ""},
		{endpoint, disabled, "coalesced,key"},
		{endpoint, `{"max_invite_hops":0}`, "invalid"},
		{endpoint, `{"expected_revision":0}`, "invalid"},
		{endpoint, `{"expected_revision":0,"max_invite_hops":null}`, "invalid"},
		{endpoint, `{"expected_revision":0,"max_invite_hops":3}`, "invalid"},
		{endpoint, `{"expected_revision":-1,"max_invite_hops":1}`, "invalid"},
		{endpoint, `{"expected_revision":0,"max_invite_hops":0,"max_targeted_wakes":4}`, "invalid"},
		{endpoint, `{"expected_revision":0,"max_invite_hops":0,"topic_id":"foreign","group_id":"foreign","sender_id":"user"}`, "invalid"},
		{base + "?topic_id=a&topic_id=b", disabled, "invalid"},
		{base + "?member_id=other", disabled, "invalid"},
	} {
		decoded[map[string]any](t, a.request("PUT", test.path, test.body, test.key), 400)
	}
	r := httptest.NewRequest("PUT", "http://"+DefaultAddress+endpoint, strings.NewReader(disabled))
	r.Header.Set("Origin", "http://other.invalid")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "valid")
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	decoded[map[string]any](t, w, 403)

	first := decoded[domain.ChatDiscussionPolicy](t, a.request("PUT", endpoint, disabled, "valid"), 200)
	if first != (domain.ChatDiscussionPolicy{ThreadRef: ref, Revision: 1}) {
		t.Fatalf("incorrect disabled policy: %+v", first)
	}
	decoded[map[string]any](t, a.request("PUT", endpoint, disabled, "stale"), 409)
	second := decoded[domain.ChatDiscussionPolicy](t, a.request("PUT", endpoint, `{"expected_revision":1,"max_invite_hops":1}`, "one-hop"), 200)
	if second != (domain.ChatDiscussionPolicy{ThreadRef: ref, Revision: 2, MaxInviteHops: 1, MaxTargetedWakes: 2}) {
		t.Fatalf("incorrect one-hop policy: %+v", second)
	}
	if replay := decoded[domain.ChatDiscussionPolicy](t, a.request("PUT", endpoint, disabled, "valid"), 200); replay != first {
		t.Fatalf("incorrect historical receipt: %+v", replay)
	}
	if got := decoded[domain.ChatDiscussionPolicy](t, a.request("GET", endpoint, ""), 200); got != second {
		t.Fatalf("current read returned the wrong revision: %+v", got)
	}
	for path, scope := range map[string]domain.ThreadRef{
		base:                {GroupID: group.ID},
		base + "?topic_id=": {GroupID: group.ID},
		base + "?topic_id=" + url.QueryEscape("精确\n话题+"): {GroupID: group.ID, TopicID: "精确\n话题+"},
		strings.Replace(endpoint, group.ID, other.ID, 1): {GroupID: other.ID, TopicID: topic},
	} {
		if got := decoded[domain.ChatDiscussionPolicy](t, a.request("GET", path, ""), 200); got != (domain.ChatDiscussionPolicy{ThreadRef: scope, MaxInviteHops: 2, MaxTargetedWakes: 4}) {
			t.Fatalf("policy crossed scope: %+v", got)
		}
	}
	decoded[map[string]any](t, a.request("GET", strings.Replace(endpoint, group.ID, "missing", 1), ""), 404)
	if _, err := raw.Exec("DROP TABLE chat_discussion_policy_revisions"); err != nil {
		t.Fatal(err)
	}
	// The history still says the capability is installed; don't mask damage as legacy.
	for _, response := range []*httptest.ResponseRecorder{
		a.request("GET", endpoint, ""),
		a.request("PUT", endpoint, disabled, "valid"),
		a.request("PUT", endpoint, `{"expected_revision":2,"max_invite_hops":1}`, "new"),
	} {
		failure := decoded[struct{ Error struct{ Code string } }](t, response, 500)
		if failure.Error.Code != "internal_error" || strings.Contains(response.Body.String(), "sqlite") {
			t.Fatalf("schema damage was hidden or leaked: %s", response.Body)
		}
	}
}
