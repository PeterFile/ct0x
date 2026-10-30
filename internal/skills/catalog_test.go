package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t testing.TB, root, relative, content string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func skill(name, body string) string {
	return "---\nname: " + name + "\ndescription: Synthetic fixture\n---\n" + body
}

func catalog(t testing.TB, roots []Root, limits Limits) *Catalog {
	t.Helper()
	c, err := Open(roots, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func list(t testing.TB, c *Catalog) Listing {
	t.Helper()
	got, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCatalogYAMLAndProgressiveDisclosure(t *testing.T) {
	root := t.TempDir()
	raw := "---\nname: \"demo\" # comment\ndescription: |-\n  First line\n  Second: # literal\ncompatibility: >-\n  Local Go\n  toolchain\nlicense: 'MIT # literal'\nmetadata:\n  author: \"a: # b\"\n  version: '1.0'\nallowed-tools: \"Bash(*) Read\"\n---\nPRIVATE_BODY_DO_NOT_DISCLOSE" + strings.Repeat("\nSynthetic body", 4096)
	put(t, root, "demo/SKILL.md", raw)
	c := catalog(t, []Root{{ID: "local", Path: root}}, DefaultLimits())
	got := list(t, c)
	if len(got.Problems) != 0 || len(got.Entries) != 1 {
		t.Fatalf("unexpected catalogue: %+v", got)
	}
	e := got.Entries[0]
	m := e.Metadata
	if m.Name != "demo" || m.Description != "First line\nSecond: # literal" || m.Compatibility != "Local Go toolchain" || m.License != "MIT # literal" || m.Extra["author"] != "a: # b" || m.Extra["version"] != "1.0" || m.AllowedTools != "Bash(*) Read" {
		t.Fatalf("YAML semantics lost: %+v", m)
	}
	hash := sha256.Sum256([]byte(raw))
	if e.ID.RootID != "local" || e.ID.Name != "demo" || e.ID.SkillSHA256 != hex.EncodeToString(hash[:]) || e.SourcePath != filepath.Join(root, "demo", "SKILL.md") {
		t.Fatalf("incorrect provenance: %+v", e)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "PRIVATE_BODY") {
		t.Fatal("listing exposed body")
	}
	e.Metadata.Extra["author"] = "mutated"
	got.Entries[0].ID.RootID = "forged"
	if fresh := list(t, c).Entries[0]; fresh.Metadata.Extra["author"] != "a: # b" || fresh.ID.RootID != "local" {
		t.Fatal("caller mutation changed catalogue")
	}
	put(t, root, "demo/SKILL.md", raw+" changed body")
	if list(t, c).Entries[0].ID.SkillSHA256 == e.ID.SkillSHA256 {
		t.Fatal("whole-file version ignored body change")
	}
}

func TestSameNamesRemainDistinctAndOrdered(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	put(t, a, "demo/SKILL.md", skill("demo", "A"))
	put(t, b, "demo/SKILL.md", skill("demo", "B"))
	c := catalog(t, []Root{{ID: "z", Path: b}, {ID: "a", Path: a}}, DefaultLimits())
	got := list(t, c)
	if len(got.Entries) != 2 || got.Entries[0].ID.RootID != "a" || got.Entries[1].ID.RootID != "z" || got.Entries[0].ID == got.Entries[1].ID {
		t.Fatalf("same-name source hidden: %+v", got)
	}
}

func TestMalformedFrontmatterIsNotPartiallyValid(t *testing.T) {
	cases := []string{
		"description: missing name", "name: demo", "name: demo\ndescription: ''", "name: demo\ndescription: 42",
		"name: demo\nname: demo\ndescription: ok", "name: demo\ndescription: ok\n7: value",
		"name: demo\ndescription: [bad]", "name: demo\ndescription: ok\nmetadata: []",
		"name: demo\ndescription: ok\nmetadata: {version: 1.0}", "name: demo\ndescription: ok\nmetadata: {x: a, x: b}",
		"name: demo\ndescription: ok\nmetadata: {true: text}", "name: demo\ndescription: ok\nallowed-tools: [Read]",
		"name: demo\ndescription: ok\ncompatibility: null", "name: demo\ndescription: ok\nlicense: 123",
		"name: demo\ndescription: &x text\nlicense: *x", "name: demo\ndescription: ok\nunknown: value",
		"name: demo\ndescription: ok\n...\nname: another", "[unclosed", "[]", "name: other\ndescription: mismatch",
		"name: demo\ndescription: " + strings.Repeat("中", 1025),
		"name: demo\ndescription: ok\ncompatibility: " + strings.Repeat("x", 501),
	}
	for n, header := range cases {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "demo/SKILL.md", "---\n"+header+"\n---\nprivate")
			got := list(t, catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits()))
			if len(got.Entries) != 0 || len(got.Problems) != 1 || !errors.Is(got.Problems[0].Err, ErrInvalid) {
				t.Fatalf("malformed entry accepted: %+v", got)
			}
		})
	}
	for _, name := range []string{"UPPER", "-demo", "demo-", "de--mo", strings.Repeat("a", 65), "中文"} {
		root := t.TempDir()
		put(t, root, name+"/SKILL.md", skill(name, "body"))
		got := list(t, catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits()))
		if len(got.Entries) != 0 || len(got.Problems) != 1 {
			t.Fatalf("invalid name accepted: %q", name)
		}
	}
	root := t.TempDir()
	name := strings.Repeat("a", 64)
	put(t, root, name+"/SKILL.md", "---\r\nname: "+name+"\r\ndescription: "+strings.Repeat("中", 1024)+"\r\ncompatibility: "+strings.Repeat("x", 500)+"\r\n---\r\nbody")
	if got := list(t, catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits())); len(got.Entries) != 1 || len(got.Problems) != 0 {
		t.Fatalf("valid character boundaries rejected: %+v", got)
	}
}

