package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"agentworkspace/internal/domain"
)

type correctionPOSTReceipt struct {
	domain.Message
	Correction *struct {
		MessageID        string `json:"message_id"`
		OriginalTopicID  string `json:"original_topic_id"`
		PreviousTopicID  string `json:"previous_topic_id"`
		EffectiveTopicID string `json:"effective_topic_id"`
		Question         string `json:"question"`
	} `json:"thread_correction"`
}

func postCorrectionHTTP(t *testing.T, a *groupAPI, group, key string, input map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return a.request("POST", "/api/v1/groups/"+group+"/messages", string(body), key)
}

func requireThreadCorrectionFixture(t *testing.T, a *groupAPI) {
	t.Helper()
	var installed bool
	if err := messageSQL(t, a).QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='thread_corrections')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Skip("threadCorrectionSchema is not registered; use the private migration overlay")
	}
}

func TestThreadCorrectionNormalPost(t *testing.T) {
	a := openMessageAPI(t, "")
	requireThreadCorrectionFixture(t, a)
	g := newGroup(t, a, "correction")
	path := "/api/v1/groups/" + g.ID + "/messages"
	decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+g.ID+"/members", `{"name":"owner","role":"observer"}`), 201)
	root := decoded[domain.Message](t, a.request("POST", path, `{"content":"新话题：NET-21 数据库连接告警","recipient_ids":[]}`, "alert"), 201)
	original := decoded[domain.Message](t, a.request("POST", path, `{"content":"只在连续两次失败后通知。"}`, "original"), 201)
	if root.TopicID == "" || original.TopicID != "" || len(original.Deliveries) != 1 {
		t.Fatalf("bad topic or delivery fixture: root=%+v original=%+v", root, original)
	}
	const correction = `{"content":"这句话指的是告警"}`
	first := a.request("POST", path, correction, "correct")
	posted := decoded[correctionPOSTReceipt](t, first, 201)
	if len(posted.Deliveries) != 0 || posted.Correction == nil || posted.Correction.MessageID != original.ID ||
		posted.Correction.OriginalTopicID != "" || posted.Correction.PreviousTopicID != "" || posted.Correction.EffectiveTopicID != root.TopicID || posted.Correction.Question != "" {
		t.Fatalf("a natural correction must move only the previous message's attribution, without dispatch: %s", first.Body)
	}
	// Both a newer local input and a process restart must leave the correction's
	// original target/receipt alone on replay. The next reply follows its new scope.
	followup := decoded[domain.Message](t, postCorrectionHTTP(t, a, g.ID, "next", map[string]any{
		"content": "继续讨论阈值。", "reply_to_message_id": original.ID,
	}), 201)
	if followup.TopicID != root.TopicID || followup.ReplyToMessageID != original.ID || len(followup.Deliveries) != 1 {
		t.Fatalf("reply did not use effective scope: %+v", followup)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = openMessageAPI(t, a.path)
	for _, w := range []*httptest.ResponseRecorder{a.request("POST", path, correction, "correct"), a.request("GET", path+"/"+posted.ID, "")} {
		if !bytes.Equal(w.Body.Bytes(), first.Body.Bytes()) {
			t.Fatalf("receipt changed after reopen: before=%s after=%s", first.Body, w.Body)
		}
	}
	if got := decoded[domain.Message](t, a.request("GET", path+"/"+original.ID, ""), 200); got.TopicID != original.TopicID || got.SourceID != original.SourceID ||
		len(got.Deliveries) != 1 || got.Deliveries[0] != original.Deliveries[0] {
		t.Fatalf("historical record changed: was %+v, is %+v", original, got)
	}
	raw := messageSQL(t, a)
	var replyStorageTopic string
	if err := raw.QueryRow(`SELECT reply_to_topic_id FROM thread_routing_links WHERE message_id=?`, followup.ID).Scan(&replyStorageTopic); err != nil || replyStorageTopic != original.TopicID {
		t.Fatalf("link must retain the raw topic FK: %q, %v", replyStorageTopic, err)
	}
	for table, want := range map[string]int{"messages": 4, "deliveries": 2, "tasks": 0, "mandates": 0, "thread_routing_topics": 1} {
		var got int
		if err := raw.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d, %v; want %d", table, got, err, want)
		}
	}
}

