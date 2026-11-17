package store

import "testing"

// This only installs the independent v21 DDL on v19. SQLite permits forward
// references; no fabricated v20 scope or public write authority is installed.
func workspaceOperationInstallSchema(t *testing.T, s *Store) {
	t.Helper()
	var exists bool
	if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='workspace_operations')").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := s.db.Exec(workspaceOperationSchemaV21); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceOperationV21Registered(t *testing.T) {
	if len(migrations) < 21 || migrations[20] != workspaceOperationSchemaV21 || migrations[19] != chatWorkspaceSchemaV20 {
		t.Fatal("workspace operation schema must be appended as v21 after the frozen v20, without rewriting history")
	}
}

func TestWorkspaceOperationIndependentSchema(t *testing.T) {
	s, _ := openTestStore(t)
	workspaceOperationInstallSchema(t, s)
	for _, name := range []string{"workspace_operations", "workspace_operation_receipts", "worker_artifact_publications"} {
		var exists bool
		if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name=? AND type='table')", name).Scan(&exists); err != nil || !exists {
			t.Fatalf("missing durable operation/publication table %s: %v", name, err)
		}
		for _, suffix := range []string{"insert", "no_update", "no_delete"} {
			var guarded bool
			if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name=? AND type='trigger')", name+"_"+suffix).Scan(&guarded); err != nil || !guarded {
				t.Fatalf("missing immutable SQL guard %s_%s: %v", name, suffix, err)
			}
		}
	}
	var notices int
	if err := s.db.QueryRow("SELECT count(*) FROM pragma_table_info('workspace_operations') WHERE name='notice_message_id'").Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("intent progress notice is not part of immutable intent: %d, %v", notices, err)
	}
}

func TestWorkspaceOperationTrustedAuditIsScoped(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	workspaceOperationInstallSchema(t, f.s)
	if unresolved, err := f.s.HasUnresolvedWorkspaceOperations(ctx, claim.GroupID, claim.TurnID); err != nil || unresolved {
		t.Fatalf("empty real Turn audit: unresolved=%t, err=%v", unresolved, err)
	}
	if _, err := f.s.HasUnresolvedWorkspaceOperations(ctx, f.otherID, claim.TurnID); err == nil {
		t.Fatal("cross-group Turn audit succeeded")
	}
	if page, err := f.s.ListWorkspaceOperationsForTurn(ctx, claim.GroupID, claim.TurnID, ListOptions{}); err != nil || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("empty bounded Turn audit: %+v, %v", page, err)
	}
}
