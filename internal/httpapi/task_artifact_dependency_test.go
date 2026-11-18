package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type artifactDependencyWire struct {
	DependencyID string `json:"dependency_id"`
	SubmissionID string `json:"submission_id"`
	ArtifactID   string `json:"artifact_id"`
	VersionID    string `json:"version_id"`
	ReadyWhen    string `json:"ready_when"`
}

func artifactDependencyBody(t *testing.T, ids []string, pins ...artifactDependencyWire) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"title": "consumer", "acceptance": "criteria", "dependency_ids": ids, "artifact_dependencies": pins,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func artifactDependencySubmit(t *testing.T, a *groupAPI, path string, rev int64, versions ...domain.ArtifactVersion) domain.Submission {
	t.Helper()
	attempt := decoded[domain.TaskAttempt](t, a.request("POST", path+"/attempts", taskLifecycleAttemptBody(rev, "")), 201)
	refs := make([]domain.ArtifactVersionRef, 0, len(versions))
	for _, v := range versions {
		refs = append(refs, domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ID})
	}
	data, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(taskLifecycleSubmissionBody(rev, attempt.ID), "}") + `,"artifact_versions":` + string(data) + `}`
	return decoded[domain.Submission](t, a.request("POST", path+"/submissions", body), 201)
}

func TestTaskArtifactDependencyHTTPExactReview(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g := newGroup(t, a, "group")
	parent, parentPath := newTaskLifecycleTask(t, a, g.ID)
	base := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, Name: "output", SourceTaskID: parent.ID})
	later := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID, SourceTaskID: parent.ID})
	sub := artifactDependencySubmit(t, a, parentPath, 1, base, later)
	pin := artifactDependencyWire{parent.ID, sub.ID, base.ArtifactID, base.ID, "accepted"}
	collection := "/api/v1/groups/" + g.ID + "/tasks"
	body := artifactDependencyBody(t, []string{parent.ID}, pin)
	created := a.request("POST", collection, body, "pin")
	child := decoded[domain.Task](t, created, 201)
	childPath := collection + "/" + child.ID
	read := a.request("GET", childPath+"/artifact-dependencies", "")
	got := decoded[listResult[artifactDependencyWire]](t, read, 200)
	if !slices.Equal(got.Items, []artifactDependencyWire{pin}) || got.NextCursor != "" {
		t.Fatalf("expected immutable historical version %v, got %+v", pin, got)
	}
	changed := pin
	changed.VersionID = later.ID // Both versions belong to this Submission.
	w := a.request("POST", collection, artifactDependencyBody(t, []string{parent.ID}, changed), "pin")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")

	blocked := func() {
		t.Helper()
		taskLifecycleError(t, a.request("POST", childPath+"/attempts", taskLifecycleAttemptBody(1, ""), "start"), 409, "conflict")
		taskLifecycleState(t, a, childPath, domain.TaskStatusOpen, 1, "", "")
	}
	blocked()
	if _, err := platform.NewLocalUser(a.db).CreateMessage(context.Background(), g.ID, platform.MessageInput{Content: "已接受成果，可以开始。"}, ""); err != nil {
		t.Fatal(err)
	}
	blocked()
	decoded[domain.ReviewDecision](t, a.request("POST", parentPath+"/reviews", taskLifecycleReviewBody(1, sub.ID, domain.ReviewOutcomeAccepted)), 201)
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = openTaskLifecycleAPI(t, a.path)
	if w := a.request("POST", collection, body, "pin"); w.Code != 201 || !bytes.Equal(w.Body.Bytes(), created.Body.Bytes()) {
		t.Fatalf("create replay changed after Review and reopen: %d %s", w.Code, w.Body)
	}
	if w := a.request("GET", childPath+"/artifact-dependencies", ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), read.Body.Bytes()) {
		t.Fatalf("pin changed after Review and reopen: %d %s", w.Code, w.Body)
	}
	decoded[domain.TaskAttempt](t, a.request("POST", childPath+"/attempts", taskLifecycleAttemptBody(1, ""), "start"), 201)
}

