package store

import (
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestArtifactPublishCompetingBaseAcrossStores(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	_, base := seedArtifact(t, s, g.ID, "artifact", "base", "user")
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	type result struct {
		version domain.ArtifactVersion
		err     error
	}
	results, start := make(chan result, 2), make(chan struct{})
	for i := range 2 {
		go func() {
			<-start
			input := artifactPublishInput(g.ID)
			input.ArtifactID, input.Name, input.ExpectedVersionID = base.ArtifactID, "", base.ID
			input.BlobHash = strings.Repeat([]string{"b", "c"}[i], 64)
			input.IdempotencyKey = []string{"first", "second"}[i]
			v, err := []*Store{s, second}[i].PublishArtifact(ctx, input)
			results <- result{v, err}
		}()
	}
	close(start)
	var winner, candidate domain.ArtifactVersion
	applied := 0
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatalf("competing publish: %v", r.err)
		}
		if r.version.Applied {
			applied++
			winner = r.version
		} else {
			candidate = r.version
		}
	}
	if applied != 1 || winner.ID == candidate.ID || winner.ObservedVersionID != base.ID ||
		candidate.ID == "" || candidate.ExpectedVersionID != base.ID || candidate.ObservedVersionID != winner.ID {
		t.Fatalf("one winner/retained candidate required: %+v, %+v", winner, candidate)
	}
	if a, err := s.GetArtifact(ctx, g.ID, base.ArtifactID); err != nil || a.CurrentVersionID != winner.ID {
		t.Fatalf("competing head: %+v, %v", a, err)
	}
	for _, want := range []domain.ArtifactVersion{base, winner, candidate} {
		if got, err := second.GetArtifactVersion(ctx, g.ID, base.ArtifactID, want.ID); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("competing history lost: %+v, %v", got, err)
		}
	}
	assertArtifactPublishCounts(t, s, 1, 3, 2)
}

func TestArtifactPublishConcurrentSameKeyAcrossStores(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	input := artifactPublishInput(g.ID)
	input.IdempotencyKey = "concurrent"
	type result struct {
		version domain.ArtifactVersion
		err     error
	}
	results, start := make(chan result, 6), make(chan struct{})
	for i := range 6 {
		go func() {
			<-start
			v, err := []*Store{s, second}[i%2].PublishArtifact(ctx, input)
			results <- result{v, err}
		}()
	}
	close(start)
	var original domain.ArtifactVersion
	for range 6 {
		r := <-results
		if r.err != nil || (original.ID != "" && !reflect.DeepEqual(r.version, original)) {
			t.Fatalf("same-key results diverged: %+v, %v; original %+v", r.version, r.err, original)
		}
		original = r.version
	}
	if original.ID == "" || !original.Applied {
		t.Fatalf("same-key original missing: %+v", original)
	}
	assertArtifactPublishCounts(t, s, 1, 1, 1)
}
