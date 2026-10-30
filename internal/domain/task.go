package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	ErrTaskInvalid  = errors.New("任务参数无效")
	ErrTaskConflict = errors.New("任务状态或版本冲突")
)

type TaskStatus string

const (
	TaskStatusOpen      TaskStatus = "open"
	TaskStatusSubmitted TaskStatus = "submitted"
	TaskStatusAccepted  TaskStatus = "accepted"
)

type ReviewOutcome string

const (
	ReviewOutcomeAccepted ReviewOutcome = "accepted"
	ReviewOutcomeRejected ReviewOutcome = "rejected"
)

// Task uses Revision as a work-generation fence. Rejection advances it;
// persistence must also fence status/pointer changes within a generation.
type Task struct {
	ID                  string     `json:"id"`
	GroupID             string     `json:"group_id"`
	Title               string     `json:"title"`
	Description         string     `json:"description"`
	Acceptance          string     `json:"acceptance"`
	AssigneeID          string     `json:"assignee_id"`
	DependencyIDs       []string   `json:"dependency_ids"`
	Status              TaskStatus `json:"status"`
	Revision            int64      `json:"revision"`
	ActiveAttemptID     string     `json:"active_attempt_id"`
	CurrentSubmissionID string     `json:"current_submission_id"`
	CreatedAt           time.Time  `json:"created_at"`
}

// TaskAttempt associates a generation with an optional opaque TurnID, not execution state.
// Empty TurnID permits human-only work; actor/Turn validation belongs to Store/platform.
type TaskAttempt struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	GroupID      string    `json:"group_id"`
	TaskRevision int64     `json:"task_revision"`
	TurnID       string    `json:"turn_id"`
	CreatedAt    time.Time `json:"created_at"`
}

// ArtifactVersionRef is an opaque artifact/version identity, not a hash or counter.
type ArtifactVersionRef struct {
	ArtifactID string `json:"artifact_id"`
	VersionID  string `json:"version_id"`
}

// Submission is immutable evidence to persist separately from Task.
// SubmittedBy and ReviewedBy are platform-bound references, not domain auth rules.
type Submission struct {
	ID               string               `json:"id"`
	TaskID           string               `json:"task_id"`
	GroupID          string               `json:"group_id"`
	TaskRevision     int64                `json:"task_revision"`
	AttemptID        string               `json:"attempt_id"`
	Summary          string               `json:"summary"`
	ArtifactVersions []ArtifactVersionRef `json:"artifact_versions"`
	SubmittedBy      string               `json:"submitted_by"`
	CreatedAt        time.Time            `json:"created_at"`
}

// ReviewDecision addresses the concrete versions of one authoritative Submission.
type ReviewDecision struct {
	ID               string               `json:"id"`
	SubmissionID     string               `json:"submission_id"`
	TaskID           string               `json:"task_id"`
	GroupID          string               `json:"group_id"`
	TaskRevision     int64                `json:"task_revision"`
	Outcome          ReviewOutcome        `json:"outcome"`
	ReviewedBy       string               `json:"reviewed_by"`
	Reason           string               `json:"reason"`
	ArtifactVersions []ArtifactVersionRef `json:"artifact_versions"`
	CreatedAt        time.Time            `json:"created_at"`
}

// NewTask requires caller-supplied identity, positive revision, and timestamp.
func NewTask(input Task) (Task, error) {
	if (input.Status != "" && input.Status != TaskStatusOpen) || input.ActiveAttemptID != "" || input.CurrentSubmissionID != "" {
		return Task{}, fmt.Errorf("%w：新任务必须开放且无当前尝试或提交", ErrTaskInvalid)
	}
	input.Status = TaskStatusOpen
	return normalizeTask(input)
}

