package coordination

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type engineMaterializeAPI interface {
	MaterializeDispatchRequest(context.Context, string, int64, time.Time) (domain.DispatchMaterialization, error)
}

func TestEngineDispatchMaterializeDefaultsClosedBeforeAnyStoreCall(t *testing.T) {
	s, observer, thread := engineLocalFixture(t)
	ctx := context.Background()
	coord, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "协调员", Role: "coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "执行者", Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCoordinator(ctx, thread.GroupID, coord.ID, 0, "binding"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{worker.ID}, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	message, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: "明确工作"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := s.RequestWork(ctx, thread.GroupID, domain.RequestWorkRequest{TopicID: thread.TopicID, MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew,
		Reason: "明确授权", NewTask: &domain.NewTaskRequest{Title: "目标", Acceptance: "验收"}}, "admit")
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.RequestDispatch(ctx, thread.GroupID, admission.Task.ID, 1, 1, "安排", "dispatch")
	if err != nil {
		t.Fatal(err)
	}
	for _, engine := range []*Engine{New(s), New(nil)} {
		api, ok := any(engine).(engineMaterializeAPI)
		if !ok {
			t.Fatal("Engine lacks a default-closed materialization gate")
		}
		before := engineCoordinatorSnapshot(t, observer)
		got, err := api.MaterializeDispatchRequest(ctx, request.ID, 1, time.Now())
		if !errors.Is(err, ErrDispatchRuntimeNotReady) || !reflect.ValueOf(got).IsZero() {
			t.Fatalf("not-ready runtime should return zero result and an explicit error: %+v, %v", got, err)
		}
		if !reflect.DeepEqual(engineCoordinatorSnapshot(t, observer), before) {
			t.Fatal("default-closed Engine changed Store state")
		}
	}
}
