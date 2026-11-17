package store

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Pin historical write-authority contracts to v19, not future migrations.
func workspaceWriteGrantV19Migrations(t *testing.T) {
	t.Helper()
	if len(migrations) < 19 {
		t.Fatalf("write authority requires appended v19; got %d migrations", len(migrations))
	}
	original := migrations
	migrations = original[:19]
	t.Cleanup(func() { migrations = original })
}

func TestWorkspaceWriteGrantV18ToV19PreservesEvidence(t *testing.T) {
	workspaceWriteGrantV19Migrations(t)
	s, path := workspaceWriteGrantV18(t)
	input := workspaceGrantInput(t, s)
	first, err := s.AuthorizeWorkspaceGrant(ctx, input, workspaceGrantValidation(input))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE members SET workspace='later-preference' WHERE id=?", input.MemberID); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, s)
	objects := messageSequenceRows(t, s.db, assignmentSchemaQuery)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reopened, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer reopened.Close()
			messageSequenceLedger(t, reopened, 19)
			messageSequencePreserved(t, reopened, before)
			current := messageSequenceRows(t, reopened.db, assignmentSchemaQuery)
			for _, object := range objects {
				if !slices.ContainsFunc(current, func(got []any) bool { return reflect.DeepEqual(got, object) }) {
					t.Fatalf("v19 rewrote a historical schema object: %v", object)
				}
			}
			var writes int
			if err := reopened.db.QueryRow("SELECT count(*) FROM workspace_write_grants").Scan(&writes); err != nil || writes != 0 {
				t.Fatalf("v19 inferred write authority from old evidence: %d, %v", writes, err)
			}
			if got, err := reopened.AuthorizeWorkspaceGrant(ctx, input, func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
				t.Fatal("migration caused physical reauthorization of an old receipt")
				return WorkspaceGrantDirectory{}, nil
			}); err != nil || got != first {
				t.Fatalf("v18 original-key replay changed: %+v, %v", got, err)
			}
			messageSequencePreserved(t, reopened, before)
			dispatchAssertFK(t, reopened)
		}()
	}
}

func TestWorkspaceWriteGrantV19MigrationRollbackAndRetry(t *testing.T) {
	workspaceWriteGrantV19Migrations(t)
	s, path := workspaceWriteGrantV18(t)
	input := workspaceGrantInput(t, s)
	first, err := s.AuthorizeWorkspaceGrant(ctx, input, workspaceGrantValidation(input))
	if err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, s)
	objects := messageSequenceRows(t, s.db, assignmentSchemaQuery)
	original := migrations
	defer func() { migrations = original }()
	migrations = slices.Clone(original)
	migrations[18] += "\nINSERT INTO missing_write_grant_migration_fixture VALUES(1);"
	if err := migrate(s.db); err == nil || !strings.Contains(err.Error(), "迁移版本 19") {
		t.Fatalf("failed v19 migration committed: %v", err)
	}
	messageSequenceLedger(t, s, 18)
	messageSequencePreserved(t, s, before)
	if got := messageSequenceRows(t, s.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
		t.Fatal("failed v19 migration leaked a table, trigger, or index")
	}
	if got, err := s.AuthorizeWorkspaceGrant(ctx, input, func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
		t.Fatal("failed migration caused reauthorization of an old receipt")
		return WorkspaceGrantDirectory{}, nil
	}); err != nil || got != first {
		t.Fatalf("failed migration broke historical replay: %+v, %v", got, err)
	}
	migrations = original
	if err := migrate(s.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, s, 19)
	messageSequencePreserved(t, s, before)
	input = workspaceGrantInput(t, s)
	input.Scope = "read_write"
	write, err := s.AuthorizeWorkspaceGrant(ctx, input, workspaceGrantValidation(input))
	if err != nil {
		t.Fatalf("retry did not enable explicit write authority: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	messageSequenceLedger(t, reopened, 19)
	if got, err := reopened.GetWorkspaceGrant(ctx, input.GroupID, input.MemberID); err != nil || got != write {
		t.Fatalf("retry/reopen lost write evidence: %+v, %v", got, err)
	}
	dispatchAssertFK(t, reopened)
}
