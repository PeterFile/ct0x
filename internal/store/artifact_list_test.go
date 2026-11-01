package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestArtifactListEmptyParentsAndValidation(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	page, err := s.ListArtifacts(ctx, g.ID, ListOptions{})
	data, _ := json.Marshal(page.Items)
	if err != nil || string(data) != "[]" || page.NextCursor != nil {
		t.Fatalf("existing empty group: %+v, %v", page, err)
	}
	a, initial := seedArtifact(t, s, g.ID, "artifact", "initial", "user")
	foreign, _ := seedArtifact(t, s, other.ID, "foreign", "foreign-v", "user")
	beforeHistory := &Cursor{CreatedAt: initial.CreatedAt.Add(-time.Second), ID: "before-history"}
	versions, err := s.ListArtifactVersions(ctx, g.ID, a.ID, ListOptions{Before: beforeHistory})
	data, _ = json.Marshal(versions.Items)
	if err != nil || string(data) != "[]" || versions.NextCursor != nil {
		t.Fatalf("existing artifact empty history page: %+v, %v", versions, err)
	}
	for _, groupID := range []string{"", "missing"} {
		if _, err := s.ListArtifacts(ctx, groupID, ListOptions{Before: beforeHistory}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing group %q returned an empty success: %v", groupID, err)
		}
	}
	for _, parent := range [][2]string{{g.ID, "missing"}, {g.ID, ""}, {"missing", a.ID}, {other.ID, a.ID}, {g.ID, foreign.ID}} {
		if _, err := s.ListArtifactVersions(ctx, parent[0], parent[1], ListOptions{Before: beforeHistory}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("inaccessible parent %v returned an empty success: %v", parent, err)
		}
	}
	for _, options := range []ListOptions{
		{Limit: -1}, {Limit: 201}, {Before: &Cursor{ID: "id"}}, {Before: &Cursor{CreatedAt: initial.CreatedAt}},
		{Before: &Cursor{ID: "id", CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}},
	} {
		if _, err := s.ListArtifacts(ctx, g.ID, options); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid artifact bounds %+v: %v", options, err)
		}
		if _, err := s.ListArtifactVersions(ctx, g.ID, a.ID, options); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid version bounds %+v: %v", options, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ListArtifacts(cancelled, g.ID, ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("artifact cancellation lost: %v", err)
	}
	if _, err := s.ListArtifactVersions(cancelled, g.ID, a.ID, ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("version cancellation lost: %v", err)
	}
}

func TestArtifactListImmutableMixedHistoryAfterReopen(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	create := artifactPublishInput(g.ID)
	base := mustPublishArtifact(t, s, create)
	appendInput := create
	appendInput.ArtifactID, appendInput.Name, appendInput.ExpectedVersionID = base.ArtifactID, "", base.ID
	applied := mustPublishArtifact(t, s, appendInput)
	candidate := mustPublishArtifact(t, s, appendInput)
	appendInput.ExpectedVersionID = applied.ID
	later := mustPublishArtifact(t, s, appendInput)
	if candidate.Applied || !later.Applied {
		t.Fatal("fixture must contain an immutable conflict and a later applied head")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	versions, err := reopened.ListArtifactVersions(ctx, g.ID, base.ArtifactID, ListOptions{Limit: 4})
	want := []domain.ArtifactVersion{base, applied, candidate, later}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	if err != nil || !reflect.DeepEqual(versions.Items, want) || versions.NextCursor != nil {
		t.Fatalf("reopened list lost immutable history: %+v, %v", versions, err)
	}
	artifacts, err := reopened.ListArtifacts(ctx, g.ID, ListOptions{Limit: 1})
	if err != nil || len(artifacts.Items) != 1 || artifacts.Items[0].CurrentVersionID != later.ID || artifacts.NextCursor != nil {
		t.Fatalf("listed artifact lost the later head: %+v, %v", artifacts, err)
	}
}
