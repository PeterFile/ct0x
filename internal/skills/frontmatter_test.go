package skills

import (
	"errors"
	"strings"
	"testing"
)

func TestFrontmatterDelimiterBoundaries(t *testing.T) {
	fields := "name: demo\ndescription: fixture\n"
	header := "---\n" + fields + "---\n"
	crlf := strings.ReplaceAll(header, "\n", "\r\n")
	cases := []struct {
		name, raw string
		limit     int64
		wantErr   error
		body      string
	}{
		{"missing initial", fields + "---\nbody", 1024, ErrInvalid, ""},
		{"missing terminal", "---\n" + fields, 1024, ErrInvalid, ""},
		{"terminal at EOF", strings.TrimSuffix(header, "\n"), int64(len(header) - 1), nil, ""},
		{"exact header limit", header + "body\n---\n", int64(len(header)), nil, "body\n---\n"},
		{"terminal newline exceeds limit", header + "body", int64(len(header) - 1), ErrLimit, ""},
		{"CRLF byte boundary", crlf + "正文🙂", int64(len(crlf)), nil, "正文🙂"},
		{"unterminated long line", "---\n" + strings.Repeat("x", 1024), 32, ErrLimit, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, offset, err := splitHeader([]byte(tc.raw), tc.limit)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("delimiter boundary: %v, want %v", err, tc.wantErr)
			}
			if err == nil && (strings.ReplaceAll(string(got), "\r\n", "\n") != fields || tc.raw[offset:] != tc.body) {
				t.Fatalf("header or body changed: %q, offset %d", got, offset)
			}
		})
	}
}

func BenchmarkSelectLargeBodyShortHeader(b *testing.B) {
	root := b.TempDir()
	header := skill("demo", "")
	body := strings.Repeat("\n", (2<<20)-len(header))
	put(b, root, "demo/SKILL.md", header+body)
	c := catalog(b, []Root{{ID: "fixture", Path: root}}, DefaultLimits())
	id := list(b, c).Entries[0].ID
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		s, err := c.Select(id)
		if err != nil || s == nil || len(s.Body()) != len(body) {
			b.Fatalf("large-body selection: %v", err)
		}
	}
}
