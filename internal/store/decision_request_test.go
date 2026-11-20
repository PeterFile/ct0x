package store

import (
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// isolateDecisionFixtureRegistry leaves decision SQL to the fixture, including
// on reopen. Call before opening any Store in a nonparallel test.
func isolateDecisionFixtureRegistry(t *testing.T) {
	t.Helper()
	prior := migrations
	if i := slices.Index(prior, decisionRequestSchema); i >= 0 {
		migrations = prior[:i:i]
		t.Cleanup(func() { migrations = prior })
	}
}

func installDecisionRequestFixture(t *testing.T, s *Store) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(decisionRequestSchema); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func decisionClarificationInput() (domain.ChatAssessment, domain.DecisionContext) {
	return domain.ChatAssessment{Disposition: "clarify", Reason: "目标样本有待用户选择。", Question: "这次只整理工作日样本吗？"}, domain.DecisionContext{
		Facts: "原消息未指定日期。", Recommendation: "先整理工作日样本。", Choices: []string{"只看工作日", "同时整理周末样本"},
	}
}

func TestDecisionRequestExplicitClarificationSurvivesReopen(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	f := chatSetupSource(t, "user", false, "请帮我整理两批样本。")
	input, details := decisionClarificationInput()
	if page, err := f.s.ListPendingDecisionRequests(ctx, ListOptions{}); !errors.Is(err, ErrDecisionRequestsUnavailable) || len(page.Items) != 0 {
		t.Fatalf("unregistered schema: %+v %v", page, err)
	}
	if _, err := f.s.ApplyChatDecisionClarification(ctx, f.scope, f.candidate.ID, input, details, input.Question, "needs-user"); !errors.Is(err, ErrDecisionRequestsUnavailable) {
		t.Fatalf("unregistered explicit write: %v", err)
	}
	installDecisionRequestFixture(t, f.s)
	before := decisionRequestEffects(t, f.s)
	r, err := f.s.ApplyChatDecisionClarification(ctx, f.scope, f.candidate.ID, input, details, input.Question, "needs-user")
	c := f.candidate
	if err != nil || r.ID == "" || r.Version != 1 || r.Kind != domain.DecisionRequestClarification || r.ThreadRef != c.ThreadRef ||
		r.Relay != domain.DecisionRelayOriginThread || r.Task != nil || r.Chat == nil || r.RecipientID != "user" ||
		r.Chat.SourceID != c.SourceID || r.Chat.CandidateID != c.ID || r.SourceMessageID != c.MessageID || r.SourceSequence != c.Generation ||
		r.AuthorTurnID != c.TurnID || r.AuthorMemberID != c.MemberID || r.AuthorAttemptID != "" || r.Question != input.Question {
		t.Fatalf("wrong clarification proof: %+v %v", r, err)
	}
	var persisted string
	if err := f.s.db.QueryRow(`SELECT candidate_id FROM chat_assessments WHERE id=? AND notice_message_id=?`, r.Chat.AssessmentID, r.Chat.NoticeMessageID).Scan(&persisted); err != nil || persisted != c.ID {
		t.Fatalf("missing native assessment: %q %v", persisted, err)
	}
	notice, err := f.s.GetMessage(ctx, c.GroupID, r.Chat.NoticeMessageID)
	if err != nil || notice.Content != input.Question || notice.TopicID != c.TopicID || len(notice.Deliveries) != 0 {
		t.Fatalf("not a non-waking notice in the original thread: %+v %v", notice, err)
	}
	if got, err := f.s.ApplyChatDecisionClarification(ctx, f.scope, c.ID, input, details, input.Question, "needs-user"); err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("replay failed: %+v %v", got, err)
	}
	messageSequencePreserved(t, f.s, before)
	dispatchAssertFK(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	page, err := reopened.ListPendingDecisionRequests(ctx, ListOptions{Limit: 1})
	if err != nil || !reflect.DeepEqual(page.Items, []domain.DecisionRequest{r}) || page.NextCursor != nil {
		t.Fatalf("exact clarification lost after reopen: %+v %v", page, err)
	}
}

func TestDecisionRequestChatSourceFence(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	for _, name := range []string{"ordinary", "default-routed", "member-relay", "legacy-user-label", "foreign-candidate", "stale-before", "stale-after"} {
		t.Run(name, func(t *testing.T) {
			origin := "user"
			if name == "member-relay" {
				origin = "agent"
			}
			f := chatSetup(t, origin, false)
			installDecisionRequestFixture(t, f.s)
			s, c, scope := f.s, f.candidate, f.scope
			input, details := decisionClarificationInput()
			want := ErrMemberToolDenied
			switch name {
			case "ordinary":
				if r, err := s.ApplyChatAssessment(ctx, scope, c.ID, input, input.Question, "ordinary"); err != nil || r.Question != input.Question {
					t.Fatalf("ordinary clarification changed: %+v %v", r, err)
				}
				want = ErrConflict // An existing assessment is not retroactively upgraded.
			case "default-routed", "legacy-user-label":
				a := member(t, s, f.groupID).ID
				message := MessageRequest{GroupID: f.groupID, TopicID: c.TopicID, ActorID: "user", Content: "另一批样本。"}
				var m domain.Message
				if name == "default-routed" {
					m = chatLocalMessage(t, s, message, []string{a})
				} else {
					var err error
					message.RecipientIDs = []string{a}
					m, err = s.CreateMessage(ctx, message, []string{a})
					if err != nil {
						t.Fatal(err)
					}
				}
				scope, c = workInputCandidate(t, f, m, 0)
			case "foreign-candidate":
				other := f
				other.groupID = f.otherID
				_, c = newWorkInputCandidate(t, other, "另一个群的请求。", f.foreign)
			case "stale-before", "stale-after":
				want = ErrConflict
				if name == "stale-after" {
					r, err := s.ApplyChatDecisionClarification(ctx, scope, c.ID, input, details, input.Question, "ask")
					if err != nil {
						t.Fatal(err)
					}
					for _, thread := range []domain.ThreadRef{{GroupID: f.otherID, TopicID: c.TopicID}, {GroupID: c.GroupID, TopicID: "other-topic"}} {
						chatLocalMessage(t, s, MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: "另一项请求。"}, nil)
					}
					page, err := s.ListPendingDecisionRequests(ctx, ListOptions{})
					if err != nil || !reflect.DeepEqual(page.Items, []domain.DecisionRequest{r}) {
						t.Fatalf("foreign topic or group invalidated proof: %+v %v", page, err)
					}
				}
				chatLocalMessage(t, s, MessageRequest{GroupID: c.GroupID, TopicID: c.TopicID, ActorID: "user", Content: "范围改了。"}, nil)
			}
			before := messageSequenceSnapshot(t, s)
			if r, err := s.ApplyChatDecisionClarification(ctx, scope, c.ID, input, details, input.Question, "ask"); !errors.Is(err, want) || r.ID != "" {
				t.Fatalf("unsafe clarification (%s): %+v %v; want %v", name, r, err, want)
			}
			page, err := s.ListPendingDecisionRequests(ctx, ListOptions{})
			if err != nil || page.Items == nil || len(page.Items) != 0 {
				t.Fatalf("unsafe item visible: %+v %v", page, err)
			}
			messageSequencePreserved(t, s, before)
		})
	}
}

