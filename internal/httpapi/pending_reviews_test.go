package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestPendingReviewsHTTP(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	const path = "/api/v1/me/pending-reviews"
	type row struct {
		Group      struct{ ID, Name string }
		Task       struct{ ID, Title, Acceptance string }
		TopicID    string `json:"topic_id"`
		Submission domain.Submission
	}
	list := func(query string) listResult[row] {
		t.Helper()
		return decoded[listResult[row]](t, a.request("GET", path+query, ""), 200)
	}
	if empty := list(""); empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty inbox: %+v", empty)
	}
	g, sibling := newGroup(t, a, "调研群"), newGroup(t, a, "写作群")
	taskA, pathA := newTaskLifecycleTask(t, a, g.ID)
	taskB, pathB := newTaskLifecycleTask(t, a, sibling.ID)
	newTaskLifecycleTask(t, a, g.ID) // An open task, even with the same title, is not a decision.

	const topic = "原话题"
	ctx := context.Background()
	origin, err := a.db.CreateMessage(ctx, store.MessageRequest{GroupID: g.ID, TopicID: topic, ActorID: "user", Content: "请开展核对"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.RequestWork(ctx, g.ID, domain.RequestWorkRequest{TopicID: topic, MessageID: origin.ID,
		Mode: domain.RoutingDecisionLinkExisting, Reason: "明确授权", ExistingTask: &domain.ExistingTaskRequest{TaskID: taskA.ID, ExpectedRevision: 1}}, "admit"); err != nil {
		t.Fatal(err)
	}
	firstVersion := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, Name: "evidence"})
	newer := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, ArtifactID: firstVersion.ArtifactID, ExpectedVersionID: firstVersion.ID})
	if !newer.Applied || newer.ID == firstVersion.ID {
		t.Fatalf("expected a newer artifact head: %+v", newer)
	}
	review := func(taskPath string, s domain.Submission, outcome domain.ReviewOutcome, key string) *domain.ReviewDecision {
		t.Helper()
		body := taskLifecycleReviewBody(s.TaskRevision, s.ID, outcome)
		w := a.request("POST", taskPath+"/reviews", body, key)
		r := decoded[domain.ReviewDecision](t, w, 201)
		replay := a.request("POST", taskPath+"/reviews", body, key)
		if replay.Code != 201 || !bytes.Equal(replay.Body.Bytes(), w.Body.Bytes()) || r.SubmissionID != s.ID || r.GroupID != s.GroupID ||
			r.TaskID != s.TaskID || r.TaskRevision != s.TaskRevision || r.Outcome != outcome || !reflect.DeepEqual(r.ArtifactVersions, s.ArtifactVersions) {
			t.Fatalf("review receipt mismatch: %+v (replay %d %s)", r, replay.Code, replay.Body)
		}
		return &r
	}
	submit := func(taskPath string, revision int64, refs []domain.ArtifactVersionRef) domain.Submission {
		t.Helper()
		attempt := decoded[domain.TaskAttempt](t, a.request("POST", taskPath+"/attempts", taskLifecycleAttemptBody(revision, "")), 201)
		body := taskLifecycleSubmissionBody(revision, attempt.ID)
		if len(refs) > 0 {
			data, err := json.Marshal(refs)
			if err != nil {
				t.Fatal(err)
			}
			body = strings.TrimSuffix(body, "}") + `,"artifact_versions":` + string(data) + `}`
		}
		return decoded[domain.Submission](t, a.request("POST", taskPath+"/submissions", body), 201)
	}
	old := submit(pathA, 1, nil)
	review(pathA, old, domain.ReviewOutcomeRejected, "historical")
	subA := submit(pathA, 2, []domain.ArtifactVersionRef{{ArtifactID: firstVersion.ArtifactID, VersionID: firstVersion.ID}})
	subB := submit(pathB, 1, nil)

	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	first := list("?limit=1")
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	second := list("?limit=1&before=" + url.QueryEscape(first.NextCursor))
	if len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatalf("final full page: %+v", second)
	}
	check := func(item row, group domain.Group, task domain.Task, submission domain.Submission, topic string) {
		t.Helper()
		if item.Group.ID != group.ID || item.Group.Name != group.Name || item.Task.ID != task.ID || item.Task.Title != task.Title ||
			item.Task.Acceptance != task.Acceptance || item.TopicID != topic || !reflect.DeepEqual(item.Submission, submission) {
			t.Fatalf("inbox item lost exact group, task, topic or submission: %+v, want %+v", item, submission)
		}
	}
	check(first.Items[0], sibling, taskB, subB, "")
	check(second.Items[0], g, taskA, subA, topic)
	for _, query := range []string{"?limit=201", "?limit=1&limit=2", "?group_id=" + g.ID, "?before=invalid"} {
		taskLifecycleError(t, a.request("GET", path+query, ""), 400, "invalid_input")
	}
	taskLifecycleError(t, a.request("GET", "/api/v1/groups?before="+url.QueryEscape(first.NextCursor), ""), 400, "invalid_input")
	taskLifecycleError(t, a.request("POST", path, "{}"), 405, "method_not_allowed")
	coordinationHTTPUnchanged(t, raw, before)

	accepted := review(pathA, subA, domain.ReviewOutcomeAccepted, "inbox")
	taskLifecycleError(t, a.request("POST", pathA+"/reviews", taskLifecycleReviewBody(subA.TaskRevision, subA.ID, domain.ReviewOutcomeRejected), "second-review"), 409, "conflict")
	if got := decoded[domain.ReviewDecision](t, a.request("GET", pathA+"/reviews/"+accepted.ID, ""), 200); !reflect.DeepEqual(got, *accepted) {
		t.Fatalf("saved review differs: %+v", got)
	}
	if remaining := list(""); len(remaining.Items) != 1 || remaining.NextCursor != "" {
		t.Fatalf("exact review did not leave only the other group's decision: %+v", remaining)
	} else {
		check(remaining.Items[0], sibling, taskB, subB, "")
	}
	review(pathB, subB, domain.ReviewOutcomeRejected, "inbox")
	if empty := list(""); empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("reviewed submissions remain in inbox: %+v", empty)
	}
	taskLifecycleCounts(t, raw, map[string]int{"task_review_decisions": 3, "turns": 0, "deliveries": 0, "resource_claims": 0})
}
