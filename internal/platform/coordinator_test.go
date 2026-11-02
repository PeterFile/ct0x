package platform

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestCoordinatorConfigurationBorrowedEngineAndFourLocalUserMethods(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	a, b, c := f.member(t, f.groupID), f.member(t, f.groupID), f.member(t, f.groupID)
	engine := coordination.New(f.db)
	service := NewCoordinatorConfiguration(engine)
	if service.engine != engine || reflect.TypeOf(service).NumMethod() != 4 {
		t.Fatal("configuration must borrow the supplied Engine and expose only four operations")
	}
	for _, name := range []string{"GetCoordinator", "SetCoordinator", "GetDispatchPolicy", "SetDispatchPolicy"} {
		if _, ok := reflect.TypeOf(service).MethodByName(name); !ok {
			t.Fatalf("missing local-user operation %s", name)
		}
	}
	if _, err := service.GetCoordinator(ctx, f.groupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing binding: %v", err)
	}
	policy, err := service.GetDispatchPolicy(ctx, f.groupID)
	if err != nil || !reflect.DeepEqual(policy, domain.GroupDispatchPolicy{GroupID: f.groupID, AllowedWorkerIDs: []string{}}) {
		t.Fatalf("default policy forwarding: %+v, %v", policy, err)
	}
	for _, key := range []string{"", " \n\t"} {
		if _, err := service.SetCoordinator(ctx, f.groupID, a.ID, 0, key); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("coordinator required key: %v", err)
		}
		if _, err := service.SetDispatchPolicy(ctx, f.groupID, nil, 0, key); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("policy required key: %v", err)
		}
	}
	binding, err := service.SetCoordinator(ctx, f.groupID, a.ID, 0, "shared")
	if err != nil || binding != (domain.GroupCoordinatorBinding{GroupID: f.groupID, MemberID: a.ID, Revision: 1}) {
		t.Fatalf("coordinator forwarding: %+v, %v", binding, err)
	}
	workers := []string{c.ID, b.ID, c.ID}
	original := slices.Clone(workers)
	policy, err = service.SetDispatchPolicy(ctx, f.groupID, workers, 0, "shared")
	want := []string{b.ID, c.ID}
	slices.Sort(want)
	if err != nil || policy.Revision != 1 || !reflect.DeepEqual(policy.AllowedWorkerIDs, want) || !reflect.DeepEqual(workers, original) {
		t.Fatalf("policy forwarding modified caller data: %+v, %v", policy, err)
	}
	if got, err := engine.GetDispatchPolicy(ctx, f.groupID); err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("service did not use the borrowed Engine: %+v, %v", got, err)
	}
	if _, err := engine.SetDispatchPolicy(ctx, f.groupID, nil, 1, "clear"); err != nil {
		t.Fatal(err)
	}
	latest, err := engine.SetCoordinator(ctx, f.groupID, b.ID, 1, "replace")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := service.GetCoordinator(ctx, f.groupID); err != nil || got != latest {
		t.Fatalf("service did not observe shared Engine changes: %+v, %v", got, err)
	}
	if got, err := service.GetDispatchPolicy(ctx, f.groupID); err != nil || got.Revision != 2 || got.AllowedWorkerIDs == nil || len(got.AllowedWorkerIDs) != 0 {
		t.Fatalf("shared policy read: %+v, %v", got, err)
	}
	if got, err := service.SetCoordinator(ctx, f.groupID, a.ID, 0, "shared"); err != nil || got != binding {
		t.Fatalf("historical coordinator forwarding: %+v, %v", got, err)
	}
	if got, err := service.SetDispatchPolicy(ctx, f.groupID, []string{b.ID, c.ID}, 0, "shared"); err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("historical policy forwarding: %+v, %v", got, err)
	}
	if got, err := f.db.GetCoordinator(ctx, f.groupID); err != nil || got != latest {
		t.Fatalf("borrowed Store became unusable: %+v, %v", got, err)
	}
}
