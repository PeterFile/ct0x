package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func openTaskLifecycleAPI(t *testing.T, path string) *groupAPI {
	t.Helper()
	a := openTaskAPI(t, path)
	local := platform.NewLocalUser(a.db)
	routes := append(LocalUserRoutes(local), TaskRoutes(local)...)
	lifecycle := TaskLifecycleRoutes(local)
	if len(lifecycle) != 9 {
		t.Fatalf("lifecycle route count = %d, want 9", len(lifecycle))
	}
	a.handler = mustServer(t, DefaultAddress, append(routes, lifecycle...)...).Handler
	return a
}

func newTaskLifecycleTask(t *testing.T, a *groupAPI, groupID string) (domain.Task, string) {
	t.Helper()
	path := "/api/v1/groups/" + groupID + "/tasks"
	task := decoded[domain.Task](t, a.request("POST", path, `{"title":"task","acceptance":"criteria"}`), 201)
	return task, path + "/" + task.ID
}

func taskLifecycleAttemptBody(revision int64, active string) string {
	return fmt.Sprintf(`{"expected_revision":%d,"expected_active_attempt_id":%q}`, revision, active)
}

func taskLifecycleSubmissionBody(revision int64, attempt string) string {
	return fmt.Sprintf(`{"expected_revision":%d,"attempt_id":%q,"summary":" exact summary "}`, revision, attempt)
}

func taskLifecycleReviewBody(revision int64, submission string, outcome domain.ReviewOutcome) string {
	return fmt.Sprintf(`{"expected_revision":%d,"submission_id":%q,"outcome":%q,"reason":" exact reason "}`, revision, submission, outcome)
}

func taskLifecycleState(t *testing.T, a *groupAPI, path string, status domain.TaskStatus, revision int64, active, submission string) {
	t.Helper()
	got := decoded[domain.Task](t, a.request("GET", path, ""), 200)
	if got.Status != status || got.Revision != revision || got.ActiveAttemptID != active || got.CurrentSubmissionID != submission {
		t.Fatalf("task lifecycle state: %+v", got)
	}
}

func taskLifecycleCounts(t *testing.T, db *sql.DB, counts map[string]int) {
	t.Helper()
	for table, want := range counts {
		var got int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s count = %d, %v; want %d", table, got, err, want)
		}
	}
}

func taskLifecycleError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), status, code)
	if status == 409 {
		var body struct{ Error struct{ Message string } }
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !strings.Contains(body.Error.Message, "任务") || !strings.Contains(body.Error.Message, "版本") || !strings.Contains(body.Error.Message, "幂等") {
			t.Fatalf("lifecycle conflict message is misleading: %s, %v", w.Body, err)
		}
	}
}

