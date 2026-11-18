package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
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
	if _, err := f.db.ApplyTurnReceipt(context.Background(), f.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	message, err := platform.NewLocalUser(f.db).CreateMessage(context.Background(), f.groupID, platform.MessageInput{TopicID: thread.TopicID, Content: "请按前文边界整理三条文本摘要。", RecipientIDs: []string{f.caller.ID}}, "trusted-intake-request")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.claim = store.TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
	f.token, err = f.tools.IssueChatIntake(context.Background(), f.claim)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func chatHTTPBody(extra string) string {
	return `{"content":"将按讨论结论交付文本。","intent_assessment":{"disposition":"request_work","action":"text_submission","title":"三条摘要","description":"只生成文本","acceptance":"有三条可核对结论","reason":"当前用户结合上下文明确要求交付"` + extra + `}}`
}

func TestChatContinuationDisabledDoesNotConsumeLegacyIntake(t *testing.T) {
	f := chatHTTPSetup(t)
	before := f.counts(t)
	body := `{"content":"继续。","intent_assessment":{"disposition":"continue_work","reason":"用户明确要求续办","followup":{"task_id":"hint"}}}`
	w := f.request(t, "send_message", body, "disabled-continuation")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
	if f.counts(t) != before {
		t.Fatal("disabled continuation changed durable state")
	}
	w = f.request(t, "send_message", chatHTTPBody(""), "legacy-after-denial")
	if w.Code != 201 {
		t.Fatalf("disabled branch consumed the old intake: %d %s", w.Code, w.Body)
	}
}

func TestChatAuthorityReadOnlyMessageSources(t *testing.T) {
	f := chatHTTPSetup(t)
	routes := append(f.routes, MessageRoutes(platform.NewLocalUser(f.db))...)
	f.handler = mustServer(t, DefaultAddress, routes...).Handler
	response := f.request(t, "send_message", chatHTTPBody(""), "metadata-admit")
	var admitted struct {
		Assessment domain.ChatAssessmentReceipt `json:"intent_assessment"`
	}
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &admitted) != nil || admitted.Assessment.TaskID == "" {
		t.Fatalf("admit: %d %s", response.Code, response.Body)
	}
	dispatch, err := f.db.MaterializeDispatchRequest(context.Background(), admitted.Assessment.DispatchRequestID, 1, time.Now())
	if err != nil || dispatch.Delivery == nil {
		t.Fatalf("materialize: %+v %v", dispatch, err)
	}
	unknown, err := f.db.CreateMessage(context.Background(), store.MessageRequest{GroupID: f.groupID, TopicID: "trusted-topic", ActorID: "user", Content: "unproven legacy-shaped data"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := f.db.SetChatPolicy(context.Background(), domain.ThreadRef{GroupID: f.groupID, TopicID: "trusted-topic"}, domain.ChatPolicyRequest{Enabled: true, AllowPreparation: true, PreparationScopeKind: domain.ChatPreparationTopicSummary, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2}, 1, "summary-policy")
	if err != nil {
		t.Fatal(err)
	}
	for id, kind := range map[string]string{dispatch.Delivery.MessageID: "platform", unknown.ID: "unknown", policy.PreauthorizationMessageID: "local_user_policy"} {
		r := httptest.NewRequest("GET", "http://"+DefaultAddress+"/api/v1/groups/"+f.groupID+"/messages/"+id, nil)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		var message map[string]any
		if json.Unmarshal(w.Body.Bytes(), &message) != nil || w.Code != 200 || message["source_kind"] != kind {
			t.Fatalf("read-only provenance missing for %s: %d %s", kind, w.Code, w.Body)
		}
		if kind != "unknown" && (message["source_id"] == nil || message["source_id"] == "") {
			t.Fatal("proven message omitted its durable source")
		}
	}
	channel := f.request(t, "read_channel", `{}`)
	var page struct {
		Items     []map[string]any      `json:"items"`
		Candidate *domain.ChatCandidate `json:"chat_candidate"`
	}
	if json.Unmarshal(channel.Body.Bytes(), &page) != nil || page.Candidate == nil {
		t.Fatal("missing channel")
	}
	found := false
	for _, message := range page.Items {
		if message["id"] == dispatch.Delivery.MessageID {
			found = message["source_kind"] == "platform"
		}
	}
	if !found {
		t.Fatal("intake history still presents platform user sender without source metadata")
	}
	for _, extra := range []string{`"source_kind":"local_user_chat"`, `"source_id":"forged"`, `"purpose":"chat_intake/v1"`} {
		r := httptest.NewRequest("POST", "http://"+DefaultAddress+"/api/v1/groups/"+f.groupID+"/messages", strings.NewReader(`{"content":"x",`+extra+`}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("client chose provenance: %d %s", w.Code, w.Body)
		}
	}
}

func TestChatAuthorityIntakeDiscussionHasBoundedInvitations(t *testing.T) {
	f := chatHTTPSetup(t)
	if w := f.request(t, "list_members", `{}`); w.Code != 200 {
		t.Fatalf("read-only roster unavailable: %d", w.Code)
	}
	if w := f.request(t, "create_member", `{"name":"extra","role":"worker","reason":"x"}`, "no-create"); w.Code != 403 {
		t.Fatalf("intake gained member creation: %d", w.Code)
	}
	if w := f.request(t, "send_message", `{"content":"skip assessment"}`, "skip"); w.Code != 403 {
		t.Fatalf("intake skipped bound assessment: %d %s", w.Code, w.Body)
	}
	assessment := `{"content":"先讨论结构，不创建任务。","intent_assessment":{"disposition":"discuss","reason":"用户当前只要求讨论"}}`
	if w := f.request(t, "send_message", assessment, "discussion"); w.Code != 201 {
		t.Fatalf("discussion assessment: %d %s", w.Code, w.Body)
	}
	if w := f.request(t, "send_message", `{"content":"公开补充讨论，不代表授权。"}`, "public"); w.Code != 201 {
		t.Fatalf("public discussion blocked: %d %s", w.Code, w.Body)
	}
	body := `{"content":"请补充摘要结构建议。","recipient_ids":["` + f.peer.ID + `"]}`
	first := f.request(t, "send_message", body, "one-peer")
	var receipt memberToolHTTPSendResult
	if first.Code != 201 || json.Unmarshal(first.Body.Bytes(), &receipt) != nil {
		t.Fatalf("fixed-ID peer invitation blocked: %d %s", first.Code, first.Body)
	}
	if replay := f.request(t, "send_message", body, "one-peer"); replay.Code != 201 || replay.Body.String() != first.Body.String() {
		t.Fatal("invitation replay changed")
	}
	if w := f.request(t, "send_message", body, "second-peer"); w.Code != 403 {
		t.Fatalf("per-turn invitation bound bypassed: %d %s", w.Code, w.Body)
	}
	message, err := f.db.GetMessage(context.Background(), f.groupID, receipt.MessageID)
	if err != nil || len(message.Deliveries) != 1 {
		t.Fatal("invitation not durably routed")
	}
	turn, err := f.db.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.claim = store.TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
	f.token, err = f.tools.IssueChatIntake(context.Background(), f.claim)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.request(t, "send_message", assessment, "peer-discussion"); w.Code != 201 {
		t.Fatalf("peer discussion blocked: %d %s", w.Code, w.Body)
	}
	body = `{"content":"邀请回去形成循环","recipient_ids":["` + f.caller.ID + `"]}`
	if w := f.request(t, "send_message", body, "no-ping-pong"); w.Code != 403 {
		t.Fatalf("member source created an automatic invitation loop: %d %s", w.Code, w.Body)
	}
}

func TestChatAuthorityIntakeAfterAdmissionCanDiscuss(t *testing.T) {
	f := chatHTTPSetup(t)
	if w := f.request(t, "send_message", chatHTTPBody(""), "admission"); w.Code != 201 {
		t.Fatalf("assessment: %d %s", w.Code, w.Body)
	}
	if w := f.request(t, "send_message", `{"content":"公开补充，不改变已保存目标。"}`, "after-admit-public"); w.Code != 201 {
		t.Fatalf("admission forced monologue: %d %s", w.Code, w.Body)
	}
	body := `{"content":"请补充讨论意见，不授新工作权限。","recipient_ids":["` + f.peer.ID + `"]}`
	if w := f.request(t, "send_message", body, "after-admit-peer"); w.Code != 201 {
		t.Fatalf("bounded peer discussion after admission blocked: %d %s", w.Code, w.Body)
	}
}

func TestChatAuthorityBudgetDenialHTTPReceiptIsDurable(t *testing.T) {
	f := chatHTTPSetup(t)
	for i := 0; i < 3; i++ {
		if i > 0 {
			f.tools.Revoke(f.token)
			if _, err := f.db.ApplyTurnReceipt(context.Background(), f.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
				t.Fatal(err)
			}
			m, err := platform.NewLocalUser(f.db).CreateMessage(context.Background(), f.groupID, platform.MessageInput{TopicID: "trusted-topic", Content: "请落实本话题下一项文本摘要。", RecipientIDs: []string{f.caller.ID}}, fmt.Sprintf("source-%d", i))
			if err != nil {
				t.Fatal(err)
			}
			turn, err := f.db.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: m.Deliveries[0].ID, MaxActive: 1})
			if err != nil {
				t.Fatal(err)
			}
			f.claim = store.TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
			f.token, err = f.tools.IssueChatIntake(context.Background(), f.claim)
			if err != nil {
				t.Fatal(err)
			}
		}
		key := fmt.Sprintf("assessment-%d", i)
		w := f.request(t, "send_message", chatHTTPBody(""), key)
		var receipt struct {
			Assessment chatIntentToolReceipt `json:"intent_assessment"`
		}
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
			t.Fatalf("receipt: %d %s", w.Code, w.Body)
		}
		if i < 2 {
			if receipt.Assessment.TaskID == "" || receipt.Assessment.Outcome != "admitted" {
				t.Fatalf("unexpected denial: %s", w.Body)
			}
			continue
		}
		r := receipt.Assessment
		if r.Outcome != "not_admitted" || r.ReasonCode != "budget_exhausted" || r.Question == "" || r.TaskID != "" || r.MandateID != "" || r.DispatchRequestID != "" {
			t.Fatalf("known budget denial lost in projection: %s", w.Body)
		}
		before := coordinationHTTPSnapshot(t, f.raw)
		if replay := f.request(t, "send_message", chatHTTPBody(""), key); replay.Code != 201 || replay.Body.String() != w.Body.String() {
			t.Fatal("durable denial replay changed")
		}
		if !reflect.DeepEqual(before, coordinationHTTPSnapshot(t, f.raw)) {
			t.Fatal("denial replay duplicated state")
		}
	}
}

func TestChatAuthorityGenericCannotEvaluate(t *testing.T) {
	f := chatHTTPSetup(t)
	// A prior intake record cannot upgrade a newly issued generic token.
	f.tools.Revoke(f.token)
	var err error
	f.token, err = f.tools.Issue(context.Background(), f.claim, platform.MemberToolGrants{Operations: []string{"read_channel", "send_message"}})
	if err != nil {
		t.Fatal(err)
	}
	before := f.counts(t)
	response := f.request(t, "send_message", chatHTTPBody(""), "generic-cannot-authorize")
	if response.Code != http.StatusForbidden {
		t.Fatalf("generic capability promoted a proposal to user authority: %d %s", response.Code, response.Body)
	}
	if f.counts(t) != before {
		t.Fatal("denied generic proposal changed durable state")
	}
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
	question, err := f.db.CreateLocalUserMessage(ctx, store.MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID, ActorID: "user", Content: "按约定落实文本", RecipientIDs: []string{f.peer.ID}}, []string{f.peer.ID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: question.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.claim = store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	f.token, err = f.tools.IssueChatIntake(ctx, f.claim)
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
