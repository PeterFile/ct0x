package httpapi

import (
	"strings"
	"testing"

	"agentworkspace/internal/platform"
)

func TestMemberHistoryToolHTTPCompleteRequestBodyCap(t *testing.T) {
	for _, request := range []struct{ operation, body string }{
		{"search_history", `{"query":"中文😀"}`},
		{"read_history", `{"message_id":"历史😀"}`},
	} {
		t.Run(request.operation, func(t *testing.T) {
			f := newMemberHistoryToolHTTP(t)
			memberHistoryToolMessage(t, f, "历史😀", f.groupID, "trusted-topic", "中文😀")
			before := memberHistoryToolState(t, f)
			atLimit := request.body + strings.Repeat(" ", 65536-len(request.body))
			w := f.request(t, request.operation, atLimit)
			if w.Code != 200 {
				t.Fatalf("exactly 65536 body bytes rejected: %d %s", w.Code, w.Body)
			}
			for _, body := range []string{atLimit + " ", atLimit + "x", `{"query":"` + strings.Repeat("x", 65536) + `"}`} {
				w := f.request(t, request.operation, body)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "body_too_large")
			}
			w = f.request(t, request.operation, request.body+"x")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			if memberHistoryToolState(t, f) != before {
				t.Fatal("bounded reads or body rejection created durable effects")
			}
		})
	}
}

func TestMemberHistoryToolHTTPWholeEscapedResponseBudget(t *testing.T) {
	for _, size := range []int{65536, 65537} {
		name := "exact-limit"
		if size > 65536 {
			name = "one-byte-overflow"
		}
		t.Run(name, func(t *testing.T) {
			f := newMemberHistoryToolHTTP(t)
			memberHistoryToolMessage(t, f, "budget-0", f.groupID, "trusted-topic", "budget-0")
			baseline := f.request(t, "search_history", `{"query":"budget-0"}`)
			decoded[platform.MemberHistorySearchResult](t, baseline, 200)
			// Measure actual HTTP metadata; equal-length immutable IDs/content keep that overhead fixed.
			encodedSenderBytes := size - (baseline.Body.Len() - len("user"))
			sender := strings.Repeat("<\x01", encodedSenderBytes/12) + strings.Repeat("x", encodedSenderBytes%12)
			if _, err := f.raw.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at)
			 VALUES('budget-1',?,'trusted-topic',?,'budget-1','2100-01-02T03:04:05.123456789Z')`, f.groupID, sender); err != nil {
				t.Fatal(err)
			}
			before := memberHistoryToolState(t, f)
			w := f.request(t, "search_history", `{"query":"budget-1"}`)
			if size > 65536 {
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "tool_result_too_large")
				if w.Body.Len() > 512 || !strings.Contains(w.Body.String(), "减小 limit") {
					t.Fatal("overflow returned partial data or omitted safe Chinese limit guidance")
				}
			} else {
				page := decoded[platform.MemberHistorySearchResult](t, w, 200)
				if w.Body.Len() != 65536 || w.Body.Bytes()[65535] != '\n' || len(page.Items) != 1 || page.Items[0].SenderID != sender || page.NextCursor != "" || !strings.Contains(w.Body.String(), `\u003c\u0001`) {
					t.Fatal("exact escaped JSON plus newline budget was truncated, replaced or rejected")
				}
			}
			if memberHistoryToolState(t, f) != before {
				t.Fatal("result sizing changed durable authority")
			}
		})
	}
}

func TestMemberHistoryToolHTTPSafeStoreErrorsWithoutPartialData(t *testing.T) {
	for _, fixture := range []struct {
		name, content, stamp, code string
		status                     int
	}{
		{"invalid-utf8", "needle PRIVATE-SQL\xff", "2100-01-02T03:04:05.123456789Z", "invalid_input", 400},
		{"scanner-error", "needle PRIVATE-SQL", "PRIVATE-SQL", "internal_error", 500},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			f := newMemberHistoryToolHTTP(t)
			if _, err := f.raw.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at)
			 VALUES('bad-evidence',?,'trusted-topic','user',?,?)`, f.groupID, fixture.content, fixture.stamp); err != nil {
				t.Fatal(err)
			}
			before := memberHistoryToolState(t, f)
			for _, request := range []struct{ operation, body string }{
				{"search_history", `{"query":"needle"}`},
				{"read_history", `{"message_id":"bad-evidence"}`},
			} {
				w := f.request(t, request.operation, request.body)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), fixture.status, fixture.code)
			}
			if memberHistoryToolState(t, f) != before {
				t.Fatal("failed Store reads changed durable authority")
			}
		})
	}
}
