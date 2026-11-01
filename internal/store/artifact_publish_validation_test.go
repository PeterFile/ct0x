package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestArtifactPublishChangedPayloadConflictsBeforeValidation(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	task := createTestTask(t, s, g.ID)
	input := artifactPublishInput(g.ID)
	input.IdempotencyKey, input.SourceTaskID, input.Size = "payload", task.ID, 1<<63-1
	original := mustPublishArtifact(t, s, input)
	for name, change := range map[string]func(*PublishArtifactInput){
		"artifact":     func(i *PublishArtifactInput) { i.ArtifactID = "missing" },
		"name":         func(i *PublishArtifactInput) { i.Name = strings.TrimSpace(i.Name) },
		"invalid name": func(i *PublishArtifactInput) { i.Name = "" },
		"task":         func(i *PublishArtifactInput) { i.SourceTaskID = "" },
		"invalid task": func(i *PublishArtifactInput) { i.SourceTaskID = "missing" },
		"hash":         func(i *PublishArtifactInput) { i.BlobHash = strings.Repeat("b", 64) },
		"invalid hash": func(i *PublishArtifactInput) { i.BlobHash = "" },
		"size":         func(i *PublishArtifactInput) { i.Size-- },
		"invalid size": func(i *PublishArtifactInput) { i.Size = -1 },
		"base":         func(i *PublishArtifactInput) { i.ExpectedVersionID = "missing" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := input
			change(&changed)
			if got, err := s.PublishArtifact(ctx, changed); !errors.Is(err, ErrConflict) || got.ID != "" {
				t.Fatalf("changed payload was validated/replayed: %+v, %v", got, err)
			}
			assertArtifactPublishCounts(t, s, 1, 1, 1)
		})
	}
	if a, err := s.GetArtifact(ctx, g.ID, original.ArtifactID); err != nil || a.CurrentVersionID != original.ID {
		t.Fatalf("changed payload moved head: %+v, %v", a, err)
	}
}

func TestArtifactPublishInvalidShapeDoesNotBurnKey(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	_, base := seedArtifact(t, s, g.ID, "artifact", "base", "user")
	for name, change := range map[string]func(*PublishArtifactInput){
		"empty name":          func(i *PublishArtifactInput) { i.Name = "" },
		"blank name":          func(i *PublishArtifactInput) { i.Name = " \t\u00a0" },
		"create base":         func(i *PublishArtifactInput) { i.ExpectedVersionID = base.ID },
		"update name":         func(i *PublishArtifactInput) { i.ArtifactID, i.ExpectedVersionID = base.ArtifactID, base.ID },
		"update without base": func(i *PublishArtifactInput) { i.ArtifactID, i.Name = base.ArtifactID, "" },
		"short hash":          func(i *PublishArtifactInput) { i.BlobHash = strings.Repeat("a", 63) },
		"long hash":           func(i *PublishArtifactInput) { i.BlobHash += "a" },
		"uppercase hash":      func(i *PublishArtifactInput) { i.BlobHash = strings.Repeat("A", 64) },
		"nonhex hash":         func(i *PublishArtifactInput) { i.BlobHash = strings.Repeat("g", 64) },
		"NUL hash":            func(i *PublishArtifactInput) { i.BlobHash += "\x00" },
		"negative size":       func(i *PublishArtifactInput) { i.Size = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			input := artifactPublishInput(g.ID)
			input.IdempotencyKey = "invalid"
			change(&input)
			if got, err := s.PublishArtifact(ctx, input); !errors.Is(err, ErrInvalid) || got.ID != "" {
				t.Fatalf("invalid publication: %+v, %v", got, err)
			}
			assertArtifactPublishCounts(t, s, 1, 1, 0)
		})
	}
	input := artifactPublishInput(g.ID)
	input.IdempotencyKey = "invalid"
	mustPublishArtifact(t, s, input)
	assertArtifactPublishCounts(t, s, 2, 2, 1)
}