func TestTaskArtifactDependencyHTTPRejectedReceiptStaysBlocked(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g := newGroup(t, a, "group")
	parent, parentPath := newTaskLifecycleTask(t, a, g.ID)
	base := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, Name: "output", SourceTaskID: parent.ID})
	first := artifactDependencySubmit(t, a, parentPath, 1, base)
	ids := []string{parent.ID}
	pin := artifactDependencyWire{parent.ID, first.ID, base.ArtifactID, base.ID, "accepted"}
	collection := "/api/v1/groups/" + g.ID + "/tasks"
	body := artifactDependencyBody(t, ids, pin)
	created := a.request("POST", collection, body, "old-pin")
	child := decoded[domain.Task](t, created, 201)
	childPath := collection + "/" + child.ID
	decoded[domain.ReviewDecision](t, a.request("POST", parentPath+"/reviews", taskLifecycleReviewBody(1, first.ID, domain.ReviewOutcomeRejected)), 201)

	later := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID, SourceTaskID: parent.ID})
	// Even an acceptance that cites the exact same version under a different
	// Submission must not fulfill the original dependency.
	second := artifactDependencySubmit(t, a, parentPath, 2, base, later)
	decoded[domain.ReviewDecision](t, a.request("POST", parentPath+"/reviews", taskLifecycleReviewBody(2, second.ID, domain.ReviewOutcomeAccepted)), 201)
	w := a.request("POST", collection, body, "new-pin")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	if w := a.request("POST", collection, body, "old-pin"); w.Code != 201 || !bytes.Equal(w.Body.Bytes(), created.Body.Bytes()) {
		t.Fatalf("historical create did not replay after rejection: %d %s", w.Code, w.Body)
	}
	taskLifecycleError(t, a.request("POST", childPath+"/attempts", taskLifecycleAttemptBody(1, ""), "blocked"), 409, "conflict")
	taskLifecycleState(t, a, childPath, domain.TaskStatusOpen, 1, "", "")
	taskLifecycleCounts(t, messageSQL(t, a), map[string]int{
		"tasks": 2, "task_dependencies": 1, "task_artifact_dependencies": 1, "task_attempts": 2, "mutation_idempotency": 1,
	})

	// The plain dependency is still ready, and an explicit new pin can use the
	// accepted Submission. The failed create did not reserve its key.
	plain := decoded[domain.Task](t, a.request("POST", collection, artifactDependencyBody(t, ids)), 201)
	pin.SubmissionID, pin.VersionID = second.ID, later.ID
	updated := decoded[domain.Task](t, a.request("POST", collection, artifactDependencyBody(t, ids, pin), "new-pin"), 201)
	for _, ready := range []domain.Task{plain, updated} {
		decoded[domain.TaskAttempt](t, a.request("POST", collection+"/"+ready.ID+"/attempts", taskLifecycleAttemptBody(1, "")), 201)
	}
}

func TestTaskArtifactDependencyHTTPSemanticValidationFollowsIdempotency(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g := newGroup(t, a, "group")
	parent, parentPath := newTaskLifecycleTask(t, a, g.ID)
	version := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, Name: "output", SourceTaskID: parent.ID})
	sub := artifactDependencySubmit(t, a, parentPath, 1, version)
	collection := "/api/v1/groups/" + g.ID + "/tasks"
	ids := []string{parent.ID}
	pin := artifactDependencyWire{parent.ID, sub.ID, version.ArtifactID, version.ID, "accepted"}
	pinned := artifactDependencyBody(t, ids, pin)
	invalidPin := pin
	invalidPin.ReadyWhen = "submitted"
	invalid := artifactDependencyBody(t, ids, invalidPin)
	invalidJSON := strings.Replace(invalid, `"ready_when":"submitted"`, `"ready_when":true`, 1)

	t.Run("fresh-key", func(t *testing.T) {
		taskLifecycleError(t, a.request("POST", collection, invalid, "fresh"), 400, "invalid_input")
		decoded[domain.Task](t, a.request("POST", collection, pinned, "fresh"), 201)
	})
	for _, tc := range []struct{ key, body string }{
		{"plain-key", artifactDependencyBody(t, ids)},
		{"pinned-key", pinned},
	} {
		t.Run(tc.key, func(t *testing.T) {
			decoded[domain.Task](t, a.request("POST", collection, tc.body, tc.key), 201)
			taskLifecycleError(t, a.request("POST", collection, invalidJSON, tc.key), 400, "invalid_json")
			w := a.request("POST", collection, invalid, tc.key)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
			if !strings.Contains(w.Body.String(), "幂等键已用于不同的请求内容") {
				t.Fatalf("expected idempotency key conflict: %s", w.Body)
			}
		})
	}
	t.Run("fresh-pin-limit", func(t *testing.T) {
		// Use distinct, syntactically valid pairs so ordinary pin validation
		// would proceed to the missing-Task lookup without the count bound.
		ids, pins := make([]string, 201), make([]artifactDependencyWire, 201)
		for i := range pins {
			ids[i] = "missing-" + strconv.Itoa(i)
			pins[i] = pin
			pins[i].DependencyID = ids[i]
		}
		oversized := artifactDependencyBody(t, ids, pins...)
		taskLifecycleError(t, a.request("POST", collection, oversized, "limit"), 400, "invalid_input")
		decoded[domain.Task](t, a.request("POST", collection, pinned, "limit"), 201)
		w := a.request("POST", collection, oversized, "limit")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
	})
}

