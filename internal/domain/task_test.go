package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func taskValues() (Task, TaskAttempt, Submission, ReviewDecision) {
	stamp := time.Date(2026, 10, 30, 12, 0, 0, 123, time.UTC)
	task := Task{ID: "task", GroupID: "group", Title: "任务", Acceptance: "提供可验证结果", AssigneeID: "assignee",
		DependencyIDs: []string{"z", "a", "z"}, Revision: 7, CreatedAt: stamp}
	attempt := TaskAttempt{ID: "attempt", TaskID: task.ID, GroupID: task.GroupID, TaskRevision: task.Revision, TurnID: "opaque-turn", CreatedAt: stamp.Add(time.Minute)}
	submission := Submission{ID: "submission", TaskID: task.ID, GroupID: task.GroupID, TaskRevision: task.Revision,
		AttemptID: attempt.ID, Summary: "result", SubmittedBy: "submitter", CreatedAt: stamp.Add(2 * time.Minute),
		ArtifactVersions: []ArtifactVersionRef{{"b", "blue-version"}, {"a", "opaque/version"}, {"b", "blue-version"}}}
	decision := ReviewDecision{ID: "review", SubmissionID: submission.ID, TaskID: task.ID, GroupID: task.GroupID,
		TaskRevision: task.Revision, Outcome: ReviewOutcomeAccepted, ReviewedBy: "reviewer", Reason: "verified",
		ArtifactVersions: slices.Clone(submission.ArtifactVersions), CreatedAt: stamp.Add(3 * time.Minute)}
	return task, attempt, submission, decision
}

func submittedTaskValues(t *testing.T) (Task, TaskAttempt, Submission, ReviewDecision) {
	t.Helper()
	raw, attempt, submission, decision := taskValues()
	task, err := NewTask(raw)
	if err != nil {
		t.Fatal(err)
	}
	task, attempt, err = task.ActivateAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	task, submission, err = task.Submit(submission)
	if err != nil {
		t.Fatal(err)
	}
	return task, attempt, submission, decision
}

func TestNewTaskNormalizesDependenciesWithoutMutation(t *testing.T) {
	input, _, _, _ := taskValues()
	input.Title, input.Acceptance = " task ", " criteria "
	input.CreatedAt = input.CreatedAt.In(time.FixedZone("test", 8*60*60))
	before := slices.Clone(input.DependencyIDs)
	task, err := NewTask(input)
	if err != nil || task.Status != TaskStatusOpen || task.Revision != input.Revision ||
		task.Title != input.Title || task.Acceptance != input.Acceptance || task.ID != input.ID ||
		!task.CreatedAt.Equal(input.CreatedAt) || task.CreatedAt.Location() != time.UTC ||
		!slices.Equal(task.DependencyIDs, []string{"a", "z"}) {
		t.Fatalf("new task: %+v, %v", task, err)
	}
	if !slices.Equal(input.DependencyIDs, before) || input.CreatedAt.Location() == time.UTC {
		t.Fatal("creation mutated caller data")
	}
	again, err := NewTask(input)
	if err != nil || !reflect.DeepEqual(task, again) {
		t.Fatalf("creation was not deterministic: %+v, %v", again, err)
	}
	task.DependencyIDs[0] = "changed"
	if !slices.Equal(input.DependencyIDs, before) {
		t.Fatal("returned task aliases caller dependencies")
	}
}

