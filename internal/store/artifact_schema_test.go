package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

const artifactInsertSQL = `INSERT INTO artifacts(id,group_id,name,current_version_id,created_at) VALUES(?,?,?,?,?)`
const artifactVersionInsertSQL = `INSERT INTO artifact_versions
 (id,artifact_id,group_id,source_task_id,author_id,blob_hash,size,expected_version_id,observed_version_id,applied,created_at)
 VALUES(?,?,?,nullif(?,''),?,?,?,nullif(?,''),nullif(?,''),?,?)`

type artifactExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func artifactValues(groupID, id, versionID, authorID string) (domain.Artifact, domain.ArtifactVersion) {
	stamp := time.Date(2026, 10, 30, 12, 0, 0, 123, time.UTC)
	return domain.Artifact{ID: id, GroupID: groupID, Name: "报告", CurrentVersionID: versionID, CreatedAt: stamp},
		domain.ArtifactVersion{ID: versionID, ArtifactID: id, GroupID: groupID, AuthorID: authorID,
			BlobHash: strings.Repeat("a", 64), Size: 42, Applied: true, CreatedAt: stamp}
}

func insertArtifactRow(db artifactExecer, a domain.Artifact) error {
	_, err := db.ExecContext(ctx, artifactInsertSQL, a.ID, a.GroupID, a.Name, a.CurrentVersionID, a.CreatedAt.Format(timestampLayout))
	return err
}

func insertArtifactVersionRow(db artifactExecer, v domain.ArtifactVersion) error {
	_, err := db.ExecContext(ctx, artifactVersionInsertSQL, v.ID, v.ArtifactID, v.GroupID, v.SourceTaskID,
		v.AuthorID, v.BlobHash, v.Size, v.ExpectedVersionID, v.ObservedVersionID, v.Applied, v.CreatedAt.Format(timestampLayout))
	return err
}

func seedArtifact(t *testing.T, s *Store, groupID, id, versionID, authorID string) (domain.Artifact, domain.ArtifactVersion) {
	t.Helper()
	a, v := artifactValues(groupID, id, versionID, authorID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insertArtifactRow(tx, a); err != nil {
		t.Fatal(err)
	}
	if err := insertArtifactVersionRow(tx, v); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return a, v
}

func artifactForeignKeys(t *testing.T, db *sql.DB) {
	t.Helper()
	var enabled int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign_keys = %d, %v; want 1", enabled, err)
	}
}

func TestArtifactDeferredHeadAndRollback(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprintf("raw=%t", raw), func(t *testing.T) {
			s, path := openTestStore(t)
			artifactForeignKeys(t, s.db)
			g, other := group(t, s), group(t, s)
			_, same := seedArtifact(t, s, g.ID, "same", "same-v", "user")
			_, foreign := seedArtifact(t, s, other.ID, "foreign", "foreign-v", "user")
			db := s.db
			if raw {
				var err error
				dsn := url.URL{Scheme: "file", Path: path}
				db, err = sql.Open("sqlite", dsn.String())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				db.SetMaxOpenConns(1)
				db.SetMaxIdleConns(1)
				if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
					t.Fatal(err)
				}
			}
			artifactForeignKeys(t, db)
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for _, head := range []any{nil, "", "missing", same.ID, foreign.ID, "initial-v"} {
				if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
					t.Fatal(err)
				}
				_, err := conn.ExecContext(ctx, artifactInsertSQL, "initial", g.ID, "报告", head, same.CreatedAt.Format(timestampLayout))
				if head == nil || head == "" {
					if err == nil {
						t.Fatal("headless artifact accepted")
					}
				} else {
					if err != nil {
						t.Fatalf("head FK was not deferred: %v", err)
					}
					if head == "initial-v" {
						_, v := artifactValues(g.ID, "initial", "initial-v", "user")
						if err := insertArtifactVersionRow(conn, v); err != nil {
							t.Fatal(err)
						}
					}
					_, err = conn.ExecContext(ctx, "COMMIT")
					if head == "initial-v" {
						if err != nil {
							t.Fatalf("legal initial commit: %v", err)
						}
						continue
					}
					if err == nil {
						t.Fatalf("invalid exact head committed: %v", head)
					}
				}
				if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM artifacts WHERE id='initial'").Scan(&count); err != nil || count != 0 {
					t.Fatalf("failed transaction leaked artifact: %d, %v", count, err)
				}
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			artifactForeignKeys(t, db)
		})
	}
}

