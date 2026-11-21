package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

const taskWorkParentColumns = `p.group_id,p.topic_id,p.task_id,p.task_revision,p.assignment_id,p.mandate_id,p.authority_generation,
 p.owner_member_id,origin.delivery_id,origin.message_id`

func taskWorkParentDest(p *domain.TaskWorkParent) []any {
	return []any{&p.GroupID, &p.TopicID, &p.TaskID, &p.TaskRevision, &p.AssignmentID, &p.MandateID,
		&p.AuthorityGeneration, &p.OwnerMemberID, &p.OriginalDeliveryID, &p.OriginalMessageID}
}

// Use the same immutable author/Attempt relation as the insert guard.
var taskWorkSavedOwnerOrigin = strings.ReplaceAll(taskWorkOwnerOrigin, "NEW.", "p.")

// A report author can lead back only to an earlier delegation of the same
// Assignment. Check this by key before reloading the author's full source.
const taskWorkBackwardAuthor = `(ad.id=origin.delivery_id OR EXISTS(
 SELECT 1 FROM task_work_deliveries aw WHERE aw.delivery_id=ad.id
 AND (aw.source_kind='followup' OR (aw.source_kind='report' AND EXISTS(
 SELECT 1 FROM task_work_reports wr JOIN task_work_delegations earlier ON earlier.id=wr.delegation_id
 WHERE (wr.id,wr.delivery_id)=(aw.source_id,aw.delivery_id)
 AND (earlier.group_id,earlier.topic_id,earlier.task_id,earlier.task_revision,earlier.assignment_id,earlier.mandate_id,earlier.authority_generation,earlier.owner_member_id)=
     (p.group_id,p.topic_id,p.task_id,p.task_revision,p.assignment_id,p.mandate_id,p.authority_generation,p.owner_member_id)
 AND earlier.ordinal BETWEEN 1 AND p.ordinal-1)))))`

func taskWorkDelegationInTx(ctx context.Context, tx *sql.Tx, groupID, id string) (d domain.TaskWorkDelegation, err error) {
	var refs, stamp string
	err = tx.QueryRowContext(ctx, `SELECT `+taskWorkParentColumns+`,p.id,p.member_id,p.question,p.allow_topic_messages,p.artifact_versions,
 p.author_turn_id,p.author_attempt_id,ad.id,ad.message_id,p.max_delegations,p.ordinal,w.delivery_id,w.message_id,w.conversation_id,p.created_at
 FROM task_work_delegations p JOIN coordination_assignment_dispatch origin ON origin.assignment_id=p.assignment_id
 JOIN turns author ON (author.id,author.group_id,author.member_id)=(p.author_turn_id,p.group_id,p.owner_member_id)
 JOIN deliveries ad ON (ad.id,ad.turn_id,ad.conversation_id)=(author.delivery_id,author.id,author.conversation_id)
 JOIN task_work_deliveries w ON (w.delivery_id,w.source_kind,w.source_id)=(p.delivery_id,'delegation',p.id)
 WHERE p.group_id=? AND p.id=? AND EXISTS(`+taskWorkSavedOwnerOrigin+`) AND `+taskWorkBackwardAuthor, groupID, id).Scan(append(taskWorkParentDest(&d.TaskWorkParent), &d.ID,
		&d.MemberID, &d.Question, &d.AllowTopicMessages, &refs, &d.AuthorTurnID, &d.AuthorAttemptID, &d.AuthorDeliveryID, &d.AuthorMessageID,
		&d.MaxDelegations, &d.Ordinal, &d.DeliveryID, &d.MessageID, &d.ConversationID, &stamp)...)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal([]byte(refs), &d.ArtifactVersions); err != nil {
		return d, err
	}
	if !taskWorkDelegateInputValid(d.DelegateWorkInput) || d.Ordinal < 1 || d.Ordinal > investigationMaxDelegations {
		return d, ErrMemberToolDenied
	}
	d.CreatedAt, err = time.Parse(timestampLayout, stamp)
	return
}

