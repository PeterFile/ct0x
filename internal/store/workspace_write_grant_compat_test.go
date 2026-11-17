package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func workspaceWriteGrantV18(t *testing.T) (*Store, string) {
	t.Helper()
	original := migrations
	defer func() { migrations = original }()
	migrations = original[:18]
	s, path := openTestStore(t)
	messageSequenceLedger(t, s, 18)
	return s, path
}

func TestWorkspaceWriteGrantLegacySchemaKeepsReadOnly(t *testing.T) {
	s, _ := workspaceWriteGrantV18(t)
	input := workspaceGrantInput(t, s)
	first, err := s.AuthorizeWorkspaceGrant(ctx, input, workspaceGrantValidation(input))
	if err != nil {
		t.Fatal(err)
	}
	never := func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
		t.Fatal("legacy replay or unsupported write request reached physical validation")
		return WorkspaceGrantDirectory{}, nil
	}
	if got, err := s.AuthorizeWorkspaceGrant(ctx, input, never); err != nil || got != first {
		t.Fatalf("legacy read-only replay failed: %+v, %v", got, err)
	}
	if got, err := s.GetWorkspaceGrant(ctx, input.GroupID, input.MemberID); err != nil || got != first {
		t.Fatalf("legacy read-only member read failed: %+v, %v", got, err)
	}
	if got, err := s.WorkspaceGrantSnapshot(ctx); err != nil || len(got) != 1 || got[0] != first {
		t.Fatalf("legacy read-only snapshot failed: %+v, %v", got, err)
	}
	changed := input
	changed.Scope = "read_write"
	if _, err := s.AuthorizeWorkspaceGrant(ctx, changed, never); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy same-key payload conflict was hidden by schema support: %v", err)
	}
	write := workspaceGrantInput(t, s)
	write.Scope = "read_write"
	before := messageSequenceSnapshot(t, s)
	if got, err := s.AuthorizeWorkspaceGrant(ctx, write, never); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "不支持") || got.ID != "" {
		t.Fatalf("legacy read-write support was not explicitly rejected: %+v, %v", got, err)
	}
	messageSequencePreserved(t, s, before)
	write.Scope = "read_only"
	if _, err := s.AuthorizeWorkspaceGrant(ctx, write, workspaceGrantValidation(write)); err != nil {
		t.Fatalf("unsupported write request consumed the key or broke legacy writes: %v", err)
	}
	if got, err := s.GetWorkspaceGrant(ctx, input.GroupID, "missing"); !errors.Is(err, ErrNotFound) || got.ID != "" {
		t.Fatalf("legacy missing member read changed: %+v, %v", got, err)
	}
}

func TestWorkspaceWriteGrantSQLFailuresAreNotAbsence(t *testing.T) {
	for _, damage := range []string{"legacy-read-table", "current-write-table", "current-write-columns"} {
		t.Run(damage, func(t *testing.T) {
			s, _ := openTestStore(t)
			if damage == "legacy-read-table" {
				s, _ = workspaceWriteGrantV18(t)
			}
			input := workspaceGrantInput(t, s)
			before, err := s.GetMember(ctx, input.GroupID, input.MemberID)
			if err != nil {
				t.Fatal(err)
			}
			table := "workspace_write_grants"
			if damage == "legacy-read-table" {
				table = "workspace_grants"
			}
			if _, err := s.db.Exec("DROP TABLE " + table); err != nil {
				t.Fatal(err)
			}
			if damage == "current-write-columns" {
				if _, err := s.db.Exec("CREATE TABLE workspace_write_grants(id TEXT)"); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := s.GetWorkspaceGrant(ctx, input.GroupID, input.MemberID); err == nil || errors.Is(err, ErrNotFound) || got.ID != "" {
				t.Fatalf("SQL failure became missing member grant: %+v, %v", got, err)
			}
			if got, err := s.WorkspaceGrantSnapshot(ctx); err == nil || got != nil {
				t.Fatalf("SQL failure became an empty root set: %+v, %v", got, err)
			}
			if got, err := s.AuthorizeWorkspaceGrant(ctx, input, func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
				t.Fatal("SQL failure was downgraded before physical validation")
				return WorkspaceGrantDirectory{}, nil
			}); err == nil || errors.Is(err, ErrNotFound) || got.ID != "" {
				t.Fatalf("SQL failure became new grant eligibility: %+v, %v", got, err)
			}
			after, err := s.GetMember(ctx, input.GroupID, input.MemberID)
			if err != nil || after != before {
				t.Fatalf("SQL failure changed member preference: %+v, %v", after, err)
			}
			var keys int
			if err := s.db.QueryRow("SELECT count(*) FROM mutation_idempotency WHERE operation='authorize_workspace_grant'").Scan(&keys); err != nil || keys != 0 {
				t.Fatalf("SQL failure reserved an authority key: %d, %v", keys, err)
			}
		})
	}
	s, _ := openTestStore(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetWorkspaceGrant(cancelled, "group", "member"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled member read became absence: %v", err)
	}
	if got, err := s.WorkspaceGrantSnapshot(cancelled); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("cancelled snapshot became an empty root set: %+v, %v", got, err)
	}
}