func TestArtifactPublishReferenceBindingRollsBack(t *testing.T) {
	f := setupMessages(t)
	task, foreignTask := createTestTask(t, f.s, f.groupID), createTestTask(t, f.s, f.otherID)
	_, base := seedArtifact(t, f.s, f.groupID, "artifact", "base", "user")
	_, sibling := seedArtifact(t, f.s, f.groupID, "sibling", "sibling-v", "user")
	_, foreign := seedArtifact(t, f.s, f.otherID, "foreign", "foreign-v", "user")
	valid := artifactPublishInput(f.groupID)
	valid.ArtifactID, valid.Name, valid.ExpectedVersionID = base.ArtifactID, "", base.ID
	valid.ActorID, valid.SourceTaskID, valid.IdempotencyKey = f.a, task.ID, "references"
	for name, change := range map[string]func(*PublishArtifactInput){
		"missing task":     func(i *PublishArtifactInput) { i.SourceTaskID = "missing" },
		"foreign task":     func(i *PublishArtifactInput) { i.SourceTaskID = foreignTask.ID },
		"missing base":     func(i *PublishArtifactInput) { i.ExpectedVersionID = "missing" },
		"sibling base":     func(i *PublishArtifactInput) { i.ExpectedVersionID = sibling.ID },
		"foreign base":     func(i *PublishArtifactInput) { i.ExpectedVersionID = foreign.ID },
		"sibling artifact": func(i *PublishArtifactInput) { i.ArtifactID = sibling.ArtifactID },
		"foreign artifact": func(i *PublishArtifactInput) { i.ArtifactID = foreign.ArtifactID },
		"missing artifact": func(i *PublishArtifactInput) { i.ArtifactID = "missing" },
		"create missing task": func(i *PublishArtifactInput) {
			i.ArtifactID, i.Name, i.ExpectedVersionID, i.SourceTaskID = "", "报告", "", "missing"
		},
		"create foreign task": func(i *PublishArtifactInput) {
			i.ArtifactID, i.Name, i.ExpectedVersionID, i.SourceTaskID = "", "报告", "", foreignTask.ID
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := valid
			change(&input)
			if got, err := f.s.PublishArtifact(ctx, input); !errors.Is(err, ErrNotFound) || got.ID != "" {
				t.Fatalf("misbound reference accepted: %+v, %v", got, err)
			}
			assertArtifactPublishCounts(t, f.s, 3, 3, 0)
		})
	}
	v := mustPublishArtifact(t, f.s, valid)
	if !v.Applied || v.AuthorID != f.a || v.SourceTaskID != task.ID {
		t.Fatalf("valid bound publication: %+v", v)
	}
	assertArtifactPublishCounts(t, f.s, 3, 4, 1)
}

func TestArtifactPublishAuthorizationPrecedesReceipt(t *testing.T) {
	f := setupMessages(t)
	input := artifactPublishInput(f.groupID)
	input.ActorID, input.IdempotencyKey = f.a, "member"
	mustPublishArtifact(t, f.s, input)
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.a); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []bool{false, true} {
		retry := input
		if changed {
			retry.BlobHash = "invalid"
		}
		if got, err := f.s.PublishArtifact(ctx, retry); !errors.Is(err, ErrNotFound) || got.ID != "" {
			t.Fatalf("left actor probed receipt/conflict: %+v, %v", got, err)
		}
	}
	for _, actor := range []string{"", " ", " user", "missing", f.foreign, f.a} {
		invalid := input
		invalid.ActorID, invalid.IdempotencyKey = actor, "unauthorized"
		if got, err := f.s.PublishArtifact(ctx, invalid); !errors.Is(err, ErrNotFound) || got.ID != "" {
			t.Fatalf("unauthorized actor %q: %+v, %v", actor, got, err)
		}
	}
	for _, groupID := range []string{"", "missing"} {
		invalid := input
		invalid.GroupID, invalid.ActorID = groupID, "user"
		if _, err := f.s.PublishArtifact(ctx, invalid); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing group %q: %v", groupID, err)
		}
	}
	var decoded PublishArtifactInput
	if err := json.Unmarshal([]byte(`{"ActorID":"user","actor_id":"user"}`), &decoded); err != nil || decoded.ActorID != "" {
		t.Fatalf("JSON bound an actor: %+v, %v", decoded, err)
	}
	assertArtifactPublishCounts(t, f.s, 1, 1, 1)
}
