package store

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

type materializeDispatchAPI interface {
	MaterializeDispatchRequest(context.Context, string, int64, time.Time) (domain.DispatchMaterialization, error)
}

var materializeAt = time.Date(2026, 11, 2, 12, 0, 0, 123456789, time.UTC)

func materializeAPI(t *testing.T, s *Store) materializeDispatchAPI {
	t.Helper()
	api, ok := any(s).(materializeDispatchAPI)
	if !ok {
		t.Fatal("Store lacks atomic DispatchRequest materialization")
	}
	return api
}

func materializePending(t *testing.T) (requestDispatchFixture, domain.DispatchRequest) {
	t.Helper()
	f := requestDispatchSetup(t)
	r, err := f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排此任务", "dispatch")
	if err != nil {
		t.Fatal(err)
	}
	return f, r
}

func materializeWantBody(r domain.DispatchRequest, objective, acceptance string) string {
	return fmt.Sprintf("系统根据本地用户调度命令及历史 Mandate 生成以下分派表示，并非用户逐字原话。\nDispatchRequest: %s\nTask: %s\nMandate: %s\n目标：\n%s\n验收条件：\n%s\n描述及原因见上述引用。", r.ID, r.TaskID, r.MandateID, objective, acceptance)
}

func materializeAssertReceipt(t *testing.T, s *Store, r domain.DispatchRequest, got domain.DispatchMaterialization, from int64, at time.Time) {
	t.Helper()
	if got.Disposition != "enqueued" || got.Generation != from+1 || got.HeldReason != "" || got.Delivery == nil || got.Delivery.MaterializedFromGeneration != from || got.Delivery.CoordinatorMemberID != r.CoordinatorMemberID || got.Delivery.CreatedAt != at {
		t.Fatalf("invalid committed receipt: %+v", got)
	}
	m, err := s.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
	if err != nil || m.GroupID != r.GroupID || m.TopicID != r.TopicID || m.SenderID != "user" || len(m.Deliveries) != 1 {
		t.Fatalf("not an exact singleton user message: %+v, %v", m, err)
	}
	d := m.Deliveries[0]
	if d.ID != got.Delivery.DeliveryID || d.MessageID != m.ID || d.ConversationID != got.Delivery.ConversationID || d.RecipientID != r.CoordinatorMemberID {
		t.Fatalf("receipt does not identify the actual delivery: %+v / %+v", got, d)
	}
	v, err := s.GetDispatchRequest(ctx, r.GroupID, r.ID)
	if err != nil || v.State.Generation != from+1 || v.State.Status != domain.DispatchStatusEnqueued || !reflect.DeepEqual(v.State.Delivery, got.Delivery) {
		t.Fatalf("receipt not committed in state: %+v, %v", v, err)
	}
}

