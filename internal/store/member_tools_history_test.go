package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

func memberHistoryMessage(t *testing.T, f messageFixture, scope MemberToolScope, id, content string) domain.Message {
	t.Helper()
	m := domain.Message{ID: id, GroupID: scope.Claim.GroupID, TopicID: scope.TopicID, SenderID: "user", Content: content,
		CreatedAt: time.Date(2025, 1, 2, 3, 4, 5, 123456789, time.UTC), Deliveries: []domain.Delivery{}}
	if _, err := f.s.db.Exec("INSERT INTO messages("+messageColumns+") VALUES(?,?,?,?,?,?)",
		m.ID, m.GroupID, m.TopicID, m.SenderID, m.Content, m.CreatedAt.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	return m
}

func memberHistoryJSON(t *testing.T, value any) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.NewEncoder(&out).Encode(value); err != nil {
		t.Fatal(err)
	}
	if out.Len() > 65536 {
		t.Fatalf("whole encoded result exceeds budget: %d", out.Len())
	}
	return out.Bytes()
}

func memberHistoryKeys(t *testing.T, data []byte, want ...string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var got []string
	for key := range fields {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected DTO fields: got %v, want %v", got, want)
	}
	return fields
}

func TestMemberHistorySearchLiteralPagingAndCursorReuse(t *testing.T) {
	f, scope := memberOperationFixture(t)
	const query = `\%_你好`
	var want []domain.Message
	for i := range 53 {
		m := memberHistoryMessage(t, f, scope, fmt.Sprintf("history-%03d", i), " exact "+query+" e\u0301😀<>&\x01 ")
		want = append(want, m)
	}
	for i := range 60 {
		for kind := range 3 {
			foreign := scope
			switch kind {
			case 0:
				foreign.Claim.GroupID = f.otherID
			case 1:
				foreign.TopicID = "foreign-topic"
			}
			content := query
			if kind == 2 {
				content = "nonmatching noise"
			}
			m := memberHistoryMessage(t, f, foreign, fmt.Sprintf("noise-%d-%03d", kind, i), content)
			// Invalid foreign timestamps must not reach the shared scanner, even before LIMIT.
			stamp := "invalid"
			if kind == 2 {
				stamp = m.CreatedAt.Add(time.Hour).Format(timestampLayout)
			}
			if _, err := f.s.db.Exec("UPDATE messages SET created_at=? WHERE id=?", stamp, m.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i].ID > want[j].ID })
	before := memberOperationCounts(t, f.s)
	for _, requested := range []int{0, 1, 50} {
		input := MemberHistorySearchInput{Query: " \u3000" + query + "\n", Limit: requested}
		limit := requested
		if limit == 0 {
			limit = 20
		}
		for offset := 0; ; {
			page, err := f.s.SearchHistoryForTool(ctx, scope, input)
			end := min(offset+limit, len(want))
			if err != nil || len(page.Items) != end-offset {
				t.Fatalf("limit=%d offset=%d: %d items, %v", requested, offset, len(page.Items), err)
			}
			for i, hit := range page.Items {
				m := want[offset+i]
				if hit.MessageID != m.ID || hit.SenderID != m.SenderID || !hit.CreatedAt.Equal(m.CreatedAt) || hit.Excerpt.Content != m.Content || hit.Excerpt.Offset != 0 || hit.Excerpt.NextOffset != utf8.RuneCountInString(m.Content) || hit.Excerpt.TotalRunes != hit.Excerpt.NextOffset || !hit.Excerpt.End || !hit.Excerpt.ContentComplete {
					t.Fatalf("wrong projected hit: %+v", hit)
				}
			}
			memberHistoryJSON(t, page)
			if end == len(want) {
				if page.NextCursor != "" {
					t.Fatal("exact final page claims more")
				}
				break
			}
			if page.NextCursor == "" || page.NextCursor == input.Before {
				t.Fatal("missing or stationary cursor")
			}
			input.Before, offset = page.NextCursor, end
		}
	}
	first, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: query, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	local, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: query, TopicID: &scope.TopicID, Limit: 1})
	if err != nil || first.NextCursor != local.NextCursor {
		t.Fatalf("member and local search do not share cursor semantics: %v", err)
	}
	continued, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "\t" + query, Limit: 50, Before: local.NextCursor})
	if err != nil || len(continued.Items) != 50 || continued.Items[0].MessageID != want[1].ID {
		t.Fatalf("local-to-member scoped cursor reuse failed: %v", err)
	}
	local, err = f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: query, TopicID: &scope.TopicID, Before: first.NextCursor, Limit: 50})
	if err != nil || local.Items[0].ID != want[1].ID {
		t.Fatalf("member-to-local cursor reuse failed: %v", err)
	}
	for _, q := range []HistoryQuery{{Query: query}, {Query: query, TopicID: new(string)}, {Query: "你好", TopicID: &scope.TopicID}} {
		q.Before = first.NextCursor
		if _, err := f.s.SearchHistory(ctx, f.groupID, q); !errors.Is(err, ErrInvalid) {
			t.Fatalf("member cursor escaped its exact scope: %v", err)
		}
	}
	if _, err := f.s.SearchHistory(ctx, f.otherID, HistoryQuery{Query: query, TopicID: &scope.TopicID, Before: first.NextCursor}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cursor escaped group: %v", err)
	}
	fields := memberHistoryKeys(t, memberHistoryJSON(t, first), "items", "next_cursor")
	var hits []json.RawMessage
	if err := json.Unmarshal(fields["items"], &hits); err != nil {
		t.Fatal(err)
	}
	hit := memberHistoryKeys(t, hits[0], "message_id", "sender_id", "created_at", "excerpt")
	memberHistoryKeys(t, hit["excerpt"], "offset", "next_offset", "end", "total_runes", "content", "content_complete")
	empty, err := f.s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "absent"})
	if err != nil || empty.Items == nil || string(bytes.TrimSpace(memberHistoryJSON(t, empty))) != `{"items":[],"next_cursor":""}` {
		t.Fatalf("empty search is not an explicit empty page: %+v, %v", empty, err)
	}
	if memberOperationCounts(t, f.s) != before {
		t.Fatal("history search created durable effects")
	}
}

