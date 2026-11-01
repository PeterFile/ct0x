package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func TestMemberToolHTTPRequestCapCountsAllUTF8AndTrailingBytes(t *testing.T) {
	const maximum = 64 * 1024
	for _, request := range memberToolHTTPRequests {
		t.Run(request.operation, func(t *testing.T) {
			f := newMemberToolHTTP(t)
			body := strings.ReplaceAll(request.body, "native message", "中文🐈")
			body = strings.ReplaceAll(body, "Explain dataset A and its outliers", "中文🐈：解释数据与问题")
			atLimit := body + strings.Repeat(" ", maximum-len(body))
			w := f.request(t, request.operation, atLimit, "bounded")
			wantStatus := 200
			if request.operation == "send_message" || request.operation == "create_member" {
				wantStatus = 201
			}
			if w.Code != wantStatus {
				t.Fatalf("exactly 64 KiB must succeed: %d %s", w.Code, w.Body)
			}
			before := f.counts(t)
			for _, oversized := range []string{atLimit + " ", atLimit + "x", `{"content":"` + strings.Repeat("x", maximum) + `"}`} {
				w := f.request(t, request.operation, oversized, "bounded")
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "body_too_large")
				if f.counts(t) != before {
					t.Fatal("body overflow executed an operation or replay")
				}
			}
			w = f.request(t, request.operation, body+"x", "bounded")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
		})
	}
}

func TestMemberToolHTTPCompleteEncodedPageCapIncludesNewline(t *testing.T) {
	const maximum = 64 * 1024
	for _, operation := range []string{"list_members", "read_channel"} {
		t.Run(operation, func(t *testing.T) {
			f := newMemberToolHTTP(t)
			baseline := f.request(t, operation, `{"limit":1}`)
			page := decoded[listResult[json.RawMessage]](t, baseline, 200)
			var item struct {
				ID      string `json:"id"`
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if len(page.Items) != 1 || json.Unmarshal(page.Items[0], &item) != nil {
				t.Fatal("page fixture lacks one ordinary item")
			}
			table, column, original := "members", "role", item.Role
			if operation == "read_channel" {
				table, column, original = "messages", "content", item.Content
			}
			encodedLength := maximum - (baseline.Body.Len() - len(original))
			// '<' expands to six JSON bytes, proving the cap measures encoding, not raw text.
			atLimit := strings.Repeat("<", encodedLength/6) + strings.Repeat("x", encodedLength%6)
			for _, overflow := range []bool{false, true} {
				text := atLimit
				if overflow {
					text += "x"
				}
				if _, err := f.raw.Exec("UPDATE "+table+" SET "+column+"=? WHERE id=?", text, item.ID); err != nil {
					t.Fatal(err)
				}
				before := f.counts(t)
				w := f.request(t, operation, `{"limit":1}`)
				if overflow {
					assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "tool_result_too_large")
					if !strings.Contains(w.Body.String(), "limit") || !strings.Contains(w.Body.String(), "减小") {
						t.Fatal("oversized page lacks generic smaller-page guidance")
					}
				} else {
					page := decoded[listResult[json.RawMessage]](t, w, 200)
					var returned struct{ Role, Content string }
					if w.Body.Len() != maximum || w.Body.Bytes()[maximum-1] != '\n' || len(page.Items) != 1 || json.Unmarshal(page.Items[0], &returned) != nil || (returned.Role != text && returned.Content != text) {
						t.Fatal("exact encoded limit must return the complete untruncated page, including newline")
					}
				}
				if f.counts(t) != before {
					t.Fatal("page sizing changed durable data")
				}
			}
		})
	}
}