func TestTaskArtifactDependencyHTTPRejectsInvalidPinsAtomically(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g, foreign := newGroup(t, a, "group"), newGroup(t, a, "foreign")
	parent, parentPath := newTaskLifecycleTask(t, a, g.ID)
	sibling, siblingPath := newTaskLifecycleTask(t, a, g.ID)
	outsider, outsiderPath := newTaskLifecycleTask(t, a, foreign.ID)
	base := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, Name: "output", SourceTaskID: parent.ID})
	wrongSource := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID, SourceTaskID: sibling.ID})
	unsubmitted := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, ArtifactID: base.ArtifactID, ExpectedVersionID: wrongSource.ID, SourceTaskID: parent.ID})
	unattributed := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, Name: "unattributed"})
	foreignVersion := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: foreign.ID, Name: "foreign", SourceTaskID: outsider.ID})
	sub := artifactDependencySubmit(t, a, parentPath, 1, base, wrongSource, unattributed)
	siblingSub := artifactDependencySubmit(t, a, siblingPath, 1, base, wrongSource)
	foreignSub := artifactDependencySubmit(t, a, outsiderPath, 1, foreignVersion)
	valid := artifactDependencyWire{parent.ID, sub.ID, base.ArtifactID, base.ID, "accepted"}
	ids := []string{parent.ID}
	collection := "/api/v1/groups/" + g.ID + "/tasks"
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"missing-base-edge", artifactDependencyBody(t, nil, valid), 400, "invalid_input"},
		{"duplicate", artifactDependencyBody(t, ids, valid, valid), 400, "invalid_input"},
		{"unknown-condition", artifactDependencyBody(t, ids, artifactDependencyWire{parent.ID, sub.ID, base.ArtifactID, base.ID, "submitted"}), 400, "invalid_input"},
		{"missing-version", artifactDependencyBody(t, ids, artifactDependencyWire{parent.ID, sub.ID, base.ArtifactID, "", "accepted"}), 400, "invalid_input"},
		{"foreign-edge", artifactDependencyBody(t, []string{outsider.ID}, artifactDependencyWire{outsider.ID, foreignSub.ID, foreignVersion.ArtifactID, foreignVersion.ID, "accepted"}), 404, "not_found"},
		{"foreign-submission", artifactDependencyBody(t, ids, artifactDependencyWire{parent.ID, foreignSub.ID, foreignVersion.ArtifactID, foreignVersion.ID, "accepted"}), 404, "not_found"},
		{"sibling-submission", artifactDependencyBody(t, ids, artifactDependencyWire{parent.ID, siblingSub.ID, base.ArtifactID, base.ID, "accepted"}), 404, "not_found"},
		{"wrong-pair", artifactDependencyBody(t, ids, artifactDependencyWire{parent.ID, sub.ID, unattributed.ArtifactID, base.ID, "accepted"}), 404, "not_found"},
		{"not-in-submission", artifactDependencyBody(t, ids, artifactDependencyWire{parent.ID, sub.ID, unsubmitted.ArtifactID, unsubmitted.ID, "accepted"}), 404, "not_found"},
		{"wrong-source", artifactDependencyBody(t, ids, artifactDependencyWire{parent.ID, sub.ID, wrongSource.ArtifactID, wrongSource.ID, "accepted"}), 404, "not_found"},
		{"missing-source", artifactDependencyBody(t, ids, artifactDependencyWire{parent.ID, sub.ID, unattributed.ArtifactID, unattributed.ID, "accepted"}), 404, "not_found"},
		{"client-reviewer", strings.Replace(artifactDependencyBody(t, ids, valid), `"ready_when":`, `"reviewed_by":"user","ready_when":`, 1), 400, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := a.request("POST", collection, tc.body, "retry")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), tc.status, tc.code)
		})
	}
	taskLifecycleCounts(t, messageSQL(t, a), map[string]int{
		"tasks": 3, "task_dependencies": 0, "task_artifact_dependencies": 0, "mutation_idempotency": 0,
	})
	// A corrected request succeeds under the same key. Reordering well-formed
	// pins and legacy dependency IDs is canonical, without deduplicating pins.
	otherPin := artifactDependencyWire{sibling.ID, siblingSub.ID, wrongSource.ArtifactID, wrongSource.ID, "accepted"}
	first := a.request("POST", collection, artifactDependencyBody(t, []string{parent.ID, sibling.ID}, valid, otherPin), "retry")
	child := decoded[domain.Task](t, first, 201)
	childPath := collection + "/" + child.ID
	target := childPath + "/artifact-dependencies"
	want := []artifactDependencyWire{valid, otherPin}
	if want[0].DependencyID < want[1].DependencyID {
		slices.Reverse(want)
	}
	firstURL := target + "?limit=1"
	head := a.request("GET", firstURL, "")
	firstPage := decoded[listResult[artifactDependencyWire]](t, head, 200)
	var cursor pageCursor
	if !slices.Equal(firstPage.Items, want[:1]) || parseCursor(firstPage.NextCursor, &cursor) != nil ||
		cursor.Scope != "task_artifact_dependencies:"+child.ID || cursor.GroupID != g.ID ||
		cursor.ID != want[0].DependencyID || !cursor.CreatedAt.Equal(child.CreatedAt) {
		t.Fatalf("first pin page or cursor: %+v %+v", firstPage, cursor)
	}
	tailURL := firstURL + "&before=" + firstPage.NextCursor
	tail := a.request("GET", tailURL, "")
	lastPage := decoded[listResult[artifactDependencyWire]](t, tail, 200)
	if !slices.Equal(lastPage.Items, want[1:]) || lastPage.NextCursor != "" {
		t.Fatalf("exactly full last pin page: %+v", lastPage)
	}

	w := a.request("POST", collection, artifactDependencyBody(t, []string{sibling.ID, parent.ID, parent.ID}, otherPin, valid), "retry")
	if w.Code != 201 || !bytes.Equal(w.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("pin ordering changed keyed create: %d %s", w.Code, w.Body)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = openTaskLifecycleAPI(t, a.path)
	for url, body := range map[string][]byte{firstURL: head.Body.Bytes(), tailURL: tail.Body.Bytes()} {
		if w := a.request("GET", url, ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), body) {
			t.Fatalf("pin page changed after create replay and reopen: %d %s", w.Code, w.Body)
		}
	}
	foreignTarget := "/api/v1/groups/" + foreign.ID + "/tasks/" + child.ID + "/artifact-dependencies"
	taskLifecycleError(t, a.request("GET", foreignTarget, ""), 404, "not_found")
	for _, wrong := range []string{foreignTarget, parentPath + "/artifact-dependencies", childPath + "/attempts"} {
		taskLifecycleError(t, a.request("GET", wrong+"?before="+firstPage.NextCursor, ""), 400, "invalid_input")
	}
	for _, query := range []string{"limit=201", "before=invalid"} {
		taskLifecycleError(t, a.request("GET", target+"?"+query, ""), 400, "invalid_input")
	}
	empty := decoded[listResult[artifactDependencyWire]](t, a.request("GET", parentPath+"/artifact-dependencies", ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("unbound Task must return an empty pin array: %+v", empty)
	}
	if w := a.request("GET", collection, ""); w.Code != 200 || strings.Contains(w.Body.String(), `"artifact_dependencies"`) {
		t.Fatalf("Task list changed shape: %d %s", w.Code, w.Body)
	}
}
