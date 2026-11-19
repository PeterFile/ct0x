package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

func decisionQuestionJSON(q domain.DecisionQuestion, kind domain.DecisionRequestKind) (string, error) {
	if !memberToolTextValid(q.Facts, 2048) || !memberToolTextValid(q.Recommendation, 1024) ||
		!memberToolTextValid(q.Question, 2048) || strings.ContainsAny(q.Question, "\r\n") || len(q.Choices) > 4 {
		return "", ErrInvalid
	}
	seen := make(map[string]bool, len(q.Choices))
	for _, label := range q.Choices {
		if !memberToolTextValid(label, 128) || strings.ContainsAny(label, "\r\n") || seen[strings.TrimSpace(label)] {
			return "", ErrInvalid
		}
		seen[strings.TrimSpace(label)] = true
	}
	if (kind == domain.DecisionRequestAuthorization) != (q.Authorization != nil) {
		return "", ErrInvalid
	}
	if a := q.Authorization; a != nil && (!memberToolTextValid(a.Scope, 1024) || !memberToolTextValid(a.Impact, 1024)) {
		return "", ErrInvalid
	}
	b, err := json.Marshal(q)
	return string(b), err
}

// ProposeTaskDecisionRequest is a trusted Engine-facing command; scope is not
// an input DTO. It records a question, never a grant, delivery or Task action.
func (s *Store) ProposeTaskDecisionRequest(ctx context.Context, scope MemberToolScope, input domain.TaskDecisionRequestInput, key string) (domain.DecisionRequest, error) {
	const action = "记录任务待决事项失败"
	var zero domain.DecisionRequest
	switch input.Kind {
	case domain.DecisionRequestDecision, domain.DecisionRequestAuthorization, domain.DecisionRequestUnblock:
	default:
		return zero, ErrInvalid
	}
	if input.ExpectedVersion < 0 || !memberToolKeyValid(key) {
		return zero, ErrInvalid
	}
	body, err := decisionQuestionJSON(input.DecisionQuestion, input.Kind)
	if err != nil {
		return zero, decisionRequestError(action, err)
	}
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return zero, decisionRequestError(action, err)
	}
	defer tx.Rollback()
	if err := requireDecisionRequestsInTx(ctx, tx); err != nil {
		return zero, decisionRequestError(action, err)
	}
	role, owner, err := s.workInputHandlingOwnerInTx(ctx, tx, scope, false)
	if err != nil {
		return zero, decisionRequestError(action, err)
	}
	out, err := taskDecisionRequestInTx(ctx, tx, role, owner, input, body, key)
	if err != nil {
		return zero, decisionRequestError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return zero, decisionRequestError("保存待决事项的结果未确认", err)
	}
	return out, nil
}

const taskDecisionSlotWhere = `q.task_id IS NOT NULL AND
 (q.group_id,q.topic_id,q.task_id,q.task_revision,q.assignment_id,q.mandate_id,q.authority_generation,q.owner_member_id,q.kind)=(?,?,?,?,?,?,?,?,?)`

func taskDecisionRequestInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, owner assignmentSnapshot, input domain.TaskDecisionRequestInput, body, key string) (domain.DecisionRequest, error) {
	var zero domain.DecisionRequest
	r, a := owner.r, owner.a
	thread := domain.ThreadRef{GroupID: r.GroupID, TopicID: r.TopicID}
	now := time.Now().UTC()
	stamp := now.Format(timestampLayout)
	var expires string
	if input.ExpiresAt != nil {
		at := input.ExpiresAt.UTC()
		if at.Year() > 9999 || !at.After(now) {
			return zero, ErrInvalid
		}
		expires = at.Format(timestampLayout)
	}
	mutation := mutationScope{operation: "task_decision_request", groupID: r.GroupID, actorID: a.WorkerMemberID, key: key}
	payload := mutationPayload("task_decision_request/v1", r.TopicID, r.TaskID, strconv.FormatInt(r.TaskRevision, 10), a.ID,
		r.MandateID, strconv.FormatInt(r.AuthorityGeneration, 10), string(input.Kind), strconv.FormatInt(input.ExpectedVersion, 10), body, expires)
	record, replay, err := lookupMutation(ctx, tx, mutation, payload)
	if err != nil {
		return zero, err
	}
	var id, savedBody, savedExpiry string
	var version int64
	var withdrawn bool
	err = tx.QueryRowContext(ctx, `SELECT q.id,v.version,v.question_json,coalesce(v.expires_at,''),q.withdrawn_at IS NOT NULL
	 FROM decision_requests q JOIN decision_request_versions v ON v.request_id=q.id
	 AND v.version=(SELECT max(version) FROM decision_request_versions WHERE request_id=q.id)
	 WHERE `+taskDecisionSlotWhere, r.GroupID, r.TopicID, r.TaskID, r.TaskRevision, a.ID, r.MandateID, r.AuthorityGeneration,
		a.WorkerMemberID, input.Kind).Scan(&id, &version, &savedBody, &savedExpiry, &withdrawn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	if withdrawn || (savedExpiry != "" && savedExpiry <= stamp) {
		return zero, ErrConflict
	}
	if id != "" {
		installed, err := decisionAnswerInstalledInTx(ctx, tx)
		if err != nil {
			return zero, err
		}
		if installed {
			var answered bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM decision_answers WHERE request_id=?)`, id).Scan(&answered); err != nil {
				return zero, err
			}
			if answered {
				return zero, ErrConflict
			}
		}
	}
	if replay {
		// An old transport replay cannot claim to be the current question.
		if id == "" || record != decisionVersionRecord(id, version) {
			return zero, ErrConflict
		}
	} else {
		changed := id == "" || savedBody != body || savedExpiry != expires
		if (input.ExpectedVersion != 0 && input.ExpectedVersion != version) ||
			(changed && (input.ExpectedVersion != version || version == math.MaxInt64)) {
			return zero, ErrConflict
		}
		if id == "" {
			id = rand.Text()
			// A Task question points to the original immutable Mandate source,
			// not the message that happens to have woken this owner Turn.
			err = coordinationDispatchWrite(ctx, tx, `INSERT INTO decision_requests(id,group_id,topic_id,recipient_id,kind,relay,
			 task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,origin_trigger_id,
			 source_message_id,source_sequence,created_at)
			 SELECT ?,m.group_id,m.topic_id,m.authorized_by,?,'task_owner',m.task_id,?,?,?,?,?,o.id,o.message_id,o.message_sequence,?
			 FROM mandates m JOIN coordination_triggers o ON o.id=m.origin_trigger_id
			 WHERE (m.id,m.group_id,m.topic_id,m.task_id,m.task_revision_at_admission)=(?,?,?,?,?)`,
				id, input.Kind, r.TaskRevision, a.ID, r.MandateID, r.AuthorityGeneration, a.WorkerMemberID, stamp,
				r.MandateID, r.GroupID, r.TopicID, r.TaskID, r.TaskRevision)
			if err != nil {
				return zero, err
			}
		}
		if changed {
			version++
			if err := appendDecisionVersionInTx(ctx, tx, id, version, body, a.WorkerMemberID, role.Member.Claim.TurnID, role.AttemptID, stamp, expires); err != nil {
				return zero, err
			}
		}
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
		 VALUES(?,?,?,?,?,?)`, mutation.operation, mutation.actorID, mutation.groupID, mutation.key, payload, decisionVersionRecord(id, version)); err != nil {
			return zero, err
		}
	}
	out, err := pendingDecisionRequestInTx(ctx, tx, thread, id, version, now)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, ErrConflict
	}
	return out, err
}

func decisionVersionRecord(id string, version int64) string {
	return id + ":" + strconv.FormatInt(version, 10)
}

func appendDecisionVersionInTx(ctx context.Context, tx *sql.Tx, id string, version int64, body, author, turn, attempt, created, expires string) error {
	return coordinationDispatchWrite(ctx, tx, `INSERT INTO decision_request_versions(request_id,version,question_json,author_member_id,
	 author_turn_id,author_attempt_id,created_at,expires_at) VALUES(?,?,?,?,?,nullif(?,''),?,nullif(?,''))`,
		id, version, body, author, turn, attempt, created, expires)
}