func TestMemberToolHTTPWriteProjectionsNeverRejectLargeCommittedResults(t *testing.T) {
	f := newMemberToolHTTP(t)
	ctx := context.Background()
	content := strings.Repeat("<", 30000)
	send := f.request(t, "send_message", `{"content":"`+content+`"}`, "large-send")
	projection := decoded[memberToolHTTPSendResult](t, send, 201)
	memberToolHTTPKeys(t, send, "message_id", "created_at", "delivery_count", "content_omitted")
	stored, err := f.db.GetMessage(ctx, f.groupID, projection.MessageID)
	if err != nil || stored.Content != content || !projection.ContentOmitted {
		t.Fatalf("small send projection changed durable content: %v", err)
	}
	fullMessage, _ := json.Marshal(stored)
	if len(fullMessage) <= 64*1024 {
		t.Fatal("test requires a durable result larger than the transport cap")
	}
	role, reason := strings.Repeat("<", 8192), strings.Repeat(">", 16384)
	body := `{"name":"Analyst","role":"` + role + `","reason":"` + reason + `"}`
	createdResponse := f.request(t, "create_member", body, "large-create")
	created := decoded[memberToolHTTPCreateResult](t, createdResponse, 201)
	memberToolHTTPKeys(t, createdResponse, "member_id", "reused", "join_message_id")
	member, err := f.db.GetMember(ctx, f.groupID, created.MemberID)
	if err != nil || member.Role != role || created.Reused || created.JoinMessageID == "" {
		t.Fatalf("small create projection lost its durable result: %v", err)
	}
	join, err := f.db.GetMessage(ctx, f.groupID, created.JoinMessageID)
	if err != nil || !strings.Contains(join.Content, reason) || len(join.Deliveries) != 1 || join.Deliveries[0].RecipientID != member.ID {
		t.Fatalf("full join background was truncated: %v", err)
	}
	fullCreate, _ := json.Marshal(platform.MemberCreateResult{Member: member, JoinMessage: &join})
	if len(fullCreate) <= 64*1024 || f.counts(t) != ([6]int{4, 3, 2, 2, 2, 1}) {
		t.Fatal("projection failed after commit or duplicated durable effects")
	}
}

func TestMemberToolHTTPSendReceiptOmitsLongBoundTopic(t *testing.T) {
	f := newMemberToolHTTP(t)
	ctx := context.Background()
	topic := strings.Repeat("<", 12000)
	encodedTopic, err := json.Marshal(topic)
	if err != nil || len(encodedTopic) <= 64*1024 {
		t.Fatal("test requires a valid topic whose escaped JSON exceeds 64 KiB")
	}
	question, err := platform.NewLocalUser(f.db).CreateMessage(ctx, f.groupID, platform.MessageInput{
		Content: "question", TopicID: topic, RecipientIDs: []string{f.peer.ID},
	}, "long-topic-question")
	if err != nil || question.TopicID != topic {
		t.Fatalf("local-user messages must retain long valid topics: %v", err)
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
	before := f.counts(t)
	first := f.request(t, "send_message", `{"content":"reply"}`, "long-topic-send")
	result := decoded[memberToolHTTPSendResult](t, first, 201)
	memberToolHTTPKeys(t, first, "message_id", "created_at", "delivery_count", "content_omitted")
	if first.Body.Len() > 512 || result.MessageID == "" || result.CreatedAt.IsZero() || result.DeliveryCount != 0 || !result.ContentOmitted {
		t.Fatal("send must return a small exact-field receipt without the bound topic")
	}
	t.Logf("escaped_topic_bytes=%d receipt_bytes=%d", len(encodedTopic), first.Body.Len())
	stored, err := f.db.GetMessage(ctx, f.groupID, result.MessageID)
	if err != nil || stored.TopicID != topic || stored.SenderID != f.peer.ID || stored.Content != "reply" || !stored.CreatedAt.Equal(result.CreatedAt) {
		t.Fatalf("small receipt changed the complete durable message: %v", err)
	}
	want := before
	want[1]++
	want[4]++
	if f.counts(t) != want {
		t.Fatal("send must commit exactly one message and idempotency receipt")
	}
	replay := f.request(t, "send_message", `{"content":"reply"}`, "long-topic-send")
	if replay.Code != 201 || replay.Body.String() != first.Body.String() || f.counts(t) != want {
		t.Fatal("long-topic replay changed the receipt or duplicated durable effects")
	}
}
