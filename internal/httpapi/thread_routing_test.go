package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func requireThreadRoutingFixture(t *testing.T, a *groupAPI) {
	t.Helper()
	var installed bool
	if err := messageSQL(t, a).QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='thread_routing_topics')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Skip("threadRoutingSchema is not registered; use the private migration overlay")
	}
}

func TestThreadRoutingNormalEntry(t *testing.T) {
	a := openMessageAPI(t, "")
	requireThreadRoutingFixture(t, a)
	group := newGroup(t, a, "routing")
	path := "/api/v1/groups/" + group.ID + "/messages"
	content := "新议题：PAY-431 下载超时。先讨论重试策略。"
	first := decoded[domain.Message](t, a.request("POST", path, `{"content":"`+content+`"}`, "first"), 201)
	if first.TopicID == "" || first.Content != content {
		t.Fatalf("normal POST did not persist the named subject: topic=%q, content=%q", first.TopicID, first.Content)
	}
	followup := decoded[domain.Message](t, a.request("POST", path, `{"content":"PAY-431 现在有什么进展？"}`, "followup"), 201)
	if followup.TopicID != first.TopicID || followup.ID == first.ID {
		t.Fatalf("exact issue followup did not reuse the topic: first=%+v followup=%+v", first, followup)
	}
	main := decoded[domain.Message](t, a.request("POST", path, `{"content":"PAY-431 继续。","topic_id":""}`, "main"), 201)
	if main.TopicID != "" {
		t.Fatalf("explicit main was inferred away: %+v", main)
	}
	w := a.request("POST", path, `{"content":"PAY-431 现在有什么进展？","topic_id":""}`, "followup")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
}

func TestThreadRoutingOpeningFollowupClause(t *testing.T) {
	a := openMessageAPI(t, "")
	requireThreadRoutingFixture(t, a)
	group := newGroup(t, a, "opening")
	path := "/api/v1/groups/" + group.ID + "/messages"
	post := func(content string) domain.Message {
		t.Helper()
		body, err := json.Marshal(map[string]any{"content": content, "recipient_ids": []string{}})
		if err != nil {
			t.Fatal(err)
		}
		return decoded[domain.Message](t, a.request("POST", path, string(body)), 201)
	}
	const opening = "新议题：导出 BUG EX-421。跟进导出故障。"
	export := post(opening)
	if export.TopicID == "" || export.Content != opening || len(export.Deliveries) != 0 {
		t.Fatalf("opening with action prose did not persist its subject as public only: %+v", export)
	}
	if followup := post("跟进那个导出 BUG"); followup.TopicID != export.TopicID {
		t.Fatalf("named followup: topic=%q, want %q", followup.TopicID, export.TopicID)
	}
	other := post("新议题：数据库连接告警 OPS-61")
	if other.TopicID == "" || other.TopicID == export.TopicID {
		t.Fatalf("second fixture did not create a distinct topic: %+v", other)
	}
	for _, content := range []string{
		"新议题：导出 BUG EX-421。跟进数据库连接告警。",
		"新议题：索引切换计划。跟进数据库连接告警。",
		"新议题：索引切换计划。跟进 OPS-61。",
		"新议题：导出 BUG EX-421。跟进另一个单号 EX-422。",
	} {
		if got := post(content); got.TopicID != "" || len(got.Deliveries) != 0 {
			t.Errorf("conflicting evidence escaped main: %q → %+v", content, got)
		}
	}
	if got := post("新议题：导出 BUG EX-421。跟进导出 BUG。"); got.TopicID != export.TopicID {
		t.Fatalf("agreeing named subject did not return to its topic: %+v", got)
	}
}

