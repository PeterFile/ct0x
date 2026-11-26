package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestWorkInputReturnDecisionPartialInstall(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	withTaskImpactSchema(t)
	for _, name := range []string{"missing-impact-marker", "missing-projection-view"} {
		t.Run(name, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			s := f.s
			installDecisionAnswerFixture(t, s)
			installWorkInputDecisionFixture(t, s)
			owner := chatAuthorityWorker(t, f)
			r, c := impactInput(t, f, "另需把结论写入项目目录。", "")
			wantImpacts := 1
			if name == "missing-impact-marker" {
				coordinationDecideExec(t, s, `DELETE FROM schema_migrations WHERE version=?`, taskImpactSchemaVersionNow())
				wantImpacts = 0
			} else {
				// Leave all replacement guards installed.
				coordinationDecideExec(t, s, `DROP VIEW `+workInputDecisionSources)
			}
			i, err := s.HandBackWorkInputWithKey(ctx, owner.Member, r.WorkInputID, "原授权不允许写入目录。", "partial-return")
			if err != nil || i.ID != r.WorkInputID || i.Return == nil || i.Consumption != nil || i.Return.NoticeMessageID == "" ||
				i.Return.Disposition != domain.TaskWorkInputReturnSeparateAuthorizationRequired || i.ThreadRef != c.ThreadRef ||
				i.SourceID != c.SourceID || i.ReplyMessageID != c.MessageID || i.SourceSequence != c.Generation {
				t.Fatalf("partial installation obstructed the B return: %+v %v", i, err)
			}
			if page, err := s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
				t.Fatalf("partial installation projected a decision: %+v %v", page, err)
			}
			for table, want := range map[string]int{"task_constraint_impacts": wantImpacts, "decision_requests": 0, "decision_request_versions": 0} {
				if rows := messageSequenceTableRows(t, s, table); len(rows) != want {
					t.Fatalf("partial installation wrote %d rows to %s; want %d", len(rows), table, want)
				}
			}

			proposal := decisionTaskInput()
			q, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, proposal, "ordinary-proposal")
			if err != nil || q.Task == nil || q.Task.WorkInputID != "" || q.Task.MandateID != i.MandateID || q.Task.AssignmentID != i.AssignmentID ||
				q.Version != 1 || q.ThreadRef != f.candidate.ThreadRef || q.SourceMessageID != f.candidate.MessageID || q.SourceSequence != f.candidate.Generation {
				t.Fatalf("partial installation obstructed the Mandate-source F proposal: %+v %v", q, err)
			}
			if page, err := s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || !reflect.DeepEqual(page.Items, []domain.DecisionRequest{q}) {
				t.Fatalf("partial installation lost the ordinary pending decision: %+v %v", page, err)
			}
			input := domain.DecisionAnswerInput{Version: q.Version, OptionIndex: 1}
			a, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, input, "ordinary-answer")
			if err != nil || a.RequestID != q.ID || a.Version != q.Version || a.ThreadRef != q.ThreadRef || a.TaskID != i.TaskID ||
				a.OptionIndex != input.OptionIndex || a.Text != proposal.Choices[0] {
				t.Fatalf("partial installation obstructed the ordinary typed F answer: %+v %v", a, err)
			}
			assertDecisionAnswerView(t, s, a, nil)
			dispatchAssertFK(t, s)
		})
	}
}

