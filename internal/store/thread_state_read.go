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
    coalesce(s.message_id,''),r.source_id,r.source_position,r.confirmed_at,
    coalesce(p.content,''),coalesce(ps.message_id,''),coalesce(p.source_id,''),coalesce(p.source_position,0),coalesce(p.confirmed_at,'')
 FROM slots JOIN thread_state_revisions r ON (r.group_id,r.topic_id,r.task_id,r.task_revision,r.revision)=
 (:grp,:topic,:task,:task_revision,(SELECT revision FROM thread_state_revisions INDEXED BY thread_state_by_slot
    WHERE ` + threadStateKey + ` AND slot=slots.slot AND revision<=:revision ORDER BY revision DESC LIMIT 1))
 LEFT JOIN thread_state_revisions p ON (p.group_id,p.topic_id,p.task_id,p.task_revision,p.revision)=
 (r.group_id,r.topic_id,r.task_id,r.task_revision,r.invalidates_revision) AND p.slot=r.slot AND p.kind=r.kind
    AND p.invalidates_revision IS NULL AND p.source_position<r.source_position
 LEFT JOIN message_log l ON (l.sequence,l.group_id,l.topic_id)=(r.source_position,r.group_id,r.topic_id)
 LEFT JOIN message_command_sources s ON s.id=r.source_id AND s.message_id=l.message_id AND s.kind='local_user_chat'
 LEFT JOIN message_log pl ON (pl.sequence,pl.group_id,pl.topic_id)=(p.source_position,p.group_id,p.topic_id)
 LEFT JOIN message_command_sources ps ON ps.id=p.source_id AND ps.message_id=pl.message_id AND ps.kind='local_user_chat'
 ORDER BY slots.slot`

func readThreadStateInTx(ctx context.Context, tx *sql.Tx, scope domain.ThreadStateScope, revision int64) (domain.ThreadStateSnapshot, error) {
	var empty domain.ThreadStateSnapshot
	args := []any{sql.Named("grp", scope.GroupID), sql.Named("topic", scope.TopicID), sql.Named("task", scope.TaskID), sql.Named("task_revision", scope.TaskRevision)}
	current := revision == 0
	if current {
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(revision),0) FROM thread_state_revisions WHERE `+threadStateKey, args...).Scan(&revision); err != nil {
			return empty, err
		}
	}
	out := domain.ThreadStateSnapshot{Scope: scope, Revision: revision, Records: []domain.ThreadStateRecord{}}
	if revision == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, threadStateSnapshotSQL, append(args, sql.Named("revision", revision))...)
	if err != nil {
		return empty, err
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var r domain.ThreadStateRecord
		var prior domain.ThreadStateSource
		var stamp, priorStamp, priorContent string
		var supersedes int64
		if err := rows.Scan(&r.Slot, &r.Kind, &r.Revision, &r.Content, &supersedes,
			&r.Source.MessageID, &r.Source.SourceID, &r.Source.MessageLogPosition, &stamp,
			&priorContent, &prior.MessageID, &prior.SourceID, &prior.MessageLogPosition, &priorStamp); err != nil {
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
		out.Records = append(out.Records, r)
		found = found || r.Revision == revision
	}
	if err := rows.Err(); err != nil {
		return empty, err
	}
	if !found {
		if current {
			return empty, errThreadStateIntegrity
		}
		return empty, sql.ErrNoRows
	}
	return out, nil
}

func confirmThreadStateSource(source *domain.ThreadStateSource, stamp string) bool {
	at, err := time.Parse(timestampLayout, stamp)
	if err != nil || !validTriggerID(source.MessageID) || !validTriggerID(source.SourceID) || source.MessageLogPosition <= 0 {
		return false
	}
	source.ConfirmedBy, source.SourceKind, source.ConfirmedAt = "user", "local_user_chat", at
	return true
}

// LoadThreadPreparation is a trusted peek after ordinary delivery binding.
// This path cannot request Task state. The immutable trigger, state and recent
// candidates are read in one transaction, with no inferred coverage bound.
func (s *Store) LoadThreadPreparation(ctx context.Context, thread domain.ThreadRef, triggerID string, limit int) ([]domain.Message, *domain.ThreadStateSnapshot, error) {
	var state *domain.ThreadStateSnapshot
	history, err := s.listPreparationHistory(ctx, thread.GroupID, triggerID, limit, func(tx *sql.Tx, trigger domain.Message) error {
		if trigger.GroupID != thread.GroupID || trigger.TopicID != thread.TopicID || trigger.ID != triggerID {
			return ErrNotFound
		}
		var err error
		state, err = preparationThreadStateInTx(ctx, tx, domain.ThreadStateScope{ThreadRef: thread}, triggerID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return history, state, nil
}

// LoadAuthorizedTaskThreadState is a preparation seam, not a member tool or
// permission to launch. Use only after the role profile authorizes this view;
// ready identity alone does not decide what a profile is allowed to render.
// Requiring the persisted ready owner/coordinator binding excludes ordinary
// chat and delegated/limited profiles. Claim/execution must still revalidate.
func (s *Store) LoadAuthorizedTaskThreadState(ctx context.Context, bound CoordinationRoleState) (*domain.ThreadStateSnapshot, error) {
	p := bound.Provenance
	if bound.Blocked != CoordinationReady || (p.Kind != CoordinationRoleCoordinator && p.Kind != CoordinationRoleWorker) || p.TaskID == "" {
		return nil, ErrMemberToolDenied
	}
	actual, err := s.ResolveCoordinationDelivery(ctx, p.GroupID, p.DeliveryID)
	if err != nil {
		return nil, err
	}
	if actual != bound {
		return nil, ErrMemberToolDenied
	}
	const action = "读取执行话题状态失败"
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, storeError(action, err)
	}
	defer tx.Rollback()
	state, err := preparationThreadStateInTx(ctx, tx, domain.ThreadStateScope{
		ThreadRef: domain.ThreadRef{GroupID: p.GroupID, TopicID: p.TopicID}, TaskID: p.TaskID, TaskRevision: p.TaskRevision,
	}, p.MessageID)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return nil, storeError(action, err)
	}
	return state, nil
}

func preparationThreadStateInTx(ctx context.Context, tx *sql.Tx, scope domain.ThreadStateScope, triggerID string) (*domain.ThreadStateSnapshot, error) {
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
	state, err := readThreadStateInTx(ctx, tx, scope, 0)
	if err != nil || state.Revision == 0 {
		return nil, err
	}
	return &state, nil
}
