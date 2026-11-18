package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"agentworkspace/internal/domain"
)

func workMemberTx[T any](ctx context.Context, s *Store, scope MemberToolScope, action string, run func(*sql.Tx) (T, error)) (zero T, err error) {
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	if err := requireTaskWorkReportsInTx(ctx, tx); err != nil {
		return zero, memberToolOperationError(action, coordinationRoleRelationError(err))
	}
	out, err := run(tx)
	if err != nil {
		if errors.Is(err, ErrTaskInvestigationNotReady) {
			return zero, ErrMemberToolDenied
		}
		return zero, memberToolOperationError(action, err)
	}
	if err := coordinationDecideLiveInTx(ctx, tx, scope); err != nil {
		return zero, memberToolOperationError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return zero, memberToolOperationError("任务工作事务结果未确认；重试时请沿用原幂等键", err)
	}
	return out, nil
}

// Look up the receipt before current-eligibility checks. apply must check the
// business record on replay and may write it only for a new mutation.
func taskWorkMutationInTx(ctx context.Context, tx *sql.Tx, scope mutationScope, payload []byte, apply func(id string, replay bool) error) (string, error) {
	id, replay, err := reserveMutation(ctx, tx, scope, payload)
	if err != nil {
		return "", err
	}
	if err := apply(id, replay); err != nil {
		return "", err
	}
	if actual, replay, err := lookupMutation(ctx, tx, scope, payload); err != nil || !replay || actual != id {
		if err != nil {
			return "", err
		}
		return "", ErrConflict
	}
	return id, nil
}

func taskWorkDelegateInputValid(in domain.DelegateWorkInput) bool {
	if !validTriggerID(in.MemberID) || !memberToolTextValid(in.Question, domain.MaxWorkQuestionBytes) || len(in.ArtifactVersions) > investigationMaxArtifactRefs {
		return false
	}
	for i, ref := range in.ArtifactVersions {
		if len(ref.ArtifactID) > investigationMaxIDBytes || len(ref.VersionID) > investigationMaxIDBytes ||
			!validTriggerID(ref.ArtifactID) || !validTriggerID(ref.VersionID) || slices.Contains(in.ArtifactVersions[:i], ref) {
			return false
		}
	}
	return true
}