func TestWorkInputReturnDecisionNoSQLBackfill(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	withTaskImpactSchema(t)
	withImpact := migrations
	t.Cleanup(func() { migrations = withImpact })
	migrations = withImpact[:taskImpactSchemaVersionNow()-1]
	f := chatSetup(t, "user", false)
	owner := chatAuthorityWorker(t, f)
	historical, _ := impactInput(t, f, "还需写入一份目录。", "")
	fresh, fc := impactInput(t, f, "还需写入另一份目录。", "")
	const reason, key = "目录写入不在原授权内。", "pre-impact-return"
	old, err := f.s.HandBackWorkInputWithKey(ctx, owner.Member, historical.WorkInputID, reason, key)
	if err != nil || old.ID != historical.WorkInputID || old.Return == nil || old.Return.TurnID != owner.Member.Claim.TurnID ||
		old.Return.Disposition != domain.TaskWorkInputReturnSeparateAuthorizationRequired {
		t.Fatalf("fixture did not return an input before B3: %+v %v", old, err)
	}
	migrations = withImpact
	s := queueHandle(t, f.path, false) // Install B3 without settling the returning Turn.
	installDecisionAnswerFixture(t, s)
	before := messageSequenceSnapshot(t, s)
	if len(before["task_constraint_impacts"]) != 0 {
		t.Fatal("B3 synthesized an impact for an earlier return")
	}
	installWorkInputDecisionFixture(t, s)
	if replay, err := s.HandBackWorkInputWithKey(ctx, owner.Member, old.ID, reason, key); err != nil || !reflect.DeepEqual(replay, old) {
		t.Fatalf("same-key replay lost the original receipt at upgrade: %+v %v", replay, err)
	}
	if page, err := s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("installation or replay synthesized a decision: %+v %v", page, err)
	}
	messageSequencePreserved(t, s, before)

	p := owner.Provenance
	dispatch, err := s.GetDispatchRequest(ctx, p.GroupID, p.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// Try only the enrollment write, with the exact historical return tuple
	// and its still-live author. Lack of an impact cannot establish freshness.
	_, err = s.db.Exec(`INSERT INTO decision_requests(id,group_id,topic_id,recipient_id,kind,relay,
	 task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,origin_trigger_id,source_message_id,source_sequence,created_at)
	 VALUES(?,?,?,'user','authorization','task_owner',?,?,?,?,?,?,?,?,?,?)`, workInputDecisionPrefix+"historical", old.GroupID, old.TopicID,
		old.TaskID, old.TaskRevision, old.AssignmentID, old.MandateID, dispatch.Request.AuthorityGeneration, old.WorkerMemberID,
		old.TriggerID, old.ReplyMessageID, old.SourceSequence, old.Return.ReturnedAt.Format(timestampLayout))
	if err == nil || !strings.Contains(err.Error(), "待决事项来源不匹配") {
		t.Fatalf("request guard did not reject the pre-install return: %v", err)
	}
	messageSequencePreserved(t, s, before)
	var boundary, notice int64
	if err := s.db.QueryRow(`SELECT b.sequence,l.sequence FROM decision_work_input_boundary b
	 JOIN message_log l ON l.message_id=?`, old.Return.NoticeMessageID).Scan(&boundary, &notice); err != nil || boundary != notice {
		t.Fatalf("install did not fence at the last committed notice: boundary=%d notice=%d err=%v", boundary, notice, err)
	}
	for _, query := range []string{
		`INSERT OR REPLACE INTO decision_work_input_boundary VALUES(0)`,
		`UPDATE decision_work_input_boundary SET sequence=0`,
		`DELETE FROM decision_work_input_boundary`,
	} {
		if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "安装边界不可修改") {
			t.Fatalf("installation boundary was not immutable: %s: %v", query, err)
		}
	}

	// A pre-install followup can still be returned after installation; only
	// the real return notice sequence, not the input's, fences enrollment.
	i, err := s.HandBackWorkInputWithKey(ctx, owner.Member, fresh.WorkInputID, reason, "post-install-return")
	if err != nil || i.ID != fresh.WorkInputID || i.Return == nil {
		t.Fatalf("install fence lost the fresh B return: %+v %v", i, err)
	}
	page, err := s.ListPendingTaskDecisions(ctx, ListOptions{})
	if err != nil || len(page.Items) != 1 || page.NextCursor != nil {
		t.Fatalf("only the fresh return should be pending: %+v %v", page, err)
	}
	assertReturnedDecisionScope(t, page.Items[0], i, fc, dispatch.Request.AuthorityGeneration)
	dispatchAssertFK(t, s)
}

