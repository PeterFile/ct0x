package store

import (
	"fmt"
	"strings"
	"testing"
)

func TestCoordinatorSchemaWithoutRowID(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	m, foreign := member(t, s, g.ID), member(t, s, other.ID)
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g.ID, 1, m.ID}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g.ID, 1, "[]"}},
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{other.ID, 1, foreign.ID}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{other.ID, 1, "[]"}},
		coordinatorSQL{coordinatorBindingInsert, []any{g.ID, 1}},
		coordinatorSQL{coordinatorPolicyInsert, []any{g.ID, 1}},
	)
	for _, table := range []struct {
		name, columns string
		args          []any
	}{
		{"group_coordinator_binding_revisions", "group_id,revision,member_id", []any{other.ID, 2, foreign.ID}},
		{"group_dispatch_policy_revisions", "group_id,revision,allowed_worker_ids", []any{other.ID, 2, "[]"}},
		{"group_coordinator_bindings", "group_id,revision", []any{other.ID, 1}},
		{"group_dispatch_policies", "group_id,revision", []any{other.ID, 1}},
	} {
		for _, alias := range []string{"rowid", "_rowid_", "oid"} {
			t.Run(table.name+"/"+alias, func(t *testing.T) {
				rows, err := s.db.Query("SELECT " + alias + " FROM " + table.name)
				if err == nil {
					rows.Close()
					t.Errorf("%s exposes hidden identity %s", table.name, alias)
				} else if !strings.Contains(err.Error(), "no such column") {
					t.Fatalf("alias SELECT failed for an unrelated reason: %v", err)
				}
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				args := append([]any{1}, table.args...)
				query := fmt.Sprintf("INSERT OR REPLACE INTO %s(%s,%s) VALUES(%s)", table.name, alias, table.columns, strings.TrimSuffix(strings.Repeat("?,", len(args)), ","))
				if _, err := tx.Exec(query, args...); err == nil || !strings.Contains(err.Error(), "no column named "+alias) {
					t.Errorf("explicit hidden identity must be unavailable, not merely guarded: %v", err)
				}
			})
		}
	}
}
