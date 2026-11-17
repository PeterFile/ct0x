package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func chatReviewSubmitted(t *testing.T) (chatFixture, CoordinationToolScope, domain.Submission) {
	t.Helper()
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	submission, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "可追溯的原版本文本成果。"}, "chat-review-result")
	if err != nil {
		t.Fatal(err)
	}
	return f, worker, submission
}

func TestChatReviewNoticeReturnsDecisionToOrigin(t *testing.T) {
	for _, outcome := range []domain.ReviewOutcome{domain.ReviewOutcomeAccepted, domain.ReviewOutcomeRejected} {
		t.Run(string(outcome), func(t *testing.T) {
			f, worker, submission := chatReviewSubmitted(t)
			if _, err := f.s.ApplyTurnReceipt(ctx, worker.Member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1}); err != nil {
				t.Fatal(err)
			}
			unchanged := messageSequenceSnapshot(t, f.s)
			for _, table := range []string{"tasks", "task_review_decisions", "mutation_idempotency", "messages", "message_log", "chat_review_notices"} {
				delete(unchanged, table)
			}
			before, err := f.s.ListMessages(ctx, f.groupID, MessageListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			input := taskReviewInput(submission)
			input.Outcome, input.Reason = outcome, "按原任务约定核对：保留此版本及依据。"
			decision, err := f.s.ReviewTask(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			after, err := f.s.ListMessages(ctx, f.groupID, MessageListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(after.Items) != len(before.Items)+1 {
				t.Fatalf("%s Review persisted (%s) but original group/topic has %d messages; want %d including exactly one new Review notice", outcome, decision.ID, len(after.Items), len(before.Items)+1)
			}
			notice := after.Items[0]
			if notice.TopicID != f.candidate.TopicID || notice.SourceKind != "platform" || notice.SourceID != "review:"+decision.ID || len(notice.Deliveries) != 0 {
				t.Fatalf("notice has wrong origin/provenance or wakes a model: %+v", notice)
			}
			for _, text := range []string{submission.TaskID, submission.ID, decision.ID, fmt.Sprintf("审查任务版本：%d", submission.TaskRevision), string(outcome), input.Reason, "平台记录", "本机用户", "不是 Agent 共识", "不代表原生执行成功"} {
				if !strings.Contains(notice.Content, text) {
					t.Errorf("notice lacks %q: %s", text, notice.Content)
				}
			}
			if outcome == domain.ReviewOutcomeRejected && (!strings.Contains(notice.Content, "保留原版本") || !strings.Contains(notice.Content, "适用授权")) {
				t.Errorf("rejection omitted preservation/authorization boundary: %s", notice.Content)
			}
			messageSequencePreserved(t, f.s, unchanged)
			for _, old := range before.Items {
				if got, err := f.s.GetMessage(ctx, f.groupID, old.ID); err != nil || !reflect.DeepEqual(got, old) {
					t.Fatalf("Review rewrote old message: %+v %v", got, err)
				}
			}
			if got, err := f.s.GetTaskSubmission(ctx, f.groupID, submission.TaskID, submission.ID); err != nil || !reflect.DeepEqual(got, submission) {
				t.Fatalf("Review changed original Submission: %+v %v", got, err)
			}
			taskReviewCounts(t, f.s, 1, 1)
			var tasks int
			if err := f.s.db.QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 1 {
				t.Fatalf("Review notice created a new Task: %d %v", tasks, err)
			}
			var held, linked int
			if err := f.s.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", worker.Member.Claim.TurnID).Scan(&held); err != nil || held != 1 {
				t.Fatalf("Review released unknown worker resource: %d %v", held, err)
			}
			if err := f.s.db.QueryRow("SELECT count(*) FROM chat_review_notices WHERE review_id=? AND message_id=?", decision.ID, notice.ID).Scan(&linked); err != nil || linked != 1 {
				t.Fatalf("missing explicit Review/notice relation: %d %v", linked, err)
			}
			current, err := f.s.GetTask(ctx, f.groupID, submission.TaskID)
			if err != nil || (outcome == domain.ReviewOutcomeAccepted && (current.Status != domain.TaskStatusAccepted || current.Revision != submission.TaskRevision)) ||
				(outcome == domain.ReviewOutcomeRejected && (current.Status != domain.TaskStatusOpen || current.Revision != submission.TaskRevision+1)) {
				t.Fatalf("existing Review transition changed: %+v %v", current, err)
			}
			dispatchAssertFK(t, f.s)
		})
	}
}

func chatReviewNotice(t *testing.T, s *Store, decision domain.ReviewDecision) domain.Message {
	t.Helper()
	var id string
	if err := s.db.QueryRow("SELECT message_id FROM chat_review_notices WHERE review_id=?", decision.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	message, err := s.GetMessage(ctx, decision.GroupID, id)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestChatReviewNoticeRollbackAtWriteBoundaries(t *testing.T) {
	for _, table := range []string{"messages", "message_log", "chat_review_notices"} {
		t.Run(table, func(t *testing.T) {
			f, _, submission := chatReviewSubmitted(t)
			before := messageSequenceSnapshot(t, f.s)
			sequence := messageSequenceRows(t, f.s.db, "SELECT * FROM sqlite_sequence ORDER BY name")
			if _, err := f.s.db.Exec("CREATE TRIGGER fail_review_notice BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT, 'synthetic review notice failure'); END"); err != nil {
				t.Fatal(err)
			}
			input := taskReviewInput(submission)
			if got, err := f.s.ReviewTask(ctx, input); err == nil || got.ID != "" || !strings.Contains(err.Error(), "synthetic review notice failure") {
				t.Fatalf("Review committed despite %s failure: %+v %v", table, got, err)
			}
			messageSequencePreserved(t, f.s, before)
			if got := messageSequenceRows(t, f.s.db, "SELECT * FROM sqlite_sequence ORDER BY name"); !reflect.DeepEqual(got, sequence) {
				t.Fatalf("rollback leaked committed sequence: %v; want %v", got, sequence)
			}
			if _, err := f.s.db.Exec("DROP TRIGGER fail_review_notice"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.ReviewTask(ctx, input); err != nil {
				t.Fatalf("rollback consumed Review key: %v", err)
			}
		})
	}
}

func TestChatReviewNoticeIgnoredRelationRollsBack(t *testing.T) {
	f, _, submission := chatReviewSubmitted(t)
	before := messageSequenceSnapshot(t, f.s)
	if _, err := f.s.db.Exec("CREATE TRIGGER ignore_review_notice BEFORE INSERT ON chat_review_notices BEGIN SELECT RAISE(IGNORE); END"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.ReviewTask(ctx, taskReviewInput(submission)); err == nil || got.ID != "" {
		t.Fatalf("ignored notice relation committed a Review: %+v %v", got, err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatReviewNoticeReplayAndPayloadConflict(t *testing.T) {
	f, _, submission := chatReviewSubmitted(t)
	input := taskReviewInput(submission)
	input.Outcome, input.Reason = domain.ReviewOutcomeRejected, "验证尚待完成，按任务约定拒收本次说明；不触发修复。"
	before := messageSequenceSnapshot(t, f.s)
	for _, actor := range []string{"", f.a, f.b} {
		bad := input
		bad.ActorID = actor
		_, err := f.s.ReviewTask(ctx, bad)
		requireMemberToolDenied(t, err)
	}
	messageSequencePreserved(t, f.s, before)
	decision, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	policy := f.policy.ChatPolicyRequest
	policy.Enabled = false
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, policy, 1, "disable-after-review"); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE group_id=?", f.groupID)
	before = messageSequenceSnapshot(t, f.s)
	for _, s := range []*Store{f.s, queueHandle(t, f.path, false)} {
		if got, err := s.ReviewTask(ctx, input); err != nil || !reflect.DeepEqual(got, decision) {
			t.Fatalf("historical Review receipt changed: %+v %v", got, err)
		}
	}
	for _, change := range []func(*ReviewTaskInput){
		func(i *ReviewTaskInput) { i.Reason += "changed" },
		func(i *ReviewTaskInput) { i.Outcome = domain.ReviewOutcomeAccepted },
		func(i *ReviewTaskInput) { i.ExpectedRevision++ },
		func(i *ReviewTaskInput) { i.SubmissionID = "missing" },
	} {
		changed := input
		change(&changed)
		if _, err := f.s.ReviewTask(ctx, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed Review payload did not conflict: %v", err)
		}
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatReviewNoticeOriginIsolationAndHistory(t *testing.T) {
	f, _, submission := chatReviewSubmitted(t)
	otherTopic := "another-topic"
	for _, thread := range []domain.ThreadRef{{GroupID: f.groupID, TopicID: otherTopic}, {GroupID: f.otherID, TopicID: f.candidate.TopicID}} {
		if _, err := f.s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: "另一条不相关讨论", RecipientIDs: []string{}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	input := taskReviewInput(submission)
	before := messageSequenceSnapshot(t, f.s)
	foreign := input
	foreign.GroupID = f.otherID
	if _, err := f.s.ReviewTask(ctx, foreign); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign group reviewed Task: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
	var highWater int64
	if err := f.s.db.QueryRow("SELECT max(sequence) FROM message_log").Scan(&highWater); err != nil {
		t.Fatal(err)
	}
	decision, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	notice := chatReviewNotice(t, f.s, decision)
	if notice.GroupID != f.groupID || notice.TopicID != f.candidate.TopicID {
		t.Fatalf("Review notice escaped its admission origin: %+v", notice)
	}
	sequence := messageSequenceRows(t, f.s.db, fmt.Sprintf("SELECT message_id,group_id,topic_id FROM message_log WHERE sequence>%d ORDER BY sequence", highWater))
	want := [][]any{{notice.ID, f.groupID, f.candidate.TopicID}}
	if !reflect.DeepEqual(sequence, want) {
		t.Fatalf("Review did not append exactly one scoped sequence: %v; want %v", sequence, want)
	}
	for _, scope := range []struct{ group, topic string }{{f.otherID, f.candidate.TopicID}, {f.groupID, otherTopic}} {
		if _, err := f.s.ReadHistory(ctx, scope.group, &scope.topic, notice.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("notice leaked into other group/topic: %v", err)
		}
	}
	topic := f.candidate.TopicID
	for _, s := range []*Store{f.s, queueHandle(t, f.path, false)} {
		if got, err := s.ReadHistory(ctx, f.groupID, &topic, notice.ID); err != nil || !reflect.DeepEqual(got, notice) {
			t.Fatalf("Review notice not durably readable: %+v %v", got, err)
		}
		page, err := s.SearchHistory(ctx, f.groupID, HistoryQuery{Query: decision.ID, TopicID: &topic, Limit: 1})
		if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], notice) {
			t.Fatalf("bounded history lost notice provenance: %+v %v", page, err)
		}
	}
}

func TestChatReviewNoticeRequiresReviewRelationNotUserLabelOrText(t *testing.T) {
	f, _, submission := chatReviewSubmitted(t)
	content := "平台记录：本机用户已接纳 Task " + submission.TaskID + "，不是实际 Review。"
	unproven, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: f.candidate.TopicID, ActorID: "user", Content: content}, nil)
	if err != nil {
		t.Fatal(err)
	}
	typed, err := f.s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: f.candidate.TopicID, ActorID: "user", Content: content}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unproven.SourceKind != "unknown" || typed.SourceKind != "local_user_chat" {
		t.Fatalf("text or sender selected platform provenance: %+v %+v", unproven, typed)
	}
	taskReviewCounts(t, f.s, 0, 0)
	decision, err := f.s.ReviewTask(ctx, taskReviewInput(submission))
	if err != nil {
		t.Fatal(err)
	}
	notice := chatReviewNotice(t, f.s, decision)
	if notice.SenderID != "user" || notice.SourceKind != "platform" || notice.SourceID != "review:"+decision.ID {
		t.Fatalf("actual Review notice mislabeled as user chat: %+v", notice)
	}
	var count int
	if err := f.s.db.QueryRow("SELECT count(*) FROM message_command_sources WHERE message_id=?", notice.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("Review notice fabricated local-user command proof: %d %v", count, err)
	}
}

func TestChatReviewNoticeReasonIsBoundedWithoutChangingReview(t *testing.T) {
	for _, reason := range []string{"", strings.Repeat("字", 682) + "ab", strings.Repeat("字", 1200), "保留原始无效字节：\xff"} {
		t.Run(fmt.Sprintf("bytes=%d", len(reason)), func(t *testing.T) {
			f, _, submission := chatReviewSubmitted(t)
			input := taskReviewInput(submission)
			input.Reason = reason
			decision, err := f.s.ReviewTask(ctx, input)
			if err != nil || decision.Reason != reason {
				t.Fatalf("notice changed Review reason semantics: %+v %v", decision, err)
			}
			notice := chatReviewNotice(t, f.s, decision)
			if !utf8.ValidString(notice.Content) || len(notice.Content) > 4096 {
				t.Fatalf("unbounded or invalid UTF-8 notice: %d bytes", len(notice.Content))
			}
			if len(reason) > 2048 {
				parts := strings.SplitN(notice.Content, "用户理由（展示摘要，完整内容见 Review）：\n", 2)
				want := strings.Repeat("字", 682) + "…（已截断，完整理由见 Review。）"
				if len(parts) != 2 || parts[1] != want {
					t.Fatalf("reason was not bounded to 2048 bytes on a UTF-8 boundary: %s", notice.Content)
				}
			}
			if reason != "" && utf8.ValidString(reason) && len(reason) <= 2048 && !strings.Contains(notice.Content, reason) {
				t.Fatal("in-boundary reason was truncated")
			}
			if got, err := f.s.GetTaskReview(ctx, f.groupID, submission.TaskID, decision.ID); err != nil || got.Reason != reason {
				t.Fatalf("full Review reason was lost: %+v %v", got, err)
			}
		})
	}
}