// Native settlement publishes the exact nonblank candidate without a command
// or a delivery. A reply pointer alone is not evidence of publication.
const taskWorkReportSelect = `SELECT ` + taskWorkParentColumns + `,r.id,r.delegation_id,r.worker_member_id,
 r.source_turn_id,child.delivery_id,child.message_id,t.reply_message_id<>'',coalesce(reply.id,''),coalesce(reply.content,''),r.kind,r.summary,
 coalesce(r.delivery_id,''),coalesce(w.message_id,''),r.created_at` + taskWorkReportChildRelations + `
 JOIN task_work_reports r ON (r.delegation_id,r.group_id,r.task_id,r.worker_member_id,r.source_turn_id)=(p.id,p.group_id,p.task_id,p.member_id,t.id)
 JOIN coordination_assignment_dispatch origin ON origin.assignment_id=p.assignment_id
 LEFT JOIN messages reply ON (reply.id,reply.group_id,reply.topic_id,reply.sender_id)=(t.reply_message_id,p.group_id,p.topic_id,p.member_id)
 AND t.accepted_at IS NOT NULL AND reply.content=t.candidate_text AND length(CAST(reply.content AS BLOB)) BETWEEN 1 AND 65536
 AND EXISTS(SELECT 1 FROM turns h JOIN conversations c ON (c.id,c.group_id,c.topic_id,c.member_id)=(h.conversation_id,p.group_id,p.topic_id,p.member_id)
 JOIN message_log incoming ON (incoming.message_id,incoming.group_id,incoming.topic_id)=(child.message_id,p.group_id,p.topic_id)
 JOIN message_log published ON (published.message_id,published.group_id,published.topic_id)=(reply.id,p.group_id,p.topic_id)
 WHERE h.id=t.id AND published.sequence>incoming.sequence AND ` + taskWorkTurnSettled + `)
 AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=reply.id)
 AND NOT EXISTS(SELECT 1 FROM task_work_deliveries WHERE message_id=reply.id)
 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=reply.id)
 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.reply_message_id=reply.id AND other.reply_message_id<>'' AND other.id<>t.id)
 LEFT JOIN task_work_deliveries w ON (w.delivery_id,w.source_kind,w.source_id)=(r.delivery_id,'report',r.id)
 AND (w.group_id,w.topic_id,w.task_id,w.task_revision,w.assignment_id,w.mandate_id,w.authority_generation,w.owner_member_id,w.member_id,w.sender_member_id)=
 (p.group_id,p.topic_id,p.task_id,p.task_revision,p.assignment_id,p.mandate_id,p.authority_generation,p.owner_member_id,p.owner_member_id,p.member_id)`

func scanTaskWorkReport(row scanner) (r domain.TaskWorkReport, hasReply bool, err error) {
	var stamp, replyText string
	err = row.Scan(append(taskWorkParentDest(&r.TaskWorkParent), &r.ID, &r.DelegationID, &r.ActorID,
		&r.SourceTurnID, &r.SourceDeliveryID, &r.SourceMessageID, &hasReply, &r.SourceReplyMessageID, &replyText,
		&r.Kind, &r.Summary, &r.DeliveryID, &r.MessageID, &stamp)...)
	if err != nil {
		return
	}
	wake := r.Kind != domain.TaskWorkReportProgress
	if !taskWorkReportKindValid(r.Kind) || wake != (r.DeliveryID != "") || wake != (r.MessageID != "") {
		return domain.TaskWorkReport{}, false, ErrConflict
	}
	if strings.TrimSpace(replyText) == "" || !utf8.ValidString(replyText) || !validTriggerID(r.SourceReplyMessageID) {
		r.SourceReplyMessageID = ""
	}
	r.CreatedAt, err = time.Parse(timestampLayout, stamp)
	return
}

// Use message source precedence to exclude platform-only notices as well.
// Classify each candidate once, even when several reports share one reply.
func taskWorkProjectRepliesInTx(ctx context.Context, tx *sql.Tx, reports []domain.TaskWorkReport) error {
	var messages []domain.Message
	positions := make(map[string]int)
	for _, r := range reports {
		if id := r.SourceReplyMessageID; id != "" {
			if _, seen := positions[id]; !seen {
				positions[id] = len(messages)
				messages = append(messages, domain.Message{ID: id})
			}
		}
	}
	if err := messageSourcesInTx(ctx, tx, messages); err != nil {
		return err
	}
	for i, r := range reports {
		if j, ok := positions[r.SourceReplyMessageID]; ok {
			m := messages[j]
			if m.SourceKind != "member" || m.SourceID != "turn:"+r.SourceTurnID {
				reports[i].SourceReplyMessageID = ""
			}
		}
	}
	return nil
}

