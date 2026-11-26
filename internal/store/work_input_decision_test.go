package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func installWorkInputDecisionFixture(t *testing.T, s *Store) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(workInputDecisionSchema); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkInputReturnDecision(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	withTaskImpactSchema(t)
	f := chatSetup(t, "user", false)
	s := f.s
	installDecisionAnswerFixture(t, s)
	installWorkInputDecisionFixture(t, s)
	owner := chatAuthorityWorker(t, f)
	p := owner.Provenance
	refusal := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	coordinator := taskWorkTestCoordinator(t, s, p.GroupID, p.AssignmentID)
	for _, scope := range []MemberToolScope{coordinator, owner.Member} {
		taskWorkTestReceipt(t, s, scope, refusal, domain.TurnStateNotStarted)
	}
	r, followup := taskWorkTestFollowup(t, f, "还请把摘要写入项目目录。")
	live := taskWorkTestClaim(t, s, followup, "return-decision-owner")
	attempt, err := s.BindTaskWorkContinuation(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, s)
	const reason, key = "原授权仅限在话题中交付，不允许写入目录。", "return-extra-work"
	wrong := live
	wrong.Claim.Token += "-wrong"
	if i, err := s.HandBackWorkInputWithKey(ctx, wrong, r.WorkInputID, reason, key); !errors.Is(err, ErrMemberToolDenied) || i.ID != "" {
		t.Fatalf("invalid scope returned an input: %+v %v", i, err)
	}
	messageSequencePreserved(t, s, before)
	// The original Attempt is a real Attempt on the same Task, but it did not
	// return this input. Probe the version guard before the usual first insert.
	coordinationDecideExec(t, s, `CREATE TEMP TRIGGER forge_return_author AFTER INSERT ON decision_requests BEGIN
	 INSERT INTO decision_request_versions(request_id,version,question_json,author_member_id,author_turn_id,author_attempt_id,created_at)
	 SELECT NEW.id,1,json_object('question',h.question,'facts',h.reason),h.member_id,b.turn_id,b.attempt_id,NEW.created_at
	 FROM task_work_input_returns h JOIN coordination_assignment_attempts b ON b.assignment_id=NEW.assignment_id;
	 END`)
	var boundary *taskWorkTransactionError
	if i, err := s.HandBackWorkInputWithKey(ctx, live, r.WorkInputID, reason, key); i.ID != "" || !errors.As(err, &boundary) ||
		!boundary.RollbackConfirmed() || !strings.Contains(boundary.cause.Error(), "待决事项与工作输入交回记录不匹配") {
		t.Fatalf("version guard accepted a different real author: %+v %v", i, err)
	}
	coordinationDecideExec(t, s, `DROP TRIGGER forge_return_author`)
	messageSequencePreserved(t, s, before)
	got, err := s.HandBackWorkInputWithKey(ctx, live, r.WorkInputID, reason, key)
	if err != nil || got.ID != r.WorkInputID || got.Return == nil || got.Consumption != nil || got.Return.Disposition != domain.TaskWorkInputReturnSeparateAuthorizationRequired {
		t.Fatalf("missing real owner return: %+v %v", got, err)
	}
	h := got.Return
	page, err := s.ListPendingTaskDecisions(ctx, ListOptions{}) // GET /api/v1/me/pending-decisions projection.
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("fresh scoped return must persist one public pending decision: %+v %v", page, err)
	}
	q := page.Items[0]
	if q.ID == "" || q.Task == nil || q.Chat != nil || q.Version != 1 || q.Kind != domain.DecisionRequestAuthorization ||
		q.ThreadRef != got.ThreadRef || q.RecipientID != "user" || q.Relay != domain.DecisionRelayTaskOwner || q.ExpiresAt != nil ||
		q.Question != h.Question || q.Facts != reason || q.AuthorMemberID != h.MemberID || q.AuthorTurnID != live.Claim.TurnID ||
		q.AuthorAttemptID != attempt.ID || q.AuthorAttemptID == owner.AttemptID || q.CreatedAt != h.ReturnedAt || q.UpdatedAt != h.ReturnedAt ||
		q.Task.TaskID != p.TaskID || q.Task.TaskRevision != p.TaskRevision || q.Task.AssignmentID != p.AssignmentID ||
		q.Task.MandateID != followup.MandateID || q.Task.AuthorityGeneration != followup.AuthorityGeneration || q.Task.OwnerMemberID != p.MemberID ||
		q.Task.OriginTriggerID != got.TriggerID || q.SourceMessageID != got.ReplyMessageID || q.SourceSequence != got.SourceSequence {
		t.Fatalf("wrong returned decision binding: %+v (task %+v), return %+v", q, q.Task, h)
	}
	body, err := json.Marshal(q.Task)
	if err != nil || !strings.Contains(string(body), `"work_input_id":"`+got.ID+`"`) {
		t.Fatalf("public task binding omitted the exact WorkInput: %s %v", body, err)
	}
	var founding string
	if err := s.db.QueryRow(`SELECT origin_trigger_id FROM mandates WHERE id=?`, q.Task.MandateID).Scan(&founding); err != nil ||
		q.Task.OriginTriggerID == founding || q.SourceMessageID == f.candidate.MessageID || q.SourceSequence == f.candidate.Generation {
		t.Fatalf("returned source was confused with the founding Mandate: %+v %q %v", q, founding, err)
	}
	var trigger, message, source, input string
	var sequence, committed int64
	plan := resolutionPlan(t, s, `SELECT b.input_id FROM `+workInputDecisionSources+` b WHERE b.trigger_id=? AND b.impacted`, got.TriggerID)
	for _, seek := range []string{"SEARCH f", "SEARCH i", "SEARCH h", "SEARCH x"} {
		if !strings.Contains(plan, seek) {
			t.Fatalf("return source binding is not an indexed lookup (%s):\n%s", seek, plan)
		}
	}
	if err := s.db.QueryRow(`SELECT q.origin_trigger_id,q.source_message_id,q.source_sequence,f.source_id,i.id,l.sequence
	 FROM decision_requests q JOIN chat_followup_routes f ON f.trigger_id=q.origin_trigger_id
	 JOIN task_work_inputs i ON i.source_id=f.source_id JOIN message_log l ON l.message_id=f.reply_message_id WHERE q.id=?`, q.ID).
		Scan(&trigger, &message, &sequence, &source, &input, &committed); err != nil ||
		trigger != got.TriggerID || message != got.ReplyMessageID || sequence != got.SourceSequence || source != got.SourceID || input != got.ID || committed != sequence {
		t.Fatalf("wrong immutable origin tuple: %s %s %d %s %s %d (%v)", trigger, message, sequence, source, input, committed, err)
	}
	impact := impacts(t, s, p.GroupID, p.TaskID)
	if len(impact) != 1 || impact[0].Kind != domain.TaskImpactWorkInputReturned || impact[0].Source.InputID != got.ID || impact[0].Decision == nil {
		t.Fatalf("missing exact B return impact: %+v", impact)
	}
	after := messageSequenceSnapshot(t, s)
	for _, table := range []string{"messages", "message_log", "task_work_input_returns", "task_constraint_impacts", "mutation_idempotency", "decision_requests", "decision_request_versions"} {
		if len(after[table]) != len(before[table])+1 {
			t.Fatalf("fresh return must add exactly one %s row", table)
		}
		delete(before, table)
	}
	messageSequencePreserved(t, s, before)
	if again, err := s.HandBackWorkInputWithKey(ctx, live, got.ID, reason, key); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("keyed return replay changed the receipt: %+v %v", again, err)
	}
	if replay, err := s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || !reflect.DeepEqual(page, replay) {
		t.Fatalf("replay changed the pending decision: %+v %v", replay, err)
	}
	messageSequencePreserved(t, s, after)

	response := domain.DecisionAnswerInput{Version: 1, Text: "是，请把额外要求留待单独授权。"}
	foreign := domain.ThreadRef{GroupID: f.otherID, TopicID: q.TopicID}
	if a, err := s.AnswerDecisionRequest(ctx, "user", foreign, q.ID, response, "answer"); !errors.Is(err, ErrConflict) || a.ID != "" {
		t.Fatalf("foreign scope answered the return: %+v %v", a, err)
	}
	stale := response
	stale.Version++
	if a, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, stale, "answer"); !errors.Is(err, ErrConflict) || a.ID != "" {
		t.Fatalf("nonexistent version answered the return: %+v %v", a, err)
	}
	messageSequencePreserved(t, s, after)
	a, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, response, "answer")
	if err != nil || a.ID == "" || a.RequestID != q.ID || a.Version != q.Version || a.ThreadRef != q.ThreadRef ||
		a.ActorID != "user" || a.TaskID != p.TaskID || a.TaskRevision != p.TaskRevision || a.Text != response.Text {
		t.Fatalf("wrong scoped answer: %+v %v", a, err)
	}
	saved := messageSequenceSnapshot(t, s)
	for table, n := range map[string]int{"messages": 2, "message_log": 2, "deliveries": 1, "task_work_deliveries": 1, "decision_answers": 1, "mutation_idempotency": 1} {
		if len(saved[table]) != len(after[table])+n {
			t.Fatalf("answer wrote the wrong number of %s rows", table)
		}
		delete(after, table)
	}
	messageSequencePreserved(t, s, after)
	assertDecisionAnswerView(t, s, a, nil)
	if page, err := s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("answered return is still pending: %+v %v", page, err)
	}
	if i, err := s.AcknowledgeWorkInputWithKey(ctx, live, a.ID, "returning-turn"); !errors.Is(err, ErrMemberToolDenied) || i.ID != "" {
		t.Fatalf("return author could ACK outside the answer's wake: %+v %v", i, err)
	}
	messageSequencePreserved(t, s, saved)
	taskWorkTestReceipt(t, s, live, refusal, domain.TurnStateNotStarted)
	wake, err := s.ResolveTaskWorkDelivery(ctx, q.GroupID, a.WakeDeliveryID)
	if err != nil || wake.Blocked != "" || wake.SourceKind != domain.TaskWorkSourceFollowup || wake.SourceID != a.ID ||
		wake.MemberID != p.MemberID || wake.OwnerMemberID != p.MemberID || wake.AssignmentID != q.Task.AssignmentID ||
		wake.MandateID != q.Task.MandateID || wake.AuthorityGeneration != q.Task.AuthorityGeneration ||
		wake.TaskID != q.Task.TaskID || wake.TaskRevision != q.Task.TaskRevision || wake.MessageID != a.WakeMessageID || wake.TopicID != q.TopicID {
		t.Fatalf("answer did not wake the exact original work: %+v %v", wake, err)
	}
	// The existing F refusal recovery (not a new logical answer) still works.
	firstWake := taskWorkTestClaim(t, s, wake, "return-decision-owner")
	taskWorkTestReceipt(t, s, firstWake, refusal, domain.TurnStateNotStarted)
	pending, err := s.ListPendingDeliveries(ctx, PendingOptions{Lane: "work"})
	if err != nil || len(pending.Items) != 1 || pending.Items[0].Delivery.ID == a.WakeDeliveryID {
		t.Fatalf("answer refusal did not retain one exact replacement: %+v %v", pending, err)
	}
	replacement, err := s.ResolveTaskWorkDelivery(ctx, q.GroupID, pending.Items[0].Delivery.ID)
	wake.DeliveryID, wake.MessageID = pending.Items[0].Delivery.ID, pending.Items[0].Delivery.MessageID
	if err != nil || replacement != wake {
		t.Fatalf("physical recovery changed the logical answer: %+v %v", replacement, err)
	}
	answerOwner := taskWorkTestClaim(t, s, replacement, "return-decision-owner")
	answerAttempt, err := s.BindTaskWorkContinuation(ctx, answerOwner)
	if err != nil || answerAttempt.ID == attempt.ID || answerAttempt.TaskID != p.TaskID || answerAttempt.TaskRevision != p.TaskRevision {
		t.Fatalf("answer did not bind a new Attempt on the original task: %+v %v", answerAttempt, err)
	}
	mandate, err := s.ReadTaskWorkMandate(ctx, answerOwner)
	if err != nil || mandate.ID != q.Task.MandateID || mandate.OriginTriggerID != founding || mandate.TaskRevisionAtAdmission != p.TaskRevision {
		t.Fatalf("answer changed the original mandate: %+v %v", mandate, err)
	}
	if i, err := s.GetWorkInputForMember(ctx, answerOwner, a.ID); err != nil || i.ReplyMessageID != a.ReplyMessageID || i.Consumption != nil ||
		!strings.Contains(i.Content, q.Question) || !strings.HasSuffix(i.Content, a.Text) || i.AfterConstraintRevision != 0 {
		t.Fatalf("wrong non-granting answer WorkInput: %+v %v", i, err)
	}
	ack, err := s.AcknowledgeWorkInputWithKey(ctx, answerOwner, a.ID, "ack-answer")
	if err != nil || ack.Consumption == nil || ack.Consumption.TurnID != answerOwner.Claim.TurnID || ack.Consumption.AttemptID != answerAttempt.ID {
		t.Fatalf("exact continuation could not ACK: %+v %v", ack, err)
	}
	assertDecisionAnswerView(t, s, a, ack.Consumption)
	afterAck := messageSequenceSnapshot(t, s)
	if again, err := s.AcknowledgeWorkInputWithKey(ctx, answerOwner, a.ID, "ack-answer"); err != nil || !reflect.DeepEqual(ack, again) {
		t.Fatalf("answer ACK replay changed its proof: %+v %v", again, err)
	}
	if again, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, response, "answer"); err != nil || again != a {
		t.Fatalf("answer replay changed its receipt: %+v %v", again, err)
	}
	if again, err := s.HandBackWorkInputWithKey(ctx, answerOwner, got.ID, reason, key); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("return replay through the answer owner changed the actor or version: %+v %v", again, err)
	}
	messageSequencePreserved(t, s, afterAck)
	dispatchAssertFK(t, s)
}

