package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agentworkspace/internal/domain"
)

type localThreadRoute struct {
	topic, recipient, replyTopic string
	newTitle, newIssue           string
	correction                   *domain.ThreadCorrection
	comparisonAuthor             string // Excluded from fallback, not from independent routing.
}

func threadRoutingInstalledInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	version := slices.Index(migrations, threadRoutingSchema) + 1
	var installed, later bool
	var objects, intact int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.schema_migrations WHERE version=:version),
	 :version>0 AND EXISTS(SELECT 1 FROM main.schema_migrations WHERE version>:version),
	 count(*),count(CASE WHEN sql IN (:topics,:links,:issue,:insert_guard,:update_guard,:delete_guard) THEN 1 END)
	 FROM main.sqlite_schema WHERE name IN ('thread_routing_topics','thread_routing_links','thread_routing_issue',
	 'thread_routing_links_insert','thread_routing_links_update','thread_routing_links_delete')`,
		sql.Named("version", version), sql.Named("topics", threadRoutingTopicsTable), sql.Named("links", threadRoutingLinksTable),
		sql.Named("issue", threadRoutingIssueIndex), sql.Named("insert_guard", threadRoutingLinksInsert),
		sql.Named("update_guard", threadRoutingLinksUpdate), sql.Named("delete_guard", threadRoutingLinksDelete)).Scan(&installed, &later, &objects, &intact)
	if err != nil {
		return false, err
	}
	if !installed {
		if later || objects != 0 {
			return false, errors.New("话题归属迁移记录缺失")
		}
		return false, nil
	}
	if objects != 6 || intact != 6 {
		return false, errors.New("话题归属数据库结构不完整")
	}
	return true, nil
}

func localMessageReceiptInTx(ctx context.Context, tx *sql.Tx, r MessageRequest, id string, normalPost bool) (domain.Message, error) {
	if normalPost && r.ThreadLinks == (domain.ThreadLinks{}) {
		if _, err := threadRoutingInstalledInTx(ctx, tx); err != nil {
			return domain.Message{}, err
		}
	}
	m, err := messageInTx(ctx, tx, r.GroupID, id)
	if err != nil {
		return domain.Message{}, err
	}
	items := []domain.Message{m}
	if err := messageThreadCorrectionsInTx(ctx, tx, r.GroupID, items); err != nil {
		return domain.Message{}, err
	}
	// Legacy reservations conflated absent and empty topic_id. Do not
	// acknowledge inferred chat as main. A correction receipt always stays
	// on main, including a clarification of a conflicting manual scope.
	if items[0].ThreadCorrection == nil && r.TopicIDExplicit && m.TopicID != r.TopicID {
		return domain.Message{}, ErrConflict
	}
	if r.ThreadLinks != (domain.ThreadLinks{}) {
		if err := messageThreadLinksInTx(ctx, tx, r.GroupID, items); err != nil {
			return domain.Message{}, err
		}
		if items[0].ThreadLinks != r.ThreadLinks {
			return domain.Message{}, errors.New("消息引用与原命令不一致")
		}
	}
	return items[0], nil
}

func publishLocalMessageInTx(ctx context.Context, tx *sql.Tx, r MessageRequest, id string, payload []byte, recipients []string, normalPost bool) (domain.Message, error) {
	route := localThreadRoute{topic: r.TopicID}
	if normalPost || r.ThreadLinks != (domain.ThreadLinks{}) {
		var err error
		route, err = resolveLocalThreadInTx(ctx, tx, r, normalPost)
		if err != nil {
			return domain.Message{}, err
		}
	}
	// A topical command has no chat proof or deliveries, even with explicit
	// recipients. Keep the original request bytes in the mutation receipt.
	if route.correction != nil {
		r.RecipientIDs, recipients = nil, nil
	}
	// Compose A's single dispatch decision here: an E recipient is a suggestion
	// for omitted recipients only. Fallback cannot wake a comparison author.
	if route.correction == nil && normalPost && r.RecipientIDs == nil {
		if route.recipient == "" {
			if err := activeDefaultRecipientInTx(ctx, tx, r.GroupID, route.comparisonAuthor, &route.recipient); err != nil {
				return domain.Message{}, err
			}
		}
		if route.recipient != "" {
			recipients = []string{route.recipient}
		}
	}
	m, err := insertRequestedMessageInTx(ctx, tx, r, id, route.topic, recipients)
	if err != nil {
		return domain.Message{}, err
	}
	if route.correction != nil {
		if err := insertThreadCorrectionInTx(ctx, tx, &m, route.correction); err != nil {
			return domain.Message{}, err
		}
	} else {
		m.SourceID, err = messageCommandSourceInTx(ctx, tx, m.ID, "local_user_chat", payload, r.IdempotencyKey, nil)
		if err != nil {
			return domain.Message{}, err
		}
		m.SourceKind = "local_user_chat"
	}
	if route.newTitle != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.thread_routing_topics(group_id,topic_id,title,title_key,issue_id,origin_message_id)
		 VALUES(?,?,?,?,?,?)`, m.GroupID, m.TopicID, route.newTitle, threadTitleKey(route.newTitle), route.newIssue, m.ID); err != nil {
			return domain.Message{}, err
		}
	}
	if r.ThreadLinks != (domain.ThreadLinks{}) {
		var replyTopic any
		if r.ReplyToMessageID != "" {
			replyTopic = route.replyTopic // An empty topic denotes main, not a missing link.
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.thread_routing_links(message_id,group_id,topic_id,reply_to_message_id,reply_to_topic_id,task_id)
		 VALUES(?,?,?,nullif(?,''),?,nullif(?,''))`, m.ID, m.GroupID, m.TopicID, r.ReplyToMessageID, replyTopic, r.RelatedTaskID); err != nil {
			return domain.Message{}, err
		}
		m.ThreadLinks = r.ThreadLinks
	}
	return m, nil
}

func resolveLocalThreadInTx(ctx context.Context, tx *sql.Tx, r MessageRequest, infer bool) (localThreadRoute, error) {
	route := localThreadRoute{topic: r.TopicID}
	subject, correction := threadCorrectionSubject(r.Content)
	correction = correction && infer
	installed, err := threadRoutingInstalledInTx(ctx, tx)
	if err != nil {
		return route, err
	}
	var correctionsInstalled bool
	if infer {
		// Even explicit or unstructured sends must not commit against a
		// damaged correction store. Check in the same routing transaction.
		correctionsInstalled, err = threadCorrectionInstalledInTx(ctx, tx)
		if err != nil {
			return route, err
		}
	}
	if !installed {
		if correction {
			return route, threadCorrectionUnavailable()
		}
		if r.ThreadLinks != (domain.ThreadLinks{}) {
			return route, fmt.Errorf("%w：当前实例未启用消息引用", ErrInvalid)
		}
		return route, nil
	}
	var replyAuthor, replyEffectiveTopic, taskOwner string
	var taskTopic sql.NullString
	if r.ReplyToMessageID != "" {
		if !validThreadReference(r.ReplyToMessageID) {
			return route, fmt.Errorf("%w：回复消息标识无效", ErrInvalid)
		}
		err = tx.QueryRowContext(ctx, `SELECT m.topic_id,coalesce(a.id,'') FROM messages m
		 LEFT JOIN members a ON a.group_id=m.group_id AND a.id=m.sender_id AND a.id<>'user' AND a.status='active'
		 WHERE m.id=? AND m.group_id=?`, r.ReplyToMessageID, r.GroupID).Scan(&route.replyTopic, &replyAuthor)
		if err != nil {
			return route, err
		}
		replyEffectiveTopic = route.replyTopic
		if correctionsInstalled && !correction {
			ref := domain.ThreadRef{GroupID: r.GroupID, TopicID: route.replyTopic}
			attr, err := correctedThreadAttributionInTx(ctx, tx, ref, r.ReplyToMessageID)
			if err != nil {
				return route, err
			}
			replyEffectiveTopic = attr.Effective.TopicID
		}
	}
	if r.RelatedTaskID != "" {
		if !validThreadReference(r.RelatedTaskID) {
			return route, fmt.Errorf("%w：关联任务标识无效", ErrInvalid)
		}
		// Existing coordination is the association; an unassociated Task is not
		// proof of a Thread. Do not create or change coordination here.
		err = tx.QueryRowContext(ctx, `SELECT c.topic_id,coalesce(a.id,'') FROM tasks t
		 LEFT JOIN task_coordination c ON c.task_id=t.id AND c.group_id=t.group_id
		 LEFT JOIN members a ON a.group_id=t.group_id AND a.id=t.assignee_id AND a.status='active'
		 WHERE t.id=? AND t.group_id=?`, r.RelatedTaskID, r.GroupID).Scan(&taskTopic, &taskOwner)
		if err != nil {
			return route, err
		}
	}
	if correction {
		if !correctionsInstalled {
			return route, threadCorrectionUnavailable()
		}
		if r.TopicID != "" || r.RelatedTaskID != "" {
			// A Task or a manual non-main scope is not authority to change it.
			route.correction = &domain.ThreadCorrection{Question: "请只引用本群原话并说明目标话题，不附带任务或指定话题，可以吗？"}
		} else {
			route.correction, err = resolveThreadCorrectionInTx(ctx, tx, r, subject)
		}
		route.topic = ""
		return route, err
	}
	if !infer || r.TopicID != "" || r.TopicIDExplicit {
		if replyAuthor != "" && threadComparisonReply(r.Content) {
			route.comparisonAuthor = replyAuthor
		} else if replyEffectiveTopic == route.topic {
			route.recipient = replyAuthor
		}
		return route, nil
	}
	text, err := resolveThreadTextInTx(ctx, tx, r.GroupID, r.Content)
	if err != nil {
		return route, err
	}
	var evidence threadTopicEvidence
	evidence.add(text.topic)
	if taskTopic.Valid {
		evidence.confirm(taskTopic.String)
	}
	// Comparison references never select the quoted topic or author, even
	// when we cannot resolve the explicitly stated current subject.
	if text.comparison {
		route.comparisonAuthor = replyAuthor
	} else if r.ReplyToMessageID != "" {
		evidence.confirm(replyEffectiveTopic)
	}
	evidence = text.resolveAliases(evidence)
	if text.conflict || evidence.conflict || (text.newTitle != "" && evidence.matched) {
		return route, nil
	}
	route.topic = evidence.topic
	if text.newTitle != "" {
		route.topic, route.newTitle, route.newIssue = rand.Text(), text.newTitle, text.newIssue
	}
	if r.RecipientIDs != nil {
		return route, nil
	}
	switch {
	case !text.comparison && replyAuthor != "" && replyEffectiveTopic == route.topic:
		route.recipient = replyAuthor
	case taskTopic.Valid && taskTopic.String == route.topic && taskOwner != "":
		route.recipient = taskOwner
	case route.topic != "" && route.newTitle == "":
		route.recipient, err = threadOriginRecipientInTx(ctx, tx, r.GroupID, route.topic)
	}
	return route, err
}

func validThreadReference(id string) bool {
	return validHistoryText(id, 256) && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\t\r\n")
}

func threadOriginRecipientInTx(ctx context.Context, tx *sql.Tx, group, topic string) (string, error) {
	var member string
	err := tx.QueryRowContext(ctx, `SELECT a.id FROM main.thread_routing_topics t
	 JOIN deliveries d ON (d.message_id,d.group_id,d.topic_id)=(t.origin_message_id,t.group_id,t.topic_id)
	 JOIN members a ON a.group_id=d.group_id AND a.id=d.recipient_id AND a.status='active'
	 WHERE t.group_id=? AND t.topic_id=?
	 AND NOT EXISTS(SELECT 1 FROM deliveries other WHERE other.message_id=d.message_id AND other.id<>d.id)`, group, topic).Scan(&member)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return member, err
}

func activeDefaultRecipientInTx(ctx context.Context, tx *sql.Tx, group, exclude string, id *string) error {
	err := tx.QueryRowContext(ctx, `SELECT id FROM members WHERE group_id=? AND status='active'
	 AND (?='' OR id<>?) ORDER BY created_at ASC,id ASC LIMIT 1`, group, exclude, exclude).Scan(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // The command has already verified the group.
	}
	return err
}
