package store

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWorkspaceWriteGrantDurableReplayAndIdentityIsolation(t *testing.T) {
	testWorkspaceGrantDurableReplayAndIdentityIsolation(t, "read_write")
}

func TestWorkspaceWriteGrantTransactionFailuresLeaveNoAuthority(t *testing.T) {
	testWorkspaceGrantTransactionFailuresLeaveNoAuthority(t, "read_write")
}

func TestWorkspaceWriteGrantScopeIsImmutablePayload(t *testing.T) {
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
			changed := input
			if initial == "read_only" {
				changed.Scope = "read_write"
			} else {
				changed.Scope = "read_only"
			}
			never := func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
				t.Fatal("scope replacement reached physical validation")
				return WorkspaceGrantDirectory{}, nil
			}
			for _, key := range []string{input.IdempotencyKey, "new-scope-key"} {
				changed.IdempotencyKey = key
				if got, err := s.AuthorizeWorkspaceGrant(ctx, changed, never); !errors.Is(err, ErrConflict) || got.ID != "" {
					t.Fatalf("scope replacement accepted: %+v, %v", got, err)
				}
			}
			if got, err := s.GetWorkspaceGrant(ctx, input.GroupID, input.MemberID); err != nil || got != first {
				t.Fatalf("immutable receipt changed: %+v, %v", got, err)
			}
			messageSequencePreserved(t, s, before)
		})
	}
}

func TestWorkspaceWriteGrantInactiveMemberKeepsHistoricalReplay(t *testing.T) {
	for _, grantScope := range []string{"read_only", "read_write"} {
		t.Run(grantScope, func(t *testing.T) {
			s, _ := openTestStore(t)
			input := workspaceGrantInput(t, s)
			input.Scope = grantScope
			first, err := s.AuthorizeWorkspaceGrant(ctx, input, workspaceGrantValidation(input))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE members SET status='left',workspace='later-preference' WHERE id=?", input.MemberID); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, s)
			never := func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
				t.Fatal("inactive member reached physical validation")
				return WorkspaceGrantDirectory{}, nil
			}
			if got, err := s.AuthorizeWorkspaceGrant(ctx, input, never); err != nil || got != first {
				t.Fatalf("inactive historical replay was rejected: %+v, %v", got, err)
			}
			changed := input
			changed.Root += "/"
			if _, err := s.AuthorizeWorkspaceGrant(ctx, changed, never); !errors.Is(err, ErrConflict) {
				t.Fatalf("inactive status hid same-key payload conflict: %v", err)
			}
			changed = input
			changed.IdempotencyKey = "inactive-new-key"
			if _, err := s.AuthorizeWorkspaceGrant(ctx, changed, never); !errors.Is(err, ErrNotFound) {
				t.Fatalf("new inactive authorization was not rejected: %v", err)
			}
			messageSequencePreserved(t, s, before)
			fresh := member(t, s, input.GroupID)
			if _, err := s.db.Exec("UPDATE members SET status='left' WHERE id=?", fresh.ID); err != nil {
				t.Fatal(err)
			}
			changed.MemberID = fresh.ID
			if _, err := s.AuthorizeWorkspaceGrant(ctx, changed, never); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ungranted inactive member obtained authority: %v", err)
			}
			changed.IdempotencyKey = input.IdempotencyKey
			if _, err := s.AuthorizeWorkspaceGrant(ctx, changed, never); !errors.Is(err, ErrConflict) {
				t.Fatalf("same-key lookup moved behind active status check: %v", err)
			}
		})
	}
}