func TestCatalogBoundsAndRootValidation(t *testing.T) {
	root := t.TempDir()
	raw := skill("demo", strings.Repeat("body", 128))
	put(t, root, "demo/SKILL.md", raw)
	put(t, root, "second/SKILL.md", skill("second", "body"))
	cases := []struct {
		name string
		edit func(*Limits)
	}{
		{"directory entries", func(l *Limits) { l.MaxEntries = 1 }},
		{"skill count", func(l *Limits) { l.MaxSkills = 1 }},
		{"header bytes", func(l *Limits) { l.MaxHeaderBytes = 10 }},
		{"file bytes", func(l *Limits) { l.MaxFileBytes = int64(len(raw) - 1); l.MaxHeaderBytes = 64 }},
		{"aggregate bytes", func(l *Limits) { l.MaxTotalBytes = int64(len(raw) - 1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := DefaultLimits()
			tc.edit(&limits)
			got, err := catalog(t, []Root{{ID: "test", Path: root}}, limits).List()
			if !errors.Is(err, ErrLimit) || len(got.Entries) != 0 {
				t.Fatalf("limit silently truncated catalogue: %+v, %v", got, err)
			}
		})
	}
	for _, roots := range [][]Root{{{ID: "", Path: root}}, {{ID: "x", Path: root}, {ID: "x", Path: root}}, {{ID: "x", Path: filepath.Join(root, "absent")}}} {
		if c, err := Open(roots, DefaultLimits()); err == nil {
			_ = c.Close()
			t.Fatal("invalid root accepted")
		}
	}
	limits := DefaultLimits()
	limits.MaxRoots = 1
	if _, err := Open([]Root{{ID: "a", Path: root}, {ID: "b", Path: root}}, limits); !errors.Is(err, ErrLimit) {
		t.Fatalf("root count unbounded: %v", err)
	}
	if _, err := Open([]Root{{ID: "a", Path: root}}, Limits{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid limits accepted: %v", err)
	}
}

func TestCatalogContainmentAndAnchoredRoot(t *testing.T) {
	outside := t.TempDir()
	put(t, outside, "SKILL.md", skill("demo", "OUTSIDE_SECRET"))
	for _, target := range []string{filepath.Join(outside, "SKILL.md"), "../../outside", "SKILL.md"} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "demo"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "demo", "SKILL.md")); err != nil {
			t.Fatal(err)
		}
		got := list(t, catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits()))
		if len(got.Entries) != 0 || len(got.Problems) != 1 {
			t.Fatalf("unsafe symlink accepted: %+v", got)
		}
	}
	root := t.TempDir()
	put(t, root, "demo/real.md", skill("demo", "ORIGINAL"))
	if err := os.Symlink("real.md", filepath.Join(root, "demo", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	c := catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits())
	before := list(t, c)
	if len(before.Entries) != 1 {
		t.Fatalf("contained symlink rejected: %+v", before)
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	after := list(t, c)
	if len(after.Entries) != 1 || after.Entries[0].ID != before.Entries[0].ID {
		t.Fatalf("root replacement escaped anchored directory: %+v", after)
	}
}

func BenchmarkCatalog32(b *testing.B) {
	root := b.TempDir()
	for n := 0; n < 32; n++ {
		name := fmt.Sprintf("skill-%02d", n)
		put(b, root, name+"/SKILL.md", skill(name, strings.Repeat("Synthetic private instructions.\n", 128)))
	}
	c := catalog(b, []Root{{ID: "fixtures", Path: root}}, DefaultLimits())
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		got, err := c.List()
		if err != nil || len(got.Entries) != 32 {
			b.Fatalf("bounded discovery: %v", err)
		}
	}
}
