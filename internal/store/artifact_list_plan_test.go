package store

import (
	"strings"
	"testing"
	"time"
)

func TestArtifactListQueryPlansUseBoundedIndexes(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	a, v := seedArtifact(t, s, g.ID, "artifact", "initial", "user")
	for _, tc := range []struct {
		name, query, index string
		args               []any
	}{
		{"artifacts", "SELECT " + artifactColumns + " FROM artifacts WHERE group_id=?", "artifacts_group_created_id", []any{g.ID}},
		{"versions", "SELECT " + artifactVersionColumns + " FROM artifact_versions WHERE group_id=? AND artifact_id=?", "artifact_versions_group_created_id", []any{g.ID, a.ID}},
	} {
		for _, before := range []bool{false, true} {
			name := tc.name + "/first"
			if before {
				name = tc.name + "/before"
			}
			t.Run(name, func(t *testing.T) {
				query, args := tc.query, append([]any{}, tc.args...)
				if before {
					query += " AND (created_at,id)<(?,?)"
					args = append(args, v.CreatedAt.Add(time.Hour).Format(timestampLayout), "position")
				}
				query += " ORDER BY created_at DESC,id DESC LIMIT ?"
				args = append(args, 201)
				rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var details []string
				for rows.Next() {
					var id, parent, unused int
					var detail string
					if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
						t.Fatal(err)
					}
					details = append(details, detail)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				plan := strings.Join(details, "; ")
				t.Logf("%s -> %s", query, plan)
				if !strings.Contains(plan, "SEARCH ") || !strings.Contains(plan, tc.index) || strings.Contains(plan, "USE TEMP B-TREE") {
					t.Fatalf("expected indexed bounded query without temporary sort: %s", plan)
				}
			})
		}
	}
}
