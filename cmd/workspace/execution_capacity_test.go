package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func TestExecutionMaxActiveRoleComposition(t *testing.T) {
	ctx := t.Context()
	f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
	hold := filepath.Join(f.root, "hold")
	if err := os.WriteFile(hold, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// startRun uses the real CLI parser and executionRunner. Only the synthetic
	// Pi CLI is discovered, and its hold gate keeps accepted turns live.
	base, _ := startRun(t, append(memberToolsArgs(t, dir, f.path), "--enable-coordination-roles", "--execution-max-active=2")...)
	selected := executionProvider(t, base)

	type queuedRole struct {
		label, groupID string
		domain.DispatchDeliveryReceipt
	}
	queueRole := func(label string) queuedRole {
		t.Helper()
		// One coordinator per group; separate members, conversations and workspaces.
		group, err := db.CreateGroup(ctx, store.CreateGroupInput{Name: label})
		if err != nil {
			t.Fatal(err)
		}
		var coordinator, worker domain.Member
		for _, member := range []*domain.Member{&coordinator, &worker} {
			*member, err = db.CreateMember(ctx, store.CreateMemberInput{GroupID: group.ID, Name: "fixture", Role: "fixture", ProviderID: selected.ID, Model: "fixture-model"})
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.SetCoordinator(ctx, group.ID, coordinator.ID, 0, "coordinator"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SetDispatchPolicy(ctx, group.ID, []string{worker.ID}, 0, "workers"); err != nil {
			t.Fatal(err)
		}
		request := coordinationCompositionRequest(t, db, group.ID)
		setup, err := db.MaterializeDispatchRequest(ctx, request.ID, 1, time.Now().UTC())
		if err != nil || setup.Delivery == nil {
			t.Fatalf("%s: trusted coordinator delivery setup failed: %v", label, err)
		}
		return queuedRole{label, group.ID, *setup.Delivery}
	}
	delivery := func(role queuedRole) domain.Delivery {
		t.Helper()
		message, err := db.GetMessage(ctx, role.groupID, role.MessageID)
		if err != nil || len(message.Deliveries) != 1 || message.Deliveries[0].ID != role.DeliveryID {
			t.Fatalf("%s: missing coordinator delivery: %v", role.label, err)
		}
		return message.Deliveries[0]
	}
	turnState := func(role queuedRole) domain.TurnState {
		t.Helper()
		if id := delivery(role).TurnID; id != "" {
			turn, err := db.GetTurn(ctx, role.groupID, id)
			if err != nil {
				t.Fatal(err)
			}
			return turn.State
		}
		return ""
	}
	awaitTurn := func(role queuedRole, want domain.TurnState) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(25 * time.Millisecond) {
			got := turnState(role)
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: want turn state %q, got %q (empty means unclaimed)", role.label, want, got)
			}
		}
	}

	first := queueRole("first coordinator")
	awaitTurn(first, domain.TurnStateRunning)
	// Queue after acceptance, so the second role must pass an occupied fence.
	second := queueRole("second coordinator while first is held")
	awaitTurn(second, domain.TurnStateRunning)

	third := queueRole("third coordinator at capacity")
	var readiness platform.RoleReadinessInspection
	coordinationCompositionHTTP(t, http.MethodGet, coordinationCompositionReadinessURL(base, third.groupID, third.MessageID, third.DeliveryID), nil, "", http.StatusOK, &readiness)
	if readiness.Role != store.CoordinationRoleCoordinator || readiness.Blocked == nil || readiness.Blocked.Code != "role_runtime_capacity" {
		t.Fatalf("third coordinator did not observe a full role fence: role=%s blocked=%v", readiness.Role, readiness.Blocked)
	}
	if d := delivery(third); d.State != domain.DeliveryStateQueued || d.TurnID != "" {
		t.Fatal("third coordinator was claimed while capacity was full")
	}
	if turnState(first) != domain.TurnStateRunning || turnState(second) != domain.TurnStateRunning {
		t.Fatal("both trusted coordinator turns must still be running before release")
	}

	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	for _, role := range []queuedRole{first, second, third} {
		awaitTurn(role, domain.TurnStateSucceeded)
	}
}
