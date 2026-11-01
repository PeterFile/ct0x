package store

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestArtifactListArtifactTuplePages(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	artifactForeignKeys(t, s.db)
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := make([]domain.Artifact, 0, 205)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 205 {
		n := (i * 73) % 205
		a, v := artifactValues(g.ID, fmt.Sprintf("artifact-%03d", n), fmt.Sprintf("initial-%03d", n), "user")
		a.CreatedAt = base.Add(time.Duration(n%5) * time.Nanosecond)
		v.CreatedAt = a.CreatedAt
		if err := insertArtifactRow(tx, a); err != nil {
			t.Fatal(err)
		}
		if err := insertArtifactVersionRow(tx, v); err != nil {
			t.Fatal(err)
		}
		want = append(want, a)
	}
	foreign, v := artifactValues(other.ID, "foreign-artifact", "foreign-version", "user")
	foreign.CreatedAt, v.CreatedAt = base.Add(time.Hour), base.Add(time.Hour)
	if err := insertArtifactRow(tx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := insertArtifactVersionRow(tx, v); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	checkPages(t, want, func(q ListOptions) (Page[domain.Artifact], error) {
		return s.ListArtifacts(ctx, g.ID, q)
	}, func(a domain.Artifact) Cursor { return Cursor{CreatedAt: a.CreatedAt, ID: a.ID} })
	for _, limit := range []int{50, 200} {
		anchor := want[len(want)-limit-1]
		page, err := s.ListArtifacts(ctx, g.ID, ListOptions{Limit: limit, Before: &Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}})
		if err != nil || !reflect.DeepEqual(page.Items, want[len(want)-limit:]) || page.NextCursor != nil {
			t.Fatalf("exact final artifact page limit=%d: %+v, %v", limit, page, err)
		}
	}
	page, err := s.ListArtifacts(ctx, g.ID, ListOptions{Before: &Cursor{CreatedAt: foreign.CreatedAt, ID: foreign.ID}})
	if err != nil || !reflect.DeepEqual(page.Items, want[:50]) {
		t.Fatalf("foreign cursor escaped artifact group: %+v, %v", page, err)
	}
	last := want[len(want)-1]
	for _, groupID := range []string{g.ID, other.ID} {
		page, err := s.ListArtifacts(ctx, groupID, ListOptions{Before: &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}})
		if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
			t.Fatalf("empty tail/foreign anchor: %+v, %v", page, err)
		}
	}
}

func TestArtifactListVersionTuplePages(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	a, initial := artifactValues(g.ID, "history", "version-000", "user")
	a.CreatedAt = base
	want := make([]domain.ArtifactVersion, 0, 205)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insertArtifactRow(tx, a); err != nil {
		t.Fatal(err)
	}
	head := initial.ID
	for i := range 205 {
		n := (i * 73) % 205
		v := initial
		v.ID, v.CreatedAt = fmt.Sprintf("version-%03d", n), base.Add(time.Duration(n%5)*time.Nanosecond)
		if i > 0 {
			v.ExpectedVersionID, v.ObservedVersionID, v.Applied = head, head, i%3 != 2
			if !v.Applied {
				v.ExpectedVersionID = initial.ID
			}
		}
		if err := insertArtifactVersionRow(tx, v); err != nil {
			t.Fatal(err)
		}
		if i > 0 && v.Applied {
			if _, err := tx.ExecContext(ctx, "UPDATE artifacts SET current_version_id=? WHERE id=?", v.ID, a.ID); err != nil {
				t.Fatal(err)
			}
			head = v.ID
		}
		want = append(want, v)
	}
	var outsiders []domain.ArtifactVersion
	for i, parent := range [][3]string{{g.ID, "sibling", "sibling-v"}, {other.ID, "foreign", "foreign-v"}} {
		a, v := artifactValues(parent[0], parent[1], parent[2], "user")
		a.CreatedAt = base.Add(time.Duration(i+1) * time.Hour)
		v.CreatedAt = a.CreatedAt
		if err := insertArtifactRow(tx, a); err != nil {
			t.Fatal(err)
		}
		if err := insertArtifactVersionRow(tx, v); err != nil {
			t.Fatal(err)
		}
		outsiders = append(outsiders, v)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	checkPages(t, want, func(q ListOptions) (Page[domain.ArtifactVersion], error) {
		return s.ListArtifactVersions(ctx, g.ID, a.ID, q)
	}, func(v domain.ArtifactVersion) Cursor { return Cursor{CreatedAt: v.CreatedAt, ID: v.ID} })
	for _, limit := range []int{50, 200} {
		anchor := want[len(want)-limit-1]
		page, err := s.ListArtifactVersions(ctx, g.ID, a.ID, ListOptions{Limit: limit, Before: &Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}})
		if err != nil || !reflect.DeepEqual(page.Items, want[len(want)-limit:]) || page.NextCursor != nil {
			t.Fatalf("exact final version page limit=%d: %+v, %v", limit, page, err)
		}
	}
	for _, foreign := range outsiders {
		page, err := s.ListArtifactVersions(ctx, g.ID, a.ID, ListOptions{Before: &Cursor{CreatedAt: foreign.CreatedAt, ID: foreign.ID}})
		if err != nil || !reflect.DeepEqual(page.Items, want[:50]) {
			t.Fatalf("foreign cursor escaped version parent: %+v, %v", page, err)
		}
		last := want[len(want)-1]
		page, err = s.ListArtifactVersions(ctx, foreign.GroupID, foreign.ArtifactID, ListOptions{Before: &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}})
		if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
			t.Fatalf("foreign anchor revealed history: %+v, %v", page, err)
		}
	}
}
