package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func requestWorkFixture(t *testing.T) (*Store, string, domain.ThreadRef, domain.RequestWorkRequest) {
	t.Helper()
	s, path := openTestStore(t)
	thread := domain.ThreadRef{GroupID: group(t, s).ID, TopicID: "topic"}
	message := coordinationTriggerMessage(t, s, thread)
	return s, path, thread, domain.RequestWorkRequest{
		TopicID: thread.TopicID, MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "明确开展此工作。 ",
		NewTask: &domain.NewTaskRequest{Title: "目标。 ", Description: "描述", Acceptance: "验证结果"},
	}
}

func requestWorkLink(request domain.RequestWorkRequest, task domain.Task) domain.RequestWorkRequest {
	request.Mode, request.NewTask = domain.RoutingDecisionLinkExisting, nil
	request.ExistingTask = &domain.ExistingTaskRequest{TaskID: task.ID, ExpectedRevision: task.Revision}
	return request
}

func requestWorkCounts(t *testing.T, s *Store, triggers, tasks, dependencies, admissions int) {
	t.Helper()
	for query, want := range map[string]int{
		"SELECT count(*) FROM coordination_triggers":                               triggers,
		"SELECT count(*) FROM tasks":                                               tasks,
		"SELECT count(*) FROM task_dependencies":                                   dependencies,
		"SELECT count(*) FROM routing_decisions":                                   admissions,
		"SELECT count(*) FROM mandates":                                            admissions,
		"SELECT count(*) FROM task_coordination":                                   admissions,
		"SELECT count(*) FROM mutation_idempotency WHERE operation='request_work'": admissions,
	} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", query, got, err, want)
		}
	}
}

func requestWorkRequireZero(t *testing.T, got domain.AdmissionReceipt, err, want error) {
	t.Helper()
	if err == nil || (want != nil && !errors.Is(err, want)) || !reflect.DeepEqual(got, domain.AdmissionReceipt{}) {
		t.Fatalf("expected zero receipt and %v, got %+v, %v", want, got, err)
	}
}

