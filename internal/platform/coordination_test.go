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

func TestCoordinationsBorrowedEngineAndFiveLocalUserMethods(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	engine := coordination.New(f.db)
	service := NewCoordinations(engine)
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: " exact\ntopic "}
	m, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: "source"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.RequestWorkRequest{TopicID: thread.TopicID, MessageID: m.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "明确授权", NewTask: &domain.NewTaskRequest{Title: "目标", Description: "描述", Acceptance: "验收"}}
	for _, key := range []string{"", " \n\t"} {
		if _, err := service.RequestWork(ctx, f.groupID, request, key); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("required key was not forwarded: %v", err)
		}
	}
	receipt, err := service.RequestWork(ctx, f.groupID, request, "local")
	if err != nil || receipt.Decision.ActorID != "user" || receipt.Mandate.AuthorizedBy != "user" {
		t.Fatalf("bound local-user admission: %+v, %v", receipt, err)
	}
	if got, err := engine.RequestWork(ctx, f.groupID, request, "local"); err != nil || !reflect.DeepEqual(got, receipt) {
		t.Fatalf("service rewrote the frozen request: %+v, %v", got, err)
	}
	if got, err := service.GetAdmissionReceipt(ctx, thread, receipt.Decision.ID); err != nil || !reflect.DeepEqual(got, receipt) {
		t.Fatalf("receipt read: %+v, %v", got, err)
	}
	view, err := service.GetTaskCoordination(ctx, f.groupID, receipt.Task.ID)
	if err != nil || !reflect.DeepEqual(view.Admission, receipt) || view.Coordination.CurrentMandateID != receipt.Mandate.ID || view.Coordination.AuthorityGeneration != 1 {
		t.Fatalf("task coordination read: %+v, %v", view, err)
	}
	trigger, err := service.GetTrigger(ctx, thread, receipt.Decision.TriggerID)
	if err != nil || trigger.MessageID != m.ID {
		t.Fatalf("trigger read: %+v, %v", trigger, err)
	}
	page, err := service.ListTriggers(ctx, thread, store.ListOptions{Limit: 1})
	if err != nil || !reflect.DeepEqual(page.Items, []domain.Trigger{trigger}) || page.NextCursor != nil {
		t.Fatalf("trigger list: %+v, %v", page, err)
	}
	foreign := domain.ThreadRef{GroupID: f.otherID, TopicID: thread.TopicID}
	if _, err := service.GetAdmissionReceipt(ctx, foreign, receipt.Decision.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign receipt: %v", err)
	}
	if _, err := service.GetTrigger(ctx, foreign, trigger.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign trigger: %v", err)
	}
	if _, err := service.GetTaskCoordination(ctx, f.otherID, receipt.Task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign task: %v", err)
	}
	if _, err := service.ListTriggers(ctx, thread, store.ListOptions{Limit: 201}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("unbounded list: %v", err)
	}
	// The service has no lifecycle methods and leaves the shared Engine/Store usable.
	if reflect.TypeOf(service).NumMethod() != 5 {
		t.Fatal("coordination service must expose only five narrow local-user operations")
	}
	if got, err := engine.ReceiveLocalMessage(ctx, thread, m.ID); err != nil || got != trigger {
		t.Fatalf("borrowed engine is unusable: %+v, %v", got, err)
	}
	if _, err := f.local.GetTask(ctx, f.groupID, receipt.Task.ID); err != nil {
		t.Fatalf("borrowed store is unusable: %v", err)
	}
}
