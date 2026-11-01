package store

import (
	"strings"
	"testing"
)

func TestTaskArtifactHistoryQueryPlans(t *testing.T) {
	s, groupID, taskID, before := taskHistoryFixture(t, 1024)
	for _, spec := range []struct{ table, columns, index string }{
		{"task_attempts", taskAttemptColumns, "task_attempts_group_task_created_id"},
		{"task_submissions", taskSubmissionColumns, "task_submissions_group_task_created_id"},
		{"task_review_decisions", taskReviewColumns, "task_reviews_group_task_created_id"},
	} {
		for _, cursor := range []bool{false, true} {
			query := "EXPLAIN QUERY PLAN SELECT " + spec.columns + " FROM " + spec.table + " WHERE group_id=? AND task_id=?"
			args := []any{groupID, taskID}
			if cursor {
				query += " AND (created_at,id)<(?,?)"
				args = append(args, before.CreatedAt.Format(timestampLayout), before.ID)
			}
			query += " ORDER BY created_at DESC,id DESC LIMIT ?"
			args = append(args, 51)
			rows, err := s.db.Query(query, args...)
			if err != nil {
				t.Fatal(err)
			}
			var parts []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				parts = append(parts, detail)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(parts, "; ")
			t.Logf("%s before=%t: %s", spec.table, cursor, plan)
			if len(migrations) >= 7 {
				if !strings.Contains(plan, spec.index) || strings.Contains(plan, "TEMP B-TREE") {
					t.Fatalf("history did not use v7 tuple index: %s", plan)
				}
			} else if !strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("baseline unexpectedly has ordered history access: %s", plan)
			}
		}
	}
}
