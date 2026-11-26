package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

const decisionAnswerSourcePrefix = "decision-answer:"

var (
	ErrDecisionAnswersUnavailable = errors.New("当前数据库未安装决定回送记录")
	ErrDecisionRelayUnsupported   = errors.New("本话题澄清尚不支持从待决事项回送")
	ErrDecisionAnswerUnknown      = errors.New("保存决定的结果未确认；请用原决定内容重试或查询回执")
)

func decisionAnswerInstalledInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var installed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='decision_answers')`).Scan(&installed)
	return installed, err
}

func requireDecisionAnswersInTx(ctx context.Context, tx *sql.Tx) error {
	installed, err := decisionAnswerInstalledInTx(ctx, tx)
	if err == nil && !installed {
		return ErrDecisionAnswersUnavailable
	}
	return err
}

// AnswerDecisionRequest is a trusted local-user command shared by both routes.
// thread is an exact assertion from the selected item, never a route to search.
func (s *Store) AnswerDecisionRequest(ctx context.Context, actor string, thread domain.ThreadRef, id string, input domain.DecisionAnswerInput, key string) (_ domain.DecisionAnswerReceipt, err error) {
	const action = "保存待决事项答复失败"
	var zero domain.DecisionAnswerReceipt
	if actor != "user" {
		return zero, ErrMemberToolDenied
	}
	if !validTriggerID(id) || !validTriggerID(thread.GroupID) || input.Version <= 0 || (key != "" && !memberToolKeyValid(key)) ||
		input.OptionIndex < 0 || input.OptionIndex > 4 || (input.OptionIndex == 0 && !memberToolTextValid(input.Text, 4096)) ||
		(input.OptionIndex != 0 && input.Text != "") {
		return zero, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, decisionRequestError(action, err)
	}
	phase := "mutation"
	defer func() {
		rollback := tx.Rollback()
		if err != nil {
			failure := taskWorkMutationFailure(action, phase, err, rollback)
			if failure.RollbackConfirmed() {
				failure.message = decisionRequestError(action, err).Error()
			} else {
				failure.message = ErrDecisionAnswerUnknown.Error()
				failure.cause = errors.Join(ErrDecisionAnswerUnknown, failure.cause)
			}
			err = failure
		}
	}()
	if err := requireDecisionAnswersInTx(ctx, tx); err != nil {
		return zero, err
	}
	a, err := s.answerDecisionInTx(ctx, tx, actor, thread, id, input, key)
	if err != nil {
		return zero, err
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return a, nil
}

func (s *Store) answerDecisionInTx(ctx context.Context, tx *sql.Tx, actor string, thread domain.ThreadRef, id string, input domain.DecisionAnswerInput, key string) (domain.DecisionAnswerReceipt, error) {
	var zero domain.DecisionAnswerReceipt
	mutation := mutationScope{operation: "answer_decision_request", actorID: actor, groupID: thread.GroupID, key: key}
	// Keep the canonical request form, not the resolved label of an option.
	payload := mutationPayload(id, thread.TopicID, strconv.FormatInt(input.Version, 10), strconv.Itoa(input.OptionIndex), input.Text)
	record, replay, err := lookupMutation(ctx, tx, mutation, payload)
	if err != nil {
		return zero, err
	}
	if replay {
		// Read the original receipt even after the Task or Thread is no longer
		// current. Reconciliation does not authorize or requeue work.
		a, err := historicalDecisionAnswerReceiptInTx(ctx, tx, thread, id, input.Version)
		if errors.Is(err, sql.ErrNoRows) {
			return zero, ErrConflict
		}
		if err != nil {
			return zero, err
		}
		if a.ID != record || a.ActorID != actor || a.OptionIndex != input.OptionIndex || (input.OptionIndex == 0 && a.Text != input.Text) {
			return zero, ErrConflict
		}
		return a, nil
	}
	columns, where, _, err := currentDecisionSQLInTx(ctx, tx)
	if err != nil {
		return zero, err
	}
	query := columns + where
	q, err := decisionRequestVersionInTx(ctx, tx, query, thread, id, input.Version, time.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return zero, ErrConflict
	}
	if err != nil {
		return zero, err
	}
	if q.RecipientID != actor {
		return zero, ErrMemberToolDenied
	}
	text := input.Text
	if input.OptionIndex != 0 {
		if input.OptionIndex > len(q.Choices) {
			return zero, ErrInvalid
		}
		text = q.Choices[input.OptionIndex-1]
	}
	load := decisionAnswerReceiptInTx
	table := "decision_answers"
	var save func() (domain.DecisionAnswerReceipt, error)
	switch q.Relay {
	case domain.DecisionRelayOriginThread:
		asker, err := s.decisionThreadAskerInTx(ctx, tx, q)
		if err != nil {
			return zero, err
		}
		load, table = decisionThreadAnswerReceiptInTx, "decision_thread_answers"
		save = func() (domain.DecisionAnswerReceipt, error) {
			return saveDecisionThreadAnswerInTx(ctx, tx, q, asker, actor, text, input.OptionIndex)
		}
	case domain.DecisionRelayTaskOwner:
		if q.Task == nil {
			return zero, ErrDecisionRelayUnsupported
		}
		facts, err := decisionTaskFactsInTx(ctx, tx, q.Task.AssignmentID)
		if err != nil {
			return zero, err
		}
		if err := decisionTaskAuthorityInTx(ctx, tx, q, facts); err != nil {
			return zero, err
		}
		save = func() (domain.DecisionAnswerReceipt, error) {
			return saveDecisionTaskAnswerInTx(ctx, tx, q, facts.v, actor, text, input.OptionIndex)
		}
	default:
		return zero, ErrDecisionRelayUnsupported
	}
	a, err := load(ctx, tx, thread, id, input.Version)
	fresh := errors.Is(err, sql.ErrNoRows)
	if fresh {
		var alreadySaved bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE request_id=?)`, q.ID).Scan(&alreadySaved); err != nil {
			return zero, err
		}
		if alreadySaved {
			return zero, ErrConflict // Broken proof is not an absent answer.
		}
		a, err = save()
	}
	if err != nil {
		return zero, err
	}
	if a.ActorID != actor || a.OptionIndex != input.OptionIndex || a.Text != text {
		return zero, ErrConflict
	}
	if key != "" {
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
		 VALUES(?,?,?,?,?,?)`, mutation.operation, mutation.actorID, mutation.groupID, key, payload, a.ID); err != nil {
			return zero, err
		}
	}
	// Last-write readback: require the same still-current version and the actual
	// source, typed marker and scope, including any effect of database triggers.
	if _, err := decisionRequestVersionInTx(ctx, tx, query, thread, id, q.Version, time.Now()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrConflict
		}
		return zero, err
	}
	if q.Relay == domain.DecisionRelayOriginThread {
		if err := verifyDecisionThreadAnswerInTx(ctx, tx, a, fresh); err != nil {
			return zero, err
		}
	} else {
		f, err := taskWorkDeliveryInTx(ctx, tx, q.GroupID, a.WakeDeliveryID)
		if err != nil {
			return zero, coordinationRoleRelationError(err)
		}
		if !f.decisionAnswer || f.d.SourceID != a.ID || f.d.MessageID != a.WakeMessageID || (fresh && f.turnID != "") {
			return zero, ErrConflict
		}
		if err := decisionTaskAuthorityInTx(ctx, tx, q, f); err != nil {
			return zero, err
		}
	}
	if key != "" {
		id, present, err := lookupMutation(ctx, tx, mutation, payload)
		if err != nil {
			return zero, err
		}
		if !present || id != a.ID {
			return zero, ErrConflict
		}
	}
	return a, nil
}

func decisionTaskFactsInTx(ctx context.Context, tx *sql.Tx, assignmentID string) (taskWorkFacts, error) {
	v, dispatch, err := coordinationAssignmentLoad(ctx, tx, assignmentID)
	if err != nil {
		return taskWorkFacts{}, coordinationRoleRelationError(err)
	}
	return taskWorkFacts{v: v, dispatch: dispatch, d: taskWorkRecord(v, v.r.CoordinatorMemberID, "")}, nil
}

func decisionTaskAuthorityInTx(ctx context.Context, tx *sql.Tx, q domain.DecisionRequest, f taskWorkFacts) error {
	b, d := q.Task, f.d
	if d.GroupID != q.GroupID || d.TopicID != q.TopicID || d.TaskID != b.TaskID || d.TaskRevision != b.TaskRevision ||
		d.AssignmentID != b.AssignmentID || d.MandateID != b.MandateID || d.AuthorityGeneration != b.AuthorityGeneration ||
		d.OwnerMemberID != b.OwnerMemberID || f.dispatch.Status != domain.AssignmentDispatchStatusEnqueued || f.dispatch.Delivery == nil {
		return ErrConflict
	}
	blocked, _, err := taskWorkCurrentInTx(ctx, tx, f)
	if err != nil {
		return err
	}
	if blocked != "" {
		return ErrConflict
	}
	return decisionTaskChatAuthorityInTx(ctx, tx, f)
}

func decisionTaskChatAuthorityInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts) error {
	basis, err := taskWorkOwnerChatBasisInTx(ctx, tx, f)
	if err == nil && basis.action == domain.ChatActionReadOnlyInvestigation {
		// Saving a queued wake does not enable a process or provider. Durable
		// policy is checked here; B/C check execution readiness on every start.
		err = checkTaskInvestigationAuthorizationInTx(ctx, tx, basis.investigation, "", false)
	}
	if errors.Is(err, errTaskWorkChatScope) || errors.Is(err, ErrTaskInvestigationNotReady) {
		err = ErrConflict
	}
	return err
}

func saveDecisionTaskAnswerInTx(ctx context.Context, tx *sql.Tx, q domain.DecisionRequest, v assignmentSnapshot, actor, text string, index int) (domain.DecisionAnswerReceipt, error) {
	var zero domain.DecisionAnswerReceipt
	m, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: q.GroupID, TopicID: q.TopicID, ActorID: actor, Content: text, RecipientIDs: []string{}}, nil)
	if err != nil {
		return zero, err
	}
	id := decisionAnswerSourcePrefix + rand.Text()
	// v30 requires a real same-group member FK for the *platform envelope*.
	// Use the exact Assignment's coordinator as its transport identity, not
	// as answer author or evidence of a member call/Turn. The author is user.
	want := taskWorkRecord(v, v.r.CoordinatorMemberID, id)
	_, err = taskWorkCreateWakeInTx(ctx, tx, want, func(wake domain.TaskWorkDelivery) error {
		return coordinationDispatchWrite(ctx, tx, `INSERT INTO decision_answers(id,request_id,version,actor_id,text,option_index,reply_message_id,wake_delivery_id,created_at)
		 VALUES(?,?,?,?,?,nullif(?,0),?,?,?)`, id, q.ID, q.Version, actor, text, index, m.ID, wake.DeliveryID, m.CreatedAt.Format(timestampLayout))
	})
	if err != nil {
		return zero, err
	}
	return decisionAnswerReceiptInTx(ctx, tx, q.ThreadRef, q.ID, q.Version)
}

// This is immutable provenance, not current-scope or handling evidence. The
// physical marker is verified independently by the existing B delivery loader.
const decisionAnswerEvidence = ` FROM decision_answers ans JOIN decision_requests q ON q.id=ans.request_id
 JOIN groups g ON g.id=q.group_id
 JOIN decision_request_versions v ON (v.request_id,v.version)=(q.id,ans.version)
 JOIN messages reply ON (reply.id,reply.group_id,reply.topic_id,reply.sender_id,reply.content)=
    (ans.reply_message_id,q.group_id,q.topic_id,ans.actor_id,ans.text)
 JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(reply.id,q.group_id,q.topic_id) AND l.sequence>q.source_sequence
 JOIN task_work_deliveries work ON (work.delivery_id,work.source_kind,work.source_id)=(ans.wake_delivery_id,'followup',ans.id)
 AND (work.group_id,work.topic_id,work.task_id,work.task_revision,work.assignment_id,work.mandate_id,
      work.authority_generation,work.owner_member_id,work.member_id)=(q.group_id,q.topic_id,q.task_id,q.task_revision,
      q.assignment_id,q.mandate_id,q.authority_generation,q.owner_member_id,q.owner_member_id)
 JOIN coordination_assignments ca ON ca.id=work.assignment_id
 JOIN coordination_dispatch_requests r ON r.id=ca.request_id AND r.coordinator_member_id=work.sender_member_id`

const decisionAnswerProof = `q.recipient_id=ans.actor_id AND q.relay='task_owner' AND (ans.option_index IS NULL
 OR ans.text IS json_extract(v.question_json,'$.choices['||(ans.option_index-1)||']'))
 AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=reply.id)
 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=reply.id)`

func decisionAnswerReceiptInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, id string, version int64) (out domain.DecisionAnswerReceipt, err error) {
	var stamp string
	err = tx.QueryRowContext(ctx, `SELECT ans.id,q.id,q.group_id,q.topic_id,ans.version,ans.actor_id,q.task_id,q.task_revision,
	 coalesce(ans.option_index,0),ans.text,reply.id,work.delivery_id,work.message_id,ans.created_at`+decisionAnswerEvidence+
		` WHERE (q.group_id,q.topic_id,q.id,ans.version)=(?,?,?,?) AND `+decisionAnswerProof,
		thread.GroupID, thread.TopicID, id, version).Scan(&out.ID, &out.RequestID, &out.GroupID, &out.TopicID, &out.Version,
		&out.ActorID, &out.TaskID, &out.TaskRevision, &out.OptionIndex, &out.Text, &out.ReplyMessageID, &out.WakeDeliveryID, &out.WakeMessageID, &stamp)
	if err == nil {
		out.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func historicalDecisionAnswerReceiptInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, id string, version int64) (domain.DecisionAnswerReceipt, error) {
	receipt, err := decisionAnswerReceiptInTx(ctx, tx, thread, id, version)
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	installed, installErr := decisionThreadAnswerInstalled(ctx, tx)
	if installErr != nil {
		return domain.DecisionAnswerReceipt{}, installErr
	}
	if !installed {
		return receipt, err
	}
	return decisionThreadAnswerReceiptInTx(ctx, tx, thread, id, version)
}

// GetDecisionAnswer reads the saved fact, even after the work/version is no
// longer runnable. It does not requeue the wake or report owner handling.
func (s *Store) GetDecisionAnswer(ctx context.Context, thread domain.ThreadRef, id string, version int64) (domain.DecisionAnswerReceipt, error) {
	view, err := s.readDecisionAnswer(ctx, thread, id, version, false)
	return view.Receipt, err
}

// GetDecisionAnswerView reads the saved receipt and its explicit handling in
// one snapshot, without checking current execution authority or waking work.
func (s *Store) GetDecisionAnswerView(ctx context.Context, thread domain.ThreadRef, id string, version int64) (domain.DecisionAnswerView, error) {
	return s.readDecisionAnswer(ctx, thread, id, version, true)
}

func (s *Store) readDecisionAnswer(ctx context.Context, thread domain.ThreadRef, id string, version int64, includeHandling bool) (domain.DecisionAnswerView, error) {
	const action = "读取用户决定回执失败"
	var zero domain.DecisionAnswerView
	if !validTriggerID(id) || !validTriggerID(thread.GroupID) || version <= 0 {
		return zero, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return zero, decisionRequestError(action, err)
	}
	defer tx.Rollback()
	if err := requireDecisionAnswersInTx(ctx, tx); err != nil {
		return zero, decisionRequestError(action, err)
	}
	receipt, err := historicalDecisionAnswerReceiptInTx(ctx, tx, thread, id, version)
	if err != nil {
		return zero, decisionRequestError(action, err)
	}
	view := domain.DecisionAnswerView{Receipt: receipt}
	if includeHandling {
		if receipt.TaskID != "" {
			view.Handling, err = decisionAnswerHandlingInTx(ctx, tx, receipt)
		} else {
			view.Handling, err = decisionThreadAnswerHandlingInTx(ctx, tx, receipt)
		}
		if err != nil {
			return zero, decisionRequestError(action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return zero, decisionRequestError(action, err)
	}
	return view, nil
}

func decisionAnswerHandlingInTx(ctx context.Context, tx *sql.Tx, receipt domain.DecisionAnswerReceipt) (*domain.DecisionAnswerHandling, error) {
	// Check the durable continuation identity, not current execution authority.
	var handling domain.DecisionAnswerHandling
	var stamp string
	var bound bool
	err := tx.QueryRowContext(ctx, `SELECT c.turn_id,c.attempt_id,c.consumed_at, `+decisionAnswerHandlingMatch+decisionAnswerEvidence+
		decisionAnswerReplacementJoin+` JOIN decision_answer_consumptions c ON c.answer_id=ans.id
	 WHERE (ans.id,work.delivery_id)=(?,?)`, receipt.ID, receipt.WakeDeliveryID).
		Scan(&handling.TurnID, &handling.AttemptID, &stamp, &bound)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !bound {
		return nil, ErrNotFound
	}
	if handling.ConsumedAt, err = time.Parse(timestampLayout, stamp); err != nil {
		return nil, err
	}
	return &handling, nil
}

func isDecisionAnswerID(id string) bool { return strings.HasPrefix(id, decisionAnswerSourcePrefix) }