func TestWorkInputReturnDecisionSlotsAndLegacy(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	withTaskImpactSchema(t)
	f := chatSetup(t, "user", false)
	s := f.s
	owner := chatAuthorityWorker(t, f)
	p := owner.Provenance
	const reason = "额外工作不属于原授权。"
	handBack := func(id, key string) domain.TaskWorkInput {
		t.Helper()
		i, err := s.HandBackWorkInputWithKey(ctx, owner.Member, id, reason, key)
		if err != nil || i.ID != id || i.Return == nil || i.Return.Disposition != domain.TaskWorkInputReturnSeparateAuthorizationRequired {
			t.Fatalf("owner failed to return %s: %+v %v", id, i, err)
		}
		return i
	}
	var historical []domain.TaskWorkInput
	for j, key := range []string{"before-F", "requests-only", "no-bridge"} {
		if j == 1 {
			installDecisionRequestFixture(t, s)
		} else if j == 2 {
			coordinationDecideExec(t, s, decisionAnswerSchema)
		}
		r, _ := impactInput(t, f, "还需要考虑额外工作。", "")
		historical = append(historical, handBack(r.WorkInputID, key))
	}
	ordinaryInput := decisionTaskInput()
	ordinaryInput.Kind, ordinaryInput.Authorization = domain.DecisionRequestUnblock, nil
	ordinary, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, ordinaryInput, "legacy-unblock")
	if err != nil || ordinary.Task == nil || ordinary.Task.WorkInputID != "" || ordinary.SourceMessageID != f.candidate.MessageID || ordinary.SourceSequence != f.candidate.Generation {
		t.Fatalf("ordinary F proposal changed without bridge: %+v %v", ordinary, err)
	}
	// The old INSERT guard allowed storing an origin it would not project.
	// Even exact historical B source/author data is not a new F request.
	old := historical[0]
	coordinationDecideExec(t, s, `INSERT INTO decision_requests(id,group_id,topic_id,recipient_id,kind,relay,
	 task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,origin_trigger_id,source_message_id,source_sequence,created_at)
	 VALUES('pre-bridge-origin',?,?,'user','authorization','task_owner',?,?,?,?,?,?,?,?,?,?)`,
		old.GroupID, old.TopicID, old.TaskID, old.TaskRevision, old.AssignmentID, old.MandateID, ordinary.Task.AuthorityGeneration,
		old.WorkerMemberID, old.TriggerID, old.ReplyMessageID, old.SourceSequence, old.Return.ReturnedAt.Format(timestampLayout))
	body, err := decisionQuestionJSON(workInputReturnQuestion(old), domain.DecisionRequestAuthorization)
	if err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, s, `INSERT INTO decision_request_versions VALUES('pre-bridge-origin',1,?,?,?,?,?,NULL)`,
		body, old.Return.MemberID, old.Return.TurnID, old.Return.AttemptID, old.Return.ReturnedAt.Format(timestampLayout))
	legacyDDL := `SELECT name,sql FROM sqlite_schema WHERE type='table' AND name IN ('decision_requests','decision_request_versions','decision_answers') ORDER BY name`
	schemaBefore := messageSequenceRows(t, s.db, legacyDDL)
	before := messageSequenceSnapshot(t, s)
	installWorkInputDecisionFixture(t, s)
	if got := messageSequenceRows(t, s.db, legacyDDL); !reflect.DeepEqual(got, schemaBefore) {
		t.Fatalf("bridge rewrote legacy table definitions: %v", got)
	}
	for j, key := range []string{"before-F", "requests-only", "no-bridge"} {
		if got := handBack(historical[j].ID, key); !reflect.DeepEqual(got, historical[j]) {
			t.Fatalf("historical replay mutated a B return: %+v", got)
		}
	}
	page, err := s.ListPendingTaskDecisions(ctx, ListOptions{})
	if err != nil || !reflect.DeepEqual(page.Items, []domain.DecisionRequest{ordinary}) {
		t.Fatalf("install/replay backfilled or promoted a historical origin: %+v %v", page, err)
	}
	if a, err := s.AnswerDecisionRequest(ctx, "user", old.ThreadRef, "pre-bridge-origin", domain.DecisionAnswerInput{Version: 1, Text: "继续。"}, "pre-bridge"); !errors.Is(err, ErrConflict) || a.ID != "" {
		t.Fatalf("rejected pre-bridge origin became answerable: %+v %v", a, err)
	}
	messageSequencePreserved(t, s, before)
	coordinationDecideExec(t, s, `UPDATE decision_requests SET withdrawn_at=created_at WHERE id=?`, ordinary.ID)
	legacy, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, decisionTaskInput(), "legacy-v1")
	if err != nil || legacy.Task == nil || *legacy.Task != *ordinary.Task || legacy.SourceMessageID != ordinary.SourceMessageID || legacy.SourceSequence != ordinary.SourceSequence {
		t.Fatalf("a historical non-Mandate origin occupied the legacy proposal slot: %+v %v", legacy, err)
	}

	// ACK of a changed constraint adds an impact, not a decision.
	ack, _ := impactInput(t, f, "注明摘要来源。", "注明摘要来源")
	if _, err := s.AcknowledgeWorkInputWithKey(ctx, owner.Member, ack.WorkInputID, "confirm"); err != nil {
		t.Fatal(err)
	}
	// Even identical text yields a distinct WorkInput/trigger on each source.
	var returned []domain.TaskWorkInput
	for _, key := range []string{"first-live", "second-live"} {
		r, _ := impactInput(t, f, "还需要考虑额外工作。", "")
		returned = append(returned, handBack(r.WorkInputID, key))
	}
	legacyInput := decisionTaskInput()
	legacyInput.ExpectedVersion, legacyInput.Question = legacy.Version, "是否只看最近一个周末？"
	amended, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, legacyInput, "legacy-v2")
	if err != nil || amended.ID != legacy.ID || amended.Task == nil || *amended.Task != *legacy.Task || amended.Version != 2 {
		t.Fatalf("ordinary amendment selected a returned slot: %+v %v", amended, err)
	}
	if _, err := s.ProposeTaskDecisionRequest(ctx, owner.Member, decisionTaskInput(), "legacy-v1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("superseded proposal replay survived: %v", err)
	}
	page, err = s.ListPendingTaskDecisions(ctx, ListOptions{})
	if err != nil || len(page.Items) != 3 || !reflect.DeepEqual(page.Items[2], amended) {
		t.Fatalf("legacy slot and two returns did not coexist: %+v %v", page, err)
	}
	for j, i := range returned {
		q := page.Items[1-j]
		if q.Task == nil || q.Task.WorkInputID != i.ID || q.Task.OriginTriggerID != i.TriggerID || q.Task.TaskID != p.TaskID ||
			q.Kind != legacy.Kind || q.SourceMessageID != i.ReplyMessageID || q.SourceSequence != i.SourceSequence || q.Version != 1 || q.Question != i.Return.Question {
			t.Fatalf("distinct input %s lost its slot: %+v (task %+v)", i.ID, q, q.Task)
		}
		for _, query := range []string{
			`INSERT OR REPLACE INTO decision_requests SELECT 'another-id',group_id,topic_id,recipient_id,kind,relay,
			 task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,origin_trigger_id,
			 chat_source_id,chat_assessment_id,source_message_id,source_sequence,created_at,withdrawn_at FROM decision_requests WHERE id=?`,
			`INSERT INTO decision_request_versions SELECT request_id,2,question_json,author_member_id,author_turn_id,author_attempt_id,created_at,expires_at
			 FROM decision_request_versions WHERE request_id=?`,
			`UPDATE decision_request_versions SET question_json=json_set(question_json,'$.question','改动') WHERE request_id=?`,
		} {
			if _, err := s.db.Exec(query, q.ID); err == nil {
				t.Fatalf("returned slot was replaceable/amendable: %s", query)
			}
		}
	}
	if _, err := s.AnswerDecisionRequest(ctx, "user", legacy.ThreadRef, legacy.ID, domain.DecisionAnswerInput{Version: 1, OptionIndex: 1}, "old-version"); !errors.Is(err, ErrConflict) {
		t.Fatalf("answer accepted an old generic version: %v", err)
	}

	// A partial bridge falls back to legacy F and B, not a half-actionable return.
	coordinationDecideExec(t, s, `DROP TRIGGER decision_answer_consumptions_insert_by_origin`)
	r, _ := impactInput(t, f, "再补充一条范围外要求。", "")
	partial := handBack(r.WorkInputID, "partial-bridge")
	page, err = s.ListPendingTaskDecisions(ctx, ListOptions{})
	if err != nil || !reflect.DeepEqual(page.Items, []domain.DecisionRequest{amended}) {
		t.Fatalf("partial bridge projected an unanswerable decision: %+v %v", page, err)
	}
	coordinationDecideExec(t, s, `CREATE TRIGGER decision_answer_consumptions_insert_by_origin`+decisionAnswerConsumptionGuard+workInputDecisionGuardAnswerScope+decisionAnswerConsumptionEnd)
	if replay := handBack(partial.ID, "partial-bridge"); !reflect.DeepEqual(replay, partial) {
		t.Fatalf("completed bridge changed partial-era receipt: %+v", replay)
	}
	if page, err = s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 3 {
		t.Fatalf("completed bridge backfilled an earlier return: %+v %v", page, err)
	}
	dispatchAssertFK(t, s)
}

