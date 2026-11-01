package store

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
)

func taskArtifactCounts(t *testing.T, s *Store, submissions, refs, receipts int) {
	t.Helper()
	for query, want := range map[string]int{"SELECT count(*) FROM task_submissions": submissions, "SELECT count(*) FROM task_submission_artifacts": refs, "SELECT count(*) FROM mutation_idempotency WHERE operation='submit_task'": receipts} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", query, got, err, want)
		}
	}
}

func taskArtifactLateInsert(t *testing.T, s *Store, sub domain.Submission, version domain.ArtifactVersion) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO task_submission_artifacts(submission_id,task_id,group_id,task_revision,artifact_id,version_id) VALUES(?,?,?,?,?,?)`, sub.ID, sub.TaskID, sub.GroupID, sub.TaskRevision, version.ArtifactID, version.ID)
	if err == nil {
		t.Fatal("completed evidence accepted a late reference")
	}
}

func taskArtifactIntegrity(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		rows.Close()
		t.Fatal("foreign key violation")
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var result string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil || result != "ok" {
		t.Fatalf("integrity: %q, %v", result, err)
	}
}

func TestTaskArtifactAssociationDatabaseGuards(t *testing.T) {
	s, _, task, attempt, _ := taskSubmissionHuman(t)
	v := mustPublishArtifact(t, s, artifactPublishInput(task.GroupID))
	other := mustPublishArtifact(t, s, artifactPublishInput(task.GroupID))
	foreignGroup := group(t, s)
	foreign := mustPublishArtifact(t, s, artifactPublishInput(foreignGroup.ID))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at) VALUES('raw-submission',?,?,1,?,'evidence','user',?)`, task.ID, task.GroupID, attempt.ID, task.CreatedAt.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO task_submission_artifacts(submission_id,task_id,group_id,task_revision,artifact_id,version_id) VALUES(?,?,?,?,?,?)`
	for _, args := range [][]any{
		{"raw-submission", "missing", task.GroupID, 1, v.ArtifactID, v.ID},
		{"raw-submission", task.ID, foreignGroup.ID, 1, foreign.ArtifactID, foreign.ID},
		{"raw-submission", task.ID, task.GroupID, 2, v.ArtifactID, v.ID},
		{"raw-submission", task.ID, task.GroupID, 1, foreign.ArtifactID, foreign.ID},
		{"raw-submission", task.ID, task.GroupID, 1, v.ArtifactID, other.ID},
	} {
		if _, err := tx.Exec(insert, args...); err == nil {
			t.Fatalf("misbound identity inserted: %v", args)
		}
	}
	args := []any{"raw-submission", task.ID, task.GroupID, 1, v.ArtifactID, v.ID}
	if _, err := tx.Exec(insert, args...); err != nil {
		t.Fatalf("initial association refused: %v", err)
	}
	for _, statement := range []string{insert, `INSERT OR REPLACE INTO task_submission_artifacts(submission_id,task_id,group_id,task_revision,artifact_id,version_id) VALUES(?,?,?,?,?,?)`} {
		if _, err := tx.Exec(statement, args...); err == nil {
			t.Fatal("duplicate/replacement reference was accepted")
		}
	}
	for _, statement := range []string{`UPDATE task_submission_artifacts SET version_id='different' WHERE submission_id='raw-submission'`, `DELETE FROM task_submission_artifacts WHERE submission_id='raw-submission'`} {
		if _, err := tx.Exec(statement); err == nil {
			t.Fatal("immutable reference changed")
		}
	}
	if _, err := tx.Exec("UPDATE tasks SET status='submitted',current_submission_id='raw-submission' WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(insert, "raw-submission", task.ID, task.GroupID, 1, other.ArtifactID, other.ID); err == nil {
		t.Fatal("submitted set accepted late append")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	taskArtifactCounts(t, s, 0, 0, 0)
	taskArtifactIntegrity(t, s)
}

func TestTaskArtifactV7IndexesAndIntegrity(t *testing.T) {
	if len(migrations) < 7 || migrations[6] != taskArtifactSchemaV7 {
		t.Fatal("v7 migration is missing or misplaced")
	}
	v7Count := 0
	for _, migration := range migrations {
		if migration == taskArtifactSchemaV7 {
			v7Count++
		}
	}
	if v7Count != 1 {
		t.Fatalf("expected v7 exactly once, got %d", v7Count)
	}
	s, _ := openTestStore(t)
	for _, index := range []string{"task_attempts_group_task_created_id", "task_submissions_group_task_created_id", "task_reviews_group_task_created_id"} {
		rows, err := s.db.Query("PRAGMA index_xinfo(" + index + ")")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var seq, cid, desc, key int
			var name, collation sql.NullString
			if err := rows.Scan(&seq, &cid, &name, &desc, &collation, &key); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if key == 1 {
				got = append(got, fmt.Sprintf("%s:%d", name.String, desc))
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, []string{"group_id:0", "task_id:0", "created_at:1", "id:1"}) {
			t.Fatalf("index %s: %v", index, got)
		}
	}
	taskArtifactIntegrity(t, s)
}
