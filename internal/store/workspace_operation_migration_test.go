package store

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/provider"
)

func TestWorkspaceOperationV20ToV21AtomicMigration(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	if len(original) < 21 || original[20] != workspaceOperationSchemaV21 {
		t.Fatal("missing appended v21")
	}
	migrations = original[:20]
	f, receipt := chatWorkspaceAdmitted(t)
	delivery := queueDelivery(t, f.s, f.groupID, f.b, "migration-unknown")
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 8, ResourceKeys: []string{receipt.WorkspaceScope.ResourceKey}})
	if err != nil {
		t.Fatal(err)
	}
	claim := TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
	if _, err := f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	objects := messageSequenceRows(t, f.s.db, assignmentSchemaQuery)
	migrations = slices.Clone(original[:21])
	migrations[20] += "\nINSERT INTO f4_missing_migration_fixture VALUES(1);"
	if err := migrate(f.s.db); err == nil || !strings.Contains(err.Error(), "迁移版本 21") {
		t.Fatalf("partial v21 migration committed: %v", err)
	}
	messageSequenceLedger(t, f.s, 20)
	messageSequencePreserved(t, f.s, before)
	if got := messageSequenceRows(t, f.s.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
		t.Fatal("failed v21 leaked schema changes")
	}
	migrations = original[:21]
	if err := migrate(f.s.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, f.s, 21)
	messageSequencePreserved(t, f.s, before)
	for _, table := range []string{"workspace_operations", "workspace_operation_receipts", "worker_artifact_publications"} {
		var n int
		if err := f.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("v21 inferred execution/publication from historical scope: %s=%d,%v", table, n, err)
		}
	}
	current := messageSequenceRows(t, f.s.db, assignmentSchemaQuery)
	for _, old := range objects {
		if !slices.ContainsFunc(current, func(got []any) bool { return reflect.DeepEqual(got, old) }) {
			t.Fatalf("v21 rewrote frozen v1–20 object: %v", old)
		}
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	messageSequenceLedger(t, reopened, 21)
	messageSequencePreserved(t, reopened, before)
	if scope, err := reopened.GetTaskWorkspaceScope(ctx, f.groupID, receipt.TaskID); err != nil || !reflect.DeepEqual(scope, *receipt.WorkspaceScope) {
		t.Fatalf("v21/reopen changed F3 scope: %+v,%v", scope, err)
	}
	if held, err := reopened.HasResourceClaim(ctx, receipt.WorkspaceScope.ResourceKey); err != nil || !held {
		t.Fatalf("v21 released unknown Turn claim: %t,%v", held, err)
	}
	dispatchAssertFK(t, reopened)
}