func TestMemberHistoryReadRuneReconstructionAndPrefixReopen(t *testing.T) {
	f, scope := memberOperationFixture(t)
	body := " \t" + strings.Repeat("中😀e\u0301<>&\x00\x01\n\\\"", 4096) + "MATCH-LATER \n"
	if len(body) <= 65536 {
		t.Fatal("fixture must exceed one tool response")
	}
	m := memberHistoryMessage(t, f, scope, "unicode-evidence", body)
	before := memberOperationCounts(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, f.path, false)
	page, err := s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "中😀"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("reopened search failed: %v", err)
	}
	runes := []rune(body)
	excerpt := page.Items[0].Excerpt
	if excerpt.Content != string(runes[:256]) || excerpt.Offset != 0 || excerpt.NextOffset != 256 || excerpt.End || excerpt.ContentComplete || excerpt.TotalRunes != len(runes) || strings.Contains(excerpt.Content, "MATCH-LATER") {
		t.Fatalf("excerpt is not an exact, explicit partial prefix: %+v", excerpt)
	}
	// SQLite LIKE does not search past stored NUL; a separate long message proves late-match prefix semantics.
	late := memberHistoryMessage(t, messageFixture{s: s}, scope, "late-match", strings.Repeat("界😀", 500)+"MATCH-LATER")
	latePage, err := s.SearchHistoryForTool(ctx, scope, MemberHistorySearchInput{Query: "MATCH-LATER"})
	if err != nil || len(latePage.Items) != 1 || latePage.Items[0].MessageID != late.ID || latePage.Items[0].Excerpt.Content != string([]rune(late.Content)[:256]) || latePage.Items[0].Excerpt.ContentComplete {
		t.Fatalf("late match must not imply a match-centered excerpt: %+v, %v", latePage, err)
	}
	var reconstructed strings.Builder
	for offset := 0; offset < len(runes); {
		fragment, err := s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID, Offset: offset, Length: 4096})
		next := min(offset+4096, len(runes))
		if err != nil || fragment.MessageID != m.ID || fragment.Offset != offset || fragment.NextOffset != next || fragment.TotalRunes != len(runes) || fragment.Content != string(runes[offset:next]) || fragment.End != (next == len(runes)) || fragment.ContentComplete {
			t.Fatalf("incorrect rune range at %d: %+v, %v", offset, fragment, err)
		}
		encoded := memberHistoryJSON(t, fragment)
		memberHistoryKeys(t, encoded, "message_id", "offset", "next_offset", "end", "total_runes", "content", "content_complete")
		var decoded MemberHistoryReadResult
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Content != fragment.Content {
			t.Fatalf("JSON did not preserve exact content: %v", err)
		}
		reconstructed.WriteString(fragment.Content)
		offset = fragment.NextOffset
	}
	if reconstructed.String() != body {
		t.Fatal("rune fragments corrupted stored evidence")
	}
	for _, topic := range []*string{nil, &scope.TopicID} {
		full, err := s.ReadHistory(ctx, f.groupID, topic, m.ID)
		if err != nil || full.Content != body || full.GroupID != f.groupID || full.Deliveries == nil {
			t.Fatalf("local full-body History changed: %v", err)
		}
	}
	before[1]++ // Only the explicit late-match fixture was added after the snapshot.
	if memberOperationCounts(t, s) != before {
		t.Fatal("history fragments created messages, deliveries or turns")
	}
	queueCounts(t, s, 1, 1, 1)
}

