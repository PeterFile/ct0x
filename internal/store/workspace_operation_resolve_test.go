package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestWorkspaceOperationRoleResolutionAndPublicationFence(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	current, grant, err := f.s.ResolveWorkspaceForRole(ctx, role)
	if err != nil || current.TaskID != role.Provenance.TaskID || current.GroupID != f.groupID || grant.MemberID != f.worker || grant.GrantSchemaVersion != 19 {
		t.Fatalf("current trusted role resolution: %+v,%+v,%v", current, grant, err)
	}
	historical, err := f.s.GetTaskWorkspaceScope(ctx, f.groupID, current.TaskID)
	if err != nil || !reflect.DeepEqual(current, historical) {
		t.Fatalf("role resolution fabricated a scope: %+v,%+v,%v", current, historical, err)
	}
	o := workspaceOperationBegin(t, f.s, role, "op")
	if _, _, err := f.s.ResolveWorkspaceOperationForPublication(ctx, role, o.ID); err == nil {
		t.Fatal("pending operation became permission to publish")
	}
	done, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, WorkspaceOperationResult{Outcome: "done", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize})
	if err != nil {
		t.Fatal(err)
	}
	resolved, exact, err := f.s.ResolveWorkspaceOperationForPublication(ctx, role, o.ID)
	if err != nil || !reflect.DeepEqual(resolved, done) || exact != grant {
		t.Fatalf("publication path provenance: %+v,%+v,%v", resolved, exact, err)
	}
	for _, name := range []string{"role", "publication"} {
		r := assignmentQueryStore(t, f.path)
		if name == "role" {
			_, _, err = r.s.ResolveWorkspaceForRole(ctx, role)
		} else {
			_, _, err = r.s.ResolveWorkspaceOperationForPublication(ctx, role, o.ID)
		}
		if err != nil || len(r.queries) < 2 || !strings.HasPrefix(r.queries[0].query, "UPDATE turns SET claim_token=claim_token") {
			t.Fatalf("readonly seam lacks same-transaction first SQL fence: %s,%v,%+v", name, err, r.queries)
		}
	}
	bad := role
	bad.AttemptID = "other-attempt"
	if _, _, err := f.s.ResolveWorkspaceOperationForPublication(ctx, bad, o.ID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("foreign attempt resolved source path: %v", err)
	}
	if _, err := f.s.RequestTurnCancel(ctx, f.groupID, o.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.ResolveWorkspaceForRole(ctx, role); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("post-IO role fence ignored cancellation: %v", err)
	}
}