func TestThreadRoutingColdIssueName(t *testing.T) {
	a := openMessageAPI(t, "")
	requireThreadRoutingFixture(t, a)
	group := newGroup(t, a, "cold names")
	path := "/api/v1/groups/" + group.ID + "/messages"
	post := func(content string, replyTo ...string) domain.Message {
		t.Helper()
		input := map[string]any{"content": content, "recipient_ids": []string{}}
		if len(replyTo) != 0 {
			input["reply_to_message_id"] = replyTo[0]
		}
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		return decoded[domain.Message](t, a.request("POST", path, string(body)), 201)
	}
	root := post("新议题：导出 BUG EX-421")
	if _, err := messageSQL(t, a).Exec(`UPDATE messages SET created_at=? WHERE id=?`, time.Now().UTC().AddDate(0, -3, 0).Format("2006-01-02T15:04:05.000000000Z"), root.ID); err != nil {
		t.Fatal(err)
	}
	byID, byName := post("EX-421 怎么样了？"), post("跟进那个导出 BUG")
	if root.TopicID == "" || byID.TopicID != root.TopicID || byName.TopicID != root.TopicID {
		t.Fatalf("dormant issue should resolve by exact ID and explicit name: root=%q id=%q name=%q", root.TopicID, byID.TopicID, byName.TopicID)
	}
	prefix := post("New topic: OPS-61: Report retention")
	if prefix.TopicID == "" || post("Follow up on report retention").TopicID != prefix.TopicID {
		t.Fatalf("issue prefix did not support its named subject: %+v", prefix)
	}
	if got := post("跟进那个导出 BUG", prefix.ID); got.TopicID != "" || got.ReplyToMessageID != prefix.ID {
		t.Fatalf("conflicting structured reference was discarded or chose a topic: %+v", got)
	}
	second := post("新话题：导出 BUG EX-422")
	if second.TopicID == "" || second.TopicID == root.TopicID || post("跟进那个导出 BUG").TopicID != "" ||
		post("跟进 EX-422").TopicID != second.TopicID || post("再核对一次", root.ID).TopicID != root.TopicID {
		t.Fatalf("ambiguous names must not override exact issue/reply evidence: first=%q second=%q", root.TopicID, second.TopicID)
	}
	generic := post("New topic: incident OPS-62")
	if generic.TopicID == "" || post("Follow up on incident").TopicID != "" || post("New topic: incident report").TopicID != "" {
		t.Fatalf("generic words created or selected a thread without an issue: %+v", generic)
	}
}