// DelegateWork derives the original Assignment, author attempt, and task from
// the live scope. It writes a bounded logical question and one native wake.
func (s *Store) DelegateWork(ctx context.Context, scope MemberToolScope, input domain.DelegateWorkInput, key string) (domain.TaskWorkDelegation, error) {
	if !memberToolKeyValid(key) || !taskWorkDelegateInputValid(input) {
		return domain.TaskWorkDelegation{}, ErrInvalid
	}
	refs, err := json.Marshal(append([]domain.ArtifactVersionRef{}, input.ArtifactVersions...))
	if err != nil {
		return domain.TaskWorkDelegation{}, ErrInvalid
	}
	return workMemberTx(ctx, s, scope, "委派任务内问题失败", func(tx *sql.Tx) (out domain.TaskWorkDelegation, err error) {
		f, attempt, err := taskWorkOwnerInTx(ctx, tx, scope)
		if err != nil {
			return out, err
		}
		p := taskWorkParent(f)
		mutation := mutationScope{operation: "delegate_work", groupID: p.GroupID, actorID: p.OwnerMemberID, key: key}
		payload := mutationPayload(p.AssignmentID, f.turnID, attempt, input.MemberID, input.Question, strconv.FormatBool(input.AllowTopicMessages), string(refs))
		_, err = taskWorkMutationInTx(ctx, tx, mutation, payload, func(id string, replay bool) error {
			var budget, ordinal int
			var created time.Time
			if !replay {
				budget, ordinal, err = s.taskWorkDelegationSlotInTx(ctx, tx, f, p, attempt, input)
				if err != nil {
					return err
				}
				created = time.Now().UTC()
			}
			wake, err := taskWorkB3WakeInTx(ctx, tx, scope, f.v, domain.TaskWorkSourceDelegation, id, input.MemberID, replay,
				func(wake domain.TaskWorkDelivery) error {
					return coordinationDispatchWrite(ctx, tx, `INSERT INTO task_work_delegations
	 (id,group_id,topic_id,task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,
	 author_turn_id,author_attempt_id,member_id,question,allow_topic_messages,artifact_versions,max_delegations,ordinal,delivery_id,created_at)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, p.GroupID, p.TopicID, p.TaskID, p.TaskRevision, p.AssignmentID, p.MandateID, p.AuthorityGeneration,
						p.OwnerMemberID, scope.Claim.TurnID, attempt, input.MemberID, input.Question, input.AllowTopicMessages,
						string(refs), budget, ordinal, wake.DeliveryID, created.Format(timestampLayout))
				})
			if err != nil {
				return err
			}
			saved := wake.delegation
			if saved == nil || saved.TaskWorkParent != p || saved.AuthorAttemptID != attempt || saved.AuthorDeliveryID != f.d.DeliveryID || saved.AuthorMessageID != f.d.MessageID ||
				saved.Question != input.Question || saved.AllowTopicMessages != input.AllowTopicMessages || !slices.Equal(saved.ArtifactVersions, input.ArtifactVersions) ||
				(!replay && (saved.MaxDelegations != budget || saved.Ordinal != ordinal || !saved.CreatedAt.Equal(created))) {
				return ErrConflict
			}
			out = *saved
			return nil
		})
		return out, err
	})
}

func (s *Store) taskWorkDelegationSlotInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts, p domain.TaskWorkParent, attempt string, in domain.DelegateWorkInput) (int, int, error) {
	if in.MemberID == p.OwnerMemberID || in.MemberID == f.v.r.CoordinatorMemberID {
		return 0, 0, ErrMemberToolDenied
	}
	investigation, err := s.taskWorkOwnerReadyInTx(ctx, tx, f, attempt)
	if err != nil {
		return 0, 0, err
	}
	if !taskWorkSubset(investigation.TaskInvestigationScope, in) {
		return 0, 0, ErrMemberToolDenied
	}
	coActive, eligible, err := coordinationDispatchMembersInTx(ctx, tx, p.GroupID, f.v.r.CoordinatorMemberID, f.v.r.PolicyRevision, in.MemberID, "")
	if err != nil {
		return 0, 0, err
	}
	if !coActive || !eligible {
		return 0, 0, ErrMemberToolDenied
	}
	if err := chatInvestigationVersionsInTx(ctx, tx, p.GroupID, in.ArtifactVersions); err != nil {
		return 0, 0, coordinationRoleRelationError(err)
	}
	var used int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM task_work_delegations WHERE group_id=? AND task_id=?`, p.GroupID, p.TaskID).Scan(&used); err != nil {
		return 0, 0, err
	}
	if used >= investigation.MaxDelegations {
		return 0, 0, ErrConflict
	}
	return investigation.MaxDelegations, used + 1, nil
}

func taskWorkReportKindValid(kind domain.TaskWorkReportKind) bool {
	switch kind {
	case domain.TaskWorkReportProgress, domain.TaskWorkReportQuestion, domain.TaskWorkReportBlocked, domain.TaskWorkReportDelivered:
		return true
	default:
		return false
	}
}

// ReportWork commits the report separately from the wake but in the same tx.
// There is no model-supplied assignment, target, task, or turn fallback.
func (s *Store) ReportWork(ctx context.Context, scope MemberToolScope, input domain.ReportWorkInput, key string) (domain.TaskWorkReport, error) {
	if !memberToolKeyValid(key) || !taskWorkReportKindValid(input.Kind) || !memberToolTextValid(input.Summary, domain.MaxWorkReportBytes) || len(input.AnswerTo) > 4096 {
		return domain.TaskWorkReport{}, ErrInvalid
	}
	return taskWorkMutationTx(ctx, s, scope, "记录协作报告失败", func(tx *sql.Tx) (out domain.TaskWorkReport, err error) {
		if err := requireTaskWorkReportsInTx(ctx, tx); err != nil {
			return out, coordinationRoleRelationError(err)
		}
		f, err := taskWorkExecutionInTx(ctx, tx, scope)
		if err != nil {
			return out, err
		}
		if f.delegation == nil {
			return out, ErrMemberToolDenied
		}
		d := f.delegation
		mutation := mutationScope{operation: "report_work", actorID: d.MemberID, groupID: d.GroupID, key: key}
		payload := mutationPayload(d.ID, f.turnID, string(input.Kind), input.Summary, input.AnswerTo)
		_, err = taskWorkMutationInTx(ctx, tx, mutation, payload, func(id string, replay bool) error {
			out, err = s.recordTaskWorkReportInTx(ctx, tx, scope, f, input, id, replay)
			return err
		})
		if err != nil {
			return out, err
		}
		return out, coordinationDecideLiveInTx(ctx, tx, scope)
	})
}

func (s *Store) recordTaskWorkReportInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, f taskWorkFacts, in domain.ReportWorkInput, id string, replay bool) (out domain.TaskWorkReport, err error) {
	// Do not accept a raw input ID. B1 integration must prove the exact
	// immutable work input and its explicit grant to this child (see docs).
	if in.AnswerTo != "" {
		return out, fmt.Errorf("%w：当前不能关联未验证的任务输入", ErrInvalid)
	}
	d := f.delegation
	var ordinal int
	var created time.Time
	if !replay {
		ordinal, err = s.taskWorkReportSlotInTx(ctx, tx, f, in.Kind)
		if err != nil {
			return out, err
		}
		created = time.Now().UTC()
	}
	persist := func(wake domain.TaskWorkDelivery) error {
		return coordinationDispatchWrite(ctx, tx, `INSERT INTO task_work_reports
	 (id,delegation_id,group_id,task_id,worker_member_id,source_turn_id,kind,summary,ordinal,delivery_id,created_at)
	 VALUES(?,?,?,?,?,?,?,?,?,nullif(?,''),?)`, id, d.ID, d.GroupID, d.TaskID, scope.MemberID,
			scope.Claim.TurnID, in.Kind, in.Summary, ordinal, wake.DeliveryID, created.Format(timestampLayout))
	}
	if in.Kind == domain.TaskWorkReportProgress {
		if !replay {
			if err := persist(domain.TaskWorkDelivery{}); err != nil {
				return out, err
			}
		}
		out, err = taskWorkReportInTx(ctx, tx, d.GroupID, id, &d.TaskWorkParent)
	} else {
		var saved taskWorkFacts
		saved, err = taskWorkB3WakeInTx(ctx, tx, scope, f.v, domain.TaskWorkSourceReport, id, d.OwnerMemberID, replay, persist)
		if err == nil && saved.report != nil {
			out = *saved.report
		}
	}
	if err != nil {
		return domain.TaskWorkReport{}, coordinationRoleRelationError(err)
	}
	if out.TaskWorkParent != d.TaskWorkParent || out.DelegationID != d.ID || out.ActorID != scope.MemberID || out.SourceTurnID != scope.Claim.TurnID ||
		out.SourceMessageID != d.MessageID || out.SourceDeliveryID != d.DeliveryID || out.ReportWorkInput != in ||
		(!replay && !out.CreatedAt.Equal(created)) {
		return domain.TaskWorkReport{}, ErrConflict
	}
	return out, nil
}

func (s *Store) taskWorkReportSlotInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts, kind domain.TaskWorkReportKind) (int, error) {
	blocked, _, err := s.taskWorkReadinessInTx(ctx, tx, f, f.turnID)
	if err != nil {
		return 0, err
	}
	if blocked != "" {
		return 0, ErrConflict
	}
	var used int
	var delivered bool
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(max(kind='delivered'),0) FROM task_work_reports WHERE delegation_id=?`, f.delegation.ID).Scan(&used, &delivered); err != nil {
		return 0, err
	}
	if delivered || used >= domain.MaxWorkReportsPerDelegation || (used == domain.MaxWorkReportsPerDelegation-1 && kind == domain.TaskWorkReportProgress) {
		return 0, ErrConflict
	}
	return used + 1, nil
}
