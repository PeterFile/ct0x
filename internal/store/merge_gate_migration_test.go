package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Every production prefix is a supported on-disk starting point, not a new empty database.
func TestMergeGateAllMigrationPrefixesUpgradeReopenReplay(t *testing.T) {
	// The trusted controller supplies its own immutable registry, not the candidate's replacement.
	authority := os.Getenv("MERGE_GATE_MIGRATIONS_AUTHORITY")
	if authority == "" {
		authority = filepath.Join("..", "..", ".github", "scripts", "merge-gate-migrations.json")
	}
	data, err := os.ReadFile(authority)
	if err != nil {
		t.Fatal(err)
	}
	var published struct {
		VersionCount int      `json:"version_count"`
		SHA256       []string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &published); err != nil {
		t.Fatal(err)
	}
	if published.VersionCount == 0 || published.VersionCount != len(published.SHA256) || len(migrations) < published.VersionCount {
		t.Fatal("published migration prefix is missing or shortened")
	}
	for i, want := range published.SHA256 {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations[i]))); got != want {
			t.Fatalf("published migration v%d changed before upgrade construction: %s != %s", i+1, got, want)
		}
	}
	latest, err := Open(filepath.Join(t.TempDir(), "latest.db"))
	if err != nil {
		t.Fatal(err)
	}
	wantSchema := mergeGateSchema(t, latest.db)
	if err := latest.Close(); err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= len(migrations); version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upgrade.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
				t.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY CHECK(version > 0))"); err != nil {
				t.Fatal(err)
			}
			for i, migration := range migrations[:version] {
				if _, err := tx.Exec(migration); err != nil {
					t.Fatalf("production migration v%d: %v", i+1, err)
				}
				if _, err := tx.Exec("INSERT INTO schema_migrations(version) VALUES(?)", i+1); err != nil {
					t.Fatal(err)
				}
			}
			// These comparable markers exist in v1 and must survive every later migration.
			for _, id := range []string{"historical-a", "historical-b"} {
				if _, err := tx.Exec("INSERT INTO groups(id,name,description,created_at) VALUES(?,?,?,?)", id, id, "preserve marker", "2026-01-01T00:00:00.000000000Z"); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec("INSERT INTO members(id,group_id,name,role,created_at) VALUES(?,?,?,?,?)", id+"-member", id, id, "worker", "2026-01-01T00:00:00.000000000Z"); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			var replayID string
			for attempt := 0; attempt < 3; attempt++ {
				s, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				func() {
					defer s.Close()
					if got := mergeGateSchema(t, s.db); !reflect.DeepEqual(got, wantSchema) {
						t.Fatalf("schema differs after prefix v%d upgrade/reopen", version)
					}
					rows, err := s.db.Query("SELECT version FROM schema_migrations ORDER BY version")
					if err != nil {
						t.Fatal(err)
					}
					count := 0
					for rows.Next() {
						var got int
						if err := rows.Scan(&got); err != nil {
							t.Fatal(err)
						}
						count++
						if got != count {
							t.Fatalf("noncontiguous ledger: %d != %d", got, count)
						}
					}
					if err := errors.Join(rows.Err(), rows.Close()); err != nil {
						t.Fatal(err)
					}
					if count != len(migrations) {
						t.Fatalf("ledger length: %d != %d", count, len(migrations))
					}
					for _, id := range []string{"historical-a", "historical-b"} {
						group, err := s.GetGroup(context.Background(), id)
						if err != nil || group.ID != id || group.Name != id || group.Description != "preserve marker" {
							t.Fatalf("lost historical marker: %+v, %v", group, err)
						}
						if _, err := s.GetMember(context.Background(), id, id+"-member"); err != nil {
							t.Fatal(err)
						}
						foreign := "historical-a-member"
						if id == "historical-a" {
							foreign = "historical-b-member"
						}
						if _, err := s.GetMember(context.Background(), id, foreign); !errors.Is(err, ErrNotFound) {
							t.Fatalf("cross-group read was not denied: %v", err)
						}
					}
					input := CreateGroupInput{Name: "durable replay", ActorID: "user", IdempotencyKey: "merge-gate-upgrade-replay"}
					for range 2 {
						group, err := s.CreateGroup(context.Background(), input)
						if err != nil {
							t.Fatal(err)
						}
						if replayID != "" && replayID != group.ID {
							t.Fatal("reopen/replay changed mutation identity")
						}
						replayID = group.ID
					}
				}()
			}
		})
	}
}

func mergeGateSchema(t *testing.T, db *sql.DB) [][4]string {
	t.Helper()
	rows, err := db.Query("SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_master ORDER BY type,name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result [][4]string
	for rows.Next() {
		var row [4]string
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