func taskWorkReportInTx(ctx context.Context, tx *sql.Tx, groupID, id string, parent *domain.TaskWorkParent) (domain.TaskWorkReport, error) {
	r, hasReply, err := scanTaskWorkReport(tx.QueryRowContext(ctx, taskWorkReportSelect+` WHERE r.group_id=? AND r.id=?`, groupID, id))
	if err != nil {
		return domain.TaskWorkReport{}, err
	}
	if parent != nil && r.TaskWorkParent != *parent {
		return domain.TaskWorkReport{}, ErrNotFound
	}
	if !hasReply {
		return r, nil
	}
	item := []domain.TaskWorkReport{r}
	if err := taskWorkProjectRepliesInTx(ctx, tx, item); err != nil {
		return domain.TaskWorkReport{}, err
	}
	if item[0].SourceReplyMessageID == "" {
		return domain.TaskWorkReport{}, ErrConflict
	}
	return item[0], nil
}

// These row proofs are used both by the producer's same-transaction callback
// (which additionally compares the command and live actor) and the resolver.
func taskWorkProveDelegationInTx(ctx context.Context, tx *sql.Tx, p domain.TaskWorkParent, wake domain.TaskWorkDelivery) (domain.TaskWorkDelegation, error) {
	d, err := taskWorkDelegationInTx(ctx, tx, wake.GroupID, wake.SourceID)
	if err != nil {
		return domain.TaskWorkDelegation{}, coordinationRoleRelationError(err)
	}
	if d.TaskWorkParent != p || d.MemberID != wake.MemberID || wake.SenderMemberID != p.OwnerMemberID ||
		d.DeliveryID != wake.DeliveryID || d.MessageID != wake.MessageID || d.ConversationID != wake.ConversationID {
		return domain.TaskWorkDelegation{}, ErrMemberToolDenied
	}
	if d.AuthorDeliveryID == p.OriginalDeliveryID {
		if d.AuthorMessageID != p.OriginalMessageID {
			return domain.TaskWorkDelegation{}, ErrMemberToolDenied
		}
		return d, nil
	}
	// Reprove the exact continuation wake, not the current Attempt or claim.
	// The ordinal check in the row query bounds every backward report hop.
	author, err := taskWorkDeliveryInTx(ctx, tx, p.GroupID, d.AuthorDeliveryID)
	if err != nil {
		return domain.TaskWorkDelegation{}, coordinationRoleRelationError(err)
	}
	if !author.d.SourceKind.IsOwnerContinuation() || taskWorkParent(author) != p ||
		author.turnID != d.AuthorTurnID || author.d.MessageID != d.AuthorMessageID {
		return domain.TaskWorkDelegation{}, ErrMemberToolDenied
	}
	return d, nil
}

func taskWorkProveReportInTx(ctx context.Context, tx *sql.Tx, p domain.TaskWorkParent, wake domain.TaskWorkDelivery) (domain.TaskWorkReport, error) {
	r, err := taskWorkReportInTx(ctx, tx, wake.GroupID, wake.SourceID, &p)
	if err != nil {
		return domain.TaskWorkReport{}, coordinationRoleRelationError(err)
	}
	if r.Kind == domain.TaskWorkReportProgress || r.ActorID != wake.SenderMemberID || wake.MemberID != p.OwnerMemberID ||
		r.DeliveryID != wake.DeliveryID || r.MessageID != wake.MessageID {
		return domain.TaskWorkReport{}, ErrMemberToolDenied
	}
	// The report SELECT pins SourceDeliveryID to a delegation marker. Verify
	// its physical wake and logical source, never a model-supplied Turn.
	child, err := taskWorkDeliveryInTx(ctx, tx, p.GroupID, r.SourceDeliveryID)
	if err != nil {
		return domain.TaskWorkReport{}, coordinationRoleRelationError(err)
	}
	if child.delegation == nil || child.delegation.TaskWorkParent != p || child.delegation.ID != r.DelegationID ||
		child.turnID != r.SourceTurnID || child.d.MemberID != r.ActorID || child.d.MessageID != r.SourceMessageID {
		return domain.TaskWorkReport{}, ErrMemberToolDenied
	}
	return r, nil
}

// Missing B3 evidence is a denial, not permission to route ordinary chat.
// Followups have their own producer proof, independent of B3.
func taskWorkReportSourceInTx(ctx context.Context, tx *sql.Tx, f *taskWorkFacts) error {
	d := f.d
	ok, err := taskWorkReportsSupportedInTx(ctx, tx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrMemberToolDenied
	}
	p := taskWorkParent(*f)
	if d.SourceKind == domain.TaskWorkSourceDelegation {
		found, err := taskWorkProveDelegationInTx(ctx, tx, p, d)
		if err == nil {
			f.delegation, f.sourceTurnID = &found, found.AuthorTurnID
		}
		return err
	}
	report, err := taskWorkProveReportInTx(ctx, tx, p, d)
	if err == nil {
		f.report, f.sourceTurnID = &report, report.SourceTurnID
	}
	return err
}