func TestRequestWorkCreateSnapshotsAndCanonicalReplay(t *testing.T) {
	s, path, thread, request := requestWorkFixture(t)
	assignee := member(t, s, thread.GroupID)
	a, b := createTestTask(t, s, thread.GroupID), createTestTask(t, s, thread.GroupID)
	request.NewTask.AssigneeID = assignee.ID
	request.NewTask.DependencyIDs = []string{b.ID, a.ID, b.ID}
	original := slices.Clone(request.NewTask.DependencyIDs)
	receipt, err := s.RequestWork(ctx, thread.GroupID, request, "request")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(original, request.NewTask.DependencyIDs) {
		t.Fatal("request dependency normalization mutated the caller")
	}
	task, err := s.GetTask(ctx, thread.GroupID, receipt.Task.ID)
	if err != nil || task.Status != domain.TaskStatusOpen || task.Revision != 1 || task.ActiveAttemptID != "" || task.CurrentSubmissionID != "" {
		t.Fatalf("new task state: %+v, %v", task, err)
	}
	wantScope := domain.TaskScope{Objective: task.Title, Description: task.Description, Acceptance: task.Acceptance, AssigneeID: task.AssigneeID, DependencyIDs: task.DependencyIDs}
	d, m := receipt.Decision, receipt.Mandate
	if !reflect.DeepEqual(m.Scope, wantScope) || receipt.Task != (domain.TaskRef{ID: task.ID, GroupID: task.GroupID, RevisionAtAdmission: 1}) ||
		d.ID == "" || d.MandateID != m.ID || m.DecisionID != d.ID || m.OriginTriggerID != d.TriggerID || d.TriggerID == "" ||
		d.GroupID != task.GroupID || m.GroupID != task.GroupID || d.TopicID != thread.TopicID || m.TopicID != thread.TopicID ||
		d.Kind != request.Mode || d.Reason != request.Reason || d.TaskID != task.ID || m.TaskID != task.ID ||
		d.TaskRevisionAtAdmission != 1 || m.TaskRevisionAtAdmission != 1 || d.ActorID != "user" || m.AuthorizedBy != "user" ||
		d.PolicyRevision != domain.LocalUserExplicitPolicyRevision || m.PolicyRevision != domain.LocalUserExplicitPolicyRevision ||
		m.AuthorityKind != domain.MandateAuthorityLocalUserExplicit || m.Strength != domain.MandateStrengthExplicit ||
		d.CreatedAt.IsZero() || m.CreatedAt.IsZero() {
		t.Fatalf("incorrect immutable authority: %+v; task=%+v", receipt, task)
	}
	ids := map[string]bool{}
	for _, id := range []string{task.ID, d.ID, m.ID, d.TriggerID} {
		if ids[id] {
			t.Fatal("server identities were reused across records")
		}
		ids[id] = true
	}
	var generation int64
	var mandateID string
	if err := s.db.QueryRow("SELECT authority_generation,current_mandate_id FROM task_coordination WHERE task_id=? AND group_id=? AND topic_id=?", task.ID, thread.GroupID, thread.TopicID).Scan(&generation, &mandateID); err != nil || generation != 1 || mandateID != m.ID {
		t.Fatalf("current authority: %d, %q, %v", generation, mandateID, err)
	}
	request.NewTask.DependencyIDs[0] = "caller-mutated"
	if !slices.Equal(receipt.Mandate.Scope.DependencyIDs, task.DependencyIDs) {
		t.Fatal("snapshot aliases caller dependencies")
	}
	request.NewTask.DependencyIDs = []string{a.ID, b.ID}
	before := messageSequenceSnapshot(t, s)
	if got, err := s.RequestWork(ctx, thread.GroupID, request, "request"); err != nil || !reflect.DeepEqual(got, receipt) {
		t.Fatalf("canonical replay differs: %+v, %v", got, err)
	}
	messageSequencePreserved(t, s, before)
	// Mutating one returned snapshot cannot corrupt the stored receipt.
	receipt.Mandate.Scope.DependencyIDs[0] = "response-mutated"
	replayed, err := s.RequestWork(ctx, thread.GroupID, request, "request")
	if err != nil || !reflect.DeepEqual(replayed.Mandate.Scope, wantScope) {
		t.Fatalf("returned receipt shares mutable state: %+v, %v", replayed, err)
	}
	if _, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task)); !errors.Is(err, ErrConflict) {
		t.Fatalf("admission bypassed the accepted-only execution dependency gate: %v", err)
	}
	requestWorkCounts(t, s, 1, 3, 2, 1)
	var inner int
	if err := s.db.QueryRow("SELECT count(*) FROM mutation_idempotency WHERE operation='create_task'").Scan(&inner); err != nil || inner != 0 {
		t.Fatalf("nested create_task receipt: %d, %v", inner, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	if got, err := reopened.RequestWork(ctx, thread.GroupID, request, "request"); err != nil || !reflect.DeepEqual(got, replayed) {
		t.Fatalf("reopened receipt changed: %+v, %v", got, err)
	}
}

func TestRequestWorkExactSourceAndRequiredKey(t *testing.T) {
	for _, topic := range []string{"", " ", " \n\t ", "line\ntopic", "nul\x00topic", string([]byte{0xff})} {
		t.Run(fmt.Sprintf("topic=%q", topic), func(t *testing.T) {
			s, _, thread, request := requestWorkFixture(t)
			thread.TopicID = topic
			request.TopicID, request.MessageID = topic, coordinationTriggerMessage(t, s, thread).ID
			before := messageSequenceSnapshot(t, s)
			for _, key := range []string{"", " ", "\n\t", "\u2003"} {
				got, err := s.RequestWork(ctx, thread.GroupID, request, key)
				requestWorkRequireZero(t, got, err, ErrInvalid)
				messageSequencePreserved(t, s, before)
			}
			for _, source := range []struct{ groupID, topicID, messageID string }{
				{group(t, s).ID, topic, request.MessageID}, {"missing", topic, request.MessageID},
				{thread.GroupID, topic + " ", request.MessageID}, {thread.GroupID, topic, "missing"},
			} {
				changed := request
				changed.TopicID, changed.MessageID = source.topicID, source.messageID
				before := messageSequenceSnapshot(t, s)
				got, err := s.RequestWork(ctx, source.groupID, changed, "source")
				requestWorkRequireZero(t, got, err, ErrNotFound)
				messageSequencePreserved(t, s, before)
			}
			got, err := s.RequestWork(ctx, thread.GroupID, request, "source")
			if err != nil || got.Decision.TopicID != topic || got.Mandate.TopicID != topic {
				t.Fatalf("opaque topic normalized or failed key burned: %+v, %v", got, err)
			}
			changed := request
			changed.TopicID += " "
			got, err = s.RequestWork(ctx, thread.GroupID, changed, "source")
			requestWorkRequireZero(t, got, err, ErrNotFound) // Source scope precedes conflict disclosure.
			requestWorkCounts(t, s, 1, 1, 0, 1)
		})
	}
}

func TestRequestWorkRejectsMalformedUnionWithoutWrites(t *testing.T) {
	s, _, thread, request := requestWorkFixture(t)
	for name, change := range map[string]func(*domain.RequestWorkRequest){
		"unknown-mode": func(r *domain.RequestWorkRequest) { r.Mode = "attach" },
		"missing-new":  func(r *domain.RequestWorkRequest) { r.NewTask = nil },
		"two-variants": func(r *domain.RequestWorkRequest) {
			r.ExistingTask = &domain.ExistingTaskRequest{TaskID: "task", ExpectedRevision: 1}
		},
		"wrong-variant":     func(r *domain.RequestWorkRequest) { r.Mode = domain.RoutingDecisionLinkExisting },
		"missing-existing":  func(r *domain.RequestWorkRequest) { r.Mode, r.NewTask = domain.RoutingDecisionLinkExisting, nil },
		"blank-reason":      func(r *domain.RequestWorkRequest) { r.Reason = " \n\t" },
		"blank-title":       func(r *domain.RequestWorkRequest) { r.NewTask.Title = " \n\t" },
		"blank-acceptance":  func(r *domain.RequestWorkRequest) { r.NewTask.Acceptance = "\u2003" },
		"blank-assignee":    func(r *domain.RequestWorkRequest) { r.NewTask.AssigneeID = " " },
		"blank-dependency":  func(r *domain.RequestWorkRequest) { r.NewTask.DependencyIDs = []string{" "} },
		"blank-message":     func(r *domain.RequestWorkRequest) { r.MessageID = " " },
		"invalid-message":   func(r *domain.RequestWorkRequest) { r.MessageID = "message\n" },
		"blank-target":      func(r *domain.RequestWorkRequest) { *r = requestWorkLink(*r, domain.Task{ID: " ", Revision: 1}) },
		"zero-revision":     func(r *domain.RequestWorkRequest) { *r = requestWorkLink(*r, domain.Task{ID: "task"}) },
		"negative-revision": func(r *domain.RequestWorkRequest) { *r = requestWorkLink(*r, domain.Task{ID: "task", Revision: -1}) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := request
			spec := *request.NewTask
			changed.NewTask = &spec
			change(&changed)
			before := messageSequenceSnapshot(t, s)
			got, err := s.RequestWork(ctx, thread.GroupID, changed, "retryable")
			requestWorkRequireZero(t, got, err, ErrInvalid)
			messageSequencePreserved(t, s, before)
		})
	}
	for _, groupID := range []string{"", " ", "group\n"} {
		got, err := s.RequestWork(ctx, groupID, request, "retryable")
		requestWorkRequireZero(t, got, err, ErrInvalid)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err := s.RequestWork(cancelled, thread.GroupID, request, "retryable")
	requestWorkRequireZero(t, got, err, context.Canceled)
	requestWorkCounts(t, s, 0, 0, 0, 0)
	if _, err := s.RequestWork(ctx, thread.GroupID, request, "retryable"); err != nil {
		t.Fatalf("validation burned key: %v", err)
	}
}

func TestRequestWorkChangedPayloadAndTriggerConflictRollback(t *testing.T) {
	s, _, thread, request := requestWorkFixture(t)
	original, err := s.RequestWork(ctx, thread.GroupID, request, "key")
	if err != nil {
		t.Fatal(err)
	}
	otherTopic := thread
	otherTopic.TopicID = "another-topic"
	otherMessage := coordinationTriggerMessage(t, s, thread)
	otherTopicMessage := coordinationTriggerMessage(t, s, otherTopic)
	target := createTestTask(t, s, thread.GroupID)
	for name, change := range map[string]func(*domain.RequestWorkRequest){
		"reason":           func(r *domain.RequestWorkRequest) { r.Reason += " " },
		"title":            func(r *domain.RequestWorkRequest) { r.NewTask.Title += " " },
		"description":      func(r *domain.RequestWorkRequest) { r.NewTask.Description += " " },
		"acceptance":       func(r *domain.RequestWorkRequest) { r.NewTask.Acceptance += " " },
		"assignee":         func(r *domain.RequestWorkRequest) { r.NewTask.AssigneeID = "missing" },
		"dependencies":     func(r *domain.RequestWorkRequest) { r.NewTask.DependencyIDs = []string{target.ID} },
		"source":           func(r *domain.RequestWorkRequest) { r.MessageID = otherMessage.ID },
		"topic-and-source": func(r *domain.RequestWorkRequest) { r.TopicID, r.MessageID = otherTopic.TopicID, otherTopicMessage.ID },
		"mode":             func(r *domain.RequestWorkRequest) { *r = requestWorkLink(*r, target) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := request
			spec := *request.NewTask
			changed.NewTask = &spec
			change(&changed)
			before := messageSequenceSnapshot(t, s)
			got, err := s.RequestWork(ctx, thread.GroupID, changed, "key")
			requestWorkRequireZero(t, got, err, ErrConflict)
			messageSequencePreserved(t, s, before)
		})
	}
	before := messageSequenceSnapshot(t, s)
	got, err := s.RequestWork(ctx, thread.GroupID, request, "different-key")
	requestWorkRequireZero(t, got, err, ErrConflict)
	messageSequencePreserved(t, s, before)
	request.MessageID = otherMessage.ID
	if got, err := s.RequestWork(ctx, thread.GroupID, request, "different-key"); err != nil || got.Task.ID == original.Task.ID {
		t.Fatalf("conflict burned an unused key: %+v, %v", got, err)
	}
	requestWorkCounts(t, s, 2, 3, 0, 2)
}

