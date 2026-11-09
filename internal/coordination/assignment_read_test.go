package coordination

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type engineAssignmentReadAPI interface {
	GetDispatchDecision(context.Context, string, string) (domain.DecisionReceipt, error)
	GetAssignment(context.Context, string, string) (domain.AssignmentView, error)
}

func TestEngineAssignmentTrustedReads(t *testing.T) {
	s, r, scope, worker, observer := engineDecideFixture(t)
	api, ok := any(New(s)).(engineAssignmentReadAPI)
	if !ok {
		t.Fatal("Engine lacks trusted exact Decision/Assignment reads")
	}
	ctx := context.Background()
	receipt, err := s.DecideDispatch(ctx, scope, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: worker, Reason: "安排"}, "decision")
	if err != nil {
		t.Fatalf("Store fixture: %v, cause=%v", err, errors.Unwrap(err))
	}
	want, err := s.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := engineCoordinatorSnapshot(t, observer)
	got, err := api.GetDispatchDecision(ctx, r.GroupID, r.ID)
	if err != nil || !reflect.DeepEqual(got, receipt) {
		t.Fatalf("decision forwarding: %+v, %v", got, err)
	}
	view, err := api.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
	if err != nil || !reflect.DeepEqual(view, want) {
		t.Fatalf("assignment forwarding: %+v, %v", view, err)
	}
	for _, id := range []string{"missing", " bad"} {
		_, err := api.GetAssignment(ctx, r.GroupID, id)
		wantErr := store.ErrNotFound
		if id == " bad" {
			wantErr = store.ErrInvalid
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("error forwarding: %v", err)
		}
	}
	if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, observer)) {
		t.Fatal("Engine reads wrote rows")
	}
}