func decisionTaskInput() domain.TaskDecisionRequestInput {
	return domain.TaskDecisionRequestInput{Kind: domain.DecisionRequestAuthorization, DecisionQuestion: domain.DecisionQuestion{
		DecisionContext: domain.DecisionContext{Facts: "已验证工作日样本。", Recommendation: "先交付工作日结果。", Choices: []string{"仅工作日", "加入周末"}},
		Question:        "是否把周末样本纳入这次验证？", Authorization: &domain.DecisionAuthorization{Scope: "只读取现有周末样本。", Impact: "多核对两批数据，不写入样本。"},
	}}
}

func decisionRequestEffects(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	protected := map[string][][]any{}
	for _, table := range []string{"tasks", "mandates", "task_coordination", "task_attempts", "coordination_dispatch_requests", "coordination_assignments",
		"chat_followup_routes", "deliveries", "task_work_deliveries", "resource_claims", "turns"} {
		protected[table] = messageSequenceTableRows(t, s, table)
	}
	return protected
}

func sameDecisionTaskInOtherGroup(t *testing.T, f chatFixture) chatFixture {
	t.Helper()
	f.groupID, f.a = f.otherID, f.foreign
	f.b = member(t, f.s, f.groupID).ID
	if _, err := f.s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.groupID, TopicID: f.scope.TopicID}, f.policy.ChatPolicyRequest, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetCoordinator(ctx, f.groupID, f.a, 0, "coordinator"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 0, "dispatch-policy"); err != nil {
		t.Fatal(err)
	}
	f.scope, f.candidate = newWorkInputCandidate(t, f, "请按约定验证样本。", f.a)
	return f
}

