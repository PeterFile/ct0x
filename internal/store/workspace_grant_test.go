package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func workspaceGrantInput(t *testing.T, s *Store) AuthorizeWorkspaceGrantInput {
	t.Helper()
	g := group(t, s)
	m := member(t, s, g.ID)
	return AuthorizeWorkspaceGrantInput{GroupID: g.ID, MemberID: m.ID, Root: t.TempDir(), Scope: "read_only", IdempotencyKey: "authorize"}
}

func workspaceGrantValidation(input AuthorizeWorkspaceGrantInput) func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
	return func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
		return WorkspaceGrantDirectory{ResolvedRoot: input.Root, ResourceKey: "workspace:1:2"}, nil
	}
}

func TestWorkspaceGrantDurableReplayAndIdentityIsolation(t *testing.T) {
	s, path := openTestStore(t)
	input := workspaceGrantInput(t, s)
	first, err := s.AuthorizeWorkspaceGrant(ctx, input, workspaceGrantValidation(input))
	if err != nil || first.ID == "" || first.GroupID != input.GroupID || first.MemberID != input.MemberID || first.Root != input.Root ||
		first.ResolvedRoot != input.Root || first.ResourceKey != "workspace:1:2" || first.Scope != "read_only" || first.NodeID != "local" || first.AuthorizedBy != "user" || first.CreatedAt.IsZero() {
		t.Fatalf("invalid durable authorization: %+v, %v", first, err)
	}
	m, err := s.GetMember(ctx, input.GroupID, input.MemberID)
	if err != nil || m.Workspace != input.Root {
		t.Fatalf("authorization did not atomically select the directory: %+v, %v", m, err)
	}
	if _, err := s.db.Exec("UPDATE members SET workspace='later-preference' WHERE id=?", input.MemberID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	neverValidate := func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
		t.Fatal("replay must return the historical receipt, not reauthorize a changed filesystem")
		return WorkspaceGrantDirectory{}, nil
	}
	got, err := s.AuthorizeWorkspaceGrant(ctx, input, neverValidate)
	if err != nil || got != first {
		t.Fatalf("reopen lost exact replay: %+v, %v", got, err)
	}
	m, err = s.GetMember(ctx, input.GroupID, input.MemberID)
	if err != nil || m.Workspace != "later-preference" {
		t.Fatalf("historical replay reset a later preference: %+v, %v", m, err)
	}
	changed := input
	changed.Root += string(filepath.Separator)
	if _, err := s.AuthorizeWorkspaceGrant(ctx, changed, neverValidate); !errors.Is(err, ErrConflict) {
		t.Fatalf("exact path payload conflict was ignored: %v", err)
	}
	changed = input
	changed.MemberID = member(t, s, input.GroupID).ID
	if _, err := s.AuthorizeWorkspaceGrant(ctx, changed, neverValidate); !errors.Is(err, ErrConflict) {
		t.Fatalf("same-group key crossed member identity: %v", err)
	}
	foreign := group(t, s)
	changed.GroupID, changed.MemberID = foreign.ID, input.MemberID
	if _, err := s.AuthorizeWorkspaceGrant(ctx, changed, neverValidate); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign member reached the replay lookup: %v", err)
	}
	if _, err := s.GetWorkspaceGrant(ctx, foreign.ID, input.MemberID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign receipt leaked: %v", err)
	}
	changed.MemberID = member(t, s, foreign.ID).ID
	other, err := s.AuthorizeWorkspaceGrant(ctx, changed, workspaceGrantValidation(changed))
	if err != nil || other.ID == first.ID || other.GroupID != foreign.ID {
		t.Fatalf("group-scoped key was not isolated: %+v, %v", other, err)
	}
	changed = input
	changed.IdempotencyKey = "replacement"
	if _, err := s.AuthorizeWorkspaceGrant(ctx, changed, neverValidate); !errors.Is(err, ErrConflict) {
		t.Fatalf("D1 silently replaced an immutable recipient grant: %v", err)
	}
	for _, sql := range []string{"UPDATE workspace_grants SET scope='read_write'", "DELETE FROM workspace_grants"} {
		if _, err := s.db.Exec(sql); err == nil {
			t.Fatalf("receipt is not immutable: %s", sql)
		}
	}
}

