package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func selectSkill(t testing.TB, c *Catalog, id Identity) *Snapshot {
	t.Helper()
	s, err := c.Select(id)
	if err != nil || s == nil {
		t.Fatalf("select: %v", err)
	}
	return s
}

func TestSnapshotIsImmutableAfterSourceChangeAndDeletion(t *testing.T) {
	root := t.TempDir()
	body := "\nPRIVATE_BODY🙂\nKeep suggestions unconfirmed.\n"
	raw := "---\nname: demo\ndescription: test\nmetadata: {label: original}\nallowed-tools: Bash(*)\n---\n" + body
	put(t, root, "demo/SKILL.md", raw)
	put(t, root, "demo/references/data.bin", "\x00\xfforiginal")
	put(t, root, "demo/scripts/never-run.sh", "#!/bin/sh\nexit 99\n")
	c := catalog(t, []Root{{ID: "fixture", Path: root}}, DefaultLimits())
	entry := list(t, c).Entries[0]
	s := selectSkill(t, c, entry.ID)
	if s.Body() != body || !reflect.DeepEqual(s.Entry(), entry) || len(s.Files()) != 3 || len(s.Version()) != 64 {
		t.Fatalf("snapshot does not match selected source: %+v", s.Entry())
	}
	originalVersion := s.Version()
	data, err := s.ReadResource("SKILL.md")
	if err != nil || string(data) != raw {
		t.Fatalf("selected full file changed: %q, %v", data, err)
	}
	data[0] = 'X'
	meta := s.Entry()
	meta.Metadata.Extra["label"], meta.ID.RootID = "changed", "forged"
	files := s.Files()
	files[0].Path, files[0].SHA256 = "forged", "bad"
	resource, err := s.ReadResource("references/data.bin")
	if err != nil || !bytes.Equal(resource, []byte("\x00\xfforiginal")) {
		t.Fatalf("binary resource damaged: %q, %v", resource, err)
	}
	resource[2] = 'X'
	put(t, root, "demo/SKILL.md", skill("demo", "replacement body"))
	put(t, root, "demo/references/data.bin", "replacement resource")
	if got, err := c.Select(entry.ID); got != nil || !errors.Is(err, ErrChanged) {
		t.Fatalf("stale selection accepted: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "demo")); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ReadResource("references/data.bin")
			full, fullErr := s.ReadResource("SKILL.md")
			if fullErr != nil || string(full) != raw || err != nil || string(got) != "\x00\xfforiginal" || s.Body() != body || s.Version() != originalVersion || s.Entry().Metadata.Extra["label"] != "original" || s.Files()[0].Path != "SKILL.md" {
				t.Errorf("snapshot changed after deletion or caller mutation: %q, %v", got, err)
			}
		}()
	}
	wg.Wait()
}

func TestSnapshotVersionIncludesNamesAndResourceBytes(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		put(t, root, "demo/SKILL.md", skill("demo", "same body"))
	}
	put(t, a, "demo/z.txt", "z")
	put(t, a, "demo/a.txt", "a")
	put(t, b, "demo/a.txt", "a")
	put(t, b, "demo/z.txt", "z")
	ca := catalog(t, []Root{{ID: "a", Path: a}}, DefaultLimits())
	cb := catalog(t, []Root{{ID: "b", Path: b}}, DefaultLimits())
	id := list(t, ca).Entries[0].ID
	first := selectSkill(t, ca, id)
	other := selectSkill(t, cb, list(t, cb).Entries[0].ID)
	if first.Version() != other.Version() || !reflect.DeepEqual(first.Files(), other.Files()) {
		t.Fatal("version depends on root identity or creation order")
	}
	files := first.Files()
	if len(files) != 3 {
		t.Fatalf("incomplete manifest: %+v", files)
	}
	for n, name := range []string{"SKILL.md", "a.txt", "z.txt"} {
		data, err := first.ReadResource(name)
		hash := sha256.Sum256(data)
		if err != nil || files[n].Path != name || files[n].Size != int64(len(data)) || files[n].SHA256 != hex.EncodeToString(hash[:]) {
			t.Fatalf("manifest mismatch: %+v, %v", files[n], err)
		}
	}
	put(t, a, "demo/a.txt", "changed")
	if list(t, ca).Entries[0].ID != id {
		t.Fatal("catalogue falsely claims to version supporting files")
	}
	second := selectSkill(t, ca, id)
	if second.Version() == first.Version() {
		t.Fatal("resource byte change not versioned")
	}
	if err := os.Rename(filepath.Join(a, "demo", "a.txt"), filepath.Join(a, "demo", "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	third := selectSkill(t, ca, id)
	if third.Version() == second.Version() {
		t.Fatal("resource name change not versioned")
	}
	put(t, a, "demo/SKILL.md", skill("demo", "new body"))
	latest := selectSkill(t, ca, list(t, ca).Entries[0].ID)
	if latest.Body() != "new body" || latest.Version() == third.Version() || first.Body() != "same body" {
		t.Fatal("new activation did not fix a separate version")
	}
}

func TestSelectionAndResourcePathsAreExplicit(t *testing.T) {
	root := t.TempDir()
	put(t, root, "demo/SKILL.md", skill("demo", "body"))
	c := catalog(t, []Root{{ID: "fixture", Path: root}}, DefaultLimits())
	id := list(t, c).Entries[0].ID
	for _, edit := range []func(*Identity){
		func(i *Identity) { i.Name = "../demo" }, func(i *Identity) { i.Name = "/demo" },
		func(i *Identity) { i.RootID = "unbound" }, func(i *Identity) { i.SkillSHA256 = "header-only-or-forged" },
	} {
		bad := id
		edit(&bad)
		if got, err := c.Select(bad); got != nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe selection accepted: %+v, %v", bad, err)
		}
	}
	s := selectSkill(t, c, id)
	for _, name := range []string{"", ".", "/etc/passwd", "../outside", "a/../SKILL.md", "a//b", "C:/outside", `a\b`, `\\server\share`, "a\x00b", "missing"} {
		if data, err := s.ReadResource(name); data != nil || err == nil {
			t.Fatalf("unsafe or missing resource accepted: %q", name)
		}
	}
	badRaw := "---\nname: wrong\ndescription: test\n---\nbody"
	put(t, root, "demo/SKILL.md", badRaw)
	hash := sha256.Sum256([]byte(badRaw))
	id.SkillSHA256 = hex.EncodeToString(hash[:])
	if got, err := c.Select(id); got != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("selected metadata was not revalidated: %v", err)
	}
}