func TestDecisionRequestTaskGenerationDedupIsolationAndVersion(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	f := chatSetup(t, "user", false)
	f.input.Title = "周末样本验证"
	installDecisionRequestFixture(t, f.s)
	s := f.s
	owner := chatAuthorityWorker(t, f)
	input := decisionTaskInput()
	before := decisionRequestEffects(t, s)
	first, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, input, "first-wire-key")
	p := owner.Provenance
	if err != nil || first.ID == "" || first.Version != 1 || first.ThreadRef != f.candidate.ThreadRef || first.RecipientID != "user" ||
		first.SourceMessageID != f.candidate.MessageID || first.Chat != nil || first.Task == nil || first.Task.TaskID != p.TaskID ||
		first.Task.TaskRevision != p.TaskRevision || first.Task.AssignmentID != p.AssignmentID || first.Task.OwnerMemberID != p.MemberID ||
		first.AuthorAttemptID != owner.AttemptID || first.Relay != domain.DecisionRelayTaskOwner {
		t.Fatalf("incorrect exact Task binding: %+v %v", first, err)
	}
	for _, key := range []string{"first-wire-key", "another-wire-key"} {
		if r, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, input, key); err != nil || !reflect.DeepEqual(r, first) {
			t.Fatalf("same slot did not coalesce: %+v %v", r, err)
		}
	}
	changed := input
	changed.Question = "是否只纳入最近一个周末的样本？"
	if _, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, changed, "new-wording"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unfenced overwrite: %v", err)
	}
	changed.ExpectedVersion = first.Version
	secondVersion, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, changed, "new-wording")
	if err != nil || secondVersion.ID != first.ID || secondVersion.Version != 2 || secondVersion.Question != changed.Question || secondVersion.CreatedAt != first.CreatedAt {
		t.Fatalf("CAS amendment failed: %+v %v", secondVersion, err)
	}
	if _, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, input, "first-wire-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("old transport replay returned superseded contents as current: %v", err)
	}
	assertDecisionVersionMissing(t, s, first, first.ThreadRef, time.Now().UTC())
	assertDecisionVersionMissing(t, s, secondVersion, domain.ThreadRef{GroupID: f.otherID, TopicID: first.TopicID}, time.Now().UTC())
	rows := messageSequenceRows(t, s.db, "SELECT version,json_extract(question_json,'$.question') FROM decision_request_versions ORDER BY version")
	if !reflect.DeepEqual(rows, [][]any{{int64(1), first.Question}, {int64(2), changed.Question}}) {
		t.Fatalf("amendment did not preserve original: %v", rows)
	}
	messageSequencePreserved(t, s, before)
	for _, scope := range []MemberToolScope{taskWorkTestCoordinator(t, s, f.groupID, p.AssignmentID), owner.Member} {
		taskWorkTestReceipt(t, s, scope, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	}

	other := sameDecisionTaskInOtherGroup(t, f)
	otherOwner := chatAuthorityWorker(t, other)
	otherItem, err := s.ProposeTaskDecisionRequest(ctx, otherOwner.Member, input, "first-wire-key")
	if err != nil || otherItem.ID == first.ID || otherItem.Task == nil || otherItem.Task.TaskTitle != first.Task.TaskTitle ||
		otherItem.Task.TaskRevision != first.Task.TaskRevision || otherItem.Task.TaskID == first.Task.TaskID || otherItem.GroupID != f.otherID ||
		otherItem.TopicID != first.TopicID || otherItem.GroupName != first.GroupName {
		t.Fatalf("same-name work was not isolated: %+v %v", otherItem, err)
	}
	page, err := s.ListPendingDecisionRequests(ctx, ListOptions{Limit: 1})
	if err != nil || !reflect.DeepEqual(page.Items, []domain.DecisionRequest{otherItem}) || page.NextCursor == nil {
		t.Fatalf("newest page: %+v %v", page, err)
	}
	older, err := s.ListPendingDecisionRequests(ctx, ListOptions{Limit: 1, Before: page.NextCursor})
	if err != nil || !reflect.DeepEqual(older.Items, []domain.DecisionRequest{secondVersion}) || older.NextCursor != nil {
		t.Fatalf("older page lost its distinct updated Task: %+v %v", older, err)
	}
	coordinationDecideExec(t, s, `UPDATE tasks SET revision=revision+1,active_attempt_id=NULL WHERE id=?`, otherItem.Task.TaskID)
	if r, err := s.ProposeTaskDecisionRequest(ctx, otherOwner.Member, input, "first-wire-key"); !errors.Is(err, ErrMemberToolDenied) || r.ID != "" {
		t.Fatalf("stale Task revision replay: %+v %v", r, err)
	}
	page, err = s.ListPendingDecisionRequests(ctx, ListOptions{Limit: 1})
	if err != nil || !reflect.DeepEqual(page.Items, []domain.DecisionRequest{secondVersion}) || page.NextCursor != nil {
		t.Fatalf("current filter must apply before LIMIT: %+v %v", page, err)
	}
	coordinationDecideExec(t, s, `UPDATE task_coordination SET authority_generation=authority_generation+1 WHERE task_id=?`, first.Task.TaskID)
	if page, err := s.ListPendingDecisionRequests(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("obsolete authorization survived: %+v %v", page, err)
	}
	dispatchAssertFK(t, s)
}