func TestTaskCreationValidation(t *testing.T) {
	for name, change := range map[string]func(*Task){
		"id": func(t *Task) { t.ID = " " }, "group": func(t *Task) { t.GroupID = "" },
		"title": func(t *Task) { t.Title = " \t" }, "criteria": func(t *Task) { t.Acceptance = "" },
		"zero revision": func(t *Task) { t.Revision = 0 }, "negative revision": func(t *Task) { t.Revision = -1 },
		"self dependency":     func(t *Task) { t.DependencyIDs = []string{t.ID} },
		"blank dependency":    func(t *Task) { t.DependencyIDs = []string{"a", " \t"} },
		"assignee":            func(t *Task) { t.AssigneeID = " " },
		"time":                func(t *Task) { t.CreatedAt = time.Time{} },
		"time range":          func(t *Task) { t.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"execution status":    func(t *Task) { t.Status = TaskStatus("running") },
		"accepted creation":   func(t *Task) { t.Status = TaskStatusAccepted },
		"active creation":     func(t *Task) { t.ActiveAttemptID = "attempt" },
		"submission creation": func(t *Task) { t.CurrentSubmissionID = "submission" },
	} {
		t.Run(name, func(t *testing.T) {
			input, _, _, _ := taskValues()
			change(&input)
			if _, err := NewTask(input); !errors.Is(err, ErrTaskInvalid) {
				t.Fatalf("want invalid input, got %v", err)
			}
		})
	}
}

func TestTaskSubmissionAndExplicitAcceptance(t *testing.T) {
	raw, attempt, submission, decision := taskValues()
	task, err := NewTask(raw)
	if err != nil {
		t.Fatal(err)
	}
	attempt.TurnID = "unrelated-process-exited-0"
	active, attempt, err := task.ActivateAttempt(attempt)
	if err != nil || active.Status != TaskStatusOpen || active.ActiveAttemptID != attempt.ID || active.Revision != task.Revision {
		t.Fatalf("activation: %+v, %v", active, err)
	}
	submission.Summary = "CLI exited 0 and Turn terminated"
	submitted, evidence, err := active.Submit(submission)
	if err != nil || submitted.Status != TaskStatusSubmitted || submitted.CurrentSubmissionID != submission.ID ||
		submitted.ActiveAttemptID != attempt.ID || submitted.Revision != task.Revision || evidence.Summary != submission.Summary ||
		!slices.Equal(evidence.ArtifactVersions, []ArtifactVersionRef{{"a", "opaque/version"}, {"b", "blue-version"}}) {
		t.Fatalf("submission must not imply acceptance: %+v, %+v, %v", submitted, evidence, err)
	}
	if _, _, err := submitted.Submit(submission); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("submitted evidence was overwritten or replayed in memory: %v", err)
	}
	later := attempt
	later.ID = "later-attempt"
	if _, _, err := submitted.ActivateAttempt(later); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("submitted task activated a new attempt: %v", err)
	}
	invalid := decision
	invalid.Outcome = ReviewOutcome("completed")
	if _, _, err := submitted.Review(evidence, invalid); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("execution output became a review outcome: %v", err)
	}
	accepted, review, err := submitted.Review(evidence, decision)
	if err != nil || accepted.Status != TaskStatusAccepted || accepted.ActiveAttemptID != "" ||
		accepted.CurrentSubmissionID != evidence.ID || accepted.Revision != task.Revision || review.Outcome != ReviewOutcomeAccepted ||
		!slices.Equal(review.ArtifactVersions, evidence.ArtifactVersions) {
		t.Fatalf("explicit acceptance: %+v, %+v, %v", accepted, review, err)
	}
	if task.ActiveAttemptID != "" || active.Status != TaskStatusOpen || submitted.Status != TaskStatusSubmitted {
		t.Fatal("transition mutated an earlier task value")
	}
}

func TestTaskRejectionFencesOldEvidence(t *testing.T) {
	task, attempt, submission, decision := submittedTaskValues(t)
	originalSubmission := submission
	originalSubmission.ArtifactVersions = slices.Clone(submission.ArtifactVersions)
	decision.Outcome, decision.Reason = ReviewOutcomeRejected, "needs correction"
	open, review, err := task.Review(submission, decision)
	if err != nil || open.Status != TaskStatusOpen || open.Revision != task.Revision+1 ||
		open.ActiveAttemptID != "" || open.CurrentSubmissionID != "" || review.SubmissionID != submission.ID ||
		review.Outcome != ReviewOutcomeRejected || !reflect.DeepEqual(submission, originalSubmission) {
		t.Fatalf("rejection lost evidence or accepted it: %+v, %+v, %v", open, review, err)
	}
	if _, _, err := open.Submit(submission); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("rejected submission became current again: %v", err)
	}
	if _, _, err := open.ActivateAttempt(attempt); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("rejected attempt reactivated: %v", err)
	}
	if _, _, err := open.Review(submission, decision); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("old review advanced a new generation: %v", err)
	}
	attempt.ID, attempt.TurnID, attempt.TaskRevision = "next-attempt", "next-turn", open.Revision
	active, _, err := open.ActivateAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	submission.ID, submission.AttemptID, submission.TaskRevision = "next-submission", attempt.ID, open.Revision
	if next, _, err := active.Submit(submission); err != nil || next.Status != TaskStatusSubmitted {
		t.Fatalf("fresh generation cannot submit: %+v, %v", next, err)
	}
	task.Revision, submission.TaskRevision, decision.TaskRevision = 1<<63-1, 1<<63-1, 1<<63-1
	submission.ID, submission.AttemptID = task.CurrentSubmissionID, task.ActiveAttemptID
	if _, _, err := task.Review(submission, decision); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("rejection overflowed the fencing revision: %v", err)
	}
}

