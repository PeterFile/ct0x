package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func artifactPublishInput(groupID string) PublishArtifactInput {
	return PublishArtifactInput{GroupID: groupID, Name: " 报告 ", BlobHash: strings.Repeat("a", 64), Size: 42, ActorID: "user"}
}

func mustPublishArtifact(t *testing.T, s *Store, input PublishArtifactInput) domain.ArtifactVersion {
	t.Helper()
	v, err := s.PublishArtifact(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func assertArtifactPublishCounts(t *testing.T, s *Store, artifacts, versions, receipts int) {
	t.Helper()
	for query, want := range map[string]int{
		"SELECT count(*) FROM artifacts":                                               artifacts,
		"SELECT count(*) FROM artifact_versions":                                       versions,
		"SELECT count(*) FROM mutation_idempotency WHERE operation='publish_artifact'": receipts,
	} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: %d, %v; want %d", query, got, err, want)
		}
	}
}

func TestArtifactPublishInitialMetadataAndReopen(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	artifactForeignKeys(t, s.db)
	input := artifactPublishInput(g.ID)
	input.IdempotencyKey = "create"
	initial := mustPublishArtifact(t, s, input)
	if initial.ID == "" || initial.ArtifactID == "" || initial.ID == initial.ArtifactID || initial.ID == input.BlobHash ||
		initial.GroupID != g.ID || initial.AuthorID != "user" || initial.SourceTaskID != "" ||
		initial.ExpectedVersionID != "" || initial.ObservedVersionID != "" || !initial.Applied ||
		initial.BlobHash != input.BlobHash || initial.Size != input.Size || initial.CreatedAt.IsZero() || initial.CreatedAt.Location() != time.UTC {
		t.Fatalf("initial publication lost identity/metadata: %+v", initial)
	}
	a, err := s.GetArtifact(ctx, g.ID, initial.ArtifactID)
	if err != nil || a.Name != input.Name || a.CurrentVersionID != initial.ID || !a.CreatedAt.Equal(initial.CreatedAt) {
		t.Fatalf("initial artifact/head: %+v, %v", a, err)
	}
	author := member(t, s, g.ID)
	task := createTestTask(t, s, g.ID)
	input.ActorID, input.SourceTaskID, input.IdempotencyKey = author.ID, task.ID, "member"
	if err := json.Unmarshal([]byte(`{"ActorID":"user","actor_id":"user"}`), &input); err != nil || input.ActorID != author.ID {
		t.Fatalf("JSON supplied trusted actor: %+v, %v", input, err)
	}
	data, err := json.Marshal(input)
	if err != nil || strings.Contains(string(data), `"ActorID"`) || strings.Contains(string(data), `"actor_id"`) {
		t.Fatalf("JSON exposed trusted actor: %s, %v", data, err)
	}
	agentVersion := mustPublishArtifact(t, s, input)
	if agentVersion.AuthorID != author.ID || agentVersion.SourceTaskID != task.ID || agentVersion.GroupID != g.ID ||
		agentVersion.ArtifactID == initial.ArtifactID || agentVersion.ID == initial.ID || agentVersion.BlobHash != initial.BlobHash {
		t.Fatalf("bound author/task or independent identity lost: %+v", agentVersion)
	}
	if got, err := s.GetTask(ctx, g.ID, task.ID); err != nil || !reflect.DeepEqual(got, task) {
		t.Fatalf("publication changed task lifecycle: %+v, %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, want := range []domain.ArtifactVersion{initial, agentVersion} {
		if got, err := reopened.GetArtifactVersion(ctx, g.ID, want.ArtifactID, want.ID); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("publication changed after reopen: %+v, %v", got, err)
		}
	}
	assertArtifactPublishCounts(t, reopened, 2, 2, 2)
}

func TestArtifactPublishHistoryConflictAndStableReplay(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	create := artifactPublishInput(g.ID)
	create.IdempotencyKey = "create"
	base := mustPublishArtifact(t, s, create)
	appendInput := create
	appendInput.ArtifactID, appendInput.Name, appendInput.ExpectedVersionID = base.ArtifactID, "", base.ID
	appendInput.BlobHash, appendInput.IdempotencyKey = strings.Repeat("b", 64), "applied"
	applied := mustPublishArtifact(t, s, appendInput)
	conflictInput := appendInput
	conflictInput.IdempotencyKey = "candidate"
	candidate := mustPublishArtifact(t, s, conflictInput)
	if !applied.Applied || applied.ExpectedVersionID != base.ID || applied.ObservedVersionID != base.ID || candidate.Applied ||
		candidate.ExpectedVersionID != base.ID || candidate.ObservedVersionID != applied.ID || candidate.ID == applied.ID {
		t.Fatalf("applied/conflict evidence: %+v, %+v", applied, candidate)
	}
	if a, err := s.GetArtifact(ctx, g.ID, base.ArtifactID); err != nil || a.CurrentVersionID != applied.ID {
		t.Fatalf("candidate advanced head: %+v, %v", a, err)
	}
	laterInput := appendInput
	laterInput.ExpectedVersionID, laterInput.IdempotencyKey = applied.ID, "later"
	later := mustPublishArtifact(t, s, laterInput)
	// Deliberately applying candidate bytes is a new publication, not candidate mutation.
	applyInput := conflictInput
	applyInput.ExpectedVersionID, applyInput.IdempotencyKey = later.ID, "apply-candidate"
	final := mustPublishArtifact(t, s, applyInput)
	if !final.Applied || final.ID == candidate.ID || final.BlobHash != candidate.BlobHash || final.ObservedVersionID != later.ID {
		t.Fatalf("candidate application rewrote evidence: %+v", final)
	}
	check := func(s *Store) {
		t.Helper()
		for _, pair := range []struct {
			input PublishArtifactInput
			want  domain.ArtifactVersion
		}{{create, base}, {appendInput, applied}, {conflictInput, candidate}} {
			if got, err := s.PublishArtifact(ctx, pair.input); err != nil || !reflect.DeepEqual(got, pair.want) {
				t.Fatalf("replay changed after head movement: %+v, %v; want %+v", got, err, pair.want)
			}
			if got, err := s.GetArtifactVersion(ctx, g.ID, base.ArtifactID, pair.want.ID); err != nil || !reflect.DeepEqual(got, pair.want) {
				t.Fatalf("history changed: %+v, %v", got, err)
			}
		}
		if a, err := s.GetArtifact(ctx, g.ID, base.ArtifactID); err != nil || a.CurrentVersionID != final.ID {
			t.Fatalf("replay moved head: %+v, %v", a, err)
		}
		assertArtifactPublishCounts(t, s, 1, 5, 5)
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
}

func TestArtifactPublishKeyScopesAndNoKey(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	author := member(t, s, g.ID)
	input := artifactPublishInput(g.ID)
	input.IdempotencyKey, input.Size = "scope", 0
	identities := map[string]bool{}
	for _, scope := range [][3]string{{g.ID, "user", "scope"}, {g.ID, author.ID, "scope"}, {other.ID, "user", "scope"},
		{g.ID, "user", ""}, {g.ID, "user", ""}} {
		input.GroupID, input.ActorID, input.IdempotencyKey = scope[0], scope[1], scope[2]
		v := mustPublishArtifact(t, s, input)
		if identities[v.ID] || identities[v.ArtifactID] || v.ID == v.ArtifactID || v.Size != 0 {
			t.Fatalf("scope/no-key identities collapsed: %+v", v)
		}
		identities[v.ID], identities[v.ArtifactID] = true, true
	}
	assertArtifactPublishCounts(t, s, 5, 5, 3)
}
