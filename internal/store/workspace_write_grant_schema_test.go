package store

import (
	"testing"
)

func TestWorkspaceWriteGrantSQLiteExclusionAndImmutability(t *testing.T) {
	for _, initial := range []string{"read_only", "read_write"} {
		t.Run(initial, func(t *testing.T) {
			s, _ := openTestStore(t)
			input := workspaceGrantInput(t, s)
			input.Scope = initial
			first, err := s.AuthorizeWorkspaceGrant(ctx, input, workspaceGrantValidation(input))
			if err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, s)
			for _, table := range []string{"workspace_grants", "workspace_write_grants"} {
				grantScope := "read_only"
				if table == "workspace_write_grants" {
					grantScope = "read_write"
				}
				if grantScope == initial && initial == "read_only" {
					continue // Preserve the original table's SQL semantics.
				}
				for _, insert := range []string{"INSERT", "INSERT OR REPLACE"} {
					for _, id := range []string{first.ID, "replacement"} {
						query := insert + " INTO " + table + "(" + workspaceGrantColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?)"
						if _, err := s.db.Exec(query, id, input.GroupID, input.MemberID, input.Root, first.ResolvedRoot, first.ResourceKey, grantScope, "local", "user", first.CreatedAt.Format(timestampLayout)); err == nil {
							t.Fatalf("SQLite admitted duplicate or cross-scope authority: %s id=%s", query, id)
						}
					}
				}
			}
			if initial == "read_write" {
				for _, query := range []string{
					"UPDATE workspace_write_grants SET root='/changed'",
					"DELETE FROM workspace_write_grants",
				} {
					if _, err := s.db.Exec(query); err == nil {
						t.Fatalf("SQLite changed immutable write authority: %s", query)
					}
				}
			}
			messageSequencePreserved(t, s, before)
			dispatchAssertFK(t, s)
		})
	}
}

func TestWorkspaceWriteGrantSQLiteIdentityAndAuthorityConstraints(t *testing.T) {
	s, _ := openTestStore(t)
	input := workspaceGrantInput(t, s)
	foreign := group(t, s)
	before := messageSequenceSnapshot(t, s)
	for _, tc := range []struct {
		name, group, member, scope, node, actor string
	}{
		{"foreign-member", foreign.ID, input.MemberID, "read_write", "local", "user"},
		{"missing-member", input.GroupID, "missing", "read_write", "local", "user"},
		{"read-only-scope", input.GroupID, input.MemberID, "read_only", "local", "user"},
		{"remote-node", input.GroupID, input.MemberID, "read_write", "remote", "user"},
		{"model-actor", input.GroupID, input.MemberID, "read_write", "local", input.MemberID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.db.Exec("INSERT INTO workspace_write_grants("+workspaceGrantColumns+") VALUES(?,?,?,?,?,?,?,?,?,?)",
				tc.name, tc.group, tc.member, input.Root, input.Root, "workspace:1:2", tc.scope, tc.node, tc.actor, "2026-11-17T00:00:00.000000000Z")
			if err == nil {
				t.Fatal("SQLite accepted invalid write authority")
			}
		})
	}
	messageSequencePreserved(t, s, before)
	input.Scope = "read_write"
	if _, err := s.AuthorizeWorkspaceGrant(ctx, input, workspaceGrantValidation(input)); err != nil {
		t.Fatalf("valid authority was not supported after rejected rows: %v", err)
	}
	dispatchAssertFK(t, s)
}