func TestTaskAttemptFencingAndValidation(t *testing.T) {
	raw, attempt, submission, _ := taskValues()
	task, err := NewTask(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*TaskAttempt){
		"task":     func(a *TaskAttempt) { a.TaskID = "foreign" },
		"group":    func(a *TaskAttempt) { a.GroupID = "foreign" },
		"revision": func(a *TaskAttempt) { a.TaskRevision-- },
	} {
		t.Run(name, func(t *testing.T) {
			bad := attempt
			change(&bad)
			if _, _, err := task.ActivateAttempt(bad); !errors.Is(err, ErrTaskConflict) {
				t.Fatalf("unfenced attempt: %v", err)
			}
		})
	}
	for _, change := range []func(*TaskAttempt){
		func(a *TaskAttempt) { a.ID = "" }, func(a *TaskAttempt) { a.TaskID = " " },
		func(a *TaskAttempt) { a.GroupID = "" }, func(a *TaskAttempt) { a.TurnID = " " },
		func(a *TaskAttempt) { a.TaskRevision = 0 }, func(a *TaskAttempt) { a.CreatedAt = time.Time{} },
	} {
		bad := attempt
		change(&bad)
		if _, _, err := task.ActivateAttempt(bad); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("malformed attempt: %v", err)
		}
	}
	active, _, err := task.ActivateAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := active.ActivateAttempt(attempt); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("domain invented an activation retry receipt: %v", err)
	}
	replacement := attempt
	replacement.ID, replacement.TurnID = "replacement", "replacement-turn"
	active, _, err = active.ActivateAttempt(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := active.Submit(submission); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("superseded attempt submitted: %v", err)
	}
}

func TestTaskSubmissionReviewFencingAndArtifacts(t *testing.T) {
	task, _, submission, decision := submittedTaskValues(t)
	open := task
	open.Status, open.CurrentSubmissionID = TaskStatusOpen, ""
	for _, field := range []string{"TaskID", "GroupID", "AttemptID"} {
		bad := submission
		reflect.ValueOf(&bad).Elem().FieldByName(field).SetString("foreign")
		if _, _, err := open.Submit(bad); !errors.Is(err, ErrTaskConflict) {
			t.Errorf("submission %s was not fenced: %v", field, err)
		}
		if _, _, err := task.Review(bad, decision); !errors.Is(err, ErrTaskConflict) {
			t.Errorf("review accepted a foreign submission %s: %v", field, err)
		}
	}
	badSubmission := submission
	badSubmission.TaskRevision--
	if _, _, err := open.Submit(badSubmission); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("old submission revision: %v", err)
	}
	if _, _, err := task.Review(badSubmission, decision); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("review accepted an old submission revision: %v", err)
	}
	multiple := submission
	multiple.ArtifactVersions = []ArtifactVersionRef{{"a", "one"}, {"a", "two"}, {"a", "one"}}
	if _, evidence, err := open.Submit(multiple); err != nil || !slices.Equal(evidence.ArtifactVersions, []ArtifactVersionRef{{"a", "one"}, {"a", "two"}}) {
		t.Fatalf("distinct versions of one artifact were collapsed: %+v, %v", evidence, err)
	}
	for _, refs := range [][]ArtifactVersionRef{{{"", "v"}}, {{"a", " "}}} {
		bad := submission
		bad.ArtifactVersions = refs
		if _, _, err := open.Submit(bad); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("incomplete artifact reference: %v", err)
		}
	}
	for _, field := range []string{"TaskID", "GroupID", "SubmissionID"} {
		bad := decision
		reflect.ValueOf(&bad).Elem().FieldByName(field).SetString("foreign")
		if _, _, err := task.Review(submission, bad); !errors.Is(err, ErrTaskConflict) {
			t.Errorf("review %s was not fenced: %v", field, err)
		}
	}
	badSubmission = submission
	badSubmission.ID = "not-current"
	badDecision := decision
	badDecision.SubmissionID = badSubmission.ID
	if _, _, err := task.Review(badSubmission, badDecision); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("review addressed a non-current submission: %v", err)
	}
	badDecision = decision
	badDecision.TaskRevision--
	if _, _, err := task.Review(submission, badDecision); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("old review revision: %v", err)
	}
	for _, refs := range [][]ArtifactVersionRef{nil, {{"a", "newer-version"}, {"b", "blue-version"}}, {{"a", "opaque/version"}}} {
		bad := decision
		bad.ArtifactVersions = refs
		if _, _, err := task.Review(submission, bad); !errors.Is(err, ErrTaskConflict) {
			t.Errorf("different concrete versions were accepted: %v", err)
		}
	}
	accepted, _, err := task.Review(submission, decision)
	if err != nil {
		t.Fatal(err)
	}
	_, attempt, _, _ := taskValues()
	if _, _, err := accepted.ActivateAttempt(attempt); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("accepted task activated: %v", err)
	}
	if _, _, err := accepted.Submit(submission); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("accepted task submitted: %v", err)
	}
	if _, _, err := accepted.Review(submission, decision); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("accepted task reviewed again: %v", err)
	}
}

