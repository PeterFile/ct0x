package store

import (
	"strings"
	"testing"
)

func TestWorkspaceOperationSourceHistoricalStorageErrorsPropagate(t *testing.T) {
	for _, tc := range []struct {
		version int
		table   string
	}{{17, "message_command_sources"}, {18, "chat_review_notices"}} {
		t.Run(tc.table, func(t *testing.T) {
			f := setupMessages(t)
			message, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, ActorID: "user", Content: "历史来源损坏", RecipientIDs: []string{}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			coordinationDecideExec(t, f.s, "DELETE FROM schema_migrations WHERE version>?", tc.version)
			coordinationDecideExec(t, f.s, "DROP TABLE "+tc.table)
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, _, err := messageSourceInTx(ctx, tx, message.ID); err == nil {
				t.Fatal("explicit historical schema storage damage was silently downgraded to absent provenance")
			}
		})
	}
}

func TestWorkspaceOperationSourceProjectionPlan(t *testing.T) {
	f := setupMessages(t)
	message, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, ActorID: "user", Content: "来源计划", RecipientIDs: []string{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	workspaceOperationInstallSchema(t, f.s)
	for version := len(migrations) + 1; version <= 21; version++ {
		coordinationDecideExec(t, f.s, "INSERT INTO schema_migrations(version) VALUES(?)", version)
	}
	r := assignmentQueryStore(t, f.path)
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, _, err := messageSourceInTx(ctx, tx, message.ID); err != nil {
		t.Fatal(err)
	}
	var projection assignmentQuery
	for _, q := range r.queries {
		if strings.Contains(q.query, "FROM messages m LEFT JOIN message_command_sources") {
			projection = q
		}
	}
	rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+projection.query, projection.args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		t.Log(detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"wb", "wf", "wo"} {
		indexed := false
		for _, detail := range plan {
			if strings.Contains(detail, "SEARCH "+alias+" USING INDEX sqlite_autoindex_workspace_") {
				indexed = true
			}
		}
		if !indexed {
			t.Errorf("new source join %s is not an indexed exact-key lookup: %v", alias, plan)
		}
	}
}

func TestWorkspaceOperationSourceHistoryGateAndBrokenV21(t *testing.T) {
	for _, history := range []string{"legacy", "v21"} {
		t.Run(history, func(t *testing.T) {
			f := setupMessages(t)
			message, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, ActorID: "user", Content: "普通历史消息", RecipientIDs: []string{}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			workspaceOperationInstallSchema(t, f.s)
			// Source-only fault fixture: declare appended history, without supplying
			// any grant, role, operation or filesystem authority to a public method.
			if history == "v21" {
				for version := len(migrations) + 1; version <= 21; version++ {
					coordinationDecideExec(t, f.s, "INSERT INTO schema_migrations(version) VALUES(?)", version)
				}
			} else {
				coordinationDecideExec(t, f.s, "DELETE FROM schema_migrations WHERE version>=21")
			}
			coordinationDecideExec(t, f.s, "DROP TABLE workspace_operation_receipts")
			r := assignmentQueryStore(t, f.path)
			tx, err := r.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			_, _, err = messageSourceInTx(ctx, tx, message.ID)
			if (err != nil) != (history == "v21") {
				t.Fatalf("source gate swallowed broken v21 storage or touched legacy tables: history=%s, err=%v", history, err)
			}
			var projections []string
			for _, q := range r.queries {
				if strings.Contains(q.query, "FROM messages m LEFT JOIN message_command_sources") {
					projections = append(projections, q.query)
				}
			}
			if len(projections) != 1 || strings.Contains(projections[0], "workspace_operations") != (history == "v21") {
				t.Fatalf("source projection is not gated by explicit v21 history: %+v", projections)
			}
		})
	}
}
