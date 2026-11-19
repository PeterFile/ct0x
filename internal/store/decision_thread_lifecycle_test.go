package store

import (
	"errors"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestThreadDecisionDepartureWithdrawsCurrentAndSupersededQuestionsOnly(t *testing.T) {
	f := chatSetup(t, "user", false)
	s := f.s
	installDecisionAnswerFixture(t, s)
	owner := chatAuthorityWorker(t, f)
	task, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, decisionTaskInput(), "ask-task")
	if err != nil {
		t.Fatal(err)
	}
	refusal := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
	for _, scope := range []MemberToolScope{taskWorkTestCoordinator(t, s, task.GroupID, owner.Provenance.AssignmentID), owner.Member} {
		taskWorkTestReceipt(t, s, scope, refusal, domain.TurnStateNotStarted)
	}

	ask := func(memberID, topic, key, question string) domain.DecisionRequest {
		t.Helper()
		thread := f
		thread.scope.TopicID = topic
		scope, candidate := newWorkInputCandidate(t, thread, "请确认本次范围。", memberID)
		input, details := decisionClarificationInput()
		input.Question = question
		q, err := s.ApplyChatDecisionClarification(ctx, scope, candidate.ID, input, details, input.Question, key)
		if err != nil {
			t.Fatal(err)
		}
		taskWorkTestReceipt(t, s, scope, refusal, domain.TurnStateNotStarted)
		return q
	}
	old := ask(owner.Member.MemberID, "departure-topic", "ask-old", "这次只整理本周样本吗？")
	installThreadDecisionAnswerFixture(t, s)
	current := ask(owner.Member.MemberID, old.TopicID, "ask-new", "是否也整理上周样本？")
	other := ask(f.a, "other-topic", "ask-other", "是否抽查两份样本？")
	if page, err := s.ListAnswerableDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 1 || page.Items[0].ID != task.ID {
		t.Fatalf("Thread readiness must not change the Task answer route: %+v %v", page, err)
	}
	taskAnswer, err := s.AnswerDecisionRequest(ctx, "user", task.ThreadRef, task.ID, domain.DecisionAnswerInput{Version: task.Version, OptionIndex: 1}, "answer-task")
	if err != nil || taskAnswer.TaskID != task.Task.TaskID {
		t.Fatalf("Task answer with Thread execution disabled: %+v %v", taskAnswer, err)
	}
	s.EnableThreadDecisionExecution()
	page, err := s.ListAnswerableDecisions(ctx, ListOptions{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("only current and independent Thread questions should be offered: %+v %v", page, err)
	}
	if a, b := page.Items[0].ID, page.Items[1].ID; !(a == other.ID && b == current.ID || a == current.ID && b == other.ID) {
		t.Fatalf("wrong questions in inbox: %+v", page)
	}
	before := messageSequenceSnapshot(t, s)
	coordinationDecideExec(t, s, `UPDATE members SET status='left' WHERE id=?`, owner.Member.MemberID)
	coordinationDecideExec(t, s, `UPDATE members SET status='active' WHERE id=?`, owner.Member.MemberID)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = queueHandle(t, f.path, false)
	s.EnableThreadDecisionExecution()
	for _, q := range []domain.DecisionRequest{old, current, other, task} {
		var withdrawn bool
		want := q.ID == old.ID || q.ID == current.ID
		if err := s.db.QueryRow(`SELECT withdrawn_at IS NOT NULL FROM decision_requests WHERE id=?`, q.ID).Scan(&withdrawn); err != nil || withdrawn != want {
			t.Fatalf("durable withdrawal of %s/%s = %t, want %t: %v", q.Relay, q.ID, withdrawn, want, err)
		}
		if want {
			answer := domain.DecisionAnswerInput{Version: q.Version, OptionIndex: 1}
			if got, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, answer, "after-rejoin"); !errors.Is(err, ErrConflict) || got.ID != "" {
				t.Fatalf("departed asker's question revived: %+v %v", got, err)
			}
		}
	}
	if page, err := s.ListAnswerableDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 1 || page.Items[0].ID != other.ID {
		t.Fatalf("rejoin either revived a question or revoked a different asker: %+v %v", page, err)
	}
	assertDecisionAnswerView(t, s, taskAnswer, nil)
	delete(before, "decision_requests") // Only the two withdrawal stamps may change.
	messageSequencePreserved(t, s, before)
}

func TestThreadDecisionInstallWithdrawsLegacyQuestionFromAlreadyLeftAsker(t *testing.T) {
	f := chatSetup(t, "user", false)
	installDecisionAnswerFixture(t, f.s)
	q := askThreadDecision(t, f)
	coordinationDecideExec(t, f.s, `UPDATE members SET status='left' WHERE id=?`, q.AuthorMemberID)
	installThreadDecisionAnswerFixture(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, f.path, false)
	s.EnableThreadDecisionExecution()
	coordinationDecideExec(t, s, `UPDATE members SET status='active' WHERE id=?`, q.AuthorMemberID)
	if page, err := s.ListAnswerableDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("legacy already-left question revived after install: %+v %v", page, err)
	}
	input := domain.DecisionAnswerInput{Version: q.Version, OptionIndex: 1}
	if got, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, input, "after-install"); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("saved a reply to an already-left asker: %+v %v", got, err)
	}
}
