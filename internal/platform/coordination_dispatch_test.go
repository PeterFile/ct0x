package platform

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestDispatchRequestPlatformBorrowedEngineAndTwoLocalUserMethods(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	lead, worker := f.member(t, f.groupID), f.member(t, f.groupID)
	engine := coordination.New(f.db)
	service := NewDispatchRequests(engine)
	if service.engine != engine || reflect.TypeOf(service).NumMethod() != 2 {
		t.Fatal("dispatch service must borrow the supplied Engine and expose only two operations without lifecycle")
	}
	if _, err := engine.SetCoordinator(ctx, f.groupID, lead.ID, 0, "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SetDispatchPolicy(ctx, f.groupID, []string{worker.ID}, 0, "shared"); err != nil {
		t.Fatal(err)
	}
	message, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.groupID, TopicID: " exact\ntopic ", ActorID: worker.ID, Content: "member-authored source"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := engine.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{TopicID: message.TopicID, MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "explicit local authorization", NewTask: &domain.NewTaskRequest{Title: "objective", Acceptance: "criteria"}}, "shared")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", " \n\t"} {
		got, err := service.RequestDispatch(ctx, f.groupID, admission.Task.ID, 1, 1, "reason", key)
		if !errors.Is(err, store.ErrInvalid) || got != (domain.DispatchRequest{}) {
			t.Fatalf("required key error not forwarded: %+v, %v", got, err)
		}
	}
	// Platform preserves trusted command bytes; HTTP alone restricts transport keys.
	reason, key := " reason\t\x00\r\n ", " \topaque\x00key "
	request, err := service.RequestDispatch(ctx, f.groupID, admission.Task.ID, 1, 1, reason, key)
	if err != nil || request.Reason != reason || request.RequestedBy != "user" || request.TaskID != admission.Task.ID || request.MandateID != admission.Mandate.ID || request.CoordinatorMemberID != lead.ID || request.TopicID != message.TopicID {
		t.Fatalf("trusted local-user command was changed: %+v, %v", request, err)
	}
	if got, err := engine.RequestDispatch(ctx, f.groupID, admission.Task.ID, 1, 1, reason, key); err != nil || got != request {
		t.Fatalf("service did not use the supplied Engine: %+v, %v", got, err)
	}
	stored, err := f.db.GetDispatchRequest(ctx, f.groupID, request.ID)
	if err != nil || stored.Request != request || stored.State.Status != domain.DispatchStatusPending || stored.State.Delivery != nil {
		t.Fatalf("service did not use the borrowed Store or started work: %+v, %v", stored, err)
	}
	if _, err := engine.SetDispatchPolicy(ctx, f.groupID, nil, 1, "clear"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.raw.Exec("UPDATE members SET status='left' WHERE group_id=?", f.groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.raw.Exec("UPDATE coordination_dispatch_state SET generation=2,status='held',held_reason='no_eligible_worker' WHERE request_id=?", request.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := service.RequestDispatch(ctx, f.groupID, admission.Task.ID, 1, 1, reason, key); err != nil || got != request {
		t.Fatalf("historical request revalidated current configuration: %+v, %v", got, err)
	}
	want, err := engine.GetDispatchRequest(ctx, f.groupID, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := service.GetDispatchRequest(ctx, f.groupID, request.ID); err != nil || !reflect.DeepEqual(got, want) || got.State.Generation != 2 || got.State.Status != domain.DispatchStatusHeld {
		t.Fatalf("service failed to observe shared current state: %+v, %v", got, err)
	}
	if got, err := service.RequestDispatch(ctx, f.groupID, admission.Task.ID, 1, 1, reason+" ", key); !errors.Is(err, store.ErrConflict) || got != (domain.DispatchRequest{}) {
		t.Fatalf("changed-payload conflict not forwarded: %+v, %v", got, err)
	}
	if got, err := service.GetDispatchRequest(ctx, f.otherID, request.ID); !errors.Is(err, store.ErrNotFound) || !reflect.ValueOf(got).IsZero() {
		t.Fatalf("foreign request scope not forwarded: %+v, %v", got, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := service.RequestDispatch(cancelled, f.groupID, admission.Task.ID, 1, 1, reason, key); !errors.Is(err, context.Canceled) || got != (domain.DispatchRequest{}) {
		t.Fatalf("cancellation not forwarded: %+v, %v", got, err)
	}
	if got, err := service.GetDispatchRequest(cancelled, f.groupID, request.ID); !errors.Is(err, context.Canceled) || !reflect.ValueOf(got).IsZero() {
		t.Fatalf("read cancellation not forwarded: %+v, %v", got, err)
	}
	for table, want := range map[string]int{"messages": 1, "deliveries": 0, "turns": 0, "tasks": 1, "mandates": 1, "resource_claims": 0, "coordination_dispatch_requests": 1, "coordination_dispatch_state": 1} {
		var count int
		if err := f.raw.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("unexpected %s count %d, %v; want %d", table, count, err, want)
		}
	}
	if _, err := f.db.GetTask(ctx, f.groupID, admission.Task.ID); err != nil {
		t.Fatalf("service took ownership of Store: %v", err)
	}
}