func TestMemberHistoryReadRangesValidationAndIsolation(t *testing.T) {
	f, scope := memberOperationFixture(t)
	body := strings.Repeat("中😀e\u0301", 1100)
	m := memberHistoryMessage(t, f, scope, "range-evidence", body)
	runes := []rune(body)
	for _, tc := range []struct{ offset, length, next int }{
		{0, 0, 1024}, {0, 4096, 4096}, {2, 1, 3}, {len(runes) - 2, 4, len(runes)}, {len(runes), 0, len(runes)},
	} {
		got, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID, Offset: tc.offset, Length: tc.length})
		if err != nil || got.Offset != tc.offset || got.NextOffset != tc.next || got.TotalRunes != len(runes) || got.Content != string(runes[tc.offset:tc.next]) || got.End != (tc.next == len(runes)) || got.ContentComplete != (tc.offset == 0 && tc.next == len(runes)) {
			t.Fatalf("range %+v: %+v, %v", tc, got, err)
		}
	}
	for _, body := range []string{"", " \t中😀e\u0301\x00<>&\n "} {
		m := memberHistoryMessage(t, f, scope, fmt.Sprintf("short-%d", len(body)), body)
		got, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID})
		if err != nil || got.Content != body || !got.End || !got.ContentComplete || got.TotalRunes != utf8.RuneCountInString(body) || got.NextOffset != got.TotalRunes {
			t.Fatalf("complete short/empty content changed: %+v, %v", got, err)
		}
	}
	for _, input := range []MemberHistoryReadInput{
		{MessageID: m.ID, Offset: -1}, {MessageID: m.ID, Offset: len(runes) + 1}, {MessageID: m.ID, Length: -1}, {MessageID: m.ID, Length: 4097},
	} {
		if got, err := f.s.ReadHistoryForTool(ctx, scope, input); !errors.Is(err, ErrInvalid) || !reflect.ValueOf(got).IsZero() {
			t.Fatalf("invalid range returned partial evidence: %+v, %v", got, err)
		}
	}
	for _, id := range []string{"", " \n", "a\x00b", "\xff", strings.Repeat("a", 257)} {
		if got, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: id}); !errors.Is(err, ErrInvalid) || got.MessageID != "" {
			t.Fatalf("invalid reference ID accepted: %v", err)
		}
	}
	id := " " + strings.Repeat("x", 254) + " " // Cursor ID validation permits exact, nonblank padded IDs.
	memberHistoryMessage(t, f, scope, id, "boundary")
	if got, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: id}); err != nil || got.MessageID != id {
		t.Fatalf("valid exact 256-byte reference changed: %v", err)
	}
	foreignTopic, foreignGroup := scope, scope
	foreignTopic.TopicID, foreignGroup.Claim.GroupID = "foreign", f.otherID
	for i, foreign := range []MemberToolScope{foreignTopic, foreignGroup} {
		m := memberHistoryMessage(t, f, foreign, fmt.Sprintf("private-%d", i), "PRIVATE")
		if _, err := f.s.db.Exec("UPDATE messages SET created_at='invalid' WHERE id=?", m.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := f.s.ReadHistoryForTool(ctx, scope, MemberHistoryReadInput{MessageID: m.ID}); !errors.Is(err, ErrNotFound) || !reflect.ValueOf(got).IsZero() {
			t.Fatalf("foreign reference reached the scanner: %+v, %v", got, err)
		}
	}
	for _, input := range []MemberHistorySearchInput{{Query: ""}, {Query: "x\x00y"}, {Query: "\xff"}, {Query: strings.Repeat("x", 257)}, {Query: "x", Limit: -1}, {Query: "x", Limit: 51}, {Query: "x", Before: "not-a-cursor"}} {
		if got, err := f.s.SearchHistoryForTool(ctx, scope, input); !errors.Is(err, ErrInvalid) || len(got.Items) != 0 || got.NextCursor != "" {
			t.Fatalf("invalid search returned a page: %+v, %v", got, err)
		}
	}
}
