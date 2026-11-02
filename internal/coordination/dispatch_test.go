package coordination

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type engineDispatchAPI interface {
	RequestDispatch(context.Context, string, string, int64, int64, string, string) (domain.DispatchRequest, error)
	GetDispatchRequest(context.Context, string, string) (domain.DispatchView, error)
}

func engineDispatchCommands(t *testing.T, engine *Engine) engineDispatchAPI {
	t.Helper()
	api, ok := any(engine).(engineDispatchAPI)
	if !ok {
		t.Fatal("Engine lacks trusted local-user dispatch commands")
	}
	return api
}

func TestEngineDispatchBorrowedStoreLocalUserAndNoFanout(t *testing.T) {
	s, observer, thread := engineLocalFixture(t)
	ctx := context.Background()
	coord, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "协调员", Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "执行者", Role: "coordinator admin"})
	if err != nil {
		t.Fatal(err)
	}
	engine := New(s)
	if _, err := engine.SetCoordinator(ctx, thread.GroupID, coord.ID, 0, "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SetDispatchPolicy(ctx, thread.GroupID, []string{worker.ID}, 0, "shared"); err != nil {
		t.Fatal(err)
	}
	message, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: worker.ID, Content: "成员来源"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := s.RequestWork(ctx, thread.GroupID, domain.RequestWorkRequest{
		TopicID: thread.TopicID, MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew,
		Reason: "本地用户明确授权", NewTask: &domain.NewTaskRequest{Title: "目标", Acceptance: "验收"},
	}, "shared")
	if err != nil {
		t.Fatal(err)
	}
	api := engineDispatchCommands(t, engine)
	before := engineCoordinatorSnapshot(t, observer)
	request, err := api.RequestDispatch(ctx, thread.GroupID, admission.Task.ID, 1, 1, " 原样原因 ", "shared")
	if err != nil || request.RequestedBy != "user" || request.CoordinatorMemberID != coord.ID || request.MandateID != admission.Mandate.ID || request.TopicID != thread.TopicID || request.Reason != " 原样原因 " {
		t.Fatalf("Engine did not forward trusted local-user command: %+v, %v", request, err)
	}
	after := engineCoordinatorSnapshot(t, observer)
	for table, rows := range before {
		if table == "coordination_dispatch_requests" || table == "coordination_dispatch_state" || table == "mutation_idempotency" {
			if len(after[table]) != len(rows)+1 {
				t.Errorf("expected one new %s row", table)
			}
		} else if !reflect.DeepEqual(after[table], rows) {
			t.Errorf("Engine dispatch changed unrelated table %s", table)
		}
	}
	storeAPI, ok := any(s).(engineDispatchAPI)
	if !ok {
		t.Fatal("borrowed Store lacks dispatch commands")
	}
	stored, err := storeAPI.GetDispatchRequest(ctx, thread.GroupID, request.ID)
	if err != nil || stored.Request != request || stored.State.Status != domain.DispatchStatusPending || stored.State.Delivery != nil {
		t.Fatalf("Engine did not borrow the same Store: %+v, %v", stored, err)
	}
	if got, err := api.GetDispatchRequest(ctx, thread.GroupID, request.ID); err != nil || !reflect.DeepEqual(got, stored) {
		t.Fatalf("Engine exact read: %+v, %v", got, err)
	}
	changed, err := api.RequestDispatch(ctx, thread.GroupID, admission.Task.ID, 1, 1, " 原样原因  ", "shared")
	engineCoordinatorRequireZero(t, changed, err, store.ErrConflict)
	if !reflect.DeepEqual(engineCoordinatorSnapshot(t, observer), after) {
		t.Fatal("Engine changed-payload conflict wrote state")
	}
	if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, nil, 1, "clear"); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Exec("UPDATE members SET status='left'"); err != nil {
		t.Fatal(err)
	}
	before = engineCoordinatorSnapshot(t, observer)
	if got, err := api.RequestDispatch(ctx, thread.GroupID, admission.Task.ID, 1, 1, " 原样原因 ", "shared"); err != nil || got != request {
		t.Fatalf("Engine revalidated historical request: %+v, %v", got, err)
	}
	if got, err := api.GetDispatchRequest(ctx, thread.GroupID, request.ID); err != nil || !reflect.DeepEqual(got, stored) {
		t.Fatalf("Engine Get requires live business eligibility: %+v, %v", got, err)
	}
	if !reflect.DeepEqual(engineCoordinatorSnapshot(t, observer), before) {
		t.Fatal("Engine replay/Get wrote state")
	}
	if _, err := s.GetGroup(ctx, thread.GroupID); err != nil {
		t.Fatalf("Engine took ownership of Store: %v", err)
	}
	var path string
	if err := observer.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	api = engineDispatchCommands(t, New(reopened))
	if got, err := api.RequestDispatch(ctx, thread.GroupID, admission.Task.ID, 1, 1, " 原样原因 ", "shared"); err != nil || got != request {
		t.Fatalf("Engine lost persisted receipt: %+v, %v", got, err)
	}
}

func TestEngineDispatchErrorsAreForwardedWithoutWrites(t *testing.T) {
	s, observer, thread := engineLocalFixture(t)
	api := engineDispatchCommands(t, New(s))
	ctx := context.Background()
	before := engineCoordinatorSnapshot(t, observer)
	for _, key := range []string{"", " ", "\u2003"} {
		got, err := api.RequestDispatch(ctx, thread.GroupID, "task", 1, 1, "安排", key)
		engineCoordinatorRequireZero(t, got, err, store.ErrInvalid)
	}
	got, err := api.RequestDispatch(ctx, thread.GroupID, "task", 0, 1, "安排", "key")
	engineCoordinatorRequireZero(t, got, err, store.ErrInvalid)
	got, err = api.RequestDispatch(ctx, thread.GroupID, "task", 1, 1, "安排", "key")
	engineCoordinatorRequireZero(t, got, err, store.ErrNotFound)
	view, err := api.GetDispatchRequest(ctx, "missing-group", "request")
	engineCoordinatorRequireZero(t, view, err, store.ErrNotFound)
	view, err = api.GetDispatchRequest(ctx, thread.GroupID, " ")
	engineCoordinatorRequireZero(t, view, err, store.ErrInvalid)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err = api.RequestDispatch(cancelled, thread.GroupID, "task", 1, 1, "安排", "key")
	engineCoordinatorRequireZero(t, got, err, context.Canceled)
	view, err = api.GetDispatchRequest(cancelled, thread.GroupID, "request")
	engineCoordinatorRequireZero(t, view, err, context.Canceled)
	if !reflect.DeepEqual(engineCoordinatorSnapshot(t, observer), before) {
		t.Fatal("Engine failure changed storage")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = api.RequestDispatch(ctx, thread.GroupID, "task", 1, 1, "安排", "key")
	engineCoordinatorRequireZero(t, got, err, nil)
	view, err = api.GetDispatchRequest(ctx, thread.GroupID, "request")
	if err == nil || errors.Is(err, store.ErrNotFound) || !reflect.ValueOf(view).IsZero() {
		t.Fatalf("Engine hid closed borrowed Store: %+v, %v", view, err)
	}
}