func TestTaskEvidenceValidation(t *testing.T) {
	task, _, submission, decision := submittedTaskValues(t)
	open := task
	open.Status, open.CurrentSubmissionID = TaskStatusOpen, ""
	for _, change := range []func(*Submission){
		func(s *Submission) { s.ID = "" }, func(s *Submission) { s.Summary = " \t" },
		func(s *Submission) { s.SubmittedBy = "" }, func(s *Submission) { s.TaskRevision = 0 },
		func(s *Submission) { s.CreatedAt = time.Time{} },
	} {
		bad := submission
		change(&bad)
		if _, _, err := open.Submit(bad); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("malformed submission: %v", err)
		}
	}
	for _, change := range []func(*ReviewDecision){
		func(d *ReviewDecision) { d.ID = "" }, func(d *ReviewDecision) { d.SubmissionID = " " },
		func(d *ReviewDecision) { d.ReviewedBy = "" }, func(d *ReviewDecision) { d.CreatedAt = time.Time{} },
		func(d *ReviewDecision) { d.ArtifactVersions = []ArtifactVersionRef{{"a", ""}} },
	} {
		bad := decision
		change(&bad)
		if _, _, err := task.Review(submission, bad); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("malformed review: %v", err)
		}
	}
}

func TestTaskTransitionsCopyEvidenceAndNormalizeTime(t *testing.T) {
	raw, attempt, input, decision := taskValues()
	task, err := NewTask(raw)
	if err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("test", 8*60*60)
	attempt.CreatedAt, input.CreatedAt, decision.CreatedAt = attempt.CreatedAt.In(zone), input.CreatedAt.In(zone), decision.CreatedAt.In(zone)
	active, a, err := task.ActivateAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	submitted, evidence, err := active.Submit(input)
	if err != nil {
		t.Fatal(err)
	}
	accepted, review, err := submitted.Review(evidence, decision)
	if err != nil || a.CreatedAt.Location() != time.UTC || evidence.CreatedAt.Location() != time.UTC || review.CreatedAt.Location() != time.UTC {
		t.Fatalf("transition time is not UTC: %+v, %+v, %+v, %v", a, evidence, review, err)
	}
	if len(input.ArtifactVersions) != 3 || len(decision.ArtifactVersions) != 3 || input.CreatedAt.Location() != zone {
		t.Fatal("normalization mutated input evidence")
	}
	active.DependencyIDs[0] = "active-change"
	if task.DependencyIDs[0] != "a" || submitted.DependencyIDs[0] != "a" {
		t.Fatal("activation/submission shares dependency slices")
	}
	accepted.DependencyIDs[0] = "changed"
	review.ArtifactVersions[0].VersionID = "changed"
	if submitted.DependencyIDs[0] != "a" || evidence.ArtifactVersions[0].VersionID != "opaque/version" || decision.ArtifactVersions[0].VersionID != "blue-version" {
		t.Fatal("returned values alias earlier task/evidence")
	}
	evidence.ArtifactVersions[0].VersionID = "changed"
	if input.ArtifactVersions[1].VersionID != "opaque/version" {
		t.Fatal("submission aliases caller artifact references")
	}
}

