package skills

import (
	"errors"
	"runtime"
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

func largeBodyShortHeaderFixture(t testing.TB) (*Catalog, Identity, int) {
	root := t.TempDir()
	header := skill("demo", "")
	body := strings.Repeat("\n", (2<<20)-len(header))
	put(t, root, "demo/SKILL.md", header+body)
	c := catalog(t, []Root{{ID: "fixture", Path: root}}, DefaultLimits())
	return c, list(t, c).Entries[0].ID, len(body)
}

func TestSelectLargeBodyAllocationBudget(t *testing.T) {
	c, id, bodyBytes := largeBodyShortHeaderFixture(t)
	const runs = 5
	// TotalAlloc is process-wide: do not use t.Parallel or concurrent Selects.
	// Setup, explicit GC and result checks stay outside the measured intervals.
	// This generous ceiling gives >2x headroom over the optimized ~15 MB while
	// rejecting the legacy ~65 MB whole-body split, without a wall-clock gate.
	const ceiling = 32 * 1024 * 1024
	var allocated uint64
	for n := 0; n < runs; n++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		s, err := c.Select(id)
		runtime.ReadMemStats(&after)
		if err != nil || s == nil || len(s.Body()) != bodyBytes {
			t.Fatalf("large-body selection: %v", err)
		}
		allocated += after.TotalAlloc - before.TotalAlloc
	}
	average := allocated / runs
	t.Logf("Select: %d bytes/op over %d sequential runs; ceiling %d", average, runs, ceiling)
	if average > ceiling {
		t.Fatalf("Select allocation regression: %d bytes/op exceeds %d-byte ceiling", average, ceiling)
	}
}

func BenchmarkSelectLargeBodyShortHeader(b *testing.B) {
	c, id, bodyBytes := largeBodyShortHeaderFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		s, err := c.Select(id)
		if err != nil || s == nil || len(s.Body()) != bodyBytes {
			b.Fatalf("large-body selection: %v", err)
		}
	}
}