func TestWorkspaceGrantTransactionFailuresLeaveNoAuthority(t *testing.T) {
	for _, fault := range []string{"validation", "insert", "preference", "receipt-ignore", "receipt-rewrite", "commit"} {
		t.Run(fault, func(t *testing.T) {
			s, _ := openTestStore(t)
			input := workspaceGrantInput(t, s)
			before, _ := s.GetMember(ctx, input.GroupID, input.MemberID)
			validate := workspaceGrantValidation(input)
			var trigger string
			switch fault {
			case "validation":
				validate = func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
					return WorkspaceGrantDirectory{}, ErrInvalid
				}
			case "insert":
				trigger = "BEFORE INSERT ON workspace_grants BEGIN SELECT RAISE(IGNORE); END"
			case "preference":
				trigger = "BEFORE UPDATE OF workspace ON members BEGIN SELECT RAISE(IGNORE); END"
			case "receipt-ignore":
				trigger = "BEFORE INSERT ON mutation_idempotency BEGIN SELECT RAISE(IGNORE); END"
			case "receipt-rewrite":
				trigger = "AFTER INSERT ON mutation_idempotency BEGIN UPDATE mutation_idempotency SET record_id='wrong' WHERE operation=NEW.operation; END"
			case "commit":
				if _, err := s.db.Exec("CREATE TABLE grant_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					t.Fatal(err)
				}
				trigger = "AFTER UPDATE OF workspace ON members BEGIN INSERT INTO grant_commit_guard VALUES('missing'); END"
			}
			if trigger != "" {
				if _, err := s.db.Exec("CREATE TRIGGER grant_fault " + trigger); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := s.AuthorizeWorkspaceGrant(ctx, input, validate); err == nil || got.ID != "" {
				t.Fatalf("partial authorization invented success: %+v, %v", got, err)
			}
			for _, table := range []string{"workspace_grants", "mutation_idempotency"} {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("failed authorization left %s rows: %d, %v", table, count, err)
				}
			}
			after, err := s.GetMember(ctx, input.GroupID, input.MemberID)
			if err != nil || after != before {
				t.Fatalf("failed authorization changed member: %+v, %v", after, err)
			}
		})
	}
}

func TestWorkspaceGrantV15UpgradePreservesUnknownClaims(t *testing.T) {
	s, path := func() (*Store, string) {
		original := migrations
		migrations = original[:14]
		defer func() { migrations = original }()
		return openTestStore(t)
	}()
	input := workspaceGrantInput(t, s)
	d := queueDelivery(t, s, input.GroupID, input.MemberID, "unknown")
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: input.GroupID, DeliveryID: d.ID, ResourceKeys: []string{"workspace:1:2"}, MaxActive: 3})
	if err != nil {
		t.Fatal(err)
	}
	turn, err = s.ApplyTurnReceipt(ctx, TurnClaim{GroupID: input.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown})
	if err != nil || turn.State != domain.TurnStateUnknown {
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
		messageSequencePreserved(t, reopened, before)
		current := messageSequenceRows(t, reopened.db, assignmentSchemaQuery)
		for _, object := range objects {
			if !slices.ContainsFunc(current, func(got []any) bool { return reflect.DeepEqual(got, object) }) {
				t.Fatalf("migration changed an existing schema object: %v", object)
			}
		}
		messageSequenceLedger(t, reopened, 15)
		var grants int
		if err := reopened.db.QueryRow("SELECT count(*) FROM workspace_grants").Scan(&grants); err != nil || grants != 0 {
			t.Fatal("migration inferred authorization from a member preference")
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