func TestWorkInputReturnDecisionRollbackAndRevocation(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	withTaskImpactSchema(t)
	f := chatSetup(t, "user", false)
	s := f.s
	installDecisionAnswerFixture(t, s)
	installWorkInputDecisionFixture(t, s)
	owner := chatAuthorityWorker(t, f)
	r, _ := impactInput(t, f, "还需要写入目录。", "还需要写入目录")
	blocked, _ := impactInput(t, f, "还需要写入另一处目录。", "")
	const reason, key = "原授权不允许目录写入。", "rollback-return"
	before := coordinationDecideSnapshot(t, s)
	for _, fault := range []string{
		`BEFORE INSERT ON decision_requests BEGIN SELECT RAISE(IGNORE); END`,
		`BEFORE INSERT ON decision_request_versions BEGIN SELECT RAISE(IGNORE); END`,
		`BEFORE INSERT ON task_constraint_impacts BEGIN SELECT RAISE(IGNORE); END`,
		`AFTER INSERT ON task_constraint_impacts BEGIN UPDATE decision_requests SET withdrawn_at=created_at; END`,
		`AFTER INSERT ON decision_request_versions BEGIN UPDATE mutation_idempotency SET record_id='wrong' WHERE operation='hand_back_work_input'; END`,
	} {
		coordinationDecideExec(t, s, `CREATE TEMP TRIGGER return_decision_fault `+fault)
		var boundary *taskWorkTransactionError
		if i, err := s.HandBackWorkInputWithKey(ctx, owner.Member, r.WorkInputID, reason, key); i.ID != "" || !errors.Is(err, ErrConflict) ||
			!errors.As(err, &boundary) || !boundary.RollbackConfirmed() {
			t.Fatalf("%s did not roll back without a receipt: %+v %v", fault, i, err)
		}
		coordinationDecideExec(t, s, `DROP TRIGGER return_decision_fault`)
		messageSequencePreserved(t, s, before)
	}

	cause := errors.New("return commit acknowledgement lost")
	lost, _ := coordinationDecideWrapped(t, f.path, "ack-error", cause)
	var boundary *taskWorkTransactionError
	if i, err := lost.HandBackWorkInputWithKey(ctx, owner.Member, r.WorkInputID, reason, key); i.ID != "" ||
		!errors.Is(err, ErrTaskWorkMutationUnknown) || !errors.Is(err, cause) || !errors.As(err, &boundary) || !boundary.CommitOutcomeUnknown() {
		t.Fatalf("return with a lost commit acknowledgement was reported as determinate: %+v %v", i, err)
	}
	if err := lost.Close(); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListPendingTaskDecisions(ctx, ListOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Task == nil || page.Items[0].Task.WorkInputID != r.WorkInputID {
		t.Fatalf("the committed return was not present after the lost acknowledgement: %+v %v", page, err)
	}
	before = messageSequenceSnapshot(t, s)
	if i, err := s.HandBackWorkInputWithKey(ctx, owner.Member, r.WorkInputID, reason, key); err != nil || i.Return == nil || i.ID != r.WorkInputID {
		t.Fatalf("same-key retry did not reconcile the committed return: %+v %v", i, err)
	}
	messageSequencePreserved(t, s, before)
	q := page.Items[0]
	// Simulate damage to the required return proof after the user selected q.
	// An impact alone cannot keep the request visible or dispatch an answer.
	coordinationDecideExec(t, s, `DROP TRIGGER task_work_input_returns_no_delete;
	 CREATE TEMP TRIGGER lose_return_proof AFTER INSERT ON decision_answers BEGIN DELETE FROM task_work_input_returns; END`)
	before = messageSequenceSnapshot(t, s)
	response := domain.DecisionAnswerInput{Version: 1, Text: "继续。"}
	if a, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, response, "lost-proof"); !errors.Is(err, ErrConflict) || a.ID != "" {
		t.Fatalf("answer committed after losing its B proof: %+v %v", a, err)
	}
	messageSequencePreserved(t, s, before)
	coordinationDecideExec(t, s, `DROP TRIGGER lose_return_proof;
	 CREATE TRIGGER task_work_input_returns_no_delete BEFORE DELETE ON task_work_input_returns
	 BEGIN SELECT RAISE(ABORT,'工作输入交回不可删除'); END`)

	// The same live canonical owner can still return a different input after
	// chat authority is revoked; that B fact is not an answerable question.
	policy := f.policy.ChatPolicyRequest
	policy.Enabled = false
	if _, err := s.SetChatPolicy(ctx, q.ThreadRef, policy, f.policy.Revision, "revoke-authorization"); err != nil {
		t.Fatal(err)
	}
	if i, err := s.HandBackWorkInputWithKey(ctx, owner.Member, blocked.WorkInputID, reason, "revoked-return"); err != nil || i.Return == nil ||
		i.Return.Disposition != domain.TaskWorkInputReturnOriginalAuthorizationUnavailable || i.Return.NoticeMessageID == "" {
		t.Fatalf("revocation lost the valid B return: %+v %v", i, err)
	}
	if page, err := s.ListPendingTaskDecisions(ctx, ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("revoked original authority remained answerable: %+v %v", page, err)
	}
	before = messageSequenceSnapshot(t, s)
	if len(before["decision_requests"]) != 1 || len(before["decision_request_versions"]) != 1 || len(before["task_work_input_returns"]) != 2 || len(before["task_constraint_impacts"]) != 2 {
		t.Fatal("revoked return did not preserve only the B evidence and original F version")
	}
	if a, err := s.AnswerDecisionRequest(ctx, "user", q.ThreadRef, q.ID, domain.DecisionAnswerInput{Version: 1, Text: "继续。"}, "revoked"); !errors.Is(err, ErrConflict) || a.ID != "" {
		t.Fatalf("revoked return decision scheduled an answer: %+v %v", a, err)
	}
	messageSequencePreserved(t, s, before)
	dispatchAssertFK(t, s)
}
