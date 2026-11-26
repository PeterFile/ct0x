package store

import (
	"context"
	"database/sql"
	"time"

	"agentworkspace/internal/domain"
)

const threadStateKey = `(group_id,topic_id,task_id,task_revision)=(:grp,:topic,:task,:task_revision)`

// Each slot gets one index seek, even if a Thread has years of revisions. Keep
// left joins on evidence so missing evidence cannot silently erase a record.
const threadStateSnapshotSQL = `WITH slots(slot) AS (VALUES(0),(1),(2),(3),(4),(5),(6),(7))
 SELECT r.slot,r.kind,r.revision,r.content,coalesce(r.invalidates_revision,0),
    coalesce(m.id,''),r.source_id,r.source_position,r.confirmed_at,r.confirmed_at_correction_tip,
    coalesce(p.content,''),coalesce(pm.id,''),coalesce(p.source_id,''),coalesce(p.source_position,0),coalesce(p.confirmed_at,''),coalesce(p.confirmed_at_correction_tip,0)
 FROM slots JOIN thread_state_revisions r ON (r.group_id,r.topic_id,r.task_id,r.task_revision,r.revision)=
 (:grp,:topic,:task,:task_revision,(SELECT revision FROM thread_state_revisions INDEXED BY thread_state_by_slot
    WHERE ` + threadStateKey + ` AND slot=slots.slot AND revision<=:revision ORDER BY revision DESC LIMIT 1))
 LEFT JOIN thread_state_revisions p ON (p.group_id,p.topic_id,p.task_id,p.task_revision,p.revision)=
 (r.group_id,r.topic_id,r.task_id,r.task_revision,r.invalidates_revision) AND p.slot=r.slot AND p.kind=r.kind
    AND p.invalidates_revision IS NULL AND p.source_position<r.source_position
 LEFT JOIN message_log l ON (l.sequence,l.group_id,l.topic_id)=(r.source_position,r.group_id,r.topic_id)
 LEFT JOIN message_command_sources s ON s.id=r.source_id AND s.message_id=l.message_id AND s.kind='local_user_chat'
    AND s.turn_id IS NULL AND s.claim_digest IS NULL
 LEFT JOIN messages m ON (m.id,m.group_id,m.topic_id)=(s.message_id,r.group_id,r.topic_id)
    AND m.sender_id='user' AND m.content=r.content
 LEFT JOIN message_log pl ON (pl.sequence,pl.group_id,pl.topic_id)=(p.source_position,p.group_id,p.topic_id)
 LEFT JOIN message_command_sources ps ON ps.id=p.source_id AND ps.message_id=pl.message_id AND ps.kind='local_user_chat'
    AND ps.turn_id IS NULL AND ps.claim_digest IS NULL
 LEFT JOIN messages pm ON (pm.id,pm.group_id,pm.topic_id)=(ps.message_id,p.group_id,p.topic_id)
    AND pm.sender_id='user' AND pm.content=p.content
 ORDER BY slots.slot`

const threadStateCoverageSQL = `SELECT r.covered_through_message_id, r.covered_through_position,
 EXISTS(SELECT 1 FROM message_log l JOIN messages m ON (m.id,m.group_id,m.topic_id)=(l.message_id,l.group_id,l.topic_id)
    WHERE (l.sequence,l.message_id,l.group_id,l.topic_id)=(r.covered_through_position,r.covered_through_message_id,r.group_id,r.topic_id)),
 coalesce((SELECT covered_through_position FROM thread_state_revisions WHERE ` + threadStateKey + ` AND revision=:revision-1),0)
 FROM thread_state_revisions r WHERE ` + threadStateKey + ` AND revision=:revision`

func readThreadStateInTx(ctx context.Context, tx *sql.Tx, scope domain.ThreadStateScope, revision int64) (domain.ThreadStateSnapshot, error) {
	return readThreadStateSnapshotInTx(ctx, tx, scope, revision, noCorrectionLedger)
}

