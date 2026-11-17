package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

type chatRepairSourceAPI interface {
	GetChatRepairSource(context.Context, domain.ThreadRef, string) (domain.ChatRepairSource, error)
}

func chatRepairReader(t *testing.T, s *Store) chatRepairSourceAPI {
	t.Helper()
	api, ok := any(s).(chatRepairSourceAPI)
	if !ok {
		t.Fatal("Store lacks official Review-sourced repair evidence read")
	}
	return api
}

func chatRepairRejected(t *testing.T) (chatFixture, CoordinationToolScope, domain.Submission, domain.ReviewDecision) {
	t.Helper()
	f, worker, submission := chatReviewSubmitted(t)
	input := taskReviewInput(submission)
	input.Outcome = domain.ReviewOutcomeRejected
	input.Reason = strings.Repeat("具体依据不符合原验收约定；", 180) + "仅说明拒收，不请求继续执行。"
	review, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	return f, worker, submission, review
}

func TestChatRepairSourceBindsOfficialReviewWithoutAuthorizingWork(t *testing.T) {
	f, worker, submission, review := chatRepairRejected(t)
	api := chatRepairReader(t, f.s)
	if _, err := f.s.ApplyTurnReceipt(ctx, worker.Member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	originalAdmission, err := f.s.GetChatAdmission(ctx, f.groupID, submission.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	coordination, err := f.s.GetTaskCoordination(ctx, f.groupID, submission.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	originalMessage, err := f.s.GetMessage(ctx, f.groupID, f.candidate.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ChatRepairSource{
		ThreadRef: f.candidate.ThreadRef, ParentTaskID: submission.TaskID, ParentSubmissionID: submission.ID,
		ParentReviewID: review.ID, ReviewedTaskRevision: review.TaskRevision,
		ParentMandate: coordination.Admission.Mandate, ParentAdmission: originalAdmission,
		ParentSubmissionSummary: submission.Summary, ReviewReason: review.Reason, ArtifactVersions: review.ArtifactVersions,
		OriginalAuthorization: domain.ChatRepairAuthorization{SourceKind: "local_user_chat", SourceID: originalMessage.SourceID, MessageID: originalMessage.ID, PolicyRevision: f.policy.Revision, Content: originalMessage.Content},
	}
	for _, reader := range []chatRepairSourceAPI{api, chatRepairReader(t, queueHandle(t, f.path, false))} {
		got, err := reader.GetChatRepairSource(ctx, f.candidate.ThreadRef, review.ID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("official immutable source: %+v, %v; want %+v", got, err, want)
		}
		data, err := json.Marshal(got)
		if err != nil || !strings.Contains(string(data), `"artifact_versions":[]`) || strings.Contains(string(data), "claim_token") {
			t.Fatalf("source DTO lost empty refs or disclosed execution identity: %s, %v", data, err)
		}
		var projection struct {
			OriginalAuthorization struct {
				SourceKind     string `json:"source_kind"`
				SourceID       string `json:"source_id"`
				MessageID      string `json:"message_id"`
				PolicyRevision int64  `json:"policy_revision"`
				Content        string `json:"content"`
			} `json:"original_authorization"`
		}
		if err := json.Unmarshal(data, &projection); err != nil {
			t.Fatal(err)
		}
		original, err := f.s.GetMessage(ctx, f.groupID, f.candidate.MessageID)
		proof := projection.OriginalAuthorization
		if err != nil || proof.SourceKind != "local_user_chat" || proof.SourceID != original.SourceID || proof.MessageID != original.ID || proof.Content != original.Content || proof.PolicyRevision != f.policy.Revision {
			t.Fatalf("repair permission depends on generated Mandate rather than complete actual user command: %+v, %v", proof, err)
		}
	}
	messageSequencePreserved(t, f.s, before)
	notice := chatReviewNotice(t, f.s, review)
	if notice.SourceKind != "platform" || notice.SourceID != "review:"+review.ID {
		t.Fatalf("read manufactured user_chat authority or scheduled work: %+v", notice)
	}
	var claims int
	if err := f.s.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", worker.Member.Claim.TurnID).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("source read released unknown resource claim: %d, %v", claims, err)
	}
	if got, err := f.s.GetTaskSubmission(ctx, f.groupID, submission.TaskID, submission.ID); err != nil || !reflect.DeepEqual(got, submission) {
		t.Fatalf("source read changed parent Submission: %+v, %v", got, err)
	}
	if got, err := f.s.GetTaskReview(ctx, f.groupID, submission.TaskID, review.ID); err != nil || !reflect.DeepEqual(got, review) {
		t.Fatalf("source read changed parent Review: %+v, %v", got, err)
	}
}

func TestChatRepairSourceScopesExactReviewNotNoticeText(t *testing.T) {
	f, _, _, review := chatRepairRejected(t)
	api := chatRepairReader(t, f.s)
	foreign := group(t, f.s)
	notice := chatReviewNotice(t, f.s, review)
	spoof := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.candidate.TopicID, ActorID: "user", Content: "Review: " + review.ID, RecipientIDs: []string{}}, []string{})
	for _, test := range []struct {
		name   string
		thread domain.ThreadRef
		id     string
		want   error
	}{
		{"foreign-group", domain.ThreadRef{GroupID: foreign.ID, TopicID: f.candidate.TopicID}, review.ID, ErrNotFound},
		{"foreign-topic", domain.ThreadRef{GroupID: f.groupID, TopicID: "another-topic"}, review.ID, ErrNotFound},
		{"missing-review", f.candidate.ThreadRef, "missing", ErrNotFound},
		{"platform-notice-is-not-review", f.candidate.ThreadRef, notice.ID, ErrNotFound},
		{"user-text-is-not-review", f.candidate.ThreadRef, spoof.ID, ErrNotFound},
		{"invalid-group", domain.ThreadRef{}, review.ID, ErrInvalid},
		{"invalid-review", f.candidate.ThreadRef, " " + review.ID, ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := api.GetChatRepairSource(ctx, test.thread, test.id)
			if !errors.Is(err, test.want) || !reflect.DeepEqual(got, domain.ChatRepairSource{}) {
				t.Fatalf("source scope returned evidence: %+v, %v; want %v", got, err, test.want)
			}
		})
	}
}

