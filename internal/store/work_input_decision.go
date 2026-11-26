package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"reflect"
	"strings"

	"agentworkspace/internal/domain"
)

// The bridge is optional until the public order is assigned. Require the
// named SQL capability, the complete answer path, and B's impact marker.
// Incomplete installations must not obstruct a B return.
func workInputDecisionInstalledInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var installed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.sqlite_schema WHERE type='view' AND name='`+workInputDecisionSources+`')
	 AND EXISTS(SELECT 1 FROM main.sqlite_schema WHERE type='index' AND name='decision_requests_task_origin_slot')
	 AND 7=(SELECT count(*) FROM main.sqlite_schema WHERE type='table' AND name IN
	     ('decision_requests','decision_request_versions','decision_answers','decision_answer_consumptions',
	      'decision_answer_wake_replacements','task_constraint_impacts','decision_work_input_boundary'))
	 AND 16=(SELECT count(*) FROM main.sqlite_schema WHERE type='trigger' AND name IN
	     ('decision_work_input_boundary_no_insert','decision_work_input_boundary_no_update','decision_work_input_boundary_no_delete',
	      'decision_requests_insert_by_origin','decision_work_input_versions_insert','decision_answers_insert_by_origin',
	      'decision_answer_consumptions_insert_by_origin','decision_answer_replacements_insert_by_origin',
	      'decision_requests_identity','decision_requests_withdraw','decision_requests_no_delete',
	      'decision_request_versions_insert','decision_request_versions_no_update','decision_request_versions_no_delete',
	      'decision_answer_versions_closed','task_work_delivery_source_insert')
	     AND (name<>'task_work_delivery_source_insert' OR instr(sql,'FROM decision_answers ans')>0))`).Scan(&installed)
	if err != nil || !installed {
		return false, err
	}
	return taskImpactSchemaInTx(ctx, tx)
}

func workInputReturnQuestion(i domain.TaskWorkInput) domain.DecisionQuestion {
	scope := i.Content
	// Keep the F display field bounded; the exact user message is linked as
	// origin and remains the full source, even when this excerpt is shortened.
	if len(scope) > 1024 {
		scope = strings.ToValidUTF8(scope[:1021], "") + "…"
	}
	return domain.DecisionQuestion{
		DecisionContext: domain.DecisionContext{Facts: i.Return.Reason, Recommendation: "保持原任务授权不变；额外要求需另行明确授权。"},
		Question:        i.Return.Question,
		Authorization:   &domain.DecisionAuthorization{Scope: scope, Impact: "仅记录用户答复并回送原任务负责人；不会创建任务、执行额外要求或扩大授权。"},
	}
}

// Call only for fresh handling, after the genuine B return and before its
// impact seals enrollment. Neither a replay nor an impact reader calls this.
func captureWorkInputDecisionInTx(ctx context.Context, tx *sql.Tx, i domain.TaskWorkInput, r domain.DispatchRequest) (string, error) {
	h := i.Return
	if h == nil || h.Disposition != domain.TaskWorkInputReturnSeparateAuthorizationRequired {
		return "", nil
	}
	installed, err := workInputDecisionInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return "", err
	}
	id, stamp := workInputDecisionPrefix+rand.Text(), h.ReturnedAt.Format(timestampLayout)
	body, err := decisionQuestionJSON(workInputReturnQuestion(i), domain.DecisionRequestAuthorization)
	if err != nil {
		return "", err
	}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO decision_requests(id,group_id,topic_id,recipient_id,kind,relay,
	 task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,origin_trigger_id,source_message_id,source_sequence,created_at)
	 VALUES(?,?,?,'user','authorization','task_owner',?,?,?,?,?,?,?,?,?,?)`, id, i.GroupID, i.TopicID, i.TaskID, i.TaskRevision,
		i.AssignmentID, i.MandateID, r.AuthorityGeneration, i.WorkerMemberID, i.TriggerID, i.ReplyMessageID, i.SourceSequence, stamp); err != nil {
		return "", err
	}
	return id, appendDecisionVersionInTx(ctx, tx, id, 1, body, h.MemberID, h.TurnID, h.AttemptID, stamp, "")
}

func verifyWorkInputDecisionInTx(ctx context.Context, tx *sql.Tx, i domain.TaskWorkInput, r domain.DispatchRequest, id string) error {
	h := i.Return
	q, err := pendingDecisionRequestInTx(ctx, tx, i.ThreadRef, id, 1, h.ReturnedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	b := q.Task
	if b == nil || b.WorkInputID != i.ID || b.TaskID != i.TaskID || b.TaskRevision != i.TaskRevision || b.AssignmentID != i.AssignmentID ||
		b.MandateID != i.MandateID || b.AuthorityGeneration != r.AuthorityGeneration || b.OwnerMemberID != i.WorkerMemberID ||
		b.OriginTriggerID != i.TriggerID || q.SourceMessageID != i.ReplyMessageID || q.SourceSequence != i.SourceSequence ||
		q.AuthorMemberID != h.MemberID || q.AuthorTurnID != h.TurnID || q.AuthorAttemptID != h.AttemptID ||
		!reflect.DeepEqual(q.DecisionQuestion, workInputReturnQuestion(i)) {
		return ErrConflict
	}
	return nil
}
