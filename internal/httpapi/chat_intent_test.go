package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type chatToolConfigurer interface {
	EnableChatIntake(*coordination.Engine) error
}

func chatHTTPSetup(t *testing.T) *memberToolHTTPFixture {
	t.Helper()
	f := newMemberToolHTTP(t, "read_channel", "send_message")
	f.tools.Revoke(f.token)
	configure, ok := any(f.tools).(chatToolConfigurer)
	if !ok {
		t.Fatal("MemberTools lacks trusted chat intake configuration")
	}
	if err := configure.EnableChatIntake(coordination.New(f.db)); err != nil {
		t.Fatal(err)
	}
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: "trusted-topic"}
	if _, err := f.db.SetChatPolicy(context.Background(), thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2}, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetCoordinator(context.Background(), f.groupID, f.caller.ID, 0, "coord"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetDispatchPolicy(context.Background(), f.groupID, []string{f.peer.ID}, 0, "workers"); err != nil {
		t.Fatal(err)
	}
	var err error
	f.token, err = f.tools.Issue(context.Background(), f.claim, platform.MemberToolGrants{Operations: []string{"read_channel", "send_message"}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func chatHTTPBody(extra string) string {
	return `{"content":"将按讨论结论交付文本。","intent_assessment":{"disposition":"request_work","action":"text_submission","title":"三条摘要","description":"只生成文本","acceptance":"有三条可核对结论","reason":"当前用户结合上下文明确要求交付"` + extra + `}}`
}

func TestChatIntentHTTPBoundSemanticAssessmentAndLegacyCompatibility(t *testing.T) {
	f := chatHTTPSetup(t)
	w := f.request(t, "read_channel", `{}`)
	var page struct {
		ChatCandidate *domain.ChatCandidate `json:"chat_candidate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK || page.ChatCandidate == nil || page.ChatCandidate.MemberID != f.caller.ID || page.ChatCandidate.Policy.Revision != 1 || page.ChatCandidate.SourceSenderID != "user" {
		t.Fatalf("captured semantic context missing: %s err=%v", w.Body, err)
	}
	w = f.request(t, "send_message", chatHTTPBody(""), "semantic-call")
	var result struct {
		memberToolHTTPSendResult
		Assessment *domain.ChatAssessmentReceipt `json:"intent_assessment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusCreated || result.Assessment == nil || result.Assessment.TaskID == "" || result.Assessment.DispatchRequestID == "" || result.MessageID != result.Assessment.NoticeMessageID || result.DeliveryCount != 0 {
		t.Fatalf("natural-chat admission failed: status=%d body=%s err=%v", w.Code, w.Body, err)
	}
	original := w.Body.String()
	if replay := f.request(t, "send_message", chatHTTPBody(""), "semantic-call"); replay.Code != 201 || replay.Body.String() != original {
		t.Fatalf("same tool key replay changed: %d %s", replay.Code, replay.Body)
	}
	if conflict := f.request(t, "send_message", `{"content":"将按讨论结论交付文本。"}`, "semantic-call"); conflict.Code != 409 {
		t.Fatalf("assessment removal reused key: %d %s", conflict.Code, conflict.Body)
	}
	if legacy := f.request(t, "send_message", `{"content":"后续普通讨论，不授予额外权限。"}`, "legacy"); legacy.Code != 201 {
		t.Fatalf("legacy send broke: %d %s", legacy.Code, legacy.Body)
	}
}

func TestChatIntentPolicyHTTPVersionScopeAndAdmissionEvidence(t *testing.T) {
	f := newMemberToolHTTP(t)
	routes := append(f.routes, CoordinationRoutes(platform.NewCoordinations(coordination.New(f.db)))...)
	f.handler = mustServer(t, DefaultAddress, routes...).Handler
	endpoint := "/api/v1/groups/" + f.groupID + "/coordination/chat-policy?topic_id=trusted-topic"
	request := func(method, path, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+DefaultAddress+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	w := request("GET", endpoint, "", "")
	var policy domain.ChatPolicy
	if json.Unmarshal(w.Body.Bytes(), &policy) != nil || w.Code != 200 || policy.Enabled || policy.Revision != 0 {
		t.Fatalf("missing policy must be disabled: %d %s", w.Code, w.Body)
	}
	body := `{"enabled":true,"allow_preparation":false,"action":"text_submission","max_admissions":2,"expected_revision":0}`
	w = request("PUT", endpoint, body, "enable")
	if json.Unmarshal(w.Body.Bytes(), &policy) != nil || w.Code != 200 || !policy.Enabled || policy.Revision != 1 || policy.AuthorizedBy != "user" {
		t.Fatalf("local user policy failed: %d %s", w.Code, w.Body)
	}
	if replay := request("PUT", endpoint, body, "enable"); replay.Code != 200 || replay.Body.String() != w.Body.String() {
		t.Fatalf("policy replay differs: %d %s", replay.Code, replay.Body)
	}
	if stale := request("PUT", endpoint, body, "stale"); stale.Code != 409 {
		t.Fatalf("stale policy revision accepted: %d %s", stale.Code, stale.Body)
	}
	if bad := request("PUT", endpoint, strings.Replace(body, `"expected_revision":0`, `"expected_revision":1,"authorized_by":"user"`, 1), "forge"); bad.Code != 400 {
		t.Fatalf("model authority field accepted: %d %s", bad.Code, bad.Body)
	}
	if other := request("GET", strings.Replace(endpoint, "trusted-topic", "other-topic", 1), "", ""); other.Code != 200 || strings.Contains(other.Body.String(), `"enabled":true`) {
		t.Fatalf("policy escaped exact topic: %d %s", other.Code, other.Body)
	}
	if missing := request("GET", "/api/v1/groups/"+f.otherID+"/tasks/missing/chat-admission", "", ""); missing.Code != 404 {
		t.Fatalf("foreign admission read: %d %s", missing.Code, missing.Body)
	}
}

func TestChatIntentHTTPCommittedReceiptOmitsLongBoundTopic(t *testing.T) {
	f := chatHTTPSetup(t)
	ctx := context.Background()
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: strings.Repeat("<", 12000)}
	if _, err := f.db.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2}, 0, "long-policy"); err != nil {
		t.Fatal(err)
	}
	question, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID, ActorID: "user", Content: "按约定落实文本", RecipientIDs: []string{f.peer.ID}}, []string{f.peer.ID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: question.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.claim = store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	f.token, err = f.tools.Issue(ctx, f.claim, platform.MemberToolGrants{Operations: []string{"send_message"}})
	if err != nil {
		t.Fatal(err)
	}
	first := f.request(t, "send_message", chatHTTPBody(""), "long-assessment")
	var result struct {
		memberToolHTTPSendResult
		Assessment domain.ChatAssessmentReceipt `json:"intent_assessment"`
	}
	if json.Unmarshal(first.Body.Bytes(), &result) != nil || first.Code != 201 || first.Body.Len() > 4096 || result.Assessment.TaskID == "" {
		t.Fatalf("committed admission did not return a small receipt: %d %s", first.Code, first.Body)
	}
	full, err := f.db.GetChatAdmission(ctx, f.groupID, result.Assessment.TaskID)
	if err != nil || full.TopicID != thread.TopicID || full.AssessedBy != f.peer.ID {
		t.Fatalf("projection changed durable source evidence: %+v %v", full, err)
	}
	if replay := f.request(t, "send_message", chatHTTPBody(""), "long-assessment"); replay.Code != 201 || replay.Body.String() != first.Body.String() {
		t.Fatal("long-topic assessment replay changed")
	}
}

func TestChatIntentHTTPRejectsModelIdentityAndUnboundAssessment(t *testing.T) {
	f := chatHTTPSetup(t)
	for _, extra := range []string{`,"caller":"user"`, `,"authorized_by":"user"`, `,"grants":["write"]`, `,"group_id":"foreign"`, `,"policy_revision":999`, `,"candidate_generation":999`} {
		w := f.request(t, "send_message", chatHTTPBody(extra), "forged")
		if w.Code != 400 {
			t.Fatalf("model chose trusted assessment identity: %d %s", w.Code, w.Body)
		}
	}
	for _, body := range []string{`{"content":"x","intent_assessment":null}`, strings.Replace(chatHTTPBody(""), `"intent_assessment":`, `"recipient_ids":["`+f.peer.ID+`"],"intent_assessment":`, 1)} {
		if w := f.request(t, "send_message", body, "invalid"); w.Code != 400 {
			t.Fatalf("unsafe assessment shape accepted: %d %s", w.Code, w.Body)
		}
	}
	legacy := newMemberToolHTTP(t, "send_message")
	if w := legacy.request(t, "send_message", chatHTTPBody(""), "unbound"); w.Code != 403 {
		t.Fatalf("ordinary legacy capability admitted work: %d %s", w.Code, w.Body)
	}
	if _, err := f.db.RequestTurnCancel(context.Background(), f.groupID, f.claim.TurnID); err != nil {
		t.Fatal(err)
	}
	if w := f.request(t, "send_message", chatHTTPBody(""), "after-cancel"); w.Code != 403 {
		t.Fatalf("cancelled execution admitted work: %d %s", w.Code, w.Body)
	}
}