func TestWorkInputReturnDecisionLateDiscussion(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	withTaskImpactSchema(t)
	f := chatSetup(t, "user", true)
	s := f.s
	installDecisionAnswerFixture(t, s)
	installWorkInputDecisionFixture(t, s)
	policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 2)
	s.EnableInvestigationExecution()
	s.EnableTaskWorkExecution()
	discussionEnd(t, s, f.scope)
	const quote = "本次只讨论，不开始调查。"
	peer := member(t, s, f.groupID).ID
	targets := []string{f.a, peer}
	root := chatLocalMessage(t, s, MessageRequest{GroupID: f.groupID, TopicID: policy.TopicID,
		ActorID: "user", Content: quote, RecipientIDs: targets}, targets)
	var admissionDelivery, competingDelivery domain.Delivery
	for _, d := range root.Deliveries {
		switch d.RecipientID {
		case f.a:
			admissionDelivery = d
		case peer:
			competingDelivery = d
		}
	}
	investigationBindCandidate(t, &f, admissionDelivery)
	f.input = investigationProposal()
	owner := investigationBindWorker(t, f)
	p := owner.Provenance

	// Record an authentic competing intake before newer chat supersedes its
	// source for intake. Stage only its immutable assessment row so the test
	// can model late arrival of the durable restriction at the pending read.
	scope, id := discussionArrive(t, s, f.groupID, competingDelivery.ID)
	limit, err := s.ApplyChatAssessment(ctx, scope, id, domain.ChatAssessment{
		Disposition: "discuss", Reason: "先交换意见。", DiscussionOnlyExcerpt: new(quote),
	}, "先讨论。", "late-limit")
	if err != nil || !limit.DiscussionOnly || limit.DiscussionOnlyExcerpt != quote || limit.ThreadRef != f.candidate.ThreadRef ||
		limit.MessageID != root.ID || limit.SourceID != root.SourceID || limit.SourceKind != "local_user_chat" ||
		limit.CandidateGeneration != f.candidate.Generation || limit.TurnID != scope.Claim.TurnID || limit.TaskID != "" {
		t.Fatalf("fixture did not record an exact same-source restriction: %+v %v", limit, err)
	}
	discussionEnd(t, s, scope)
	var deleteGuard string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='chat_assessments_no_delete'`).Scan(&deleteGuard); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, s, `CREATE TEMP TABLE deferred_discussion_limit AS SELECT * FROM chat_assessments WHERE id=?`, limit.ID)
	coordinationDecideExec(t, s, `DROP TRIGGER chat_assessments_no_delete`)
	coordinationDecideExec(t, s, `DELETE FROM chat_assessments WHERE id=?`, limit.ID)
	coordinationDecideExec(t, s, deleteGuard)

	ordinary := decisionTaskInput()
	older, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, ordinary, "older-ordinary")
	if err != nil {
		t.Fatal(err)
	}
	r, c := impactInput(t, f, "还请把调查结论写到目录。", "")
	i, err := s.HandBackWorkInputWithKey(ctx, owner.Member, r.WorkInputID, "调查只允许读取。", "investigation-return")
	if err != nil || i.ID != r.WorkInputID || i.Return == nil || i.Return.PolicyRevision != policy.Revision ||
		i.Return.PolicySourceID != policy.ConfigurationSourceID {
		t.Fatalf("missing scoped investigation return: %+v %v", i, err)
	}
	ordinary.Kind, ordinary.Authorization = domain.DecisionRequestUnblock, nil
	newer, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, ordinary, "newer-ordinary")
	if err != nil {
		t.Fatal(err)
	}
	// Pending concerns durable authority, not whether this handle can start a provider.
	inbox := queueHandle(t, f.path, false)
	page, err := inbox.ListPendingTaskDecisions(ctx, ListOptions{})
	if err != nil || len(page.Items) != 3 || !reflect.DeepEqual(page.Items[0], newer) || !reflect.DeepEqual(page.Items[2], older) {
		t.Fatalf("expected the investigation return between two ordinary items: %+v %v", page, err)
	}
	q := page.Items[1]
	dispatch, err := s.GetDispatchRequest(ctx, p.GroupID, p.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	assertReturnedDecisionScope(t, q, i, c, dispatch.Request.AuthorityGeneration)

	coordinationDecideExec(t, s, `INSERT INTO chat_assessments SELECT * FROM temp.deferred_discussion_limit;
	 DROP TABLE temp.deferred_discussion_limit`)
	if err := s.CheckTaskInvestigationReady(ctx, p.GroupID, p.TaskID); !errors.Is(err, ErrTaskInvestigationNotReady) {
		t.Fatalf("same-source limit did not revoke investigation authority: %v", err)
	}
	before := messageSequenceSnapshot(t, s)
	if a, err := inbox.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, domain.DecisionAnswerInput{Version: q.Version, Text: "继续。"}, "late-answer"); !errors.Is(err, ErrConflict) || a.ID != "" {
		t.Fatalf("late restriction did not fence the exact answer: %+v %v", a, err)
	}
	for _, list := range []func(context.Context, ListOptions) (Page[domain.DecisionRequest], error){inbox.ListPendingTaskDecisions, inbox.ListPendingDecisionRequests} {
		first, err := list(ctx, ListOptions{Limit: 1})
		if err != nil || !reflect.DeepEqual(first.Items, []domain.DecisionRequest{newer}) || first.NextCursor == nil ||
			*first.NextCursor != (Cursor{CreatedAt: newer.CreatedAt, ID: newer.ID}) {
			t.Errorf("restricted return broke the first page or lookahead: %+v %v", first, err)
		}
		for _, item := range []domain.DecisionRequest{newer, q} {
			last, err := list(ctx, ListOptions{Limit: 1, Before: &Cursor{CreatedAt: item.CreatedAt, ID: item.ID}})
			if err != nil || !reflect.DeepEqual(last.Items, []domain.DecisionRequest{older}) || last.NextCursor != nil {
				t.Errorf("restricted return leaked or shortened the page after %s: %+v %v", item.ID, last, err)
			}
		}
	}
	messageSequencePreserved(t, s, before)
	dispatchAssertFK(t, s)
}

func assertReturnedDecisionScope(t *testing.T, q domain.DecisionRequest, i domain.TaskWorkInput, c domain.ChatCandidate, generation int64) {
	t.Helper()
	if q.ID == "" || q.Task == nil || q.Kind != domain.DecisionRequestAuthorization || q.Version != 1 || q.Question != i.Return.Question ||
		q.ThreadRef != i.ThreadRef || i.ThreadRef != c.ThreadRef || q.SourceMessageID != c.MessageID || q.SourceSequence != c.Generation ||
		i.SourceID != c.SourceID || q.SourceMessageID != i.ReplyMessageID || q.SourceSequence != i.SourceSequence ||
		q.Task.OriginTriggerID != i.TriggerID || q.Task.WorkInputID != i.ID || q.Task.TaskID != i.TaskID || q.Task.TaskRevision != i.TaskRevision ||
		q.Task.MandateID != i.MandateID || q.Task.AuthorityGeneration != generation || q.Task.AssignmentID != i.AssignmentID ||
		q.Task.OwnerMemberID != i.WorkerMemberID {
		t.Fatalf("wrong current request scope for returned input %s: %+v (task %+v)", i.ID, q, q.Task)
	}
}
