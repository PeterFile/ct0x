package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestArtifactGettersPreserveScopedHistory(t *testing.T) {
	s, path := openTestStore(t)
	g, other := group(t, s), group(t, s)
	author := member(t, s, g.ID)
	task := createTestTask(t, s, g.ID)
	a, base := seedArtifact(t, s, g.ID, "artifact/opaque", "version/initial", "user")
	next := base
	next.ID, next.AuthorID = "version/head", author.ID
	next.ExpectedVersionID, next.ObservedVersionID = base.ID, base.ID
	if err := insertArtifactVersionRow(s.db, next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE artifacts SET current_version_id=? WHERE id=?", next.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	a.CurrentVersionID = next.ID
	candidate := next
	candidate.ID, candidate.SourceTaskID, candidate.Applied = "version/candidate", task.ID, false
	candidate.ObservedVersionID, candidate.BlobHash, candidate.Size = next.ID, strings.Repeat("b", 64), 123
	candidate.CreatedAt = candidate.CreatedAt.Add(time.Second)
	if err := insertArtifactVersionRow(s.db, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE members SET status='left' WHERE id=?", author.ID); err != nil {
		t.Fatal(err)
	}
	check := func(s *Store) {
		t.Helper()
		if got, err := s.GetArtifact(ctx, g.ID, a.ID); err != nil || !reflect.DeepEqual(got, a) {
			t.Fatalf("artifact read: %+v, %v", got, err)
		}
		for _, want := range []domain.ArtifactVersion{base, next, candidate} {
			if got, err := s.GetArtifactVersion(ctx, g.ID, a.ID, want.ID); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("immutable version read: %+v, %v; want %+v", got, err, want)
			}
		}
		for _, pair := range [][2]string{{other.ID, a.ID}, {"missing", a.ID}, {g.ID, "missing"}} {
			if _, err := s.GetArtifact(ctx, pair[0], pair[1]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("scoped artifact miss %v: %v", pair, err)
			}
		}
		for _, triple := range [][3]string{{other.ID, a.ID, base.ID}, {g.ID, "missing", base.ID}, {g.ID, a.ID, "missing"}} {
			if _, err := s.GetArtifactVersion(ctx, triple[0], triple[1], triple[2]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("scoped version miss %v: %v", triple, err)
			}
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for _, want := range []domain.ArtifactVersion{base, candidate} {
			if got, err := artifactVersionInTx(ctx, tx, g.ID, a.ID, want.ID); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("transactional historical/candidate read: %+v, %v", got, err)
			}
		}
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

func TestArtifactVersionInTxUsesCallerTransaction(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(bounded, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	a, v := artifactValues(g.ID, "uncommitted", "uncommitted-v", "user")
	if err := insertArtifactRow(tx, a); err != nil {
		t.Fatal(err)
	}
	if err := insertArtifactVersionRow(tx, v); err != nil {
		t.Fatal(err)
	}
	if got, err := artifactVersionInTx(bounded, tx, g.ID, a.ID, v.ID); err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("read-your-writes on single connection: %+v, %v", got, err)
	}
	for _, triple := range [][3]string{{"missing", a.ID, v.ID}, {g.ID, "missing", v.ID}, {g.ID, a.ID, "missing"}} {
		if _, err := artifactVersionInTx(bounded, tx, triple[0], triple[1], triple[2]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("transactional scoped miss %v: %v", triple, err)
		}
	}
	var alive int
	if err := tx.QueryRowContext(bounded, "SELECT 1").Scan(&alive); err != nil || alive != 1 {
		t.Fatalf("helper ended caller transaction: %d, %v", alive, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetArtifact(ctx, g.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("caller rollback lost artifact ownership: %v", err)
	}
	if _, err := s.GetArtifactVersion(ctx, g.ID, a.ID, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("caller rollback leaked version: %v", err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := s.GetArtifact(cancelled, g.ID, a.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("artifact cancellation lost: %v", err)
	}
	if _, err := s.GetArtifactVersion(cancelled, g.ID, a.ID, v.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("version cancellation lost: %v", err)
	}
}