func TestThreadRoutingEvidenceAndRecipients(t *testing.T) {
	a := openMessageAPI(t, "")
	requireThreadRoutingFixture(t, a)
	group, other := newGroup(t, a, "routing"), newGroup(t, a, "other")
	path := "/api/v1/groups/" + group.ID + "/messages"
	members := "/api/v1/groups/" + group.ID + "/members"
	first := decoded[domain.Member](t, a.request("POST", members, `{"name":"first","role":"role"}`), 201)
	second := decoded[domain.Member](t, a.request("POST", members, `{"name":"second","role":"role"}`), 201)
	post := func(key string, payload map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return a.request("POST", path, string(body), key)
	}
	alert := decoded[domain.Message](t, post("alert", map[string]any{
		"content": "开个新话题：数据库连接告警。先聊阈值。", "recipient_ids": []string{second.ID},
	}), 201)
	plan := decoded[domain.Message](t, post("plan", map[string]any{
		"content": "我们来讨论明年仓库容量规划。先列风险。", "recipient_ids": []string{},
	}), 201)
	if alert.TopicID == "" || plan.TopicID == "" || alert.TopicID == plan.TopicID || len(plan.Deliveries) != 0 {
		t.Fatalf("opening did not preserve distinct subjects / public-only: %+v %+v", alert, plan)
	}
	if _, err := messageSQL(t, a).Exec(`UPDATE messages SET created_at=? WHERE id=?`, time.Now().UTC().AddDate(0, -6, 0).Format("2006-01-02T15:04:05.000000000Z"), alert.ID); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct{ content, topic, recipient string }{
		{"数据库连接告警现在怎么样？", alert.TopicID, second.ID},
		{"当前讨论明年仓库容量规划。\n> 数据库连接告警也先参考一下", plan.TopicID, first.ID},
		{"数据库连接告警与明年仓库容量规划都看一下", "", first.ID},
		{"今天随便聊两句", "", first.ID},
	} {
		m := decoded[domain.Message](t, post(fmt.Sprintf("watch-%d", i), map[string]any{"content": tc.content}), 201)
		if m.TopicID != tc.topic || len(m.Deliveries) != 1 || m.Deliveries[0].RecipientID != tc.recipient {
			t.Fatalf("%q: got topic=%q deliveries=%+v; want topic=%q recipient=%q", tc.content, m.TopicID, m.Deliveries, tc.topic, tc.recipient)
		}
	}
	foreign := decoded[domain.Message](t, a.request("POST", "/api/v1/groups/"+other.ID+"/messages", `{"content":"foreign"}`), 201)
	bad := post("reply", map[string]any{"content": "新话题：不会注册的议题", "reply_to_message_id": foreign.ID})
	assertError(t, bad.Code, bad.Header(), bad.Body.Bytes(), 404, "not_found")
	payload := map[string]any{"content": "具体样本呢？", "reply_to_message_id": alert.ID, "recipient_ids": []string{first.ID}}
	w := post("reply", payload)
	reply := decoded[struct {
		domain.Message
		ReplyTo string `json:"reply_to_message_id"`
	}](t, w, 201)
	if reply.TopicID != alert.TopicID || reply.ReplyTo != alert.ID || len(reply.Deliveries) != 1 || reply.Deliveries[0].RecipientID != first.ID {
		t.Fatalf("reply linkage overrode explicit target or selected wrong thread: %+v", reply)
	}
	detail := a.request("GET", path+"/"+reply.ID, "")
	replay := post("reply", payload)
	if detail.Code != 200 || replay.Code != 201 || !bytes.Equal(w.Body.Bytes(), detail.Body.Bytes()) || !bytes.Equal(w.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("reply links did not round trip: post=%s get=%s replay=%s", w.Body, detail.Body, replay.Body)
	}
	payload["reply_to_message_id"] = plan.ID
	w = post("reply", payload)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")

	author, err := a.db.CreateMessage(context.Background(), store.MessageRequest{
		GroupID: group.ID, TopicID: alert.TopicID, ActorID: second.ID, Content: "阈值尚未确认。",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key, content, topic, recipient string
		public                         bool
	}{
		{"author", "阈值是多少？", alert.TopicID, second.ID, false},
		{"public-reply", "只记录这个问题。", alert.TopicID, "", true},
		{"comparison", "引用仅作对照，当前讨论明年仓库容量规划。", plan.TopicID, first.ID, false},
		{"conflicting-reply", "明年仓库容量规划还需要讨论。", "", first.ID, false},
	} {
		input := map[string]any{"content": tc.content, "reply_to_message_id": author.ID}
		if tc.public {
			input["recipient_ids"] = []string{}
		}
		m := decoded[domain.Message](t, post(tc.key, input), 201)
		if m.TopicID != tc.topic || m.ReplyToMessageID != author.ID || (tc.public && len(m.Deliveries) != 0) ||
			(!tc.public && (len(m.Deliveries) != 1 || m.Deliveries[0].RecipientID != tc.recipient)) {
			t.Fatalf("%s: wrong scope, link, or delivery: %+v", tc.key, m)
		}
	}
	page := decoded[listResult[domain.Message]](t, a.request("GET", path, ""), 200)
	if len(page.Items) != 12 || page.Items[0].ReplyToMessageID != author.ID || page.NextCursor != "" {
		t.Fatalf("one group timeline did not carry the stable link: %+v", page)
	}

	raw := messageSQL(t, a)
	for table, want := range map[string]int{"thread_routing_topics": 2, "tasks": 0, "mandates": 0, "deliveries": 9} {
		var got int
		if err := raw.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d, %v; want %d", table, got, err, want)
		}
	}
}