func TestWorkspaceWriteGrantConcurrentScopesAndReplayAcrossHandles(t *testing.T) {
	s, path := openTestStore(t)
	read := workspaceGrantInput(t, s)
	write := read
	write.Scope, write.Root, write.IdempotencyKey = "read_write", read.Root+"-write", "authorize-write"
	handles := []*Store{queueHandle(t, path, false), queueHandle(t, path, false)}
	type result struct {
		input   AuthorizeWorkspaceGrantInput
		receipt WorkspaceGrantReceipt
		err     error
	}
	start, results := make(chan struct{}), make(chan result, 8)
	var validations atomic.Int32
	for i := range 8 {
		input := read
		if i%2 != 0 {
			input = write
		}
		go func() {
			<-start
			receipt, err := handles[(i/2)%2].AuthorizeWorkspaceGrant(ctx, input, func(existing []WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
				validations.Add(1)
				if len(existing) != 0 {
					return WorkspaceGrantDirectory{}, ErrConflict
				}
				return workspaceGrantValidation(input)(existing)
			})
			results <- result{input, receipt, err}
		}()
	}
	close(start)
	var winner WorkspaceGrantReceipt
	var success, conflicts int
	for range 8 {
		r := <-results
		if r.err != nil {
			if !errors.Is(r.err, ErrConflict) || r.receipt.ID != "" {
				t.Errorf("unexpected concurrent result: %+v, %v", r.receipt, r.err)
			}
			conflicts++
			continue
		}
		if winner.ID == "" {
			winner = r.receipt
		}
		if r.receipt != winner || r.receipt.Scope != r.input.Scope || r.receipt.Root != r.input.Root {
			t.Errorf("same-key replay diverged: %+v, want %+v", r.receipt, winner)
		}
		success++
	}
	if success != 4 || conflicts != 4 || validations.Load() != 1 {
		t.Fatalf("competing scopes did not serialize: success=%d conflicts=%d validations=%d", success, conflicts, validations.Load())
	}
	if got, err := s.GetWorkspaceGrant(ctx, read.GroupID, read.MemberID); err != nil || got != winner {
		t.Fatalf("merged member read lost the winner: %+v, %v", got, err)
	}
	m, err := s.GetMember(ctx, read.GroupID, read.MemberID)
	if err != nil || m.Workspace != winner.Root {
		t.Fatalf("losing grant changed selected directory: %+v, %v", m, err)
	}
	var grants, keys int
	err = s.db.QueryRow(`SELECT (SELECT count(*) FROM workspace_grants)+(SELECT count(*) FROM workspace_write_grants),
	 (SELECT count(*) FROM mutation_idempotency WHERE operation='authorize_workspace_grant')`).Scan(&grants, &keys)
	if err != nil || grants != 1 || keys != 1 {
		t.Fatalf("race left extra evidence: grants=%d keys=%d err=%v", grants, keys, err)
	}
}

func TestWorkspaceWriteGrantCompleteRootsReachValidation(t *testing.T) {
	s, _ := openTestStore(t)
	var expected []WorkspaceGrantReceipt
	for _, grantScope := range []string{"read_write", "read_only", "read_write"} {
		input := workspaceGrantInput(t, s)
		input.Scope = grantScope
		called := false
		receipt, err := s.AuthorizeWorkspaceGrant(ctx, input, func(existing []WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
			called = true
			if !slices.Equal(existing, expected) {
				t.Fatalf("physical validation omitted or changed durable roots: got=%+v want=%+v", existing, expected)
			}
			return WorkspaceGrantDirectory{ResolvedRoot: input.Root + "-resolved", ResourceKey: "workspace:" + input.MemberID}, nil
		})
		if err != nil || !called {
			t.Fatalf("new grant bypassed full-root validation: %+v, %v", receipt, err)
		}
		if receipt.Root != input.Root || receipt.ResolvedRoot != input.Root+"-resolved" || receipt.ResourceKey != "workspace:"+input.MemberID || receipt.Scope != grantScope || receipt.NodeID != "local" || receipt.AuthorizedBy != "user" {
			t.Fatalf("trusted directory identity was changed: %+v", receipt)
		}
		expected = append(expected, receipt)
		slices.SortFunc(expected, func(a, b WorkspaceGrantReceipt) int {
			if byGroup := strings.Compare(a.GroupID, b.GroupID); byGroup != 0 {
				return byGroup
			}
			return strings.Compare(a.MemberID, b.MemberID)
		})
		if got, err := s.WorkspaceGrantSnapshot(ctx); err != nil || !reflect.DeepEqual(got, expected) {
			t.Fatalf("complete snapshot lost evidence: got=%+v want=%+v err=%v", got, expected, err)
		}
		if got, err := s.GetWorkspaceGrant(ctx, input.GroupID, input.MemberID); err != nil || got != receipt {
			t.Fatalf("member read lost evidence: %+v, %v", got, err)
		}
	}
	input := workspaceGrantInput(t, s)
	before := messageSequenceSnapshot(t, s)
	if _, err := s.AuthorizeWorkspaceGrant(ctx, input, func(existing []WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
		if !reflect.DeepEqual(existing, expected) {
			t.Fatalf("rejection callback did not see every root: %+v", existing)
		}
		return WorkspaceGrantDirectory{}, ErrConflict
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("physical validator rejection was ignored: %v", err)
	}
	messageSequencePreserved(t, s, before)
}