func TestRequestWorkEmptyDependenciesAndRequestJSON(t *testing.T) {
	s, _, thread, request := requestWorkFixture(t)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.RequestWorkRequest
	if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded, request) {
		t.Fatalf("request JSON round trip: %s, %+v, %v", data, decoded, err)
	}
	for _, forbidden := range []string{"Actor", "Policy", "Strength", "Mandate", "Turn", "Scope", "Grant", "Sender", "Sequence", "Status", "Revision"} {
		for _, value := range []any{request, *request.NewTask} {
			typeOf := reflect.TypeOf(value)
			for i := range typeOf.NumField() {
				if strings.Contains(typeOf.Field(i).Name, forbidden) {
					t.Fatalf("public DTO exposes %s", typeOf.Field(i).Name)
				}
			}
		}
	}
	receipt, err := s.RequestWork(ctx, thread.GroupID, decoded, "empty")
	if err != nil || receipt.Mandate.Scope.DependencyIDs == nil {
		t.Fatalf("empty snapshot is nil: %+v, %v", receipt, err)
	}
	data, err = json.Marshal(receipt)
	if err != nil || !strings.Contains(string(data), `"dependency_ids":[]`) || strings.Contains(string(data), `"status"`) || strings.Contains(string(data), `"revision":`) {
		t.Fatalf("receipt contains live task fields or null deps: %s, %v", data, err)
	}
	request.NewTask.DependencyIDs = []string{}
	if got, err := s.RequestWork(ctx, thread.GroupID, request, "empty"); err != nil || !reflect.DeepEqual(got, receipt) {
		t.Fatalf("nil/empty deps have different payloads: %+v, %v", got, err)
	}
}
