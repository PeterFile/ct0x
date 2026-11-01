package skills

import "testing"

func TestCatalogOperationsAfterClose(t *testing.T) {
	root := t.TempDir()
	put(t, root, "demo/SKILL.md", skill("demo", "body"))
	c := catalog(t, []Root{{ID: "fixture", Path: root}}, DefaultLimits())
	id := list(t, c).Entries[0].ID
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := c.List(); err == nil || len(got.Entries) != 0 {
		t.Fatalf("closed catalogue listed entries: %+v, %v", got, err)
	}
	if got, err := c.Select(id); err == nil || got != nil {
		t.Fatalf("closed catalogue selected skill: %v", err)
	}
}
