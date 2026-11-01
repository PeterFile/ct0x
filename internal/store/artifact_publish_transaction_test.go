package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestArtifactPublishInTxOwnsNoTransactionBoundary(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(bounded, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	input := artifactPublishInput(g.ID)
	input.IdempotencyKey = "caller"
	initial, err := publishArtifactInTx(bounded, tx, input)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := artifactVersionInTx(bounded, tx, g.ID, initial.ArtifactID, initial.ID); err != nil || !reflect.DeepEqual(got, initial) {
		t.Fatalf("helper did not use caller connection: %+v, %v", got, err)
	}
	if got, err := publishArtifactInTx(bounded, tx, input); err != nil || !reflect.DeepEqual(got, initial) {
		t.Fatalf("uncommitted receipt replay: %+v, %v", got, err)
	}
	appendInput := input
	appendInput.ArtifactID, appendInput.Name, appendInput.ExpectedVersionID = initial.ArtifactID, "", initial.ID
	appendInput.IdempotencyKey = "append"
	applied, err := publishArtifactInTx(bounded, tx, appendInput)
	if err != nil || !applied.Applied {
		t.Fatalf("caller append: %+v, %v", applied, err)
	}
	appendInput.IdempotencyKey = "candidate"
	candidate, err := publishArtifactInTx(bounded, tx, appendInput)
	if err != nil || candidate.Applied || candidate.ObservedVersionID != applied.ID {
		t.Fatalf("caller conflict candidate: %+v, %v", candidate, err)
	}
	var head string
	if err := tx.QueryRowContext(bounded, "SELECT current_version_id FROM artifacts WHERE id=?", initial.ArtifactID).Scan(&head); err != nil || head != applied.ID {
		t.Fatalf("caller head visibility: %q, %v", head, err)
	}
	invalid := input
	invalid.IdempotencyKey, invalid.BlobHash = "invalid", "invalid"
	if _, err := publishArtifactInTx(bounded, tx, invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("helper validation error: %v", err)
	}
	var alive int
	if err := tx.QueryRowContext(bounded, "SELECT 1").Scan(&alive); err != nil || alive != 1 {
		t.Fatalf("helper ended caller transaction on error: %d, %v", alive, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertArtifactPublishCounts(t, s, 0, 0, 0)
	mustPublishArtifact(t, s, input)
	assertArtifactPublishCounts(t, s, 1, 1, 1)
}

func TestArtifactPublishHeadFailureRollsBackRowsAndReceipt(t *testing.T) {
	for _, failure := range []string{"IGNORE", "ABORT, 'injected head failure'"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := openTestStore(t)
			g := group(t, s)
			_, base := seedArtifact(t, s, g.ID, "artifact", "base", "user")
			query := fmt.Sprintf(`CREATE TRIGGER artifact_publish_fail_head BEFORE UPDATE OF current_version_id ON artifacts
 BEGIN SELECT RAISE(%s); END`, failure)
			if _, err := s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			input := artifactPublishInput(g.ID)
			input.ArtifactID, input.Name, input.ExpectedVersionID, input.IdempotencyKey = base.ArtifactID, "", base.ID, "retry"
			if got, err := s.PublishArtifact(ctx, input); err == nil || got.ID != "" {
				t.Fatalf("failed CAS/head returned evidence: %+v, %v", got, err)
			}
			assertArtifactPublishCounts(t, s, 1, 1, 0)
			if a, err := s.GetArtifact(ctx, g.ID, base.ArtifactID); err != nil || a.CurrentVersionID != base.ID {
				t.Fatalf("failed update changed head: %+v, %v", a, err)
			}
			if _, err := s.db.Exec("DROP TRIGGER artifact_publish_fail_head"); err != nil {
				t.Fatal(err)
			}
			if v := mustPublishArtifact(t, s, input); !v.Applied || v.ObservedVersionID != base.ID {
				t.Fatalf("failed CAS burned key: %+v", v)
			}
			assertArtifactPublishCounts(t, s, 1, 2, 1)
		})
	}
}

func TestArtifactPublishDeferredCommitFailureAndCancellation(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	if _, err := s.db.Exec(`CREATE TRIGGER artifact_publish_skip_version BEFORE INSERT ON artifact_versions
 BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	input := artifactPublishInput(g.ID)
	input.IdempotencyKey = "retry"
	if got, err := s.PublishArtifact(ctx, input); err == nil || got.ID != "" {
		t.Fatalf("missing deferred initial version committed: %+v, %v", got, err)
	}
	assertArtifactPublishCounts(t, s, 0, 0, 0)
	if _, err := s.db.Exec("DROP TRIGGER artifact_publish_skip_version"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.PublishArtifact(cancelled, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	assertArtifactPublishCounts(t, s, 0, 0, 0)
	mustPublishArtifact(t, s, input)
	assertArtifactPublishCounts(t, s, 1, 1, 1)
}