// A nonnegative correction tip selects ordinary presentation, not a different
// state timeline. First validate the immutable latest record of each slot and
// its evidence; an ineligible retirement must not fall back to an active plan.
func readThreadStateSnapshotInTx(ctx context.Context, tx *sql.Tx, scope domain.ThreadStateScope, revision, correctionTip int64) (domain.ThreadStateSnapshot, error) {
	var empty domain.ThreadStateSnapshot
	args := []any{sql.Named("grp", scope.GroupID), sql.Named("topic", scope.TopicID), sql.Named("task", scope.TaskID), sql.Named("task_revision", scope.TaskRevision)}
	// The head independently records the committed tip, so a surviving
	// contiguous prefix cannot look current (or empty). Count detects gaps.
	var head sql.NullInt64
	var latest, count int64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT revision FROM thread_state_heads WHERE `+threadStateKey+`),
	 coalesce(max(revision),0), count(*) FROM thread_state_revisions WHERE `+threadStateKey, args...).Scan(&head, &latest, &count); err != nil {
		return empty, err
	}
	if latest != count || head.Valid != (latest > 0) || head.Int64 != latest {
		return empty, errThreadStateIntegrity
	}
	if revision > latest {
		return empty, sql.ErrNoRows
	}
	if revision == 0 {
		revision = latest
	}
	out := domain.ThreadStateSnapshot{Scope: scope, Revision: revision, Records: []domain.ThreadStateRecord{}}
	if revision == 0 {
		return out, nil
	}
	args = append(args, sql.Named("revision", revision))
	var err error
	out.CoveredThrough, err = readThreadStateCoverageInTx(ctx, tx, args)
	if err != nil {
		return empty, err
	}
	rows, err := tx.QueryContext(ctx, threadStateSnapshotSQL, args...)
	if err != nil {
		return empty, err
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var r domain.ThreadStateRecord
		var prior domain.ThreadStateSource
		var stamp, priorStamp, priorContent string
		var supersedes, sourceTip, priorTip int64
		if err := rows.Scan(&r.Slot, &r.Kind, &r.Revision, &r.Content, &supersedes,
			&r.Source.MessageID, &r.Source.SourceID, &r.Source.MessageLogPosition, &stamp, &sourceTip,
			&priorContent, &prior.MessageID, &prior.SourceID, &prior.MessageLogPosition, &priorStamp, &priorTip); err != nil {
			return empty, err
		}
		if !validThreadStateKind(r.Kind) || !validThreadStateContent(r.Content) || !confirmThreadStateSource(&r.Source, stamp) {
			return empty, errThreadStateIntegrity
		}
		r.Active = supersedes == 0
		if !r.Active {
			if !validThreadStateContent(priorContent) || !confirmThreadStateSource(&prior, priorStamp) {
				return empty, errThreadStateIntegrity
			}
			r.InvalidatedBy = &domain.ThreadStateInvalidation{Reason: r.Content, Source: r.Source}
			r.Content, r.Source = priorContent, prior
		}
		found = found || r.Revision == revision
		if correctionTip >= 0 {
			if sourceTip > correctionTip || priorTip > correctionTip {
				return empty, errThreadStateIntegrity
			}
			proof := r.Source
			if r.InvalidatedBy != nil {
				proof = r.InvalidatedBy.Source
			}
			current, err := ordinaryEvidenceCurrentInTx(ctx, tx, scope.ThreadRef, proof.MessageID, sourceTip)
			if err != nil {
				return empty, err
			}
			if current && r.InvalidatedBy != nil {
				current, err = ordinaryEvidenceCurrentInTx(ctx, tx, scope.ThreadRef, r.Source.MessageID, priorTip)
				if err != nil {
					return empty, err
				}
			}
			if !current {
				// A partial handoff cannot rely on the prior history cutoff.
				out.CoveredThrough = nil
				continue
			}
		}
		out.Records = append(out.Records, r)
	}
	if err := rows.Err(); err != nil {
		return empty, err
	}
	if err := rows.Close(); err != nil {
		return empty, err
	}
	if !found {
		return empty, errThreadStateIntegrity
	}
	if out.CoveredThrough != nil && correctionTip >= 0 {
		current, err := ordinaryCoverageCurrentInTx(ctx, tx, scope.ThreadRef, out.CoveredThrough, correctionTip)
		if err != nil {
			return empty, err
		}
		if !current {
			out.CoveredThrough = nil
		}
	}
	return out, nil
}

func readThreadStateCoverageInTx(ctx context.Context, tx *sql.Tx, args []any) (*domain.ThreadStateCoverage, error) {
	var messageID sql.NullString
	var position sql.NullInt64
	var verified bool
	var previous int64
	if err := tx.QueryRowContext(ctx, threadStateCoverageSQL, args...).Scan(&messageID, &position, &verified, &previous); err != nil {
		return nil, err
	}
	if messageID.Valid != position.Valid || position.Int64 < previous {
		return nil, errThreadStateIntegrity
	}
	if !position.Valid {
		return nil, nil
	}
	if !verified || position.Int64 <= 0 || !validTriggerID(messageID.String) {
		return nil, errThreadStateIntegrity
	}
	return &domain.ThreadStateCoverage{MessageID: messageID.String, MessageLogPosition: position.Int64}, nil
}

func confirmThreadStateSource(source *domain.ThreadStateSource, stamp string) bool {
	at, err := time.Parse(timestampLayout, stamp)
	if err != nil || !validTriggerID(source.MessageID) || !validTriggerID(source.SourceID) || source.MessageLogPosition <= 0 {
		return false
	}
	source.ConfirmedBy, source.SourceKind, source.ConfirmedAt = "user", "local_user_chat", at
	return true
}

type ThreadPreparation struct {
	History []domain.Message
	State   *domain.ThreadStateSnapshot
	Claim   *PreparedThreadState
}

// LoadThreadPreparation is a trusted peek after ordinary delivery binding,
// never a Task state selector. History, evidence, coverage and claim stamp
// share one read snapshot. Corrections do not rebind the trigger's delivery.
func (s *Store) LoadThreadPreparation(ctx context.Context, thread domain.ThreadRef, triggerID string, limit int) (ThreadPreparation, error) {
	limit, err := listLimit(ListOptions{Limit: limit})
	if err != nil {
		return ThreadPreparation{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ThreadPreparation{}, queueError(preparationHistoryError, err)
	}
	defer tx.Rollback()
	out, err := loadThreadPreparationInTx(ctx, tx, thread, triggerID, limit)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return ThreadPreparation{}, queueError(preparationHistoryError, err)
	}
	return out, nil
}

func loadThreadPreparationInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, triggerID string, limit int) (ThreadPreparation, error) {
	var out ThreadPreparation
	bound, before, err := preparationTriggerInTx(ctx, tx, thread.GroupID, triggerID)
	if err != nil {
		return out, err
	}
	if bound != thread {
		return out, ErrNotFound
	}
	tip, err := correctionTipInTx(ctx, tx)
	if err != nil {
		return out, err
	}
	state, err := preparationThreadStateInTx(ctx, tx, domain.ThreadStateScope{ThreadRef: thread}, triggerID, tip)
	if err != nil {
		return out, err
	}
	var rows *sql.Rows
	if state != nil && state.CoveredThrough != nil {
		rows, err = coveredPreparationHistoryInTx(ctx, tx, thread, triggerID, state.CoveredThrough.MessageLogPosition, tip >= 0, limit)
	} else {
		rows, err = queryScopedThreadMessagesInTx(ctx, tx, thread.GroupID, thread.TopicID, effectiveHistory, tip >= 0, &before, "", limit)
	}
	if err != nil {
		return out, err
	}
	out.History, err = scanPreparationHistoryInTx(ctx, tx, rows, thread.TopicID, effectiveHistory, limit)
	if err != nil {
		return out, err
	}
	out.State = state
	if state != nil || tip >= 0 {
		out.Claim = &PreparedThreadState{ThreadRef: thread, MessageID: triggerID, stateInstalled: state != nil, correctionTip: tip}
		if state != nil {
			out.Claim.Revision = state.Revision
		}
	}
	return out, nil
}

// LoadAuthorizedTaskThreadState is a preparation seam, not a member tool or
// permission to launch. Use only after the role profile authorizes this view;
// ready identity alone does not decide what a profile is allowed to render.
// Requiring the persisted ready owner/coordinator binding excludes ordinary
// chat and non-owner delegates. Claim/execution must still revalidate.
func (s *Store) LoadAuthorizedTaskThreadState(ctx context.Context, bound CoordinationRoleState) (*domain.ThreadStateSnapshot, error) {
	state, err := s.loadAuthorizedTaskThreadState(ctx, bound, false)
	if state != nil && state.Revision == 0 {
		return nil, nil
	}
	return state, err
}

// TaskThreadPreparationStaleError reports a verified readiness change for the
// exact bound formal role, not failed identity or provenance validation.
type TaskThreadPreparationStaleError struct{ Readiness CoordinationReadinessCode }

func (*TaskThreadPreparationStaleError) Error() string {
	return "准备期间正式任务角色已变更"
}

// LoadAuthorizedTaskThreadPreparation is the process-local counterpart for
// initial formal role profiles that already permit this exact Task view. The
// same authorized read issues the claim stamp, even when E2 is unregistered or
// its state is empty, so formal claims still fence role, readiness, and return
// markers before inserting a Turn.
func (s *Store) LoadAuthorizedTaskThreadPreparation(ctx context.Context, bound CoordinationRoleState) (*domain.ThreadStateSnapshot, *PreparedThreadState, error) {
	role := bound.Provenance
	if role.SourceKind != "" || role.SourceID != "" || bound.AttemptID != "" {
		return nil, nil, ErrMemberToolDenied
	}
	state, err := s.loadAuthorizedTaskThreadState(ctx, bound, true)
	if err != nil {
		return nil, nil, err
	}
	boundCopy := bound
	stamp := &PreparedThreadState{
		ThreadRef: domain.ThreadRef{GroupID: role.GroupID, TopicID: role.TopicID},
		MessageID: role.MessageID,
		role:      &boundCopy,
	}
	if state != nil {
		stamp.ThreadRef = state.Scope.ThreadRef
		stamp.Revision = state.Revision
		stamp.stateInstalled = true
		if state.Revision == 0 {
			state = nil // Fence an empty scope without changing the packet.
		}
	}
	return state, stamp, nil
}

func (s *Store) loadAuthorizedTaskThreadState(ctx context.Context, bound CoordinationRoleState, preparing bool) (*domain.ThreadStateSnapshot, error) {
	p := bound.Provenance
	if bound.Blocked != CoordinationReady || (p.Kind != CoordinationRoleCoordinator && p.Kind != CoordinationRoleWorker) || p.TaskID == "" {
		return nil, ErrMemberToolDenied
	}
	if !validTriggerID(p.GroupID) || !validTriggerID(p.DeliveryID) {
		return nil, coordinationAssignmentFinish(nil, "input", ErrInvalid)
	}
	const action = "读取执行话题状态失败"
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, coordinationAssignmentFinish(nil, "begin", err)
	}
	defer tx.Rollback()
	// Classification, current readiness and exact Task state must all refer
	// to this snapshot, not to separate resolver and state transactions.
	actual, err := s.resolveQueuedCoordinationInTx(ctx, tx, p.GroupID, p.DeliveryID, "", nil)
	if err != nil {
		return nil, coordinationAssignmentFinish(tx, "source", err)
	}
	if actual != bound {
		if !preparing {
			return nil, ErrMemberToolDenied
		}
		blocked, err := preparedTaskRoleReadiness(ctx, tx, bound, actual)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, coordinationAssignmentFinish(tx, "commit", err)
		}
		return nil, &TaskThreadPreparationStaleError{Readiness: blocked}
	}
	state, err := preparationThreadStateInTx(ctx, tx, domain.ThreadStateScope{
		ThreadRef: domain.ThreadRef{GroupID: p.GroupID, TopicID: p.TopicID}, TaskID: p.TaskID, TaskRevision: p.TaskRevision,
	}, p.MessageID, noCorrectionLedger)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return nil, storeError(action, err)
	}
	return state, nil
}

func preparationThreadStateInTx(ctx context.Context, tx *sql.Tx, scope domain.ThreadStateScope, triggerID string, correctionTip int64) (*domain.ThreadStateSnapshot, error) {
	installed, err := threadStateInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return nil, err
	}
	if !validThreadStateScope(scope) || !validTriggerID(triggerID) {
		return nil, ErrInvalid
	}
	var found bool
	// The anchor proves scope, not that prior messages were seen or consumed.
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM message_log WHERE message_id=? AND group_id=? AND topic_id=?`,
		triggerID, scope.GroupID, scope.TopicID).Scan(&found); err != nil {
		return nil, err
	}
	if err := verifyThreadStateTaskInTx(ctx, tx, scope, false); err != nil {
		return nil, err
	}
	state, err := readThreadStateSnapshotInTx(ctx, tx, scope, 0, correctionTip)
	if err != nil {
		return nil, err
	}
	return &state, nil
}