func TestChatRepairSourceRequiresRejectedUnchangedGeneration(t *testing.T) {
	t.Run("accepted-review", func(t *testing.T) {
		f, _, submission := chatReviewSubmitted(t)
		api := chatRepairReader(t, f.s)
		review, err := f.s.ReviewTask(ctx, taskReviewInput(submission))
		if err != nil {
			t.Fatal(err)
		}
		got, err := api.GetChatRepairSource(ctx, f.candidate.ThreadRef, review.ID)
		if !errors.Is(err, ErrConflict) || !reflect.DeepEqual(got, domain.ChatRepairSource{}) {
			t.Fatalf("accepted Review became repair source: %+v, %v", got, err)
		}
	})
	t.Run("newer-generation", func(t *testing.T) {
		f, _, submission, review := chatRepairRejected(t)
		api := chatRepairReader(t, f.s)
		// Simulate a later server-side generation without bypassing Assignment ownership to execute it.
		if _, err := f.s.db.Exec("UPDATE tasks SET revision=revision+1 WHERE id=?", submission.TaskID); err != nil {
			t.Fatal(err)
		}
		got, err := api.GetChatRepairSource(ctx, f.candidate.ThreadRef, review.ID)
		if !errors.Is(err, ErrConflict) || !reflect.DeepEqual(got, domain.ChatRepairSource{}) {
			t.Fatalf("old Review became current repair source: %+v, %v", got, err)
		}
	})
}

func TestChatRepairSourceLegacyTaskHasNoChatAuthority(t *testing.T) {
	s, _, submission, input := taskReviewSubmitted(t)
	api := chatRepairReader(t, s)
	input.Outcome = domain.ReviewOutcomeRejected
	review, err := s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.GetChatRepairSource(ctx, domain.ThreadRef{GroupID: submission.GroupID}, review.ID)
	if !errors.Is(err, ErrNotFound) || !reflect.DeepEqual(got, domain.ChatRepairSource{}) {
		t.Fatalf("legacy Task gained chat repair authority: %+v, %v", got, err)
	}
}

func TestChatRepairSourceSQLFailureIsNotMissingEvidence(t *testing.T) {
	f, _, _, review := chatRepairRejected(t)
	api := chatRepairReader(t, f.s)
	if _, err := f.s.db.Exec("DROP TABLE chat_intake_executions"); err != nil {
		t.Fatal(err)
	}
	got, err := api.GetChatRepairSource(ctx, f.candidate.ThreadRef, review.ID)
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || !reflect.DeepEqual(got, domain.ChatRepairSource{}) {
		t.Fatalf("broken authority table was treated as absence: %+v, %v", got, err)
	}
}