func TestArtifactVersionBindingsAndValues(t *testing.T) {
	s, _ := openTestStore(t)
	artifactForeignKeys(t, s.db)
	g, other := group(t, s), group(t, s)
	author, foreignAuthor := member(t, s, g.ID), member(t, s, other.ID)
	task, foreignTask := createTestTask(t, s, g.ID), createTestTask(t, s, other.ID)
	_, base := seedArtifact(t, s, g.ID, "artifact", "base", "user")
	_, sibling := seedArtifact(t, s, g.ID, "sibling", "sibling-v", "user")
	_, foreign := seedArtifact(t, s, other.ID, "foreign", "foreign-v", "user")
	next := base
	next.ID, next.SourceTaskID, next.AuthorID = "next", task.ID, author.ID
	next.ExpectedVersionID, next.ObservedVersionID = base.ID, base.ID
	for name, change := range map[string]func(*domain.ArtifactVersion){
		"missing artifact":           func(v *domain.ArtifactVersion) { v.ArtifactID = "missing" },
		"foreign artifact":           func(v *domain.ArtifactVersion) { v.ArtifactID = foreign.ArtifactID },
		"wrong group":                func(v *domain.ArtifactVersion) { v.GroupID = other.ID },
		"missing task":               func(v *domain.ArtifactVersion) { v.SourceTaskID = "missing" },
		"foreign task":               func(v *domain.ArtifactVersion) { v.SourceTaskID = foreignTask.ID },
		"missing author":             func(v *domain.ArtifactVersion) { v.AuthorID = "missing" },
		"foreign author":             func(v *domain.ArtifactVersion) { v.AuthorID = foreignAuthor.ID },
		"empty author":               func(v *domain.ArtifactVersion) { v.AuthorID = "" },
		"missing base":               func(v *domain.ArtifactVersion) { v.ExpectedVersionID, v.ObservedVersionID = "missing", "missing" },
		"sibling base":               func(v *domain.ArtifactVersion) { v.ExpectedVersionID, v.ObservedVersionID = sibling.ID, sibling.ID },
		"foreign base":               func(v *domain.ArtifactVersion) { v.ExpectedVersionID, v.ObservedVersionID = foreign.ID, foreign.ID },
		"candidate missing expected": func(v *domain.ArtifactVersion) { v.Applied, v.ExpectedVersionID = false, "" },
		"candidate missing observed": func(v *domain.ArtifactVersion) { v.Applied, v.ObservedVersionID = false, "" },
	} {
		t.Run(name, func(t *testing.T) {
			v := next
			change(&v)
			if err := insertArtifactVersionRow(s.db, v); err == nil {
				t.Fatalf("invalid version accepted: %+v", v)
			}
		})
	}
	for _, values := range []struct{ hash, size, applied any }{
		{strings.Repeat("A", 64), 0, 1}, {strings.Repeat("g", 64), 0, 1}, {"short", 0, 1},
		{[]byte(base.BlobHash), 0, 1}, {base.BlobHash + "\x00", 0, 1},
		{base.BlobHash, -1, 1}, {base.BlobHash, 1.5, 1},
		{base.BlobHash, "invalid", 1}, {base.BlobHash, 0, 2},
	} {
		_, err := s.db.Exec(`INSERT INTO artifact_versions
 (id,artifact_id,group_id,author_id,blob_hash,size,expected_version_id,observed_version_id,applied,created_at)
 VALUES('invalid',?,?,'user',?,?,?,?,?,?)`, base.ArtifactID, g.ID, values.hash, values.size, base.ID, base.ID, values.applied, base.CreatedAt.Format(timestampLayout))
		if err == nil {
			t.Fatalf("invalid digest/size/boolean accepted: %+v", values)
		}
	}
	if err := insertArtifactVersionRow(s.db, next); err != nil {
		t.Fatalf("same-group task/member version: %v", err)
	}
	if _, err := s.db.Exec("UPDATE artifacts SET current_version_id=? WHERE id=?", next.ID, base.ArtifactID); err != nil {
		t.Fatalf("legal applied transition: %v", err)
	}
	candidate := next
	candidate.ID, candidate.Applied = "candidate", false
	candidate.ObservedVersionID = next.ID
	if err := insertArtifactVersionRow(s.db, candidate); err != nil {
		t.Fatalf("valid retained candidate: %v", err)
	}
	for _, refs := range [][2]string{{sibling.ID, next.ID}, {foreign.ID, next.ID}, {"missing", next.ID},
		{base.ID, sibling.ID}, {base.ID, foreign.ID}, {base.ID, "missing"}, {base.ID, base.ID}} {
		bad := candidate
		bad.ID, bad.ExpectedVersionID, bad.ObservedVersionID = "bad", refs[0], refs[1]
		if err := insertArtifactVersionRow(s.db, bad); err == nil {
			t.Fatalf("candidate crossed exact base/observed binding: %+v", bad)
		}
	}
	for _, head := range []any{nil, "", "missing", base.ID, sibling.ID, foreign.ID, candidate.ID} {
		if _, err := s.db.Exec("UPDATE artifacts SET current_version_id=? WHERE id=?", head, base.ArtifactID); err == nil {
			t.Fatalf("illegal head target accepted: %v", head)
		}
	}
}