// GetTaskWorkDelegation is a trusted startup projection by typed source ID.
// It is not a member tool; the preparer must also use the ready typed delivery
// and matching runtime profile, then recheck the postclaim execution fence.
func (s *Store) GetTaskWorkDelegation(ctx context.Context, groupID, delegationID string) (domain.TaskWorkDelegation, error) {
	return coordinationDecideRead(ctx, s, groupID, delegationID, func(ctx context.Context, tx *sql.Tx, groupID, id string) (domain.TaskWorkDelegation, error) {
		if err := requireTaskWorkReportsInTx(ctx, tx); err != nil {
			return domain.TaskWorkDelegation{}, err
		}
		var deliveryID string
		if err := tx.QueryRowContext(ctx, `SELECT delivery_id FROM task_work_deliveries WHERE source_kind='delegation' AND source_id=? AND group_id=?`, id, groupID).Scan(&deliveryID); err != nil {
			return domain.TaskWorkDelegation{}, err
		}
		f, err := taskWorkDeliveryInTx(ctx, tx, groupID, deliveryID)
		if err != nil {
			return domain.TaskWorkDelegation{}, err
		}
		if f.delegation == nil {
			return domain.TaskWorkDelegation{}, ErrNotFound
		}
		return *f.delegation, nil
	})
}

// GetTaskWorkReport is a trusted preview lookup by typed source ID. Live tools
// must use GetWorkReportForMember or ListWorkReportsForMember instead.
func (s *Store) GetTaskWorkReport(ctx context.Context, groupID, reportID string) (domain.TaskWorkReport, error) {
	return coordinationDecideRead(ctx, s, groupID, reportID, func(ctx context.Context, tx *sql.Tx, groupID, id string) (domain.TaskWorkReport, error) {
		if err := requireTaskWorkReportsInTx(ctx, tx); err != nil {
			return domain.TaskWorkReport{}, err
		}
		return taskWorkReportInTx(ctx, tx, groupID, id, nil)
	})
}

func (s *Store) GetWorkReportForMember(ctx context.Context, scope MemberToolScope, reportID string) (domain.TaskWorkReport, error) {
	if !validTriggerID(reportID) {
		return domain.TaskWorkReport{}, ErrInvalid
	}
	return workMemberTx(ctx, s, scope, "读取任务工作报告失败", func(tx *sql.Tx) (out domain.TaskWorkReport, err error) {
		p, err := s.workReportReaderInTx(ctx, tx, scope)
		if err != nil {
			return out, err
		}
		return taskWorkReportInTx(ctx, tx, p.GroupID, reportID, &p)
	})
}

func (s *Store) ListWorkReportsForMember(ctx context.Context, scope MemberToolScope, options ListOptions) (Page[domain.TaskWorkReport], error) {
	return workMemberTx(ctx, s, scope, "列出任务工作报告失败", func(tx *sql.Tx) (Page[domain.TaskWorkReport], error) {
		p, err := s.workReportReaderInTx(ctx, tx, scope)
		if err != nil {
			return Page[domain.TaskWorkReport]{}, err
		}
		return taskWorkReportPageInTx(ctx, tx, p.GroupID, p.TaskID, &p, options)
	})
}

func (s *Store) workReportReaderInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope) (domain.TaskWorkParent, error) {
	f, attempt, err := taskWorkOwnerInTx(ctx, tx, scope)
	if err == nil {
		_, err = s.taskWorkOwnerReadyInTx(ctx, tx, f, attempt)
	}
	return taskWorkParent(f), err
}

