package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestRunTaskLifecycleRoutes(t *testing.T) {
	// Trusted temporary Registry/blob setup is not an HTTP publication grant.
	f := newArtifactCmdFixture(t)
	initial := taskLifecycleSeedStarting(t, f.dbPath, f.other.ID)
	dsn := url.URL{Scheme: "file", Path: f.dbPath, RawQuery: "mode=ro"}
	snapshot := func() map[string][]string { return artifactBusinessSnapshot(t, dsn.String()) }
	seeded := snapshot()
	if len(seeded["turns"]) != 1 || len(seeded["resource_claims"]) != 1 {
		t.Fatal("synthetic sentinel must hold a nonempty resource claim before startup")
	}
	args := []string{"--data-dir", f.dir, "--listen", "127.0.0.1:0"}
	base, stop := startRunWithSearch(t, provider.Search{}, args...)
	baseline := taskLifecycleRecoverySnapshot(t, dsn.String(), seeded)
	t.Logf("synthetic Store-only sentinel %s: starting -> unknown after owned startup; one resource remains occupied; no provider CLI", initial.ID)
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	request := func(method, path, body, key string, status int, result any) []byte {
		t.Helper()
		r, err := http.NewRequest(method, base+path, strings.NewReader(body))
		artifactCmdMust(t, err)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		response, err := client.Do(r)
		artifactCmdMust(t, err)
		data, readErr := io.ReadAll(response.Body)
		artifactCmdMust(t, readErr)
		artifactCmdMust(t, response.Body.Close())
		if response.StatusCode != status || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || !json.Valid(data) {
			t.Fatalf("%s %s: status=%d want=%d type=%q body=%s", method, path, response.StatusCode, status, response.Header.Get("Content-Type"), data)
		}
		if status == http.StatusConflict {
			var failure struct {
				Error struct{ Code, Message string }
			}
			artifactCmdMust(t, json.Unmarshal(data, &failure))
			if failure.Error.Code != "conflict" || !chinese(failure.Error.Message) {
				t.Fatalf("changed-payload replay must return Chinese conflict JSON: %s", data)
			}
		}
		if result != nil {
			artifactCmdMust(t, json.Unmarshal(data, result))
		}
		return data
	}
	checkUnrelated := func() {
		t.Helper()
		after := snapshot()
		if len(after) != len(baseline) {
			t.Fatal("manual lifecycle changed the business table set")
		}
		for table, want := range baseline {
			switch table {
			case "tasks", "task_attempts", "task_submissions", "task_submission_artifacts", "task_review_decisions", "mutation_idempotency":
				continue
			}
			if got := after[table]; !reflect.DeepEqual(got, want) {
				t.Fatalf("manual lifecycle changed unrelated %s (including unknown Turn/held claim): got=%v want=%v", table, got, want)
			}
		}
	}

	var task domain.Task
	input := platform.TaskInput{Title: "人工交付", Description: "固定历史与候选成果", Acceptance: "只有显式用户评审可接纳"}
	executionJSON(t, "POST", base+"/api/v1/groups/"+f.group.ID+"/tasks", input, &task)
	if task.ID == "" || task.GroupID != f.group.ID || task.Title != input.Title || task.Description != input.Description || task.Acceptance != input.Acceptance || task.Status != domain.TaskStatusOpen || task.Revision != 1 || task.AssigneeID != "" || task.DependencyIDs == nil || len(task.DependencyIDs) != 0 || task.ActiveAttemptID != "" || task.CurrentSubmissionID != "" || task.CreatedAt.IsZero() {
		t.Fatalf("manual Task creation: %+v", task)
	}
	path := "/api/v1/groups/" + f.group.ID + "/tasks/" + task.ID
	checkTask := func(status domain.TaskStatus, active, submission string) {
		t.Helper()
		var got domain.Task
		request("GET", path, "", "", 200, &got)
		want := task
		want.Status, want.ActiveAttemptID, want.CurrentSubmissionID = status, active, submission
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("authoritative Task state: got=%+v want=%+v", got, want)
		}
	}
	for _, collection := range []string{"attempts", "submissions", "reviews"} {
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"next_cursor"`
		}
		request("GET", path+"/"+collection, "", "", 200, &page)
		if page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
			t.Fatalf("manual lifecycle must start empty, excluding trusted fixture work: %s %+v", collection, page)
		}
	}
	checkUnrelated()
	type receipt struct {
		path, body, key, changed, id, operation string
		response, payload                       []byte
	}
	var receipts []receipt
	post := func(collection, operation string, input, changed, result any) {
		t.Helper()
		body, err := json.Marshal(input)
		artifactCmdMust(t, err)
		different, err := json.Marshal(changed)
		artifactCmdMust(t, err)
		r := receipt{path: path + "/" + collection, body: string(body), key: "manual-" + collection, changed: string(different), operation: operation}
		r.response = request("POST", r.path, r.body, r.key, 201, result)
		var identity struct{ ID string }
		artifactCmdMust(t, json.Unmarshal(r.response, &identity))
		r.id = identity.ID
		if r.id == "" || !bytes.Equal(request("POST", r.path, r.body, r.key, 201, nil), r.response) {
			t.Fatalf("immediate %s replay changed original 201 receipt", collection)
		}
		request("POST", r.path, r.changed, r.key, 409, nil)
		raw, err := sql.Open("sqlite", dsn.String())
		artifactCmdMust(t, err)
		defer func() { artifactCmdMust(t, raw.Close()) }()
		var recordID string
		artifactCmdMust(t, raw.QueryRow(`SELECT record_id,payload FROM mutation_idempotency
			WHERE operation=? AND actor_id='user' AND group_id=? AND idempotency_key=?`, operation, f.group.ID, r.key).Scan(&recordID, &r.payload))
		if recordID != r.id || len(r.payload) == 0 {
			t.Fatalf("original %s receipt was not persisted with the user identity: %q", collection, recordID)
		}
		receipts = append(receipts, r)
		t.Logf("original POST %s key=%q body=%s status=201 receipt_bytes=%q", r.path, r.key, r.body, r.response)
	}

	var attempt domain.TaskAttempt
	activation := platform.TaskAttemptInput{ExpectedRevision: 1}
	changedActivation := activation
	changedActivation.ExpectedRevision++
	post("attempts", "activate_task_attempt", activation, changedActivation, &attempt)
	if attempt.ID == "" || attempt.TaskID != task.ID || attempt.GroupID != f.group.ID || attempt.TaskRevision != 1 || attempt.TurnID != "" || attempt.CreatedAt.IsZero() || attempt.CreatedAt.Location() != time.UTC {
		t.Fatalf("human Attempt must not bind the synthetic Turn or create execution: %+v", attempt)
	}
	checkTask(domain.TaskStatusOpen, attempt.ID, "")
	checkUnrelated()
	refs := []domain.ArtifactVersionRef{
		{ArtifactID: f.base.ArtifactID, VersionID: f.base.ID},
		{ArtifactID: f.candidate.ArtifactID, VersionID: f.candidate.ID},
	}
	slices.SortFunc(refs, func(a, b domain.ArtifactVersionRef) int { return strings.Compare(a.VersionID, b.VersionID) })
	checkArtifacts := func() {
		t.Helper()
		var artifact domain.Artifact
		request("GET", "/api/v1/groups/"+f.group.ID+"/artifacts/"+f.base.ArtifactID, "", "", 200, &artifact)
		if artifact.CurrentVersionID != f.latest.ID {
			t.Fatalf("fixture head must remain later than both fixed evidence versions: %+v", artifact)
		}
		for _, version := range []domain.ArtifactVersion{f.base, f.candidate} {
			var got domain.ArtifactVersion
			request("GET", artifactVersionURL(version), "", "", 200, &got)
			response, data := artifactSocketRequest(t, client, "GET", base+artifactVersionURL(version)+"/content", 200)
			if got != version || string(data) != f.data[version.ID] || fmt.Sprintf("%x", sha256.Sum256(data)) != version.BlobHash || response.Header.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("fixed historical/candidate metadata/content drifted: version=%+v got=%+v bytes=%d", version, got, len(data))
			}
		}
	}
	checkArtifacts()
	t.Logf("fixed artifact %s: historical=%s applied=%t candidate=%s applied=%t observed=%s later_head=%s; exact metadata/content verified", f.base.ArtifactID, f.base.ID, f.base.Applied, f.candidate.ID, f.candidate.Applied, f.candidate.ObservedVersionID, f.latest.ID)
	var submission domain.Submission
	submit := platform.TaskSubmissionInput{ExpectedRevision: 1, AttemptID: attempt.ID, Summary: " 原始人工交付证据 ", ArtifactVersions: refs}
	changedSubmit := submit
	changedSubmit.Summary = "changed evidence"
	post("submissions", "submit_task", submit, changedSubmit, &submission)
	if submission.ID == "" || submission.TaskID != task.ID || submission.GroupID != f.group.ID || submission.TaskRevision != 1 || submission.AttemptID != attempt.ID || submission.SubmittedBy != "user" || submission.Summary != submit.Summary || !reflect.DeepEqual(submission.ArtifactVersions, refs) || submission.CreatedAt.IsZero() || submission.CreatedAt.Location() != time.UTC {
		t.Fatalf("manual Submission lost exact user/fixed evidence: %+v", submission)
	}
	checkTask(domain.TaskStatusSubmitted, attempt.ID, submission.ID)
	var beforeReview struct {
		Items      []domain.ReviewDecision `json:"items"`
		NextCursor string                  `json:"next_cursor"`
	}
	request("GET", path+"/reviews", "", "", 200, &beforeReview)
	if beforeReview.Items == nil || len(beforeReview.Items) != 0 || beforeReview.NextCursor != "" || len(snapshot()["task_review_decisions"]) != len(baseline["task_review_decisions"]) {
		t.Fatalf("Submission must not fabricate acceptance or a Review: %+v", beforeReview)
	}
	checkUnrelated()
	var review domain.ReviewDecision
	reviewInput := platform.TaskReviewInput{ExpectedRevision: 1, SubmissionID: submission.ID, Outcome: domain.ReviewOutcomeAccepted, Reason: " 显式人工接纳 "}
	changedReview := reviewInput
	changedReview.Outcome = domain.ReviewOutcomeRejected
	post("reviews", "review_task", reviewInput, changedReview, &review)
	if review.ID == "" || review.SubmissionID != submission.ID || review.TaskID != task.ID || review.GroupID != f.group.ID || review.TaskRevision != 1 || review.Outcome != domain.ReviewOutcomeAccepted || review.ReviewedBy != "user" || review.Reason != reviewInput.Reason || !reflect.DeepEqual(review.ArtifactVersions, refs) || review.CreatedAt.IsZero() || review.CreatedAt.Location() != time.UTC {
		t.Fatalf("only explicit user Review may accept the original Submission refs: %+v", review)
	}
	checkTask(domain.TaskStatusAccepted, "", submission.ID)
	checkUnrelated()
	accepted := snapshot()
	for table, extra := range map[string]int{"tasks": 1, "task_attempts": 1, "task_submissions": 1, "task_submission_artifacts": 2, "task_review_decisions": 1, "mutation_idempotency": 3} {
		if len(accepted[table]) != len(baseline[table])+extra {
			t.Fatalf("unexpected manual flow writes to %s: got=%d fixture=%d extra=%d", table, len(accepted[table]), len(baseline[table]), extra)
		}
		for _, original := range baseline[table] {
			if !slices.Contains(accepted[table], original) {
				t.Fatalf("manual flow replaced trusted fixture row in %s: %s", table, original)
			}
		}
	}
	checkReadsAndReplay := func() {
		t.Helper()
		for _, r := range receipts {
			if !bytes.Equal(request("POST", r.path, r.body, r.key, 201, nil), r.response) {
				t.Fatalf("historical/reopened replay changed original immutable receipt: %s", r.path)
			}
			request("POST", r.path, r.changed, r.key, 409, nil)
			if !bytes.Equal(request("GET", r.path+"/"+r.id, "", "", 200, nil), r.response) {
				t.Fatalf("detail GET differs from original receipt: %s", r.path)
			}
			var page struct {
				Items      []json.RawMessage `json:"items"`
				NextCursor string            `json:"next_cursor"`
			}
			request("GET", r.path, "", "", 200, &page)
			if len(page.Items) != 1 || page.NextCursor != "" || !bytes.Equal(page.Items[0], bytes.TrimSpace(r.response)) {
				t.Fatalf("list GET differs from sole original receipt: %s %+v", r.path, page)
			}
			checkTask(domain.TaskStatusAccepted, "", submission.ID)
		}
		checkArtifacts()
		checkUnrelated()
		if got := snapshot(); !reflect.DeepEqual(got, accepted) {
			t.Fatalf("replays, conflicts or GETs changed persisted state: got=%v want=%v", got, accepted)
		}
	}
	checkDurable := func() {
		t.Helper()
		if got := snapshot(); !reflect.DeepEqual(got, accepted) {
			t.Fatalf("read-only stopped SQLite lost exact lifecycle/receipt/fixture state: got=%v want=%v", got, accepted)
		}
		raw, err := sql.Open("sqlite", dsn.String())
		artifactCmdMust(t, err)
		defer func() { artifactCmdMust(t, raw.Close()) }()
		var actor, submittedBy, reviewedBy string
		var noTurn bool
		artifactCmdMust(t, raw.QueryRow(`SELECT a.actor_id,a.turn_id IS NULL,s.submitted_by,r.reviewed_by
			FROM task_attempts a JOIN task_submissions s ON s.attempt_id=a.id
			JOIN task_review_decisions r ON r.submission_id=s.id
			WHERE a.id=? AND s.id=? AND r.id=? AND a.task_id=? AND s.task_id=a.task_id AND r.task_id=a.task_id
			AND a.group_id=? AND s.group_id=a.group_id AND r.group_id=a.group_id
			AND a.task_revision=1 AND s.task_revision=a.task_revision AND r.task_revision=a.task_revision`, attempt.ID, submission.ID, review.ID, task.ID, f.group.ID).Scan(&actor, &noTurn, &submittedBy, &reviewedBy))
		if actor != "user" || !noTurn || submittedBy != "user" || reviewedBy != "user" {
			t.Fatalf("durable human actors/empty TurnID changed: %q %t %q %q", actor, noTurn, submittedBy, reviewedBy)
		}
		var status domain.TaskStatus
		var revision int64
		var active, current string
		artifactCmdMust(t, raw.QueryRow(`SELECT status,revision,coalesce(active_attempt_id,''),current_submission_id
			FROM tasks WHERE id=? AND group_id=?`, task.ID, f.group.ID).Scan(&status, &revision, &active, &current))
		if status != domain.TaskStatusAccepted || revision != 1 || active != "" || current != submission.ID {
			t.Fatalf("durable accepted Task pointer changed: %s %d %q %q", status, revision, active, current)
		}
		rows, err := raw.Query(`SELECT artifact_id,version_id FROM task_submission_artifacts
			WHERE submission_id=? AND task_id=? AND group_id=? AND task_revision=1 ORDER BY artifact_id,version_id`, submission.ID, task.ID, f.group.ID)
		artifactCmdMust(t, err)
		var savedRefs []domain.ArtifactVersionRef
		for rows.Next() {
			var ref domain.ArtifactVersionRef
			artifactCmdMust(t, rows.Scan(&ref.ArtifactID, &ref.VersionID))
			savedRefs = append(savedRefs, ref)
		}
		artifactCmdMust(t, rows.Err())
		artifactCmdMust(t, rows.Close())
		if !reflect.DeepEqual(savedRefs, refs) {
			t.Fatalf("durable fixed pairs replaced by head: got=%+v want=%+v", savedRefs, refs)
		}
		for _, r := range receipts {
			var id string
			var payload []byte
			artifactCmdMust(t, raw.QueryRow(`SELECT record_id,payload FROM mutation_idempotency
				WHERE operation=? AND actor_id='user' AND group_id=? AND idempotency_key=?`, r.operation, f.group.ID, r.key).Scan(&id, &payload))
			if id != r.id || !bytes.Equal(payload, r.payload) {
				t.Fatalf("durable original user receipt changed: %s id=%q", r.path, id)
			}
		}
		for _, name := range []string{"runtime", "workspaces", "member-tools"} {
			if _, err := os.Lstat(filepath.Join(f.dir, name)); !os.IsNotExist(err) {
				t.Fatalf("manual lifecycle introduced execution/credential root %s: %v", name, err)
			}
		}
		t.Log("stopped mode=ro SQLite: exact lifecycle rows, user actors, NULL human TurnID, fixed pairs, original receipts, accepted Task and occupied unknown sentinel retained")
	}
	checkReadsAndReplay()
	artifactCmdMust(t, stop())
	checkDurable()
	beforeReopen := snapshot()
	base, stop = startRunWithSearch(t, provider.Search{}, args...)
	accepted = taskLifecycleRecoverySnapshot(t, dsn.String(), beforeReopen)
	baseline = accepted
	checkReadsAndReplay()
	artifactCmdMust(t, stop())
	checkDurable()
	t.Log("all nine cmd lifecycle routes exercised over loopback; immediate, post-acceptance and reopened 201 receipts byte-identical; changed payloads 409 without extra writes; synthetic claim is not provider/agent or full DSH acceptance")
}

func taskLifecycleSeedStarting(t *testing.T, path, groupID string) domain.Turn {
	t.Helper()
	db, err := store.Open(path)
	artifactCmdMust(t, err)
	defer func() { artifactCmdMust(t, db.Close()) }()
	ctx := context.Background()
	member, err := db.CreateMember(ctx, store.CreateMemberInput{GroupID: groupID, Name: "synthetic sentinel", Role: "Store-only unknown-resource fixture", ActorID: "user"})
	artifactCmdMust(t, err)
	message, err := db.CreateMessage(ctx, store.MessageRequest{GroupID: groupID, ActorID: "user", Content: "synthetic sentinel; never execute", TopicID: "sentinel"}, []string{member.ID})
	artifactCmdMust(t, err)
	if len(message.Deliveries) != 1 {
		t.Fatalf("synthetic sentinel requires one real stored delivery: %+v", message)
	}
	turn, err := db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1, ResourceKeys: []string{"synthetic:task-lifecycle-held"}})
	artifactCmdMust(t, err)
	if turn.State != domain.TurnStateStarting || turn.ClaimToken == "" {
		t.Fatalf("synthetic sentinel was not claimed through Store: %+v", turn)
	}
	return turn
}

// Only the existing singleton RecoverInFlight may change these three Turn fields.
func taskLifecycleRecoverySnapshot(t *testing.T, dsn string, before map[string][]string) map[string][]string {
	t.Helper()
	after := artifactBusinessSnapshot(t, dsn)
	if len(before) != len(after) || len(before["turns"]) != 1 || len(after["turns"]) != 1 || len(after["resource_claims"]) != 1 {
		t.Fatal("recovery must retain the sole synthetic unknown Turn and nonempty held claim")
	}
	for table, want := range before {
		if table != "turns" && !reflect.DeepEqual(after[table], want) {
			t.Fatalf("startup recovery changed %s: got=%v want=%v", table, after[table], want)
		}
	}
	raw, err := sql.Open("sqlite", dsn)
	artifactCmdMust(t, err)
	defer func() { artifactCmdMust(t, raw.Close()) }()
	rows, err := raw.Query("SELECT * FROM turns LIMIT 0")
	artifactCmdMust(t, err)
	columns, err := rows.Columns()
	artifactCmdMust(t, err)
	artifactCmdMust(t, rows.Close())
	var old, current []any
	artifactCmdMust(t, json.Unmarshal([]byte(before["turns"][0]), &old))
	artifactCmdMust(t, json.Unmarshal([]byte(after["turns"][0]), &current))
	if len(old) != len(columns) || len(current) != len(columns) {
		t.Fatal("Turn snapshot shape changed")
	}
	for i, column := range columns {
		switch column {
		case "state":
			if (old[i] != string(domain.TurnStateStarting) && old[i] != string(domain.TurnStateUnknown)) || current[i] != string(domain.TurnStateUnknown) {
				t.Fatalf("uncertain synthetic start must remain unknown after recovery: %v -> %v", old[i], current[i])
			}
			old[i] = current[i]
		case "claim_token":
			if current[i] == "" || current[i] == old[i] {
				t.Fatal("new owner must rotate the existing unknown Turn fence")
			}
			old[i] = current[i]
		case "updated_at":
			stamp, ok := current[i].(string)
			if !ok {
				t.Fatal("recovery timestamp is not text")
			}
			parsed, err := time.Parse(time.RFC3339Nano, stamp)
			artifactCmdMust(t, err)
			previous, err := time.Parse(time.RFC3339Nano, old[i].(string))
			artifactCmdMust(t, err)
			if parsed.Before(previous) {
				t.Fatal("recovery timestamp moved backwards")
			}
			old[i] = current[i]
		}
	}
	if !reflect.DeepEqual(old, current) {
		t.Fatalf("recovery changed execution evidence beyond state/fence/timestamp: got=%v want=%v", current, old)
	}
	return after
}