func TestTaskLifecycleHTTPHumanRejectRetryAcceptAndDurableReplays(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g := newGroup(t, a, "group")
	task, path := newTaskLifecycleTask(t, a, g.ID)
	dependent := decoded[domain.Task](t, a.request("POST", "/api/v1/groups/"+g.ID+"/tasks", fmt.Sprintf(`{"title":"dependent","acceptance":"criteria","dependency_ids":[%q]}`, task.ID)), 201)
	dependentPath := "/api/v1/groups/" + g.ID + "/tasks/" + dependent.ID + "/attempts"
	blocked := func() {
		t.Helper()
		taskLifecycleError(t, a.request("POST", dependentPath, taskLifecycleAttemptBody(1, ""), "blocked"), 409, "conflict")
	}
	type receipt struct {
		path, body, key, changed string
		response                 *httptest.ResponseRecorder
	}
	var receipts []receipt
	post := func(collection, body, key, changed string) *httptest.ResponseRecorder {
		t.Helper()
		w := a.request("POST", path+"/"+collection, body, key)
		if w.Code != 201 {
			t.Fatalf("%s creation: %d %s", collection, w.Code, w.Body)
		}
		receipts = append(receipts, receipt{path + "/" + collection, body, key, changed, w})
		retry := a.request("POST", path+"/"+collection, body, key)
		if retry.Code != 201 || !bytes.Equal(w.Body.Bytes(), retry.Body.Bytes()) {
			t.Fatalf("immediate %s replay: %d %s", collection, retry.Code, retry.Body)
		}
		return w
	}
	blocked()
	activation := taskLifecycleAttemptBody(1, "")
	attempt := decoded[domain.TaskAttempt](t, post("attempts", activation, "cycle-1", taskLifecycleAttemptBody(2, "")), 201)
	if attempt.ID == "" || attempt.TaskID != task.ID || attempt.GroupID != g.ID || attempt.TaskRevision != 1 || attempt.TurnID != "" || attempt.CreatedAt.IsZero() || attempt.CreatedAt.Location() != time.UTC {
		t.Fatalf("human attempt contract: %+v", attempt)
	}
	taskLifecycleState(t, a, path, domain.TaskStatusOpen, 1, attempt.ID, "")
	raw := messageSQL(t, a)
	taskLifecycleCounts(t, raw, map[string]int{"messages": 0, "deliveries": 0, "turns": 0, "resource_claims": 0})
	taskLifecycleError(t, a.request("POST", path+"/attempts", activation), 409, "conflict")
	taskLifecycleError(t, a.request("POST", path+"/submissions", taskLifecycleSubmissionBody(2, attempt.ID)), 409, "conflict")
	submit := taskLifecycleSubmissionBody(1, attempt.ID)
	submission := decoded[domain.Submission](t, post("submissions", submit, "cycle-1", strings.Replace(submit, " exact summary ", "changed", 1)), 201)
	if submission.ID == "" || submission.TaskID != task.ID || submission.GroupID != g.ID || submission.TaskRevision != 1 || submission.AttemptID != attempt.ID || submission.SubmittedBy != "user" || submission.Summary != " exact summary " || submission.ArtifactVersions == nil || len(submission.ArtifactVersions) != 0 {
		t.Fatalf("human submission contract: %+v", submission)
	}
	taskLifecycleState(t, a, path, domain.TaskStatusSubmitted, 1, attempt.ID, submission.ID)
	blocked()
	taskLifecycleError(t, a.request("POST", path+"/reviews", taskLifecycleReviewBody(2, submission.ID, domain.ReviewOutcomeAccepted)), 409, "conflict")
	reject := taskLifecycleReviewBody(1, submission.ID, domain.ReviewOutcomeRejected)
	decision := decoded[domain.ReviewDecision](t, post("reviews", reject, "cycle-1", taskLifecycleReviewBody(1, submission.ID, domain.ReviewOutcomeAccepted)), 201)
	if decision.ID == "" || decision.TaskID != task.ID || decision.GroupID != g.ID || decision.TaskRevision != 1 || decision.SubmissionID != submission.ID || decision.Outcome != domain.ReviewOutcomeRejected || decision.ReviewedBy != "user" || decision.Reason != " exact reason " || decision.ArtifactVersions == nil {
		t.Fatalf("rejected decision contract: %+v", decision)
	}
	taskLifecycleState(t, a, path, domain.TaskStatusOpen, 2, "", "")
	blocked()
	taskLifecycleError(t, a.request("POST", path+"/attempts", activation), 409, "conflict")
	secondActivation := taskLifecycleAttemptBody(2, "")
	secondAttempt := decoded[domain.TaskAttempt](t, post("attempts", secondActivation, "cycle-2", taskLifecycleAttemptBody(2, attempt.ID)), 201)
	if secondAttempt.ID == attempt.ID || secondAttempt.TaskRevision != 2 || secondAttempt.TurnID != "" {
		t.Fatalf("retry attempt: %+v", secondAttempt)
	}
	taskLifecycleError(t, a.request("POST", path+"/submissions", taskLifecycleSubmissionBody(2, attempt.ID)), 409, "conflict")
	secondSubmit := taskLifecycleSubmissionBody(2, secondAttempt.ID)
	secondSubmission := decoded[domain.Submission](t, post("submissions", secondSubmit, "cycle-2", strings.Replace(secondSubmit, " exact summary ", "changed", 1)), 201)
	taskLifecycleState(t, a, path, domain.TaskStatusSubmitted, 2, secondAttempt.ID, secondSubmission.ID)
	blocked()
	taskLifecycleError(t, a.request("POST", path+"/reviews", taskLifecycleReviewBody(2, submission.ID, domain.ReviewOutcomeAccepted)), 409, "conflict")
	accept := taskLifecycleReviewBody(2, secondSubmission.ID, domain.ReviewOutcomeAccepted)
	accepted := decoded[domain.ReviewDecision](t, post("reviews", accept, "cycle-2", strings.Replace(accept, " exact reason ", "changed", 1)), 201)
	if accepted.Outcome != domain.ReviewOutcomeAccepted || accepted.SubmissionID != secondSubmission.ID || accepted.TaskRevision != 2 || accepted.ReviewedBy != "user" {
		t.Fatalf("acceptance contract: %+v", accepted)
	}
	taskLifecycleState(t, a, path, domain.TaskStatusAccepted, 2, "", secondSubmission.ID)
	decoded[domain.TaskAttempt](t, a.request("POST", dependentPath, taskLifecycleAttemptBody(1, ""), "blocked"), 201)
	for _, r := range receipts {
		retry := a.request("POST", r.path, r.body, r.key)
		if retry.Code != 201 || !bytes.Equal(r.response.Body.Bytes(), retry.Body.Bytes()) {
			t.Fatalf("historical replay changed immutable evidence: %d %s", retry.Code, retry.Body)
		}
		taskLifecycleError(t, a.request("POST", r.path, r.changed, r.key), 409, "conflict")
		var value struct{ ID string }
		if err := json.Unmarshal(r.response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		detail := a.request("GET", r.path+"/"+value.ID, "")
		if detail.Code != 200 || !bytes.Equal(r.response.Body.Bytes(), detail.Body.Bytes()) {
			t.Fatalf("detail changed immutable evidence: %d %s", detail.Code, detail.Body)
		}
	}
	taskLifecycleCounts(t, raw, map[string]int{"messages": 0, "deliveries": 0, "turns": 0, "resource_claims": 0, "task_attempts": 3, "task_submissions": 2, "task_review_decisions": 2, "mutation_idempotency": 7})
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTaskLifecycleAPI(t, a.path)
	for _, r := range receipts {
		retry := reopened.request("POST", r.path, r.body, r.key)
		if retry.Code != 201 || !bytes.Equal(r.response.Body.Bytes(), retry.Body.Bytes()) {
			t.Fatalf("durable lifecycle replay changed: %d %s", retry.Code, retry.Body)
		}
	}
	taskLifecycleState(t, reopened, path, domain.TaskStatusAccepted, 2, "", secondSubmission.ID)
}

func TestTaskLifecycleHTTPSemanticValidationFollowsIdempotency(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g := newGroup(t, a, "group")
	_, path := newTaskLifecycleTask(t, a, g.ID)
	raw := messageSQL(t, a)
	counts := map[string]int{"tasks": 1, "task_attempts": 0, "task_submissions": 0, "task_review_decisions": 0, "mutation_idempotency": 0}
	type invalidPayload struct{ name, body string }
	type receipt struct {
		collection, body string
		invalid          []invalidPayload
		response         *httptest.ResponseRecorder
	}
	var receipts []receipt
	previousID := ""
	for _, operation := range []struct {
		collection, table string
		body              func(string) string
	}{
		{"attempts", "task_attempts", func(string) string { return taskLifecycleAttemptBody(1, "") }},
		{"submissions", "task_submissions", func(id string) string { return taskLifecycleSubmissionBody(1, id) }},
		{"reviews", "task_review_decisions", func(id string) string {
			return taskLifecycleReviewBody(1, id, domain.ReviewOutcomeRejected)
		}},
	} {
		body := operation.body(previousID)
		invalid := []invalidPayload{{"revision", strings.Replace(body, `"expected_revision":1`, `"expected_revision":0`, 1)}}
		if operation.collection == "reviews" {
			invalid = append(invalid, invalidPayload{"outcome", strings.Replace(body, `"outcome":"rejected"`, `"outcome":"invalid"`, 1)})
		}
		before := decoded[domain.Task](t, a.request("GET", path, ""), 200)
		for _, bad := range invalid {
			taskLifecycleError(t, a.request("POST", path+"/"+operation.collection, bad.body, "semantic"), 400, "invalid_input")
			if after := decoded[domain.Task](t, a.request("GET", path, ""), 200); !reflect.DeepEqual(after, before) {
				t.Fatalf("fresh invalid %s/%s changed task: %+v", operation.collection, bad.name, after)
			}
			taskLifecycleCounts(t, raw, counts)
		}
		response := a.request("POST", path+"/"+operation.collection, body, "semantic")
		previousID = decoded[struct{ ID string }](t, response, 201).ID
		counts[operation.table]++
		counts["mutation_idempotency"]++
		receipts = append(receipts, receipt{operation.collection, body, invalid, response})
	}
	// Rejection advances the revision and clears both current pointers.
	taskLifecycleState(t, a, path, domain.TaskStatusOpen, 2, "", "")
	before := decoded[domain.Task](t, a.request("GET", path, ""), 200)
	taskLifecycleCounts(t, raw, counts)
	for _, r := range receipts {
		target := path + "/" + r.collection
		for _, bad := range r.invalid {
			t.Run(r.collection+"/used-key-"+bad.name, func(t *testing.T) {
				taskLifecycleError(t, a.request("POST", target, bad.body, "semantic"), 409, "conflict")
			})
		}
		t.Run(r.collection+"/invalid-json", func(t *testing.T) {
			for _, body := range []string{
				`{`,
				strings.Replace(r.body, `"expected_revision":1`, `"expected_revision":"1"`, 1),
				strings.TrimSuffix(r.body, "}") + `,"actor_id":"forged"}`,
				strings.TrimSuffix(r.body, "}") + `,"expected_revision":1}`,
			} {
				for _, keys := range [][]string{nil, {"fresh-json"}, {"semantic"}} {
					taskLifecycleError(t, a.request("POST", target, body, keys...), 400, "invalid_json")
				}
			}
		})
		replay := a.request("POST", target, r.body, "semantic")
		if replay.Code != 201 || !bytes.Equal(replay.Body.Bytes(), r.response.Body.Bytes()) {
			t.Fatalf("%s historical replay changed: %d %s", r.collection, replay.Code, replay.Body)
		}
	}
	if after := decoded[domain.Task](t, a.request("GET", path, ""), 200); !reflect.DeepEqual(after, before) {
		t.Fatalf("conflicts, decode errors or replays changed task: %+v", after)
	}
	taskLifecycleCounts(t, raw, counts)
}

func TestTaskLifecycleHTTPRejectsInvalidInputAndMissingRefsWithoutWrites(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g := newGroup(t, a, "group")
	_, path := newTaskLifecycleTask(t, a, g.ID)
	attempt := decoded[domain.TaskAttempt](t, a.request("POST", path+"/attempts", taskLifecycleAttemptBody(1, "")), 201)
	inputs := []struct{ collection, body string }{
		{"attempts", taskLifecycleAttemptBody(1, attempt.ID)},
		{"submissions", taskLifecycleSubmissionBody(1, attempt.ID)},
		{"reviews", taskLifecycleReviewBody(1, "missing", domain.ReviewOutcomeAccepted)},
	}
	for _, input := range inputs {
		for _, replacement := range []string{"", `"expected_revision":0,`, `"expected_revision":-1,`} {
			body := strings.Replace(input.body, `"expected_revision":1,`, replacement, 1)
			taskLifecycleError(t, a.request("POST", path+"/"+input.collection, body, "retry"), 400, "invalid_input")
		}
		taskLifecycleError(t, a.request("POST", path+"/"+input.collection, strings.TrimSuffix(input.body, "}")+`,"actor_id":"forged"}`, "retry"), 400, "invalid_json")
	}
	// Each server-owned field is forbidden; none is a caller-selectable DTO field.
	for i, field := range []struct{ name, value string }{
		{"member_scope", `{}`}, {"turn_id", `"forged"`}, {"group_id", `"forged"`}, {"task_id", `"forged"`},
		{"id", `"forged"`}, {"status", `"accepted"`}, {"revision", `99`}, {"submitted_by", `"forged"`},
		{"reviewed_by", `"forged"`}, {"active_attempt_id", `"forged"`}, {"current_submission_id", `"forged"`},
		{"idempotency_key", `"forged"`}, {"ActorID", `"forged"`},
	} {
		input := inputs[i%len(inputs)]
		body := strings.TrimSuffix(input.body, "}") + fmt.Sprintf(`,%q:%s}`, field.name, field.value)
		taskLifecycleError(t, a.request("POST", path+"/"+input.collection, body, "retry"), 400, "invalid_json")
	}
	for _, body := range []string{`{`, `null`, `{"expected_revision":1,"Expected_Revision":1}`, inputs[0].body + ` {}`, `{"expected_revision":"1"}`, `{"expected_revision":1,"extra":true}`} {
		taskLifecycleError(t, a.request("POST", path+"/attempts", body, "retry"), 400, "invalid_json")
	}
	for _, outcome := range []domain.ReviewOutcome{"", "submitted", "Accepted"} {
		taskLifecycleError(t, a.request("POST", path+"/reviews", taskLifecycleReviewBody(1, "missing", outcome), "retry"), 400, "invalid_input")
	}
	for _, keys := range [][]string{{""}, {"same", "same"}, {"a,b"}, {"has space"}} {
		taskLifecycleError(t, a.request("POST", path+"/submissions", inputs[1].body, keys...), 400, "invalid_input")
	}
	for _, target := range []struct{ method, path, body string }{
		{"POST", path + "/submissions?actor_id=forged", inputs[1].body},
		{"GET", path + "/attempts/" + attempt.ID + "?limit=1", ""},
		{"GET", path + "/submissions?unknown=x", ""},
	} {
		taskLifecycleError(t, a.request(target.method, target.path, target.body, "retry"), 400, "invalid_input")
	}
	r := httptest.NewRequest("POST", "http://"+DefaultAddress+path+"/attempts", strings.NewReader(inputs[0].body))
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	taskLifecycleError(t, w, 415, "unsupported_media_type")
	taskLifecycleError(t, a.request("POST", path+"/submissions", strings.Replace(inputs[1].body, " exact summary ", " ", 1), "retry"), 400, "invalid_input")
	refs := strings.TrimSuffix(inputs[1].body, "}") + `,"artifact_versions":[{"artifact_id":"opaque-artifact","version_id":"opaque-version"}]}`
	taskLifecycleError(t, a.request("POST", path+"/submissions", refs, "retry"), 404, "not_found")
	taskLifecycleState(t, a, path, domain.TaskStatusOpen, 1, attempt.ID, "")
	taskLifecycleCounts(t, messageSQL(t, a), map[string]int{"task_attempts": 1, "task_submissions": 0, "task_review_decisions": 0, "mutation_idempotency": 0, "turns": 0, "deliveries": 0})
	decoded[domain.Submission](t, a.request("POST", path+"/submissions", inputs[1].body, "retry"), 201)
}

// These trusted registry metadata fixtures do not publish or validate blob bytes.
func taskLifecyclePublishMetadata(t *testing.T, db *store.Store, input store.PublishArtifactInput) domain.ArtifactVersion {
	t.Helper()
	input.ActorID, input.BlobHash, input.Size = "user", strings.Repeat("a", 64), 42
	version, err := db.PublishArtifact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func TestTaskLifecycleHTTPArtifactEvidenceSurvivesReviewAndHeadChanges(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g, otherGroup := newGroup(t, a, "group"), newGroup(t, a, "other")
	task, path := newTaskLifecycleTask(t, a, g.ID)
	source, _ := newTaskLifecycleTask(t, a, g.ID)
	attempt := decoded[domain.TaskAttempt](t, a.request("POST", path+"/attempts", taskLifecycleAttemptBody(1, "")), 201)
	ctx := context.Background()
	base := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, Name: "metadata fixture", SourceTaskID: source.ID})
	update := store.PublishArtifactInput{GroupID: g.ID, ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID, SourceTaskID: source.ID}
	applied := taskLifecyclePublishMetadata(t, a.db, update)
	candidate := taskLifecyclePublishMetadata(t, a.db, update)
	other := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: g.ID, Name: "other metadata"})
	foreign := taskLifecyclePublishMetadata(t, a.db, store.PublishArtifactInput{GroupID: otherGroup.ID, Name: "foreign metadata"})
	head, err := a.db.GetArtifact(ctx, g.ID, base.ArtifactID)
	if err != nil || head.CurrentVersionID != applied.ID || !applied.Applied || candidate.Applied || base.SourceTaskID != source.ID || source.ID == task.ID || candidate.SourceTaskID != source.ID {
		t.Fatalf("historical/candidate/cross-task fixture: %+v, %+v, %v", head, candidate, err)
	}
	wantRefs := []domain.ArtifactVersionRef{
		{ArtifactID: base.ArtifactID, VersionID: base.ID},
		{ArtifactID: base.ArtifactID, VersionID: candidate.ID},
		{ArtifactID: other.ArtifactID, VersionID: other.ID},
	}
	slices.SortFunc(wantRefs, func(a, b domain.ArtifactVersionRef) int {
		if order := strings.Compare(a.ArtifactID, b.ArtifactID); order != 0 {
			return order
		}
		return strings.Compare(a.VersionID, b.VersionID)
	})
	body := func(refs []domain.ArtifactVersionRef) string {
		t.Helper()
		data, err := json.Marshal(refs)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(taskLifecycleSubmissionBody(1, attempt.ID), "}") + `,"artifact_versions":` + string(data) + `}`
	}
	raw := messageSQL(t, a)
	for _, test := range []struct {
		name   string
		ref    domain.ArtifactVersionRef
		status int
		code   string
	}{
		{"missing-artifact-field", domain.ArtifactVersionRef{VersionID: base.ID}, 400, "invalid_input"},
		{"missing-version-field", domain.ArtifactVersionRef{ArtifactID: base.ArtifactID}, 400, "invalid_input"},
		{"missing-version", domain.ArtifactVersionRef{ArtifactID: base.ArtifactID, VersionID: "missing"}, 404, "not_found"},
		{"foreign-group", domain.ArtifactVersionRef{ArtifactID: foreign.ArtifactID, VersionID: foreign.ID}, 404, "not_found"},
		{"wrong-pair", domain.ArtifactVersionRef{ArtifactID: base.ArtifactID, VersionID: other.ID}, 404, "not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A mixed valid/invalid set must not leave partial associations.
			taskLifecycleError(t, a.request("POST", path+"/submissions", body([]domain.ArtifactVersionRef{wantRefs[0], test.ref}), "artifact-evidence"), test.status, test.code)
			taskLifecycleState(t, a, path, domain.TaskStatusOpen, 1, attempt.ID, "")
			taskLifecycleCounts(t, raw, map[string]int{"task_submissions": 0, "task_submission_artifacts": 0, "mutation_idempotency": 0})
		})
	}
	submitResponse := a.request("POST", path+"/submissions", body([]domain.ArtifactVersionRef{wantRefs[2], wantRefs[1], wantRefs[0], wantRefs[1]}), "artifact-evidence")
	submission := decoded[domain.Submission](t, submitResponse, 201)
	if !slices.Equal(submission.ArtifactVersions, wantRefs) || submission.SubmittedBy != "user" || submission.AttemptID != attempt.ID {
		t.Fatalf("corrected submission dropped or rewrote concrete refs: %+v", submission)
	}
	taskLifecycleState(t, a, path, domain.TaskStatusSubmitted, 1, attempt.ID, submission.ID)
	update.ExpectedVersionID = applied.ID
	later := taskLifecyclePublishMetadata(t, a.db, update)
	head, err = a.db.GetArtifact(ctx, g.ID, base.ArtifactID)
	if err != nil || !later.Applied || head.CurrentVersionID != later.ID {
		t.Fatalf("head did not advance before review: %+v, %v", head, err)
	}
	taskLifecycleState(t, a, path, domain.TaskStatusSubmitted, 1, attempt.ID, submission.ID)
	reviewBody := taskLifecycleReviewBody(1, submission.ID, domain.ReviewOutcomeAccepted)
	latestRef := domain.ArtifactVersionRef{ArtifactID: later.ArtifactID, VersionID: later.ID}
	replacement, err := json.Marshal([]domain.ArtifactVersionRef{latestRef})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"artifact_versions":` + string(replacement), `"reviewed_by":"forged"`} {
		forged := strings.TrimSuffix(reviewBody, "}") + `,` + field + `}`
		taskLifecycleError(t, a.request("POST", path+"/reviews", forged, "artifact-review"), 400, "invalid_json")
	}
	taskLifecycleState(t, a, path, domain.TaskStatusSubmitted, 1, attempt.ID, submission.ID)
	taskLifecycleCounts(t, raw, map[string]int{"task_review_decisions": 0, "mutation_idempotency": 1})
	reviewResponse := a.request("POST", path+"/reviews", reviewBody, "artifact-review")
	decision := decoded[domain.ReviewDecision](t, reviewResponse, 201)
	if !slices.Equal(decision.ArtifactVersions, wantRefs) || decision.ReviewedBy != "user" || decision.SubmissionID != submission.ID {
		t.Fatalf("review did not derive immutable submission refs: %+v", decision)
	}
	retry := a.request("POST", path+"/submissions", body([]domain.ArtifactVersionRef{wantRefs[1], wantRefs[2], wantRefs[0], wantRefs[0]}), "artifact-evidence")
	if retry.Code != 201 || !bytes.Equal(retry.Body.Bytes(), submitResponse.Body.Bytes()) {
		t.Fatalf("canonical submission replay after review/head change: %d %s", retry.Code, retry.Body)
	}
	taskLifecycleError(t, a.request("POST", path+"/submissions", body(append(slices.Clone(wantRefs), latestRef)), "artifact-evidence"), 409, "conflict")
	retry = a.request("POST", path+"/reviews", reviewBody, "artifact-review")
	if retry.Code != 201 || !bytes.Equal(retry.Body.Bytes(), reviewResponse.Body.Bytes()) {
		t.Fatalf("review replay after head change: %d %s", retry.Code, retry.Body)
	}
	gotSubmission := decoded[domain.Submission](t, a.request("GET", path+"/submissions/"+submission.ID, ""), 200)
	submissions := decoded[listResult[domain.Submission]](t, a.request("GET", path+"/submissions", ""), 200)
	if !reflect.DeepEqual(gotSubmission, submission) || !reflect.DeepEqual(submissions.Items, []domain.Submission{submission}) || submissions.NextCursor != "" {
		t.Fatalf("submission detail/list followed the head: %+v, %+v", gotSubmission, submissions)
	}
	gotReview := decoded[domain.ReviewDecision](t, a.request("GET", path+"/reviews/"+decision.ID, ""), 200)
	reviews := decoded[listResult[domain.ReviewDecision]](t, a.request("GET", path+"/reviews", ""), 200)
	if !reflect.DeepEqual(gotReview, decision) || !reflect.DeepEqual(reviews.Items, []domain.ReviewDecision{decision}) || reviews.NextCursor != "" {
		t.Fatalf("review detail/list followed the head: %+v, %+v", gotReview, reviews)
	}
	taskLifecycleState(t, a, path, domain.TaskStatusAccepted, 1, "", submission.ID)
	taskLifecycleCounts(t, raw, map[string]int{"artifacts": 3, "artifact_versions": 6, "task_submissions": 1, "task_submission_artifacts": 3, "task_review_decisions": 1, "mutation_idempotency": 2, "messages": 0, "turns": 0, "resource_claims": 0})
}

func TestTaskLifecycleHTTPReadIsolationAndForeignBodyPointers(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	task, path := newTaskLifecycleTask(t, a, g.ID)
	_, siblingPath := newTaskLifecycleTask(t, a, g.ID)
	_, foreignPath := newTaskLifecycleTask(t, a, other.ID)
	attempt := decoded[domain.TaskAttempt](t, a.request("POST", path+"/attempts", taskLifecycleAttemptBody(1, "")), 201)
	submission := decoded[domain.Submission](t, a.request("POST", path+"/submissions", taskLifecycleSubmissionBody(1, attempt.ID)), 201)
	decision := decoded[domain.ReviewDecision](t, a.request("POST", path+"/reviews", taskLifecycleReviewBody(1, submission.ID, domain.ReviewOutcomeAccepted)), 201)
	for _, item := range []struct{ collection, id string }{{"attempts", attempt.ID}, {"submissions", submission.ID}, {"reviews", decision.ID}} {
		for _, target := range []string{siblingPath + "/" + item.collection + "/" + item.id, foreignPath + "/" + item.collection + "/" + item.id, "/api/v1/groups/" + other.ID + "/tasks/" + task.ID + "/" + item.collection + "/" + item.id, "/api/v1/groups/" + other.ID + "/tasks/" + task.ID + "/" + item.collection, path + "/" + item.collection + "/missing", "/api/v1/groups/missing/tasks/" + task.ID + "/" + item.collection} {
			taskLifecycleError(t, a.request("GET", target, ""), 404, "not_found")
		}
	}
	for _, target := range []string{siblingPath, foreignPath} {
		current := decoded[domain.TaskAttempt](t, a.request("POST", target+"/attempts", taskLifecycleAttemptBody(1, "")), 201)
		// Store checks the current-attempt fence before reading a foreign attempt.
		taskLifecycleError(t, a.request("POST", target+"/submissions", taskLifecycleSubmissionBody(1, attempt.ID)), 409, "conflict")
		taskLifecycleError(t, a.request("POST", target+"/reviews", taskLifecycleReviewBody(1, submission.ID, domain.ReviewOutcomeAccepted)), 404, "not_found")
		taskLifecycleState(t, a, target, domain.TaskStatusOpen, 1, current.ID, "")
	}
	for _, collection := range []string{"attempts", "submissions", "reviews"} {
		body := taskLifecycleAttemptBody(1, "")
		if collection == "submissions" {
			body = taskLifecycleSubmissionBody(1, attempt.ID)
		} else if collection == "reviews" {
			body = taskLifecycleReviewBody(1, submission.ID, domain.ReviewOutcomeAccepted)
		}
		taskLifecycleError(t, a.request("POST", "/api/v1/groups/"+other.ID+"/tasks/"+task.ID+"/"+collection, body, "inaccessible"), 404, "not_found")
	}
}

func TestTaskLifecycleHTTPUserCannotSubmitAgentOwnedAttempt(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g := newGroup(t, a, "group")
	task, path := newTaskLifecycleTask(t, a, g.ID)
	member := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+g.ID+"/members", `{"name":"member","role":"role"}`), 201)
	ctx := context.Background()
	message, err := platform.NewLocalUser(a.db).CreateMessage(ctx, g.ID, platform.MessageInput{Content: "fixture", RecipientIDs: []string{member.ID}}, "")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1, ResourceKeys: []string{"shared"}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := a.db.BindMemberTool(ctx, store.TurnClaim{GroupID: g.ID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := a.db.ActivateTaskAttempt(ctx, store.ActivateTaskAttemptInput{GroupID: g.ID, TaskID: task.ID, ExpectedRevision: 1, ActorID: member.ID, MemberScope: &scope})
	if err != nil {
		t.Fatal(err)
	}
	taskLifecycleError(t, a.request("POST", path+"/submissions", taskLifecycleSubmissionBody(1, attempt.ID), "denied"), 404, "not_found")
	taskLifecycleState(t, a, path, domain.TaskStatusOpen, 1, attempt.ID, "")
	after, err := a.db.GetTurn(ctx, g.ID, turn.ID)
	if err != nil || !reflect.DeepEqual(after, turn) {
		t.Fatalf("denial changed external execution: %+v, %v", after, err)
	}
	taskLifecycleCounts(t, messageSQL(t, a), map[string]int{"task_submissions": 0, "mutation_idempotency": 0, "turns": 1, "resource_claims": 1})
}

func TestTaskLifecycleHTTPPaginationBoundsScopesAndArrays(t *testing.T) {
	a := openTaskLifecycleAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	task, path := newTaskLifecycleTask(t, a, g.ID)
	_, siblingPath := newTaskLifecycleTask(t, a, g.ID)
	want := map[string][]string{"attempts": {}, "submissions": {}, "reviews": {}}
	for _, collection := range []string{"attempts", "submissions", "reviews"} {
		empty := decoded[listResult[domain.TaskAttempt]](t, a.request("GET", path+"/"+collection, ""), 200)
		if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
			t.Fatalf("empty %s must contain an array: %+v", collection, empty)
		}
	}
	ctx := context.Background()
	for revision := int64(1); revision <= 51; revision++ {
		attempt, err := a.db.ActivateTaskAttempt(ctx, store.ActivateTaskAttemptInput{GroupID: g.ID, TaskID: task.ID, ActorID: "user", ExpectedRevision: revision})
		if err != nil {
			t.Fatal(err)
		}
		submission, err := a.db.SubmitTask(ctx, store.SubmitTaskInput{GroupID: g.ID, TaskID: task.ID, ActorID: "user", ExpectedRevision: revision, AttemptID: attempt.ID, Summary: "text"})
		if err != nil {
			t.Fatal(err)
		}
		decision, err := a.db.ReviewTask(ctx, store.ReviewTaskInput{GroupID: g.ID, TaskID: task.ID, ActorID: "user", ExpectedRevision: revision, SubmissionID: submission.ID, Outcome: domain.ReviewOutcomeRejected})
		if err != nil {
			t.Fatal(err)
		}
		for collection, id := range map[string]string{"attempts": attempt.ID, "submissions": submission.ID, "reviews": decision.ID} {
			want[collection] = append([]string{id}, want[collection]...)
		}
	}
	type item struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}
	tasksPath := "/api/v1/groups/" + g.ID + "/tasks"
	tasks := decoded[listResult[domain.Task]](t, a.request("GET", tasksPath+"?limit=1", ""), 200)
	if tasks.NextCursor == "" {
		t.Fatal("missing basic tasks cursor fixture")
	}
	for collection, expected := range want {
		t.Run(collection, func(t *testing.T) {
			target := path + "/" + collection
			defaults := decoded[listResult[item]](t, a.request("GET", target, ""), 200)
			maximum := decoded[listResult[item]](t, a.request("GET", target+"?limit=200", ""), 200)
			first := decoded[listResult[item]](t, a.request("GET", target+"?limit=1", ""), 200)
			if len(defaults.Items) != 50 || defaults.NextCursor == "" || len(maximum.Items) != 51 || maximum.NextCursor != "" || len(first.Items) != 1 || first.NextCursor == "" {
				t.Fatalf("%s bounds: default=%d max=%d min=%d", collection, len(defaults.Items), len(maximum.Items), len(first.Items))
			}
			var cursor pageCursor
			if err := parseCursor(first.NextCursor, &cursor); err != nil || cursor.Scope != "task_"+collection+":"+task.ID || cursor.GroupID != g.ID || cursor.ID != first.Items[0].ID || !cursor.CreatedAt.Equal(first.Items[0].CreatedAt) {
				t.Fatalf("%s cursor context: %+v, %v", collection, cursor, err)
			}
			tail := decoded[listResult[item]](t, a.request("GET", target+"?limit=200&before="+first.NextCursor, ""), 200)
			got := append(first.Items, tail.Items...)
			if len(got) != len(expected) || tail.NextCursor != "" {
				t.Fatalf("%s continuation: %+v", collection, tail)
			}
			for i, record := range got {
				if record.ID != expected[i] || maximum.Items[i].ID != expected[i] {
					t.Fatalf("%s pagination skipped/reordered record %d", collection, i)
				}
			}
			last := decoded[listResult[item]](t, a.request("GET", target+"?before="+defaults.NextCursor, ""), 200)
			if len(last.Items) != 1 || last.NextCursor != "" || last.Items[0].ID != expected[50] {
				t.Fatalf("%s default continuation: %+v", collection, last)
			}
			for _, wrongTarget := range []string{
				siblingPath + "/" + collection + "?before=" + first.NextCursor,
				"/api/v1/groups/" + other.ID + "/tasks/" + task.ID + "/" + collection + "?before=" + first.NextCursor,
				path + "/" + map[string]string{"attempts": "submissions", "submissions": "reviews", "reviews": "attempts"}[collection] + "?before=" + first.NextCursor,
				tasksPath + "?before=" + first.NextCursor,
				target + "?before=" + tasks.NextCursor,
			} {
				taskLifecycleError(t, a.request("GET", wrongTarget, ""), 400, "invalid_input")
			}
			for _, query := range []string{"limit=0", "limit=201", "limit=1&limit=2", "before=invalid"} {
				taskLifecycleError(t, a.request("GET", target+"?"+query, ""), 400, "invalid_input")
			}
		})
	}
}

func TestTaskLifecycleHTTPRoutesRemainOptIn(t *testing.T) {
	a := openTaskAPI(t, "")
	g := newGroup(t, a, "group")
	_, path := newTaskLifecycleTask(t, a, g.ID)
	for _, collection := range []string{"attempts", "submissions", "reviews"} {
		for _, target := range []string{path + "/" + collection, path + "/" + collection + "/missing"} {
			taskLifecycleError(t, a.request("GET", target, ""), 404, "not_found")
		}
	}
}

func TestTaskLifecycleHTTPErrorMappingPreservesUnknownAndContextSemantics(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("wrapped: %w", store.ErrMemberToolDenied), 404, "not_found"},
		{fmt.Errorf("wrapped: %w", store.ErrConflict), 409, "conflict"},
		{context.Canceled, 408, "request_canceled"},
		{context.DeadlineExceeded, 504, "deadline_exceeded"},
		{errors.New("private SQL/path details"), 500, "internal_error"},
	} {
		w := httptest.NewRecorder()
		taskLifecycleResult(w, 201, domain.TaskAttempt{}, tt.err)
		taskLifecycleError(t, w, tt.status, tt.code)
		if tt.status >= 500 || tt.status == 408 {
			original := httptest.NewRecorder()
			operationError(original, tt.err)
			if !bytes.Equal(w.Body.Bytes(), original.Body.Bytes()) {
				t.Fatalf("global error semantics changed: %s != %s", w.Body, original.Body)
			}
		}
	}
}
