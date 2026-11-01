package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestCoordinationReadReceiptExactScopeAndOpaqueTopics(t *testing.T) {
	s, _, thread, request := requestWorkFixture(t)
	foreignGroup := group(t, s).ID
	for i, topic := range []string{"", " ", " \n\t ", "nul\x00topic", strings.Repeat("long-topic", 600)} {
		thread.TopicID = topic
		request.TopicID, request.MessageID = topic, coordinationTriggerMessage(t, s, thread).ID
		original, err := s.RequestWork(ctx, thread.GroupID, request, fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		before := messageSequenceSnapshot(t, s)
		got, err := s.GetAdmissionReceipt(ctx, thread, original.Decision.ID)
		if err != nil || !reflect.DeepEqual(got, original) {
			t.Fatalf("original receipt for %q: %+v, %v", topic, got, err)
		}
		for _, scope := range []domain.ThreadRef{
			{GroupID: foreignGroup, TopicID: topic}, {GroupID: "missing", TopicID: topic},
			{GroupID: thread.GroupID, TopicID: topic + " "},
		} {
			got, err := s.GetAdmissionReceipt(ctx, scope, original.Decision.ID)
			requestWorkRequireZero(t, got, err, ErrNotFound)
		}
		got, err = s.GetAdmissionReceipt(ctx, thread, "missing")
		requestWorkRequireZero(t, got, err, ErrNotFound)
		messageSequencePreserved(t, s, before)
	}
	for _, id := range []string{"", " ", "bad\n", "bad\x00id", strings.Repeat("x", 4097), string([]byte{0xff})} {
		got, err := s.GetAdmissionReceipt(ctx, thread, id)
		requestWorkRequireZero(t, got, err, ErrInvalid)
		view, err := s.GetTaskCoordination(ctx, thread.GroupID, id)
		if !errors.Is(err, ErrInvalid) || !reflect.DeepEqual(view, domain.TaskCoordinationView{}) {
			t.Fatalf("malformed task returned a view: %+v, %v", view, err)
		}
		got, err = s.GetAdmissionReceipt(ctx, domain.ThreadRef{GroupID: id}, "decision")
		requestWorkRequireZero(t, got, err, ErrInvalid)
		if _, err := s.GetTaskCoordination(ctx, id, "task"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("malformed group: %v", err)
		}
	}
}

// This is a valid future history fixture, not an admission or pointer setter API.
func coordinationReadNext(t *testing.T, s *Store, original domain.AdmissionReceipt) domain.AdmissionReceipt {
	t.Helper()
	task, err := s.GetTask(ctx, original.Task.GroupID, original.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	thread := domain.ThreadRef{GroupID: task.GroupID, TopicID: " future\ntopic "}
	rows := coordinationAdmissionRows(task, coordinationAdmissionSource(t, s, thread))
	rows.decision[4], rows.decision[6], rows.decision[7] = "link_existing", domain.LocalUserExplicitPolicyRevision, "新的明确授权"
	rows.current = nil
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := coordinationAdmissionInsert(tx, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE task_coordination SET topic_id=?,current_mandate_id=?,authority_generation=2 WHERE task_id=?`, thread.TopicID, rows.mandate[0], task.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if violations := messageSequenceRows(t, s.db, "PRAGMA foreign_key_check"); len(violations) != 0 {
		t.Fatalf("future pointer fixture is invalid: %v", violations)
	}
	next, err := s.GetAdmissionReceipt(ctx, thread, rows.decision[0].(string))
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestCoordinationReadCurrentPointerAndHistoricalReceipt(t *testing.T) {
	s, path, thread, request := requestWorkFixture(t)
	original, err := s.RequestWork(ctx, thread.GroupID, request, "original")
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.GetTaskCoordination(ctx, thread.GroupID, original.Task.ID)
	want := domain.TaskCoordinationView{
		Coordination: domain.TaskCoordination{TaskID: original.Task.ID, GroupID: thread.GroupID, TopicID: thread.TopicID, CurrentMandateID: original.Mandate.ID, AuthorityGeneration: 1},
		Admission:    original,
	}
	if err != nil || !reflect.DeepEqual(view, want) {
		t.Fatalf("initial projection: %+v, %v", view, err)
	}
	// Current descriptive fields are not part of either historical read.
	if _, err := s.db.Exec("UPDATE tasks SET title='current title',description='current description',acceptance='current criteria' WHERE id=?", original.Task.ID); err != nil {
		t.Fatal(err)
	}
	next := coordinationReadNext(t, s, original)
	want.Coordination.TopicID, want.Coordination.CurrentMandateID, want.Coordination.AuthorityGeneration = next.Decision.TopicID, next.Mandate.ID, 2
	want.Admission = next
	manual := createTestTask(t, s, thread.GroupID)
	foreign := group(t, s).ID
	check := func(s *Store) {
		t.Helper()
		before := messageSequenceSnapshot(t, s)
		got, err := s.GetTaskCoordination(ctx, thread.GroupID, original.Task.ID)
		if err != nil || !reflect.DeepEqual(got, want) || got.Admission.Mandate.Scope.Objective != "current title" {
			t.Fatalf("current pointer did not select its exact receipt: %+v, %v", got, err)
		}
		old, err := s.GetAdmissionReceipt(ctx, thread, original.Decision.ID)
		if err != nil || !reflect.DeepEqual(old, original) {
			t.Fatalf("new pointer contaminated old decision: %+v, %v", old, err)
		}
		for _, pair := range [][2]string{{foreign, original.Task.ID}, {"missing", original.Task.ID}, {thread.GroupID, "missing"}, {thread.GroupID, manual.ID}} {
			got, err := s.GetTaskCoordination(ctx, pair[0], pair[1])
			if !errors.Is(err, ErrNotFound) || !reflect.DeepEqual(got, domain.TaskCoordinationView{}) {
				t.Fatalf("missing/foreign/uncoordinated task %v: %+v, %v", pair, got, err)
			}
		}
		body, err := json.Marshal(got)
		if err != nil || strings.Contains(string(body), `"status"`) || strings.Contains(string(body), `"revision":`) {
			t.Fatalf("view mixed in live task fields: %s, %v", body, err)
		}
		messageSequencePreserved(t, s, before)
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	check(queueHandle(t, path, false))
}

func TestCoordinationReadPointerAndReceiptShareSnapshot(t *testing.T) {
	s, path, thread, request := requestWorkFixture(t)
	original, err := s.RequestWork(ctx, thread.GroupID, request, "original")
	if err != nil {
		t.Fatal(err)
	}
	next := coordinationReadNext(t, s, original)
	writer := queueHandle(t, path, false)
	done := make(chan error, 1)
	go func() {
		for i := range 60 {
			r := []domain.AdmissionReceipt{original, next}[i%2]
			_, err := writer.db.Exec(`UPDATE task_coordination SET topic_id=?,current_mandate_id=?,authority_generation=? WHERE task_id=?`, r.Decision.TopicID, r.Mandate.ID, int64(i%2+1), original.Task.ID)
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 60 {
		view, err := s.GetTaskCoordination(ctx, thread.GroupID, original.Task.ID)
		if err != nil {
			t.Error(err)
			break
		}
		want := []domain.AdmissionReceipt{original, next}[view.Coordination.AuthorityGeneration-1]
		if !reflect.DeepEqual(view.Admission, want) || view.Coordination.CurrentMandateID != want.Mandate.ID || view.Coordination.TopicID != want.Decision.TopicID {
			t.Errorf("torn pointer/receipt snapshot: %+v", view)
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCoordinationReadContextAndClosedStoreErrors(t *testing.T) {
	s, _, thread, request := requestWorkFixture(t)
	r, err := s.RequestWork(ctx, thread.GroupID, request, "original")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err := s.GetAdmissionReceipt(cancelled, thread, r.Decision.ID)
	requestWorkRequireZero(t, got, err, context.Canceled)
	if view, err := s.GetTaskCoordination(cancelled, thread.GroupID, r.Task.ID); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(view, domain.TaskCoordinationView{}) {
		t.Fatalf("cancelled read: %+v, %v", view, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAdmissionReceipt(ctx, thread, r.Decision.ID)
	requestWorkRequireZero(t, got, err, nil)
	if view, err := s.GetTaskCoordination(ctx, thread.GroupID, r.Task.ID); err == nil || !reflect.DeepEqual(view, domain.TaskCoordinationView{}) {
		t.Fatalf("closed read returned partial evidence: %+v, %v", view, err)
	}
}
