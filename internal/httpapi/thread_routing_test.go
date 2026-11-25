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

func threadRoutingFixtureInstalled(t *testing.T, a *groupAPI) bool {
	t.Helper()
	var installed bool
	if err := messageSQL(t, a).QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='thread_routing_topics')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	t.Logf("thread routing schema installed=%t", installed)
	return installed
}

func TestThreadRoutingNormalEntry(t *testing.T) {
	a := openMessageAPI(t, "")
	installed := threadRoutingFixtureInstalled(t, a)
	group := newGroup(t, a, "routing")
	path := "/api/v1/groups/" + group.ID + "/messages"
	content := "新议题：PAY-431 下载超时。先讨论重试策略。"
	first := decoded[domain.Message](t, a.request("POST", path, `{"content":"`+content+`"}`, "first"), 201)
	if (first.TopicID != "") != installed || first.Content != content {
		t.Fatalf("normal POST with routing installed=%t: topic=%q, content=%q", installed, first.TopicID, first.Content)
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

func TestThreadRoutingMainSelectorReplay(t *testing.T) {
	a := openMessageAPI(t, "")
	group := newGroup(t, a, "selector choices")
	path := "/api/v1/groups/" + group.ID + "/messages"
	checks := []struct{ key, first, changed string }{
		{"auto-first", `{"content":"just chatting"}`, `{"content":"just chatting","topic_id":""}`},
		{"main-first", `{"content":"just chatting","topic_id":""}`, `{"content":"just chatting"}`},
	}
	receipts := make([]domain.Message, len(checks))
	for i, tc := range checks {
		m := decoded[domain.Message](t, a.request("POST", path, tc.first, tc.key), 201)
		if m.ID == "" || m.TopicID != "" || m.GroupID != group.ID || m.SenderID != "user" || m.Content != "just chatting" {
			t.Fatalf("selector fixture should persist the original main message: %+v", m)
		}
		receipts[i] = m
		w := a.request("POST", path, tc.changed, tc.key)
		if w.Code != 409 {
			t.Errorf("%s: selector change returned %d, want conflict: %s", tc.key, w.Code, w.Body)
		} else {
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
		}
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = openMessageAPI(t, a.path)
	for i, tc := range checks {
		m := decoded[domain.Message](t, a.request("POST", path, tc.first, tc.key), 201)
		if m.ID != receipts[i].ID || m.SourceID != receipts[i].SourceID {
			t.Errorf("%s: reopen changed receipt: %+v", tc.key, m)
		}
		w := a.request("POST", path, tc.changed, tc.key)
		if w.Code != 409 {
			t.Errorf("%s: reopened selector change returned %d, want conflict: %s", tc.key, w.Code, w.Body)
		} else {
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
		}
	}
	page := decoded[listResult[domain.Message]](t, a.request("GET", path, ""), 200)
	if len(page.Items) != 2 {
		t.Fatalf("retries changed the number of messages: %d", len(page.Items))
	}
}

func TestThreadRoutingOpeningFollowupClause(t *testing.T) {
	a := openMessageAPI(t, "")
	installed := threadRoutingFixtureInstalled(t, a)
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
	if (export.TopicID != "") != installed || export.Content != opening || len(export.Deliveries) != 0 {
		t.Fatalf("public opening with routing installed=%t: %+v", installed, export)
	}
	if followup := post("跟进那个导出 BUG"); followup.TopicID != export.TopicID {
		t.Fatalf("named followup: topic=%q, want %q", followup.TopicID, export.TopicID)
	}
	other := post("新议题：数据库连接告警 OPS-61")
	if (other.TopicID != "") != installed || (installed && other.TopicID == export.TopicID) {
		t.Fatalf("second opening with routing installed=%t: %+v", installed, other)
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
	installed := threadRoutingFixtureInstalled(t, a)
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
	if (root.TopicID != "") != installed || byID.TopicID != root.TopicID || byName.TopicID != root.TopicID {
		t.Fatalf("dormant issue with routing installed=%t: root=%q id=%q name=%q", installed, root.TopicID, byID.TopicID, byName.TopicID)
	}
	if !installed {
		return // Both forms of follow-up were verified to stay on main.
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
	t.Run("strong_evidence_disambiguates_alias", func(t *testing.T) {
		for _, tc := range []struct {
			name, content, reply, want string
		}{
			{"issue", "跟进那个导出 BUG，EX-421", "", root.TopicID},
			{"reply", "跟进那个导出 BUG", second.ID, second.TopicID},
			{"contradictory_issue_and_reply", "跟进那个导出 BUG，EX-421", second.ID, ""},
			{"contradictory_exact_title_and_reply", "当前讨论导出 BUG EX-421", second.ID, ""},
			{"incompatible_reply", "跟进那个导出 BUG", prefix.ID, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var refs []string
				if tc.reply != "" {
					refs = append(refs, tc.reply)
				}
				if m := post(tc.content, refs...); m.TopicID != tc.want || m.ReplyToMessageID != tc.reply || len(m.Deliveries) != 0 {
					t.Fatalf("topic=%q, want %q; receipt=%+v", m.TopicID, tc.want, m)
				}
			})
		}
	})
	t.Run("exact_title_beats_shared_alias", func(t *testing.T) {
		plain := post("新议题：导出 BUG")
		if plain.TopicID == "" || plain.TopicID == root.TopicID || plain.TopicID == second.TopicID {
			t.Fatalf("exact-title fixture did not open a different topic: %+v", plain)
		}
		if m := post("当前讨论导出 BUG"); m.TopicID != plain.TopicID {
			t.Fatalf("full title must not lose to shared shortened titles: got %q want %q", m.TopicID, plain.TopicID)
		}
		if m := post("当前讨论导出 BUG，EX-421"); m.TopicID != "" {
			t.Fatalf("contradictory exact title and issue escaped main: %+v", m)
		}
		if m := post("当前讨论导出 BUG EX-421"); m.TopicID != root.TopicID {
			t.Fatalf("contained title must not contradict the longest complete title: got %q want %q", m.TopicID, root.TopicID)
		}
		for _, content := range []string{
			"导出 BUG EX-421 和导出 BUG 都要跟进。",
			"导出 BUG 和导出 BUG EX-421 都要跟进。",
			"导出 BUG，EX-421，引用仅作对照。",
		} {
			if m := post(content); m.TopicID != "" {
				t.Errorf("independent mention of the shorter title escaped main: %q → %q", content, m.TopicID)
			}
		}
	})
	generic := post("New topic: incident OPS-62")
	if generic.TopicID == "" || post("Follow up on incident").TopicID != "" || post("New topic: incident report").TopicID != "" {
		t.Fatalf("generic words created or selected a thread without an issue: %+v", generic)
	}
}

func TestThreadRoutingComparisonDoesNotWakeQuotedAuthor(t *testing.T) {
	a := openMessageAPI(t, "")
	installed := threadRoutingFixtureInstalled(t, a)
	group := newGroup(t, a, "comparison")
	path := "/api/v1/groups/" + group.ID
	newMember := func(name string) string {
		t.Helper()
		return decoded[domain.Member](t, a.request("POST", path+"/members", `{"name":"`+name+`","role":"role"}`), 201).ID
	}
	post := func(t *testing.T, content, reply string, recipients []string) domain.Message {
		t.Helper()
		input := map[string]any{"content": content}
		if reply != "" {
			input["reply_to_message_id"] = reply
		}
		if recipients != nil {
			input["recipient_ids"] = recipients
		}
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		return decoded[domain.Message](t, a.request("POST", path+"/messages", string(body)), 201)
	}
	quotedAuthor := newMember("quoted author")
	quotedTopic := post(t, "新话题：EX-421 导出错误", "", []string{})
	if (quotedTopic.TopicID != "") != installed || len(quotedTopic.Deliveries) != 0 {
		t.Fatalf("public comparison source with routing installed=%t: %+v", installed, quotedTopic)
	}
	quoted, err := a.db.CreateMessage(t.Context(), store.MessageRequest{
		GroupID: group.ID, TopicID: quotedTopic.TopicID, ActorID: quotedAuthor, Content: "旧导出故障。",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !installed {
		var quotesInstalled bool
		if err := messageSQL(t, a).QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='message_quotes')`).Scan(&quotesInstalled); err != nil {
			t.Fatal(err)
		}
		if !quotesInstalled {
			t.Run("legacy_reply_unavailable", func(t *testing.T) {
				const body = `{"content":"引用仅作对照。","topic_id":"legacy-topic"`
				w := a.request("POST", path+"/messages", body+`,"reply_to_message_id":"`+quoted.ID+`"}`, "legacy-reply")
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
				// The rejected reference must not consume the key or persist a row.
				m := decoded[domain.Message](t, a.request("POST", path+"/messages", body+`}`, "legacy-reply"), 201)
				if m.TopicID != "legacy-topic" || m.Content != "引用仅作对照。" || m.ReplyToMessageID != "" ||
					len(m.Deliveries) != 1 || m.Deliveries[0].RecipientID != quotedAuthor {
					t.Fatalf("legacy explicit topic or default delivery changed: %+v", m)
				}
				page := decoded[listResult[domain.Message]](t, a.request("GET", path+"/messages", ""), 200)
				if len(page.Items) != 3 || page.Items[0].ID != m.ID {
					t.Fatalf("rejected reply changed the timeline: %+v", page)
				}
			})
			return
		}
		t.Run("a3_only_comparison_and_task_link_unavailable", func(t *testing.T) {
			const body = `{"content":"引用仅作对照。","topic_id":"legacy-topic"`
			// Related task links still require thread routing; rejection must not consume the key or persist a row.
			w := a.request("POST", path+"/messages", body+`,"related_task_id":"task-unavailable"}`, "legacy-task-link")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
			// With message quotes registered (A3) and thread routing absent, a comparison-only quote
			// persists the quote on the explicit topic without waking the quoted author.
			m := decoded[domain.Message](t, a.request("POST", path+"/messages", body+`,"reply_to_message_id":"`+quoted.ID+`"}`, "legacy-task-link"), 201)
			if m.TopicID != "legacy-topic" || m.Content != "引用仅作对照。" || m.ReplyToMessageID != quoted.ID ||
				m.ReplyTo == nil || m.ReplyTo.MessageID != quoted.ID || len(m.Deliveries) != 0 {
				t.Fatalf("A3-only comparison woke the quoted author or lost explicit scope: %+v", m)
			}
			page := decoded[listResult[domain.Message]](t, a.request("GET", path+"/messages", ""), 200)
			if len(page.Items) != 3 || page.Items[0].ID != m.ID {
				t.Fatalf("A3-only comparison changed the timeline count: %+v", page)
			}
		})
		return
	}
	check := func(t *testing.T, content, topic, recipient string, explicit []string) {
		t.Helper()
		m := post(t, content, quoted.ID, explicit)
		if m.Content != content || m.TopicID != topic || m.ReplyToMessageID != quoted.ID {
			t.Errorf("comparison changed scope or raw reference: %+v, want topic %q", m, topic)
		}
		if (recipient == "" && len(m.Deliveries) != 0) ||
			(recipient != "" && (len(m.Deliveries) != 1 || m.Deliveries[0].RecipientID != recipient)) {
			t.Errorf("comparison got deliveries %+v, want recipient %q", m.Deliveries, recipient)
		}
	}
	t.Run("only_quoted_author", func(t *testing.T) {
		check(t, "当前讨论支付网关故障，引用仅作对照。", "", "", nil)
	})
	fallback := newMember("eligible fallback")
	t.Run("earliest_quoted_author", func(t *testing.T) {
		check(t, "当前讨论支付网关故障，引用仅作对照。", "", fallback, nil)
		check(t, "引用仅作对照。", "", fallback, nil)
	})
	t.Run("same_clause", func(t *testing.T) {
		current := post(t, "新议题：支付网关故障", "", []string{quotedAuthor})
		if current.TopicID == "" || current.TopicID == quotedTopic.TopicID {
			t.Fatalf("missing independent current topic: %+v", current)
		}
		check(t, "当前讨论支付网关故障（EX-421仅作对照）", current.TopicID, quotedAuthor, nil)
		check(t, "当前讨论支付网关故障 (EX-421, 仅供参考)。", current.TopicID, "", []string{})
		check(t, "当前讨论支付网关故障 EX-421仅作对照", "", fallback, nil)
		check(t, "当前讨论支付网关故障（EX-421，仅作对照", "", fallback, nil)
		check(t, "当前讨论账单入账故障（EX-421仅作对照）", "", quotedAuthor, []string{quotedAuthor})
	})
}

func TestThreadRoutingEvidenceAndRecipients(t *testing.T) {
	a := openMessageAPI(t, "")
	installed := threadRoutingFixtureInstalled(t, a)
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
	if (alert.TopicID != "") != installed || (plan.TopicID != "") != installed || (installed && alert.TopicID == plan.TopicID) ||
		len(plan.Deliveries) != 0 || len(alert.Deliveries) != 1 || alert.Deliveries[0].RecipientID != second.ID {
		t.Fatalf("opening with routing installed=%t did not preserve topics or recipients: %+v %+v", installed, alert, plan)
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
		if !installed {
			tc.topic, tc.recipient = "", first.ID // Legacy delivery is independent of message text.
		}
		m := decoded[domain.Message](t, post(fmt.Sprintf("watch-%d", i), map[string]any{"content": tc.content}), 201)
		if m.TopicID != tc.topic || len(m.Deliveries) != 1 || m.Deliveries[0].RecipientID != tc.recipient {
			t.Fatalf("%q: got topic=%q deliveries=%+v; want topic=%q recipient=%q", tc.content, m.TopicID, m.Deliveries, tc.topic, tc.recipient)
		}
	}
	if !installed {
		return // Typed reply rejection on v30 is covered by the comparison test.
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
		public, explicitTopic          bool
	}{
		{"author", "阈值是多少？", alert.TopicID, second.ID, false, false},
		{"public-reply", "只记录这个问题。", alert.TopicID, "", true, false},
		{"comparison", "引用仅作对照，当前讨论明年仓库容量规划。", plan.TopicID, first.ID, false, false},
		{"unknown-comparison", "当前讨论支付网关故障，引用仅作对照。", "", first.ID, false, false},
		{"conflicting-reply", "明年仓库容量规划还需要讨论。", "", "", false, false},
		{"explicit-different-topic", "阈值是多少？", plan.TopicID, "", false, true},
	} {
		input := map[string]any{"content": tc.content, "reply_to_message_id": author.ID}
		if tc.public {
			input["recipient_ids"] = []string{}
		}
		if tc.explicitTopic {
			input["topic_id"] = tc.topic
		}
		m := decoded[domain.Message](t, post(tc.key, input), 201)
		if m.TopicID != tc.topic || m.ReplyToMessageID != author.ID || (tc.recipient == "" && len(m.Deliveries) != 0) ||
			(tc.recipient != "" && (len(m.Deliveries) != 1 || m.Deliveries[0].RecipientID != tc.recipient)) {
			t.Errorf("%s: wrong scope, link, or delivery: %+v", tc.key, m)
		}
	}
	explicitComparison := decoded[domain.Message](t, post("explicit-comparison", map[string]any{
		"content": "当前讨论支付网关故障，引用仅作对照。", "reply_to_message_id": author.ID, "topic_id": alert.TopicID,
	}), 201)
	if explicitComparison.TopicID != alert.TopicID || explicitComparison.ReplyToMessageID != author.ID ||
		len(explicitComparison.Deliveries) != 1 || explicitComparison.Deliveries[0].RecipientID != first.ID {
		t.Fatalf("explicit topic changed or comparison selected the quoted author: %+v", explicitComparison)
	}
	page := decoded[listResult[domain.Message]](t, a.request("GET", path, ""), 200)
	if len(page.Items) != 15 || page.Items[0].ReplyToMessageID != author.ID || page.NextCursor != "" {
		t.Fatalf("one group timeline did not carry the stable link: %+v", page)
	}

	raw := messageSQL(t, a)
	for table, want := range map[string]int{"thread_routing_topics": 2, "tasks": 0, "mandates": 0, "deliveries": 10} {
		var got int
		if err := raw.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d, %v; want %d", table, got, err, want)
		}
	}
}