func TestTaskTextOnlySubmission(t *testing.T) {
	raw, attempt, submission, decision := taskValues()
	raw.DependencyIDs, submission.ArtifactVersions, decision.ArtifactVersions = nil, nil, nil
	task, err := NewTask(raw)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err = task.ActivateAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	task, evidence, err := task.Submit(submission)
	if err != nil || task.Status != TaskStatusSubmitted || evidence.ArtifactVersions == nil || len(evidence.ArtifactVersions) != 0 || task.DependencyIDs == nil {
		t.Fatalf("text-only submission: %+v, %+v, %v", task, evidence, err)
	}
	if accepted, review, err := task.Review(evidence, decision); err != nil || accepted.Status != TaskStatusAccepted || review.ArtifactVersions == nil {
		t.Fatalf("text-only explicit review: %+v, %+v, %v", accepted, review, err)
	}
}

func TestTaskHumanAttemptWithoutTurn(t *testing.T) {
	raw, attempt, submission, decision := taskValues()
	attempt.TurnID = ""
	submission.ArtifactVersions, decision.ArtifactVersions = nil, nil
	task, err := NewTask(raw)
	if err != nil {
		t.Fatal(err)
	}
	task, active, err := task.ActivateAttempt(attempt)
	if err != nil || active.TurnID != "" || task.Status != TaskStatusOpen {
		t.Fatalf("human attempt requires no synthetic Turn: %+v, %+v, %v", task, active, err)
	}
	task, evidence, err := task.Submit(submission)
	if err != nil || task.Status != TaskStatusSubmitted || evidence.AttemptID != active.ID || len(evidence.ArtifactVersions) != 0 {
		t.Fatalf("human text submission must await review: %+v, %+v, %v", task, evidence, err)
	}
	accepted, review, err := task.Review(evidence, decision)
	if err != nil || accepted.Status != TaskStatusAccepted || review.SubmissionID != evidence.ID || review.Outcome != ReviewOutcomeAccepted {
		t.Fatalf("explicit human-submission acceptance: %+v, %+v, %v", accepted, review, err)
	}
}

func TestTaskJSONContractsAndEmptyArrays(t *testing.T) {
	stamp := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		value       any
		keys, array string
	}{
		{Task{CreatedAt: stamp}, "id group_id title description acceptance assignee_id dependency_ids status revision active_attempt_id current_submission_id created_at", "dependency_ids"},
		{TaskAttempt{CreatedAt: stamp}, "id task_id group_id task_revision turn_id created_at", ""},
		{Submission{CreatedAt: stamp}, "id task_id group_id task_revision attempt_id summary artifact_versions submitted_by created_at", "artifact_versions"},
		{ReviewDecision{CreatedAt: stamp}, "id submission_id task_id group_id task_revision outcome reviewed_by reason artifact_versions created_at", "artifact_versions"},
	} {
		data, err := json.Marshal(tt.value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		want := strings.Fields(tt.keys)
		sort.Strings(keys)
		sort.Strings(want)
		if !slices.Equal(keys, want) || string(fields["created_at"]) != `"2026-10-30T12:00:00Z"` || (tt.array != "" && string(fields[tt.array]) != "[]") {
			t.Fatalf("unexpected domain JSON: %s", data)
		}
	}
	data, err := json.Marshal(ArtifactVersionRef{ArtifactID: "artifact", VersionID: "opaque-version"})
	if err != nil || string(data) != `{"artifact_id":"artifact","version_id":"opaque-version"}` {
		t.Fatalf("artifact version is not an opaque pair: %s, %v", data, err)
	}
	if TaskStatusOpen != "open" || TaskStatusSubmitted != "submitted" || TaskStatusAccepted != "accepted" || ReviewOutcomeAccepted != "accepted" || ReviewOutcomeRejected != "rejected" {
		t.Fatal("task/review tokens do not match the contract")
	}
}
