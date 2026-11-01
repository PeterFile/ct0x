package store

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestHistoryQueryValidation(t *testing.T) {
	f := setupMessages(t)
	for i, query := range []string{
		"", " \t\n\u3000", "\xff", "x\x00y", strings.Repeat("a", 257), strings.Repeat("界", 257),
		strings.Repeat("😀", 257), strings.Repeat(" ", 4096) + "x", strings.Repeat(" ", 65536) + "x",
	} {
		t.Run(fmt.Sprintf("invalid-query-%d", i), func(t *testing.T) {
			if _, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: query}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid query (%d bytes): %v", len(query), err)
			}
		})
	}
	for _, query := range []string{"x", " \u3000x\n", strings.Repeat("a", 256), strings.Repeat("界", 256), strings.Repeat("😀", 256), strings.Repeat(" ", 4095) + "x"} {
		if page, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: query}); err != nil || page.Items == nil {
			t.Fatalf("valid query boundary (%d bytes): %+v, %v", len(query), page, err)
		}
	}
	for _, limit := range []int{-1, 201} {
		if _, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: "x", Limit: limit}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid limit %d: %v", limit, err)
		}
	}
	for _, topic := range []string{"\xff", "a\x00b", strings.Repeat("a", 257), strings.Repeat("😀", 65)} {
		if _, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: "x", TopicID: &topic}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid search topic (%d bytes): %v", len(topic), err)
		}
		if _, err := f.s.ReadHistory(ctx, f.groupID, &topic, "missing"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid read topic (%d bytes): %v", len(topic), err)
		}
	}
	for _, topic := range []string{"", " ", strings.Repeat("a", 256), strings.Repeat("😀", 64)} {
		if _, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: "x", TopicID: &topic}); err != nil {
			t.Fatalf("valid topic boundary (%d bytes): %v", len(topic), err)
		}
		if _, err := f.s.ReadHistory(ctx, f.groupID, &topic, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("valid read topic boundary (%d bytes): %v", len(topic), err)
		}
	}
}

func TestHistoryCursorScopeAndCanonicalValidation(t *testing.T) {
	f := setupMessages(t)
	for _, topic := range []string{"", "", "other"} {
		if _, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, ActorID: "user", TopicID: topic, Content: "private-keyword needle"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	q := HistoryQuery{Query: "private-keyword needle", Limit: 1}
	first, err := f.s.SearchHistory(ctx, f.groupID, q)
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil || len(first.NextCursor) > 2048 || strings.Contains(string(raw), q.Query) || strings.Contains(string(raw), "private-keyword") {
		t.Fatalf("unbounded or keyword-bearing cursor: %s, %v", raw, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	var digest string
	if err := json.Unmarshal(fields["scope"], &digest); err != nil {
		t.Fatal(err)
	}
	if sum, err := hex.DecodeString(digest); err != nil || len(sum) != 32 {
		t.Fatalf("scope is not a SHA256 digest: %q, %v", digest, err)
	}
	// Normalization and changing page size preserve scope; SQLite case folding does not.
	q.Query, q.Before, q.Limit = " \nprivate-keyword needle\u3000", first.NextCursor, 200
	next, err := f.s.SearchHistory(ctx, f.groupID, q)
	if err != nil || len(next.Items) != 2 || next.Items[0].ID == first.Items[0].ID || next.NextCursor != "" {
		t.Fatalf("normalized scope or variable page size: %+v, %v", next, err)
	}
	empty, other := "", "other"
	for _, scope := range []struct {
		group, query string
		topic        *string
	}{
		{f.otherID, q.Query, nil}, {"missing", q.Query, nil},
		{f.groupID, "PRIVATE-KEYWORD NEEDLE", nil}, {f.groupID, "needle", nil},
		{f.groupID, q.Query, &empty}, {f.groupID, q.Query, &other},
	} {
		if _, err := f.s.SearchHistory(ctx, scope.group, HistoryQuery{Query: scope.query, TopicID: scope.topic, Before: first.NextCursor}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("cursor escaped scope %+v: %v", scope, err)
		}
	}
	exact, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: q.Query, TopicID: &empty, Limit: 1})
	if err != nil || exact.NextCursor == "" {
		t.Fatalf("exact empty-topic first page: %+v, %v", exact, err)
	}
	for _, topic := range []*string{nil, &other} {
		if _, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: q.Query, TopicID: topic, Before: exact.NextCursor}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("exact empty-topic cursor escaped: %v", err)
		}
	}
	continued, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: q.Query, TopicID: &empty, Before: exact.NextCursor})
	if err != nil || len(continued.Items) != 1 || continued.Items[0].TopicID != "" || continued.NextCursor != "" {
		t.Fatalf("exact empty-topic continuation: %+v, %v", continued, err)
	}
	encode := func(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }
	// Edit the real cursor in place: field mutations retain canonical key order.
	field := func(key, value string) string {
		t.Helper()
		old := `"` + key + `":` + string(fields[key])
		if strings.Count(string(raw), old) != 1 {
			t.Fatalf("missing unique cursor field %s", key)
		}
		return encode(strings.Replace(string(raw), old, `"`+key+`":`+value, 1))
	}
	invalid := map[string]string{
		"bad-base64": "%%%", "padded": first.NextCursor + "=", "newline": first.NextCursor + "\n",
		"truncated": first.NextCursor[:len(first.NextCursor)-1], "oversized": strings.Repeat("A", 2049),
		"null": encode("null"), "array": encode("[]"), "empty-object": encode("{}"),
		"whitespace": encode(" " + string(raw)), "trailing-json": encode(string(raw) + "{}"),
		"duplicate-key":   encode(`{"v":1,` + string(raw[1:])),
		"unknown-field":   encode(strings.TrimSuffix(string(raw), "}") + `,"extra":0}`),
		"escaped-key":     encode(strings.Replace(string(raw), `"id":`, `"\u0069d":`, 1)),
		"unknown-version": field("v", "2"), "zero-version": field("v", "0"), "fractional-version": field("v", "1.0"),
		"tampered-scope": field("scope", `"`+strings.Repeat("0", 64)+`"`),
		"empty-id":       field("id", `""`), "blank-id": field("id", `" "`), "nul-id": field("id", `"\u0000"`),
		"oversized-id": field("id", `"`+strings.Repeat("a", 257)+`"`), "invalid-utf8-id": field("id", "\"\xff\""),
	}
	for _, stamp := range []string{
		"invalid", "2025-01-02T03:04:05Z", "2025-01-02T03:04:05.1Z", "2025-01-02T03:04:05.0000000000Z",
		"2025-01-02T03:04:05.000000000+00:00", "0000-01-01T00:00:00.000000000Z", "0001-01-01T00:00:00.000000000Z",
		"10000-01-01T00:00:00.000000000Z", "2025-02-30T00:00:00.000000000Z", "2025-01-02T03:04:60.000000000Z",
	} {
		value, err := json.Marshal(stamp)
		if err != nil {
			t.Fatal(err)
		}
		invalid["timestamp-"+stamp] = field("created_at", string(value))
	}
	for name, before := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := f.s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: q.Query, Before: before}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid cursor was accepted: %v", err)
			}
		})
	}
}
