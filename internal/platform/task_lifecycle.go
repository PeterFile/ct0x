package platform

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type TaskAttemptInput struct {
	ExpectedRevision        int64  `json:"expected_revision"`
	ExpectedActiveAttemptID string `json:"expected_active_attempt_id"`
}

type TaskSubmissionInput struct {
	ExpectedRevision int64                       `json:"expected_revision"`
	AttemptID        string                      `json:"attempt_id"`
	Summary          string                      `json:"summary"`
	ArtifactVersions []domain.ArtifactVersionRef `json:"artifact_versions,omitempty"`
}

type TaskReviewInput struct {
	ExpectedRevision int64                `json:"expected_revision"`
	SubmissionID     string               `json:"submission_id"`
	Outcome          domain.ReviewOutcome `json:"outcome"`
	Reason           string               `json:"reason,omitempty"`
}

func (s *LocalUser) ActivateTaskAttempt(ctx context.Context, groupID, taskID string, input TaskAttemptInput, key string) (domain.TaskAttempt, error) {
	return s.db.ActivateTaskAttempt(ctx, store.ActivateTaskAttemptInput{
		GroupID: groupID, TaskID: taskID, ActorID: "user", MemberScope: nil,
		ExpectedRevision: input.ExpectedRevision, ExpectedActiveAttemptID: input.ExpectedActiveAttemptID, IdempotencyKey: key,
	})
}

func (s *LocalUser) GetTaskAttempt(ctx context.Context, groupID, taskID, attemptID string) (domain.TaskAttempt, error) {
	return s.db.GetTaskAttempt(ctx, groupID, taskID, attemptID)
}

func (s *LocalUser) ListTaskAttempts(ctx context.Context, groupID, taskID string, options store.ListOptions) (store.Page[domain.TaskAttempt], error) {
	return s.db.ListTaskAttempts(ctx, groupID, taskID, options)
}

func (s *LocalUser) SubmitTask(ctx context.Context, groupID, taskID string, input TaskSubmissionInput, key string) (domain.Submission, error) {
	return s.db.SubmitTask(ctx, store.SubmitTaskInput{
		GroupID: groupID, TaskID: taskID, ActorID: "user", MemberScope: nil,
		ExpectedRevision: input.ExpectedRevision, AttemptID: input.AttemptID, Summary: input.Summary,
		ArtifactVersions: input.ArtifactVersions, IdempotencyKey: key,
	})
}

func (s *LocalUser) GetTaskSubmission(ctx context.Context, groupID, taskID, submissionID string) (domain.Submission, error) {
	return s.db.GetTaskSubmission(ctx, groupID, taskID, submissionID)
}

func (s *LocalUser) ListTaskSubmissions(ctx context.Context, groupID, taskID string, options store.ListOptions) (store.Page[domain.Submission], error) {
	return s.db.ListTaskSubmissions(ctx, groupID, taskID, options)
}

func (s *LocalUser) ReviewTask(ctx context.Context, groupID, taskID string, input TaskReviewInput, key string) (domain.ReviewDecision, error) {
	return s.db.ReviewTask(ctx, store.ReviewTaskInput{
		GroupID: groupID, TaskID: taskID, ActorID: "user", ExpectedRevision: input.ExpectedRevision,
		SubmissionID: input.SubmissionID, Outcome: input.Outcome, Reason: input.Reason, IdempotencyKey: key,
	})
}

func (s *LocalUser) GetTaskReview(ctx context.Context, groupID, taskID, reviewID string) (domain.ReviewDecision, error) {
	return s.db.GetTaskReview(ctx, groupID, taskID, reviewID)
}

func (s *LocalUser) ListTaskReviews(ctx context.Context, groupID, taskID string, options store.ListOptions) (store.Page[domain.ReviewDecision], error) {
	return s.db.ListTaskReviews(ctx, groupID, taskID, options)
}