func TestMaterializeDispatchConcurrentReceiptReplayAndReopen(t *testing.T) {
	f, r := materializePending(t)
	api := materializeAPI(t, f.s)
	for _, key := range []string{"dispatch_materialize:" + r.ID, "dispatch_materialize:" + r.ID + ":1", r.ID} {
		if _, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: r.GroupID, TopicID: r.TopicID, ActorID: "user", Content: "用户的同前缀幂等键", IdempotencyKey: key}, nil); err != nil {
			t.Fatal(err)
		}
	}
	second := queueHandle(t, f.path, false)
	before := messageSequenceSnapshot(t, f.s)
	type result struct {
		value domain.DispatchMaterialization
		err   error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for _, handle := range []materializeDispatchAPI{api, materializeAPI(t, second)} {
		go func() {
			<-start
			v, err := handle.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			results <- result{v, err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || !reflect.DeepEqual(a.value, b.value) {
		t.Fatalf("same-generation race diverged: %+v / %+v", a, b)
	}
	materializeAssertReceipt(t, second, r, a.value, 1, materializeAt)
	after := messageSequenceSnapshot(t, second)
	for table, rows := range before {
		switch table {
		case "messages", "message_log", "deliveries", "conversations":
			if len(after[table]) != len(rows)+1 {
				t.Errorf("expected one new %s row", table)
			}
		case "coordination_dispatch_state":
		default:
			if !reflect.DeepEqual(rows, after[table]) {
				t.Errorf("materialization changed unrelated %s", table)
			}
		}
	}
	m, err := second.GetMessage(ctx, r.GroupID, a.value.Delivery.MessageID)
	if err != nil || m.Content != materializeWantBody(r, f.admission.Mandate.Scope.Objective, f.admission.Mandate.Scope.Acceptance) {
		t.Fatalf("wrong historical representation: %q, %v", m.Content, err)
	}
	for _, gen := range []int64{2, 3} {
		got, err := api.MaterializeDispatchRequest(ctx, r.ID, gen, materializeAt)
		requestDispatchZero(t, got, err, ErrConflict)
	}
	messageSequencePreserved(t, second, after)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	got, err := materializeAPI(t, reopened).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(got, a.value) {
		t.Fatalf("reopen changed original receipt: %+v, %v", got, err)
	}
	messageSequencePreserved(t, reopened, after)
	dispatchAssertFK(t, reopened)
}

func TestMaterializeDispatchInputAndStorageFailuresAreZero(t *testing.T) {
	f, r := materializePending(t)
	api := materializeAPI(t, f.s)
	before := messageSequenceSnapshot(t, f.s)
	for _, id := range []string{"", " id", "id\n", "id\x00", string([]byte{0xff}), strings.Repeat("x", 4097)} {
		got, err := api.MaterializeDispatchRequest(ctx, id, 1, materializeAt)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	for _, gen := range []int64{0, -1, math.MinInt64} {
		got, err := api.MaterializeDispatchRequest(ctx, r.ID, gen, materializeAt)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	for _, at := range []time.Time{time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		got, err := api.MaterializeDispatchRequest(ctx, r.ID, 1, at)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	got, err := api.MaterializeDispatchRequest(ctx, "missing", 1, materializeAt)
	requestDispatchZero(t, got, err, ErrNotFound)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err = api.MaterializeDispatchRequest(cancelled, r.ID, 1, materializeAt)
	requestDispatchZero(t, got, err, context.Canceled)
	messageSequencePreserved(t, f.s, before)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = api.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	requestDispatchZero(t, got, err, nil)
}

func materializePayloadFixture(t *testing.T, objective, acceptance, description, topic string) (*Store, domain.DispatchRequest) {
	t.Helper()
	s, _, thread, input := requestWorkFixture(t)
	coord, worker := member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID
	if _, err := s.SetCoordinator(ctx, thread.GroupID, coord, 0, "binding"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{worker}, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	message, err := s.CreateMessage(ctx, MessageRequest{GroupID: thread.GroupID, TopicID: topic, ActorID: worker, Content: "来源"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	input.TopicID, input.MessageID = topic, message.ID
	input.NewTask.Title, input.NewTask.Acceptance, input.NewTask.Description = objective, acceptance, description
	a, err := s.RequestWork(ctx, thread.GroupID, input, "admit-payload")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestDispatch(ctx, thread.GroupID, a.Task.ID, 1, 1, "原因为引用，不应拼接", "payload")
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestMaterializeDispatchPayloadBudgetEncodingAndOpaqueTopic(t *testing.T) {
	// IDs generated by rand.Text have fixed byte length; the representation has no escaping.
	dummy := domain.DispatchRequest{ID: strings.Repeat("x", 26), TaskID: strings.Repeat("x", 26), MandateID: strings.Repeat("x", 26)}
	budget := 32*1024 - len(materializeWantBody(dummy, "", "验收"))
	for _, tc := range []struct {
		name, objective, acceptance, held string
	}{
		{"exact", strings.Repeat("x", budget), "验收", ""},
		{"multibyte", strings.Repeat("目", budget/3) + strings.Repeat("x", budget%3), "验收", ""},
		{"one-byte-over", strings.Repeat("x", budget+1), "验收", domain.DispatchHeldPayloadTooLarge},
		{"huge-objective", strings.Repeat("目", 100000), "验收", domain.DispatchHeldPayloadTooLarge},
		{"huge-acceptance", "目标", strings.Repeat("验", 100000), domain.DispatchHeldPayloadTooLarge},
		{"invalid-objective", "目标\xff", "验收", domain.DispatchHeldPayloadNotEncodable},
		{"invalid-acceptance", "目标", "验收\xff", domain.DispatchHeldPayloadNotEncodable},
		{"embedded-nul", "目标\x00完整目标", "验收\x00完整验收", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			topic := " \n\x00\xff\t "
			s, r := materializePayloadFixture(t, tc.objective, tc.acceptance, strings.Repeat("无关描述", 100000), topic)
			before := messageSequenceSnapshot(t, s)
			got, err := materializeAPI(t, s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
			if err != nil {
				t.Fatal(err)
			}
			if tc.held != "" {
				if got.Disposition != "held" || got.Generation != 2 || got.HeldReason != tc.held || got.Delivery != nil {
					t.Fatalf("bad payload did not hold: %+v", got)
				}
				delete(before, "coordination_dispatch_state")
				messageSequencePreserved(t, s, before)
				return
			}
			materializeAssertReceipt(t, s, r, got, 1, materializeAt)
			m, err := s.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
			if err != nil || m.Content != materializeWantBody(r, tc.objective, tc.acceptance) || !utf8.ValidString(m.Content) || len(m.Content) > 32*1024 || m.TopicID != topic {
				t.Fatalf("payload/topic changed: length=%d topic=%q, %v", len(m.Content), m.TopicID, err)
			}
			if (tc.name == "exact" || tc.name == "multibyte") && len(m.Content) != 32*1024 {
				t.Fatalf("not the exact positive boundary: %d", len(m.Content))
			}
		})
	}
}

func TestMaterializeDispatchSameGenerationReceiptPrecedesLiveEligibility(t *testing.T) {
	f, r := materializePending(t)
	api := materializeAPI(t, f.s)
	original, err := api.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE tasks SET revision=revision+1 WHERE id=?", r.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE task_coordination SET authority_generation=authority_generation+1 WHERE task_id=?", r.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetCoordinator(ctx, r.GroupID, member(t, f.s, r.GroupID).ID, 1, "replace"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, r.GroupID, nil, 1, "clear"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE group_id=?", r.GroupID); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	got, err := api.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("historical receipt rejected by new eligibility: %+v, %v", got, err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestMaterializeDispatchRejectsSubstitutedMessageContent(t *testing.T) {
	f, r := materializePending(t)
	observer := queueHandle(t, f.path, false)
	if _, err := f.s.db.Exec(`PRAGMA recursive_triggers=OFF;
	 CREATE TRIGGER materialize_substitute_message BEFORE INSERT ON messages BEGIN
	  INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at)
	  VALUES(NEW.id,NEW.group_id,NEW.topic_id,NEW.sender_id,'substituted authority text',NEW.created_at);
	  SELECT RAISE(IGNORE);
	 END;`); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, observer)
	before["schema_migrations"] = messageSequenceTableRows(t, observer, "schema_migrations")
	before["sqlite_sequence"] = messageSequenceTableRows(t, observer, "sqlite_sequence")
	got, err := materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	want := materializeWantBody(r, f.admission.Mandate.Scope.Objective, f.admission.Mandate.Scope.Acceptance)
	if err == nil && got.Delivery != nil {
		m, readErr := observer.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
		t.Fatalf("substituted message committed: stored=%q, want=%q, receipt=%+v, read_err=%v", m.Content, want, got, readErr)
	}
	requestDispatchZero(t, got, err, ErrConflict)
	messageSequencePreserved(t, observer, before)
	messageSequencePreserved(t, f.s, before)
	if _, err := f.s.db.Exec("DROP TRIGGER materialize_substitute_message"); err != nil {
		t.Fatal(err)
	}
	got, err = materializeAPI(t, observer).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err != nil {
		t.Fatalf("substitution rollback made original request unretryable: %v", err)
	}
	materializeAssertReceipt(t, observer, r, got, 1, materializeAt)
	m, err := observer.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
	if err != nil || m.Content != want || m.Deliveries[0].State != domain.DeliveryStateQueued || m.Deliveries[0].TurnID != "" {
		t.Fatalf("retry did not persist the complete queued authority message: %+v, %v", m, err)
	}
}

func TestMaterializeDispatchRejectsDeliveryDeactivatedDuringStateWrite(t *testing.T) {
	f, r := materializePending(t)
	observer := queueHandle(t, f.path, false)
	if _, err := f.s.db.Exec(`CREATE TRIGGER materialize_deactivate_delivery AFTER UPDATE ON coordination_dispatch_state
	 WHEN NEW.status='enqueued' BEGIN
	  UPDATE deliveries SET state='failed',error='injected before commit' WHERE id=NEW.delivery_id;
	 END;`); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, observer)
	before["schema_migrations"] = messageSequenceTableRows(t, observer, "schema_migrations")
	before["sqlite_sequence"] = messageSequenceTableRows(t, observer, "sqlite_sequence")
	got, err := materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err == nil && got.Delivery != nil {
		m, readErr := observer.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
		t.Fatalf("deactivated delivery committed: message=%+v, receipt=%+v, read_err=%v", m, got, readErr)
	}
	requestDispatchZero(t, got, err, ErrConflict)
	messageSequencePreserved(t, observer, before)
	messageSequencePreserved(t, f.s, before)
	if _, err := f.s.db.Exec("DROP TRIGGER materialize_deactivate_delivery"); err != nil {
		t.Fatal(err)
	}
	got, err = materializeAPI(t, observer).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err != nil {
		t.Fatalf("delivery rollback made original request unretryable: %v", err)
	}
	materializeAssertReceipt(t, observer, r, got, 1, materializeAt)
	m, err := observer.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
	if err != nil || m.Content != materializeWantBody(r, f.admission.Mandate.Scope.Objective, f.admission.Mandate.Scope.Acceptance) || m.Deliveries[0].State != domain.DeliveryStateQueued || m.Deliveries[0].TurnID != "" {
		t.Fatalf("retry did not persist the complete queued authority message: %+v, %v", m, err)
	}
}

func TestMaterializeDispatchRejectsQueuedDeliveryErrorDuringStateWrite(t *testing.T) {
	f, r := materializePending(t)
	observer := queueHandle(t, f.path, false)
	if _, err := f.s.db.Exec(`CREATE TRIGGER materialize_delivery_error AFTER UPDATE ON coordination_dispatch_state
	 WHEN NEW.status='enqueued' BEGIN
	  UPDATE deliveries SET error='injected during state write' WHERE id=NEW.delivery_id;
	 END;`); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, observer)
	before["schema_migrations"] = messageSequenceTableRows(t, observer, "schema_migrations")
	before["sqlite_sequence"] = messageSequenceTableRows(t, observer, "sqlite_sequence")
	got, err := materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err == nil && got.Delivery != nil {
		materializeAssertReceipt(t, observer, r, got, 1, materializeAt)
		m, readErr := observer.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
		t.Fatalf("queued delivery with initial error committed: message=%+v, receipt=%+v, read_err=%v", m, got, readErr)
	}
	requestDispatchZero(t, got, err, ErrConflict)
	messageSequencePreserved(t, observer, before)
	messageSequencePreserved(t, f.s, before)
	if _, err := f.s.db.Exec("DROP TRIGGER materialize_delivery_error"); err != nil {
		t.Fatal(err)
	}
	original, err := materializeAPI(t, observer).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err != nil {
		t.Fatalf("initial delivery error rollback made original request unretryable: %v", err)
	}
	materializeAssertReceipt(t, observer, r, original, 1, materializeAt)
	m, err := observer.GetMessage(ctx, r.GroupID, original.Delivery.MessageID)
	if err != nil || m.Content != materializeWantBody(r, f.admission.Mandate.Scope.Objective, f.admission.Mandate.Scope.Acceptance) || m.Deliveries[0].State != domain.DeliveryStateQueued || m.Deliveries[0].TurnID != "" || m.Deliveries[0].Error != "" {
		t.Fatalf("retry did not persist the complete clean queued authority message: %+v, %v", m, err)
	}
	if _, err := f.s.db.Exec("UPDATE deliveries SET error='later runtime error' WHERE id=?", original.Delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	m, err = observer.GetMessage(ctx, r.GroupID, original.Delivery.MessageID)
	if err != nil || len(m.Deliveries) != 1 || m.Deliveries[0].Error != "later runtime error" || m.Deliveries[0].State != domain.DeliveryStateQueued || m.Deliveries[0].TurnID != "" {
		t.Fatalf("postcommit error-only mutation was not persisted: %+v, %v", m, err)
	}
	after := messageSequenceSnapshot(t, observer)
	after["schema_migrations"] = messageSequenceTableRows(t, observer, "schema_migrations")
	after["sqlite_sequence"] = messageSequenceTableRows(t, observer, "sqlite_sequence")
	replay, err := materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("historical receipt rejected by a later delivery error: %+v, %v", replay, err)
	}
	messageSequencePreserved(t, observer, after)
	messageSequencePreserved(t, f.s, after)
}