func TestThreadCorrectionDefaultPostPersistsClarification(t *testing.T) {
	a := openMessageAPI(t, "")
	requireThreadCorrectionFixture(t, a)
	g := newGroup(t, a, "ambiguous correction")
	path := "/api/v1/groups/" + g.ID + "/messages"
	decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+g.ID+"/members", `{"name":"on call","role":"observer"}`), 201)
	for _, title := range []string{"NET-56 网关告警", "OPS-42 备份告警"} {
		m := decoded[domain.Message](t, postCorrectionHTTP(t, a, g.ID, "topic-"+title[:3], map[string]any{
			"content": "新话题：" + title, "recipient_ids": []string{},
		}), 201)
		if m.TopicID == "" {
			t.Fatalf("unregistered fixture topic: %+v", m)
		}
	}
	original := decoded[domain.Message](t, a.request("POST", path, `{"content":"先确认是否有第二次失败。","recipient_ids":[]}`, "original"), 201)
	const body = `{"content":"这句话指的是告警"}`
	w := a.request("POST", path, body, "correction")
	m := decoded[domain.Message](t, w, 201)
	if m.ThreadCorrection == nil || m.ThreadCorrection.Question == "" || m.ThreadCorrection.MessageID != "" || m.TopicID != "" ||
		m.SourceID != "" || len(m.Deliveries) != 0 {
		t.Fatalf("ordinary composer did not get a durable, inert clarification: %s", w.Body)
	}
	if get := a.request("GET", path+"/"+m.ID, ""); get.Code != 200 || !bytes.Equal(get.Body.Bytes(), w.Body.Bytes()) {
		t.Fatalf("timeline detail lost the clarification: %s", get.Body)
	}
	page := decoded[listResult[domain.Message]](t, a.request("GET", path, ""), 200)
	if len(page.Items) != 4 || page.Items[0].ID != m.ID || page.Items[0].ThreadCorrection == nil || *page.Items[0].ThreadCorrection != *m.ThreadCorrection {
		t.Fatalf("newest-page refresh lost the persisted question: %+v", page)
	}
	if attr, err := a.db.MessageThreadAttribution(t.Context(), g.ID, original.ID); err != nil || attr.Effective != attr.Storage || attr.CorrectionMessageID != "" {
		t.Fatalf("ambiguous default correction moved the source: %+v, %v", attr, err)
	}
}

func TestThreadCorrectionExplicitReplyAndAmbiguity(t *testing.T) {
	a := openMessageAPI(t, "")
	requireThreadCorrectionFixture(t, a)
	g, other := newGroup(t, a, "same group"), newGroup(t, a, "different group")
	m := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+g.ID+"/members", `{"name":"owner","role":"observer"}`), 201)
	put := func(key, content string) domain.Message {
		t.Helper()
		return decoded[domain.Message](t, postCorrectionHTTP(t, a, g.ID, key, map[string]any{"content": content, "recipient_ids": []string{}}), 201)
	}
	one, two := put("alpha", "新话题：API-85 下载延迟告警"), put("beta", "新话题：MQ-29 队列容量告警")
	original := put("old", "API-85 下载延迟告警先记录三个时间点。")
	intervening := put("intervening", "休息一下。")
	if one.TopicID == "" || one.TopicID == two.TopicID || original.TopicID != one.TopicID || intervening.TopicID != "" {
		t.Fatal("expected two different E1a topics and one main input")
	}
	foreign := decoded[domain.Message](t, postCorrectionHTTP(t, a, other.ID, "foreign", map[string]any{"content": "别群原文"}), 201)
	input := map[string]any{"content": "这句话指的是 MQ-29。", "reply_to_message_id": foreign.ID, "recipient_ids": []string{m.ID}}
	w := postCorrectionHTTP(t, a, g.ID, "explicit", input)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	input["reply_to_message_id"] = original.ID
	w = postCorrectionHTTP(t, a, g.ID, "explicit", input)
	explicit := decoded[correctionPOSTReceipt](t, w, 201)
	if len(explicit.Deliveries) != 0 || explicit.ReplyToMessageID != original.ID || explicit.Correction == nil || explicit.Correction.MessageID != original.ID ||
		explicit.Correction.OriginalTopicID != one.TopicID || explicit.Correction.PreviousTopicID != one.TopicID || explicit.Correction.EffectiveTopicID != two.TopicID {
		t.Fatalf("explicit reference was not used exclusively, or requested recipients were woken: %s", w.Body)
	}
	input["content"] = "这句话指的是告警"
	w = postCorrectionHTTP(t, a, g.ID, "conflict", input)
	conflict := decoded[correctionPOSTReceipt](t, w, 201)
	if conflict.TopicID != "" || len(conflict.Deliveries) != 0 || conflict.Correction == nil || conflict.Correction.Question == "" || conflict.Correction.MessageID != "" {
		t.Fatalf("ambiguous destination needs a public main-timeline question, not a move/delivery: %s", w.Body)
	}
	// A different preceding input cannot make a prior ambiguous result actionable.
	put("later", "另一条讨论。")
	if got := postCorrectionHTTP(t, a, g.ID, "conflict", input); got.Code != 201 || !bytes.Equal(got.Body.Bytes(), w.Body.Bytes()) {
		t.Fatalf("ambiguous replay was reinterpreted: %d %s", got.Code, got.Body)
	}
	input["reply_to_message_id"] = intervening.ID
	w = postCorrectionHTTP(t, a, g.ID, "conflict", input)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
	input["topic_id"], input["reply_to_message_id"] = one.TopicID, original.ID
	input["content"] = "这句话指的是 MQ-29。"
	manual := postCorrectionHTTP(t, a, g.ID, "manual", input)
	asked := decoded[correctionPOSTReceipt](t, manual, 201)
	if asked.TopicID != "" || len(asked.Deliveries) != 0 || asked.Correction == nil || asked.Correction.Question == "" ||
		!bytes.Equal(manual.Body.Bytes(), postCorrectionHTTP(t, a, g.ID, "manual", input).Body.Bytes()) {
		t.Fatalf("manual conflict was not a stable main-timeline clarification: %s", manual.Body)
	}
	attr, err := a.db.MessageThreadAttribution(t.Context(), g.ID, original.ID)
	if err != nil || attr.Storage.TopicID != one.TopicID || attr.Effective.TopicID != two.TopicID || attr.CorrectionMessageID != explicit.ID {
		t.Fatalf("conflict changed previous attribution: %+v, %v", attr, err)
	}
}