// ActivateAttempt supersedes an open task's prior pointer, fencing its submission.
// The caller supplies a fresh immutable attempt identity; existence/reuse checks,
// execution eligibility, and same-request retry receipts belong to persistence.
func (t Task) ActivateAttempt(attempt TaskAttempt) (Task, TaskAttempt, error) {
	t, err := normalizeTask(t)
	if err != nil {
		return Task{}, TaskAttempt{}, err
	}
	if !taskFieldsPresent(attempt.ID, attempt.TaskID, attempt.GroupID) ||
		(attempt.TurnID != "" && !taskFieldsPresent(attempt.TurnID)) || attempt.TaskRevision < 1 || !validTaskTime(attempt.CreatedAt) {
		return Task{}, TaskAttempt{}, fmt.Errorf("%w：尝试身份、版本或创建时间无效", ErrTaskInvalid)
	}
	if t.Status != TaskStatusOpen || !t.matches(attempt.TaskID, attempt.GroupID, attempt.TaskRevision) || t.ActiveAttemptID == attempt.ID {
		return Task{}, TaskAttempt{}, fmt.Errorf("%w：尝试不匹配开放任务或已激活", ErrTaskConflict)
	}
	t.ActiveAttemptID = attempt.ID
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	return t, attempt, nil
}

// Submit returns copied task/evidence values; it never implies acceptance.
func (t Task) Submit(submission Submission) (Task, Submission, error) {
	t, err := normalizeTask(t)
	if err != nil {
		return Task{}, Submission{}, err
	}
	submission, err = normalizeSubmission(submission)
	if err != nil {
		return Task{}, Submission{}, err
	}
	if t.Status != TaskStatusOpen || !t.matches(submission.TaskID, submission.GroupID, submission.TaskRevision) || t.ActiveAttemptID != submission.AttemptID {
		return Task{}, Submission{}, fmt.Errorf("%w：提交不属于当前任务尝试或版本", ErrTaskConflict)
	}
	t.Status, t.CurrentSubmissionID = TaskStatusSubmitted, submission.ID
	return t, submission, nil
}

// Review requires the authoritative submission, not a replacement evidence body.
// Acceptance retains its pointer; rejection clears both pointers and advances the
// generation so the rejected attempt cannot reactivate or submit again.
func (t Task) Review(submission Submission, decision ReviewDecision) (Task, ReviewDecision, error) {
	t, err := normalizeTask(t)
	if err != nil {
		return Task{}, ReviewDecision{}, err
	}
	submission, err = normalizeSubmission(submission)
	if err != nil {
		return Task{}, ReviewDecision{}, err
	}
	if !taskFieldsPresent(decision.ID, decision.SubmissionID, decision.TaskID, decision.GroupID, decision.ReviewedBy) || decision.TaskRevision < 1 || !validTaskTime(decision.CreatedAt) ||
		(decision.Outcome != ReviewOutcomeAccepted && decision.Outcome != ReviewOutcomeRejected) {
		return Task{}, ReviewDecision{}, fmt.Errorf("%w：评审身份、版本、结论或时间无效", ErrTaskInvalid)
	}
	if t.Status != TaskStatusSubmitted || !t.matches(submission.TaskID, submission.GroupID, submission.TaskRevision) ||
		t.CurrentSubmissionID != submission.ID || t.ActiveAttemptID != submission.AttemptID ||
		!t.matches(decision.TaskID, decision.GroupID, decision.TaskRevision) || decision.SubmissionID != submission.ID {
		return Task{}, ReviewDecision{}, fmt.Errorf("%w：评审未指向当前提交及其版本", ErrTaskConflict)
	}
	decision.ArtifactVersions, err = normalizeArtifactVersions(decision.ArtifactVersions)
	if err != nil {
		return Task{}, ReviewDecision{}, err
	}
	if !slices.Equal(decision.ArtifactVersions, submission.ArtifactVersions) {
		return Task{}, ReviewDecision{}, fmt.Errorf("%w：评审成果版本必须与提交完全一致", ErrTaskConflict)
	}
	if decision.Outcome == ReviewOutcomeAccepted {
		t.Status = TaskStatusAccepted
	} else {
		if t.Revision == 1<<63-1 {
			return Task{}, ReviewDecision{}, fmt.Errorf("%w：任务版本已耗尽，无法重新开放", ErrTaskConflict)
		}
		t.Status, t.CurrentSubmissionID = TaskStatusOpen, ""
		t.Revision++
	}
	t.ActiveAttemptID = ""
	decision.CreatedAt = decision.CreatedAt.UTC()
	return t, decision, nil
}

