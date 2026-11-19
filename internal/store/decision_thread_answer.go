package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"agentworkspace/internal/domain"
)

const threadDecisionAnswerPrefix = "thread-decision-answer:"
const decisionThreadUnanswered = ` AND NOT EXISTS(SELECT 1 FROM decision_thread_answers answer WHERE answer.request_id=q.id)`

type threadDecisionSchemaQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func decisionThreadAnswerInstalled(ctx context.Context, q threadDecisionSchemaQueryer) (bool, error) {
	var installed bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.sqlite_schema WHERE type='table' AND name='decision_thread_answers')`).Scan(&installed)
	return installed, err
}

// EnableThreadDecisionExecution is a startup assertion by trusted composition,
// only after the typed consumer is ready on this Store. A reopened Store starts
// disabled. The assertion neither installs SQL nor grants member tools.
func (s *Store) EnableThreadDecisionExecution() { s.threadDecisionExecution.Store(true) }

func (s *Store) threadDecisionExecutionReady(ctx context.Context, q threadDecisionSchemaQueryer) (bool, error) {
	if !s.threadDecisionExecution.Load() {
		return false, nil
	}
	var installed bool
	err := q.QueryRowContext(ctx, `SELECT count(*)=2 FROM main.sqlite_schema WHERE type='table'
	 AND name IN ('decision_thread_answers','decision_thread_answer_consumptions')`).Scan(&installed)
	return installed, err
}

func (s *Store) answerableDecisionSQL(ctx context.Context, tx *sql.Tx, taskOnly string) (string, error) {
	ready, err := s.threadDecisionExecutionReady(ctx, tx)
	if err != nil || !ready {
		return taskOnly, err
	}
	return decisionRequestCurrent + decisionRequestUnanswered + decisionThreadUnanswered + ` AND (q.task_id IS NOT NULL
	 OR EXISTS(SELECT 1 FROM decision_thread_answerable r WHERE (r.request_id,r.version)=(q.id,v.version)))`, nil
}

type decisionThreadAsker struct{ memberID, conversationID string }

func (s *Store) decisionThreadAskerInTx(ctx context.Context, tx *sql.Tx, q domain.DecisionRequest) (asker decisionThreadAsker, err error) {
	if q.Kind != domain.DecisionRequestClarification || q.Relay != domain.DecisionRelayOriginThread || q.Chat == nil || q.Task != nil {
		return asker, ErrDecisionRelayUnsupported
	}
	ready, err := s.threadDecisionExecutionReady(ctx, tx)
	if err != nil {
		return asker, err
	}
	if !ready {
		return asker, ErrDecisionRelayUnsupported
	}
	err = tx.QueryRowContext(ctx, `SELECT asker_id,conversation_id FROM decision_thread_answerable
	 WHERE (request_id,version,group_id,topic_id)=(?,?,?,?) AND (expires_at IS NULL OR expires_at>?)`,
		q.ID, q.Version, q.GroupID, q.TopicID, time.Now().UTC().Format(timestampLayout)).Scan(&asker.memberID, &asker.conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrConflict
	}
	return asker, err
}

func saveDecisionThreadAnswerInTx(ctx context.Context, tx *sql.Tx, q domain.DecisionRequest, asker decisionThreadAsker, actor, text string, index int) (domain.DecisionAnswerReceipt, error) {
	var zero domain.DecisionAnswerReceipt
	// The exact conversation already exists. Publish one normal message and
	// delivery, without issuing a local_user_chat command or routing defaults.
	m := domain.Message{ID: threadDecisionAnswerPrefix + rand.Text(), GroupID: q.GroupID, TopicID: q.TopicID,
		SenderID: actor, Content: text, CreatedAt: time.Now().UTC()}
	if q.ExpiresAt != nil && !m.CreatedAt.Before(*q.ExpiresAt) {
		return zero, ErrConflict
	}
	if err := insertMessageRow(ctx, tx, m); err != nil {
		return zero, err
	}
	deliveryID, stamp := threadDecisionAnswerPrefix+rand.Text(), m.CreatedAt.Format(timestampLayout)
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO deliveries(id,message_id,group_id,topic_id,conversation_id,recipient_id,created_at)
	 VALUES(?,?,?,?,?,?,?)`, deliveryID, m.ID, q.GroupID, q.TopicID, asker.conversationID, asker.memberID, stamp); err != nil {
		return zero, err
	}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO decision_thread_answers(id,request_id,version,option_index,reply_message_id,delivery_id,created_at)
	 VALUES(?,?,?,nullif(?,0),?,?,?)`, threadDecisionAnswerPrefix+rand.Text(), q.ID, q.Version, index, m.ID, deliveryID, stamp); err != nil {
		return zero, err
	}
	return decisionThreadAnswerReceiptInTx(ctx, tx, q.ThreadRef, q.ID, q.Version)
}

// Historical reads deliberately do not join the current route, member, policy,
// delivery state or native outcome. The text and exact Thread are immutable.
func decisionThreadAnswerReceiptInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, id string, version int64) (out domain.DecisionAnswerReceipt, err error) {
	var stamp string
	err = tx.QueryRowContext(ctx, `SELECT ans.id,q.id,q.group_id,q.topic_id,ans.version,m.sender_id,
	 coalesce(ans.option_index,0),m.content,m.id,ans.delivery_id,ans.created_at
	 FROM decision_thread_answers ans JOIN decision_requests q ON q.id=ans.request_id
	 JOIN groups g ON g.id=q.group_id
	 JOIN messages m ON (m.id,m.group_id,m.topic_id,m.sender_id)=(ans.reply_message_id,q.group_id,q.topic_id,q.recipient_id)
	 WHERE (q.group_id,q.topic_id,q.id,ans.version)=(?,?,?,?) AND q.relay='origin_thread'`,
		thread.GroupID, thread.TopicID, id, version).Scan(&out.ID, &out.RequestID, &out.GroupID, &out.TopicID, &out.Version,
		&out.ActorID, &out.OptionIndex, &out.Text, &out.ReplyMessageID, &out.WakeDeliveryID, &stamp)
	if err == nil {
		out.WakeMessageID = out.ReplyMessageID
		out.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func verifyDecisionThreadAnswerInTx(ctx context.Context, tx *sql.Tx, a domain.DecisionAnswerReceipt, fresh bool) error {
	wake, state, turnID, err := decisionThreadWakeInTx(ctx, tx, a.GroupID, a.WakeDeliveryID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (wake.Receipt != a || fresh && (state != domain.DeliveryStateQueued || turnID != "")) {
		return ErrConflict
	}
	return err
}
