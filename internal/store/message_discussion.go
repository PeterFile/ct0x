package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"agentworkspace/internal/domain"
)

// projectMessageDiscussionsInTx verifies the scoped relationships before reading
// admission or interruption in the same snapshot. Pending proof adds neither.
func projectMessageDiscussionsInTx(ctx context.Context, tx *sql.Tx, groupID string, messages []domain.Message) error {
	if err := messageDiscussionsInTx(ctx, tx, groupID, messages); err != nil {
		return storeError("读取讨论关联失败", err)
	}
	var targets []string
	for i := range messages {
		m := &messages[i]
		if d := m.Discussion; m.SourceKind == "member" && len(m.Deliveries) == 1 && d != nil &&
			(d.Kind == domain.ChatDiscussionInvite || d.Kind == domain.ChatDiscussionReply) {
			targets = append(targets, m.ID)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	heads, err := chatDiscussionHeadsInTx(ctx, tx, groupID, targets...)
	if err != nil {
		return fmt.Errorf("读取讨论状态失败：%w", err)
	}
	for i := range messages {
		head := heads[messages[i].ID]
		messages[i].DiscussionClosureReason, messages[i].DiscussionWork = head.closureReason, head.work
	}
	return nil
}

func messageDiscussionsInTx(ctx context.Context, tx *sql.Tx, groupID string, messages []domain.Message) error {
	if len(messages) == 0 {
		return nil
	}
	// The migration ledger, not table presence, distinguishes legacy storage
	// from a damaged installation. Let the projection surface schema errors.
	version := slices.Index(migrations, chatDiscussionSchema) + 1
	if version == 0 {
		return nil
	}
	var applied bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.schema_migrations WHERE version=?)`, version).Scan(&applied); err != nil || !applied {
		return err
	}

	args := make([]any, 1+len(messages))
	args[0] = groupID
	for i := range messages {
		args[i+1] = messages[i].ID
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(messages)), ",")
	// Probe exact page keys first. Resolve the discussion columns used below
	// even when the registered schema has no matching events for this page.
	var hasEvents bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
	 SELECT e.message_id,e.origin_message_id,e.kind,e.actor_id,e.recipient_id,e.delivery_id,
	    e.source_message_id,e.candidate_id,e.step,e.related_invitation_id,e.expected_reply,e.group_id,e.topic_id,c.root_inviter_id
	 FROM main.chat_discussion_events e
	 CROSS JOIN main.chat_discussions c ON c.origin_message_id=e.origin_message_id AND c.group_id=e.group_id AND c.topic_id=e.topic_id
	 WHERE e.group_id=? AND e.message_id IN (`+placeholders+`) AND e.expected_reply IS NOT NULL
	)`, args...).Scan(&hasEvents); err != nil || !hasEvents {
		return err
	}

	positions := make(map[string]int, len(messages))
	for i := range messages {
		positions[messages[i].ID] = i
	}
	// Find roots by exact page keys, then verify just those bounded paths once.
	// Keep the exact-key evidence checks separate to avoid planning one wide join.
	// Missing or contradictory evidence drops the relation, not the message.
	rows, err := tx.QueryContext(ctx, `WITH page_events AS MATERIALIZED (
	 SELECT message_id,origin_message_id,group_id,topic_id FROM main.chat_discussion_events
	 WHERE group_id=? AND message_id IN (`+placeholders+`)
	 ), roots AS (
	 SELECT DISTINCT origin_message_id,group_id,topic_id FROM page_events
	 ), published AS MATERIALIZED (
	 SELECT e.message_id,e.origin_message_id,e.group_id,e.topic_id,e.step,e.kind,e.actor_id,e.recipient_id,
	    e.source_message_id,e.delivery_id,e.related_invitation_id,e.expected_reply,t.delivery_id AS incoming_delivery_id
	 FROM roots
	 CROSS JOIN main.chat_discussion_events e ON e.origin_message_id=roots.origin_message_id AND e.group_id=roots.group_id AND e.topic_id=roots.topic_id
	 CROSS JOIN main.chat_candidates b ON b.id=e.candidate_id AND b.message_id=e.source_message_id
	    AND b.group_id=e.group_id AND b.topic_id=e.topic_id AND b.member_id=e.actor_id
	 CROSS JOIN main.turns t ON t.id=b.turn_id AND t.group_id=b.group_id AND t.member_id=b.member_id
	 WHERE e.actor_id NOT IN ('','user')
	   AND EXISTS(
	      SELECT 1 FROM main.messages m
	      JOIN main.members a ON a.id=e.actor_id AND a.group_id=e.group_id
	      WHERE m.id=e.message_id AND m.group_id=e.group_id AND m.topic_id=e.topic_id AND m.sender_id=e.actor_id)
	   -- Compare the recorded claim facts, not the live token or mutable status.
	   AND EXISTS(
	      SELECT 1 FROM main.chat_intake_executions x
	      JOIN main.chat_assessments assessment ON assessment.candidate_id=b.id
	      JOIN main.message_command_sources s ON s.message_id=e.message_id AND s.kind='member' AND s.turn_id=b.turn_id AND s.claim_digest=x.claim_digest
	      JOIN main.message_command_sources source ON source.message_id=b.message_id AND source.id=x.source_id AND source.kind=x.source_kind
	      WHERE x.candidate_id=b.id AND x.turn_id=b.turn_id AND x.purpose='`+domain.ChatIntakePurpose+`' AND length(x.claim_digest)=32
	        AND ((e.step=1 AND e.source_message_id=e.origin_message_id AND e.related_invitation_id IS NULL AND source.kind='local_user_chat')
	          OR (e.step>1 AND e.source_message_id<>e.origin_message_id AND e.related_invitation_id IS NOT NULL AND source.kind='member')))
	   AND EXISTS(SELECT 1 FROM main.message_log l
	      WHERE l.sequence=b.generation AND l.message_id=b.message_id AND l.group_id=b.group_id AND l.topic_id=b.topic_id)
	   AND EXISTS(
	      SELECT 1 FROM main.deliveries incoming
	      JOIN main.conversations arrival ON arrival.id=t.conversation_id
	         AND arrival.group_id=b.group_id AND arrival.topic_id=b.topic_id AND arrival.member_id=b.member_id
	      WHERE incoming.id=t.delivery_id AND incoming.turn_id=t.id AND incoming.conversation_id=t.conversation_id
	         AND incoming.message_id=b.message_id AND incoming.group_id=b.group_id AND incoming.topic_id=b.topic_id AND incoming.recipient_id=b.member_id)
	   AND (e.delivery_id IS NULL OR EXISTS(
	      SELECT 1 FROM main.deliveries d
	      JOIN main.conversations c ON c.id=d.conversation_id AND c.group_id=d.group_id AND c.topic_id=d.topic_id AND c.member_id=d.recipient_id
	      JOIN main.members r ON r.id=d.recipient_id AND r.group_id=d.group_id
	      WHERE d.id=e.delivery_id AND d.message_id=e.message_id AND d.group_id=e.group_id AND d.topic_id=e.topic_id AND d.recipient_id=e.recipient_id))
	   AND NOT EXISTS(SELECT 1 FROM main.deliveries d WHERE d.message_id=e.message_id AND d.id IS NOT e.delivery_id)
	   AND ((e.kind='invite' AND e.delivery_id IS NOT NULL AND e.recipient_id NOT IN ('','user',e.actor_id)
	         AND e.expected_reply IS NOT NULL AND (e.step=1 OR e.related_invitation_id=e.source_message_id))
	     OR (e.kind='reply' AND e.step>1 AND e.recipient_id IS NOT NULL AND e.expected_reply='')
	     OR (e.kind='resolve' AND e.recipient_id IS NULL AND e.delivery_id IS NULL AND e.expected_reply=''))
	 )
	 SELECT e.message_id,e.kind,e.origin_message_id,i.message_id,
	    coalesce(i.related_invitation_id,''),i.actor_id,i.recipient_id,i.expected_reply
	 FROM page_events page
	 CROSS JOIN published e ON e.message_id=page.message_id
	 CROSS JOIN published i ON i.message_id=CASE WHEN e.kind='invite' THEN e.message_id ELSE e.related_invitation_id END
	    AND i.origin_message_id=e.origin_message_id AND i.group_id=e.group_id AND i.topic_id=e.topic_id AND i.kind='invite'
	 JOIN main.chat_discussions c ON c.origin_message_id=e.origin_message_id AND c.group_id=e.group_id AND c.topic_id=e.topic_id
	 JOIN main.messages o ON o.id=c.origin_message_id AND o.group_id=c.group_id AND o.topic_id=c.topic_id AND o.sender_id='user'
	 JOIN main.message_command_sources s ON s.message_id=o.id AND s.kind='local_user_chat' AND s.turn_id IS NULL AND s.claim_digest IS NULL
	 WHERE ((i.step=1 AND i.actor_id=c.root_inviter_id) OR (i.step>1 AND EXISTS(
	      SELECT 1 FROM published p WHERE p.message_id=i.source_message_id AND p.kind='invite'
	        AND p.origin_message_id=i.origin_message_id AND p.group_id=i.group_id AND p.topic_id=i.topic_id
	        AND p.step=i.step-1 AND p.recipient_id=i.actor_id AND p.delivery_id=i.incoming_delivery_id
	        AND (p.step<>1 OR p.actor_id=c.root_inviter_id))))
	   -- The triggering delivery may carry a child reply rather than the invitation being answered.
	   AND (e.kind='invite' OR (e.step>i.step AND EXISTS(
	      SELECT 1 FROM published p WHERE p.message_id=e.source_message_id
	        AND p.origin_message_id=e.origin_message_id AND p.group_id=e.group_id AND p.topic_id=e.topic_id
	        AND p.step=e.step-1 AND p.recipient_id=e.actor_id AND p.delivery_id=e.incoming_delivery_id
	        AND ((e.kind='reply' AND e.actor_id=i.recipient_id AND e.recipient_id=i.actor_id
	              AND ((p.kind='invite' AND p.message_id=i.message_id) OR (p.kind='reply' AND EXISTS(
	                 SELECT 1 FROM published child WHERE child.message_id=p.related_invitation_id AND child.kind='invite'
	                   AND child.origin_message_id=i.origin_message_id AND child.group_id=i.group_id AND child.topic_id=i.topic_id
	                   AND child.related_invitation_id=i.message_id AND child.step=i.step+1 AND child.step<p.step
	                   AND child.actor_id=p.recipient_id AND child.recipient_id=p.actor_id))))
	          OR (e.kind='resolve' AND i.step=1 AND e.actor_id=i.actor_id AND p.kind='reply'
	              AND p.related_invitation_id=i.message_id AND p.actor_id=i.recipient_id)))))`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var d domain.MessageDiscussion
		if err := rows.Scan(&id, &d.Kind, &d.RootMessageID, &d.InvitationMessageID,
			&d.ParentInvitationMessageID, &d.InviterID, &d.AssigneeID, &d.ExpectedReply); err != nil {
			return err
		}
		messages[positions[id]].Discussion = &d
	}
	return rows.Err()
}
