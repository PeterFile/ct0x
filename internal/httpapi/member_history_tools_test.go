package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"agentworkspace/internal/platform"
)

func newMemberHistoryToolHTTP(t *testing.T, operations ...string) *memberToolHTTPFixture {
	t.Helper()
	if len(operations) == 0 {
		operations = []string{"search_history", "read_history"}
	}
	f := newMemberToolHTTP(t, operations...)
	f.routes = append(f.routes, MemberHistoryToolRoutes(f.tools)...)
	f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
	return f
}

func memberHistoryToolMessage(t *testing.T, f *memberToolHTTPFixture, id, groupID, topicID, content string) {
	t.Helper()
	if _, err := f.raw.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at)
	 VALUES(?,?,?,'user',?,'2100-01-02T03:04:05.123456789Z')`, id, groupID, topicID, content); err != nil {
		t.Fatal(err)
	}
}

// Compare durable authority as well as counts: reads must not acknowledge delivery or release claims.
func memberHistoryToolState(t *testing.T, f *memberToolHTTPFixture) string {
	t.Helper()
	var state bytes.Buffer
	encoder := json.NewEncoder(&state)
	if err := encoder.Encode(f.counts(t)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"deliveries", "turns", "resource_claims", "tasks", "task_attempts", "task_submissions"} {
		rows, err := f.raw.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if err := encoder.Encode(values); err != nil {
				rows.Close()
				t.Fatal(err)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return state.String()
}

func TestMemberHistoryToolHTTPReadAndSearch(t *testing.T) {
	f := newMemberHistoryToolHTTP(t)
	before := memberHistoryToolState(t, f)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, f.newRequest("search_history", `{"query":"  initial  "}`))
	page := decoded[platform.MemberHistorySearchResult](t, response, 200)
	f.safeResponse(t, response.Code, response.Header(), response.Body.Bytes(), true)
	memberToolHTTPKeys(t, response, "items", "next_cursor")
	if len(page.Items) != 1 || page.NextCursor != "" || page.Items[0].Excerpt.Content != "initial question" || !page.Items[0].Excerpt.ContentComplete {
		t.Fatalf("bound search projection: %+v", page)
	}
	response = f.request(t, "read_history", fmt.Sprintf(`{"message_id":%q}`, page.Items[0].MessageID))
	fragment := decoded[platform.MemberHistoryReadResult](t, response, 200)
	memberToolHTTPKeys(t, response, "message_id", "offset", "next_offset", "end", "total_runes", "content", "content_complete")
	if fragment.MessageID != page.Items[0].MessageID || fragment.Content != "initial question" || fragment.Offset != 0 || fragment.NextOffset != 16 || fragment.TotalRunes != 16 || !fragment.End || !fragment.ContentComplete {
		t.Fatalf("bound read projection: %+v", fragment)
	}
	if memberHistoryToolState(t, f) != before {
		t.Fatal("history reads created durable effects")
	}
}
