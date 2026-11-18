package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskWorkFollowupHandback(t *testing.T) {
	for _, name := range []string{"original", "continuation"} {
		t.Run(name, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			f.s.EnableTaskWorkExecution()
			old := chatAuthorityWorker(t, f)
			p := old.Provenance
			const text = "还需要将摘要写入项目目录"
			s, c := newWorkInputCandidate(t, f, text)
			cmd := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "由执行成员核对是否属于原授权"}
			r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "handback-fixture")
			if err != nil || r.WorkDeliveryID == "" {
				t.Fatalf("work input: %+v %v", r, err)
			}
			scope, attemptID := old.Member, old.AttemptID
			if name == "continuation" {
				co := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
				for _, prior := range []MemberToolScope{co, old.Member} {
					taskWorkTestReceipt(t, f.s, prior, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
				}
				d, err := f.s.ResolveTaskWorkDelivery(ctx, p.GroupID, r.WorkDeliveryID)
				if err != nil || d.Blocked != "" {
					t.Fatalf("followup not ready: %+v %v", d, err)
				}
				scope = taskWorkTestClaim(t, f.s, d, "worker-resource")
				a, err := f.s.BindTaskWorkContinuation(ctx, scope)
				if err != nil {
					t.Fatal(err)
				}
				attemptID = a.ID
			}
			const reason = "原任务只授权在群内交付文本，未授权目录写入"
			for _, denied := range []MemberToolScope{s, {Claim: scope.Claim, MemberID: f.foreign, ConversationID: scope.ConversationID, TopicID: scope.TopicID}} {
				if _, err := f.s.HandBackWorkInput(ctx, denied, r.WorkInputID, reason); !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("another scope could return the owner's input: %v", err)
				}
			}
			protected := []string{"tasks", "task_attempts", "turns", "resource_claims", "coordination_assignments", "mandates", "message_command_sources"}
			before := messageSequenceSnapshot(t, f.s)
			got, err := f.s.HandBackWorkInput(ctx, scope, r.WorkInputID, reason)
			if err != nil || got.Return == nil || got.Return.MemberID != p.MemberID || got.Return.AttemptID != attemptID || got.Return.TurnID != scope.Claim.TurnID || got.Return.Reason != reason || got.Content != text || got.ReplyMessageID != c.MessageID || got.Consumption != nil {
				t.Fatalf("handback merged authority or lost the human source: %+v %v", got, err)
			}
			after := messageSequenceSnapshot(t, f.s)
			for _, table := range protected {
				if !reflect.DeepEqual(before[table], after[table]) {
					t.Fatalf("handback rewrote protected facts in %s", table)
				}
			}
			notice, err := f.s.GetMessage(ctx, p.GroupID, got.Return.NoticeMessageID)
			if err != nil || notice.TopicID != p.TopicID || notice.SourceKind != "platform" || !strings.Contains(notice.Content, c.MessageID) ||
				!strings.HasSuffix(notice.Content, got.Return.Question) || strings.Count(notice.Content, "？") != 1 || len(notice.Deliveries) != 0 {
				t.Fatalf("handback was not one scoped clarification: %+v %v", notice, err)
			}
			if again, err := f.s.HandBackWorkInput(ctx, scope, r.WorkInputID, reason); err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("return replay changed reason, actor or notice: %+v %v", again, err)
			}
			if _, err := f.s.HandBackWorkInput(ctx, scope, r.WorkInputID, "不同原因"); !errors.Is(err, ErrConflict) {
				t.Fatalf("return history was replaceable: %v", err)
			}
			if _, err := f.s.AcknowledgeWorkInput(ctx, scope, r.WorkInputID); !errors.Is(err, ErrConflict) {
				t.Fatalf("handback was converted into confirmation: %v", err)
			}
			page, err := f.s.ListWorkInputHistoryForMember(ctx, scope, ListOptions{Limit: 1})
			if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], got) || page.NextCursor != nil {
				t.Fatalf("work history lost returned input: %+v %v", page, err)
			}
			messageSequencePreserved(t, f.s, after)
			dispatchAssertFK(t, f.s)
		})
	}
}

func TestTaskWorkFollowupDormantAndAtomic(t *testing.T) {
	f := chatSetup(t, "user", false)
	old := chatAuthorityWorker(t, f)
	cmd := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "保持原任务范围"}
	s, c := newWorkInputCandidate(t, f, "保持旧格式兼容")
	before := messageSequenceSnapshot(t, f.s)
	if r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "dormant"); !errors.Is(err, ErrTaskWorkExecutionNotReady) || r.ID != "" {
		t.Fatalf("unwired process silently recorded work: %+v %v", r, err)
	}
	messageSequencePreserved(t, f.s, before)
	dormant, err := f.s.ApplyChatAssessmentDormant(ctx, s, c.ID, cmd, "记录要求。", "dormant")
	if err != nil || !dormant.WorkInputPending || dormant.WorkDeliveryID != "" || dormant.WorkInputID == "" {
		t.Fatalf("explicit dormant receipt: %+v %v", dormant, err)
	}
	f.s.EnableTaskWorkExecution()
	if got, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "dormant"); err != nil || !reflect.DeepEqual(dormant, got) {
		t.Fatalf("startup opt-in retroactively woke a replay: %+v %v", got, err)
	}
	if _, err := f.s.AcknowledgeWorkInput(ctx, old.Member, dormant.WorkInputID); err != nil {
		t.Fatal(err)
	}
	taskWorkTestReceipt(t, f.s, s, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	s, c = newWorkInputCandidate(t, f, "列出兼容事项")
	before = messageSequenceSnapshot(t, f.s)
	coordinationDecideExec(t, f.s, `CREATE TEMP TRIGGER suppress_followup_wake BEFORE INSERT ON task_work_deliveries BEGIN SELECT RAISE(IGNORE); END`)
	if r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "atomic"); err == nil || r.ID != "" {
		t.Fatalf("undelivered input was reported as official and queued: %+v %v", r, err)
	}
	coordinationDecideExec(t, f.s, `DROP TRIGGER suppress_followup_wake`)
	messageSequencePreserved(t, f.s, before)
	if r, err := f.s.ApplyChatAssessment(ctx, s, c.ID, cmd, "记录要求。", "atomic"); err != nil || r.WorkDeliveryID == "" {
		t.Fatalf("rolled-back transport key could not be retried: %+v %v", r, err)
	}
}