func normalizeTask(t Task) (Task, error) {
	if !taskFieldsPresent(t.ID, t.GroupID, t.Title, t.Acceptance) || t.Revision < 1 || !validTaskTime(t.CreatedAt) ||
		(t.AssigneeID != "" && !taskFieldsPresent(t.AssigneeID)) || (t.ActiveAttemptID != "" && !taskFieldsPresent(t.ActiveAttemptID)) {
		return Task{}, fmt.Errorf("%w：任务身份、标题、验收条件、版本或时间无效", ErrTaskInvalid)
	}
	switch t.Status {
	case TaskStatusOpen:
		if t.CurrentSubmissionID != "" {
			return Task{}, fmt.Errorf("%w：开放任务不能有当前提交", ErrTaskInvalid)
		}
	case TaskStatusSubmitted:
		if !taskFieldsPresent(t.ActiveAttemptID, t.CurrentSubmissionID) {
			return Task{}, fmt.Errorf("%w：已提交任务缺少当前尝试或提交", ErrTaskInvalid)
		}
	case TaskStatusAccepted:
		if t.ActiveAttemptID != "" || !taskFieldsPresent(t.CurrentSubmissionID) {
			return Task{}, fmt.Errorf("%w：已接纳任务的当前引用无效", ErrTaskInvalid)
		}
	default:
		return Task{}, fmt.Errorf("%w：未知任务状态", ErrTaskInvalid)
	}
	t.DependencyIDs = append([]string{}, t.DependencyIDs...)
	for _, id := range t.DependencyIDs {
		if !taskFieldsPresent(id) || id == t.ID {
			return Task{}, fmt.Errorf("%w：依赖标识不能为空或指向自身", ErrTaskInvalid)
		}
	}
	slices.Sort(t.DependencyIDs)
	t.DependencyIDs = slices.Compact(t.DependencyIDs)
	t.CreatedAt = t.CreatedAt.UTC()
	return t, nil
}

func normalizeSubmission(s Submission) (Submission, error) {
	if !taskFieldsPresent(s.ID, s.TaskID, s.GroupID, s.AttemptID, s.Summary, s.SubmittedBy) || s.TaskRevision < 1 || !validTaskTime(s.CreatedAt) {
		return Submission{}, fmt.Errorf("%w：提交身份、摘要、版本或时间无效", ErrTaskInvalid)
	}
	var err error
	s.ArtifactVersions, err = normalizeArtifactVersions(s.ArtifactVersions)
	s.CreatedAt = s.CreatedAt.UTC()
	return s, err
}

func normalizeArtifactVersions(input []ArtifactVersionRef) ([]ArtifactVersionRef, error) {
	refs := append([]ArtifactVersionRef{}, input...)
	for _, ref := range refs {
		if !taskFieldsPresent(ref.ArtifactID, ref.VersionID) {
			return nil, fmt.Errorf("%w：成果标识和版本标识必须完整", ErrTaskInvalid)
		}
	}
	slices.SortFunc(refs, func(a, b ArtifactVersionRef) int {
		if order := strings.Compare(a.ArtifactID, b.ArtifactID); order != 0 {
			return order
		}
		return strings.Compare(a.VersionID, b.VersionID)
	})
	return slices.Compact(refs), nil
}

func (t Task) matches(taskID, groupID string, revision int64) bool {
	return t.ID == taskID && t.GroupID == groupID && t.Revision == revision
}

func taskFieldsPresent(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func validTaskTime(stamp time.Time) bool {
	year := stamp.UTC().Year()
	return !stamp.IsZero() && year >= 1 && year <= 9999
}

func (t Task) MarshalJSON() ([]byte, error) {
	type task Task
	if t.DependencyIDs == nil {
		t.DependencyIDs = []string{}
	}
	return json.Marshal(task(t))
}

func (s Submission) MarshalJSON() ([]byte, error) {
	type submission Submission
	if s.ArtifactVersions == nil {
		s.ArtifactVersions = []ArtifactVersionRef{}
	}
	return json.Marshal(submission(s))
}

func (d ReviewDecision) MarshalJSON() ([]byte, error) {
	type decision ReviewDecision
	if d.ArtifactVersions == nil {
		d.ArtifactVersions = []ArtifactVersionRef{}
	}
	return json.Marshal(decision(d))
}
