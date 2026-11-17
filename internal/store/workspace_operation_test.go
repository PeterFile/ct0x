package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestWorkspaceOperationFirstSQLAndTextTaskFailClosed(t *testing.T) {
	f, _, role := assignmentBoundWorker(t)
	workspaceOperationInstallSchema(t, f.s)
	for name, call := range map[string]func(*Store) error{
		"lookup": func(s *Store) error {
			_, _, err := s.LookupWorkspaceOperation(ctx, role, workspaceOperationInput(), "key")
			return err
		},
		"begin": func(s *Store) error {
			_, _, err := s.BeginWorkspaceOperation(ctx, role, workspaceOperationInput(), workspaceOperationPreimage(), "key")
			return err
		},
		"finish": func(s *Store) error {
			_, err := s.FinishWorkspaceOperation(ctx, role, "missing", WorkspaceOperationResult{Outcome: "unknown", ErrorCode: "io_unknown"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := assignmentQueryStore(t, f.path)
			if err := call(r.s); err == nil {
				t.Fatal("legacy text Task acquired workspace write authority")
			}
			if len(r.queries) < 2 || !strings.HasPrefix(r.queries[0].query, "UPDATE turns SET claim_token=claim_token") || !strings.Contains(r.queries[1].query, memberToolBinding) {
				t.Fatalf("operation did not first fence the live role in its own transaction: %+v", r.queries)
			}
			bad := role
			bad.Member.Claim.Token = "stale"
			_, _, err := r.s.BeginWorkspaceOperation(ctx, bad, workspaceOperationInput(), workspaceOperationPreimage(), "key")
			if !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("old generation bypassed scope fence: %v", err)
			}
		})
	}
}

func TestWorkspaceOperationRoleResolutionAPI(t *testing.T) {
	for _, name := range []string{"ResolveWorkspaceForRole", "ResolveWorkspaceOperationForPublication"} {
		if _, ok := reflect.TypeFor[*Store]().MethodByName(name); !ok {
			t.Fatalf("Store lacks %s: platform filesystem access requires same-transaction trusted role/scope/claim resolution", name)
		}
	}
}

func TestWorkspaceOperationStoreAPI(t *testing.T) {
	for _, name := range []string{"LookupWorkspaceOperation", "BeginWorkspaceOperation", "FinishWorkspaceOperation", "HasUnresolvedWorkspaceOperations", "ListWorkspaceOperationsForTurn"} {
		if _, ok := reflect.TypeFor[*Store]().MethodByName(name); !ok {
			t.Fatalf("Store lacks %s: durable intent and unresolved audit are required before workspace IO", name)
		}
	}
}