func TestArtifactVersionRejectsSelfReferences(t *testing.T) {
	s, _ := openTestStore(t)
	artifactForeignKeys(t, s.db)
	g := group(t, s)
	_, base := seedArtifact(t, s, g.ID, "artifact", "base", "user")
	for _, field := range []string{"expected", "observed"} {
		t.Run(field, func(t *testing.T) {
			candidate := base
			candidate.ID, candidate.Applied = "self-"+field, false
			candidate.ExpectedVersionID, candidate.ObservedVersionID = base.ID, base.ID
			if field == "expected" {
				candidate.ExpectedVersionID = candidate.ID
			} else {
				candidate.ObservedVersionID = candidate.ID
			}
			if err := insertArtifactVersionRow(s.db, candidate); err == nil {
				t.Fatalf("self-referencing %s version was accepted: %+v", field, candidate)
			}
		})
	}
}

func TestArtifactImmutableRowsResistReplace(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	a, base := seedArtifact(t, s, g.ID, "artifact", "base", "user")
	next := base
	next.ID, next.ExpectedVersionID, next.ObservedVersionID = "next", base.ID, base.ID
	if err := insertArtifactVersionRow(s.db, next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE artifacts SET current_version_id='next' WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	candidate := next
	candidate.ID, candidate.ObservedVersionID, candidate.Applied = "candidate", next.ID, false
	if err := insertArtifactVersionRow(s.db, candidate); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"UPDATE artifacts SET id='replacement'", "UPDATE artifacts SET group_id='replacement'",
		"UPDATE artifacts SET name='replacement'", "UPDATE artifacts SET created_at='replacement'", "DELETE FROM artifacts",
		"INSERT OR REPLACE INTO artifacts SELECT * FROM artifacts",
	} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatalf("immutable row changed: %s", query)
		}
	}
	for _, id := range []string{base.ID, candidate.ID} {
		for _, query := range []string{
			"UPDATE artifact_versions SET blob_hash=blob_hash WHERE id=?", "DELETE FROM artifact_versions WHERE id=?",
			`INSERT OR REPLACE INTO artifact_versions(id,artifact_id,group_id,source_task_id,author_id,blob_hash,size,expected_version_id,observed_version_id,applied,created_at)
 SELECT id,artifact_id,group_id,source_task_id,author_id,blob_hash,size,expected_version_id,observed_version_id,applied,created_at FROM artifact_versions WHERE id=?`,
		} {
			if _, err := s.db.Exec(query, id); err == nil {
				t.Fatalf("immutable version %s changed: %s", id, query)
			}
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM artifact_versions").Scan(&count); err != nil || count != 3 {
		t.Fatalf("guard failure changed history: %d, %v", count, err)
	}
}

func TestArtifactBoundedReadIndexes(t *testing.T) {
	s, _ := openTestStore(t)
	for index, want := range map[string]string{
		"artifacts_group_created_id":         "group_id:0,created_at:1,id:1",
		"artifact_versions_group_created_id": "group_id:0,artifact_id:0,created_at:1,id:1",
	} {
		var got string
		if err := s.db.QueryRow(`SELECT group_concat(name || ':' || desc, ',') FROM
 (SELECT name,desc FROM pragma_index_xinfo(?) WHERE key=1 ORDER BY seqno)`, index).Scan(&got); err != nil || got != want {
			t.Fatalf("read index %s: %q, %v; want %q", index, got, err, want)
		}
	}
}