// ListTaskWorkReports is a Task detail read for trusted local-user composition.
// Historical facts remain available after policy change, submission or review.
func (s *Store) ListTaskWorkReports(ctx context.Context, groupID, taskID string, options ListOptions) (Page[domain.TaskWorkReport], error) {
	var zero Page[domain.TaskWorkReport]
	const action = "列出任务工作报告失败"
	if !validTriggerID(groupID) || !validTriggerID(taskID) {
		return zero, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return zero, queueError(action, err)
	}
	defer tx.Rollback()
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE group_id=? AND id=?`, groupID, taskID).Scan(&found); err != nil {
		return zero, queueError(action, err)
	}
	page, err := taskWorkReportPageInTx(ctx, tx, groupID, taskID, nil, options)
	if err != nil {
		return zero, queueError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return zero, queueError(action, err)
	}
	return page, nil
}

func taskWorkReportPageInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string, owner *domain.TaskWorkParent, opts ListOptions) (Page[domain.TaskWorkReport], error) {
	var page Page[domain.TaskWorkReport]
	limit, err := listLimit(opts)
	if err != nil {
		return page, err
	}
	installed, err := taskWorkReportsSupportedInTx(ctx, tx)
	if err != nil || !installed {
		return Page[domain.TaskWorkReport]{Items: []domain.TaskWorkReport{}}, err
	}
	query, args := taskWorkReportSelect+` WHERE r.group_id=? AND r.task_id=?`, []any{groupID, taskID}
	if owner != nil {
		query += ` AND (p.assignment_id,p.topic_id,p.task_revision,p.owner_member_id,p.mandate_id,p.authority_generation)=(?,?,?,?,?,?)`
		args = append(args, owner.AssignmentID, owner.TopicID, owner.TaskRevision, owner.OwnerMemberID, owner.MandateID, owner.AuthorityGeneration)
	}
	if c := opts.Before; c != nil {
		query += ` AND (r.created_at,r.id)<(?,?)`
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY r.created_at DESC,r.id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	page.Items = []domain.TaskWorkReport{}
	for rows.Next() {
		r, _, err := scanTaskWorkReport(rows)
		if err != nil {
			return Page[domain.TaskWorkReport]{}, err
		}
		page.Items = append(page.Items, r)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.TaskWorkReport]{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	// Do not let a broken optional link hide valid report history.
	if err := taskWorkProjectRepliesInTx(ctx, tx, page.Items); err != nil {
		return Page[domain.TaskWorkReport]{}, err
	}
	return page, nil
}

// ReadTaskWorkChannel is bound to the live typed turn's exact topic.
func (s *Store) ReadTaskWorkChannel(ctx context.Context, scope MemberToolScope, opts ListOptions) (Page[domain.Message], error) {
	return workMemberTx(ctx, s, scope, "读取任务工作话题失败", func(tx *sql.Tx) (Page[domain.Message], error) {
		p, err := s.taskWorkMaterialsForMemberInTx(ctx, tx, scope)
		if err != nil {
			return Page[domain.Message]{}, err
		}
		if !p.AllowTopicMessages {
			return Page[domain.Message]{}, ErrMemberToolDenied
		}
		return readCurrentChannelInTx(ctx, tx, scope, opts, storageHistory)
	})
}

// ReadTaskWorkArtifact resolves metadata for an exact granted version. Neither
// a head lookup nor a relation to another version/task is granted by this read.
func (s *Store) ReadTaskWorkArtifact(ctx context.Context, scope MemberToolScope, artifactID, versionID string) (domain.Artifact, domain.ArtifactVersion, error) {
	type material struct {
		a domain.Artifact
		v domain.ArtifactVersion
	}
	m, err := workMemberTx(ctx, s, scope, "读取任务工作资料失败", func(tx *sql.Tx) (out material, err error) {
		p, err := s.taskWorkMaterialsForMemberInTx(ctx, tx, scope)
		if err != nil {
			return out, err
		}
		if !slices.Contains(p.ArtifactVersions, domain.ArtifactVersionRef{ArtifactID: artifactID, VersionID: versionID}) {
			return out, ErrMemberToolDenied
		}
		out.v, err = artifactVersionInTx(ctx, tx, p.GroupID, artifactID, versionID)
		if err != nil {
			return material{}, coordinationRoleRelationError(err)
		}
		out.v = taskWorkArtifactProjection(out.v, p.ArtifactVersions)
		out.a, err = scanArtifact(tx.QueryRowContext(ctx, `SELECT id,group_id,name,'',created_at FROM artifacts WHERE group_id=? AND id=?`, p.GroupID, artifactID))
		return out, err
	})
	return m.a, m.v, err
}

func taskWorkArtifactProjection(v domain.ArtifactVersion, allowed []domain.ArtifactVersionRef) domain.ArtifactVersion {
	if !slices.Contains(allowed, domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ExpectedVersionID}) {
		v.ExpectedVersionID = ""
	}
	if !slices.Contains(allowed, domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ObservedVersionID}) {
		v.ObservedVersionID = ""
	}
	v.SourceTaskID = ""
	return v
}