func assertDecisionVersionMissing(t *testing.T, s *Store, request domain.DecisionRequest, thread domain.ThreadRef, now time.Time) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if got, err := pendingDecisionRequestInTx(ctx, tx, thread, request.ID, request.Version, now); !errors.Is(err, sql.ErrNoRows) || got.ID != "" {
		t.Fatalf("obsolete or foreign version was still selectable: %+v %v", got, err)
	}
}

func TestDecisionRequestTaskScopeWithdrawalAndExpiry(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	f := chatSetup(t, "user", false)
	installDecisionRequestFixture(t, f.s)
	input := decisionTaskInput()
	if r, err := f.s.ProposeTaskDecisionRequest(ctx, f.scope, input, "ordinary-turn"); !errors.Is(err, ErrMemberToolDenied) || r.ID != "" {
		t.Fatalf("ordinary intake could bind a formal Task: %+v %v", r, err)
	}
	owner := chatAuthorityWorker(t, f)
	for _, damage := range []func(*domain.TaskDecisionRequestInput){
		func(i *domain.TaskDecisionRequestInput) { i.Kind = "review" },
		func(i *domain.TaskDecisionRequestInput) { i.Authorization = nil },
		func(i *domain.TaskDecisionRequestInput) { i.Question = "第一问？\n第二问？" },
		func(i *domain.TaskDecisionRequestInput) { i.Facts = strings.Repeat("字", 683) },
		func(i *domain.TaskDecisionRequestInput) { i.Choices = []string{"同一标签", " 同一标签 "} },
	} {
		bad := input
		damage(&bad)
		if _, err := f.s.ProposeTaskDecisionRequest(ctx, owner.Member, bad, "invalid"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unbounded proposal: %+v %v", bad, err)
		}
	}
	deadline := time.Now().UTC().Add(time.Hour)
	input.ExpiresAt = &deadline
	r, err := f.s.ProposeTaskDecisionRequest(ctx, owner.Member, input, "bound")
	if err != nil || r.ExpiresAt == nil || !r.ExpiresAt.Equal(deadline) {
		t.Fatalf("deadline not recorded: %+v %v", r, err)
	}
	assertDecisionVersionMissing(t, f.s, r, r.ThreadRef, deadline)
	coordinationDecideExec(t, f.s, `UPDATE decision_requests SET withdrawn_at=? WHERE id=?`, time.Now().UTC().Format(timestampLayout), r.ID)
	if _, err := f.s.ProposeTaskDecisionRequest(ctx, owner.Member, input, "new-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("withdrawn slot reopened: %v", err)
	}
	if page, err := f.s.ListPendingDecisionRequests(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("withdrawn version visible: %+v %v", page, err)
	}
}