func TestSnapshotLimitsFailWithoutPartialSnapshot(t *testing.T) {
	root := t.TempDir()
	raw := skill("demo", "body")
	put(t, root, "demo/SKILL.md", raw)
	put(t, root, "demo/assets/large.bin", strings.Repeat("x", 1000))
	put(t, root, "demo/other.txt", "abc")
	cases := []struct {
		name string
		edit func(*Limits)
	}{
		{"files", func(l *Limits) { l.MaxFiles = 2 }},
		{"directories", func(l *Limits) { l.MaxDirs = 1 }},
		{"directory entries", func(l *Limits) { l.MaxEntries = 2 }},
		{"file bytes", func(l *Limits) { l.MaxFileBytes = 128; l.MaxHeaderBytes = 64 }},
		{"total bytes", func(l *Limits) { l.MaxTotalBytes = int64(len(raw) + 1002) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := DefaultLimits()
			tc.edit(&limits)
			c := catalog(t, []Root{{ID: "test", Path: root}}, limits)
			id := list(t, c).Entries[0].ID
			got, err := c.Select(id)
			if got != nil || !errors.Is(err, ErrLimit) {
				t.Fatalf("partial or oversized snapshot returned: %v", err)
			}
		})
	}
	limits := DefaultLimits()
	limits.MaxFiles, limits.MaxDirs, limits.MaxEntries = 3, 2, 3
	limits.MaxFileBytes, limits.MaxHeaderBytes, limits.MaxTotalBytes = 1000, int64(len(raw)), int64(len(raw)+1003)
	c := catalog(t, []Root{{ID: "exact", Path: root}}, limits)
	if s := selectSkill(t, c, list(t, c).Entries[0].ID); len(s.Files()) != 3 {
		t.Fatal("exact bounds rejected")
	}
}

func BenchmarkDiscoveryAndSnapshot32x8(b *testing.B) {
	root := b.TempDir()
	for n := 0; n < 32; n++ {
		name := fmt.Sprintf("skill-%02d", n)
		put(b, root, name+"/SKILL.md", skill(name, strings.Repeat("Synthetic private instructions.\n", 128)))
		for resource := 0; resource < 8; resource++ {
			put(b, root, fmt.Sprintf("%s/references/%02d.txt", name, resource), strings.Repeat("Synthetic reference data.\n", 160))
		}
	}
	c := catalog(b, []Root{{ID: "fixtures", Path: root}}, DefaultLimits())
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		entries, err := c.List()
		if err != nil || len(entries.Entries) != 32 {
			b.Fatalf("discovery: %v", err)
		}
		s, err := c.Select(entries.Entries[n%32].ID)
		if err != nil || s == nil || len(s.Files()) != 9 {
			b.Fatalf("snapshot: %v", err)
		}
	}
}
