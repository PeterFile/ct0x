package store

import (
	"context"
	"database/sql"
	"strings"

	"agentworkspace/internal/domain"
)

// messageDiscussionsInTx enriches only the selected public messages, not write
// receipts or delivery state. The same read works for a page or an exact message.
func messageDiscussionsInTx(ctx context.Context, tx *sql.Tx, groupID string, messages []domain.Message) error {
	if len(messages) == 0 {
		return nil
	}
	// An unregistered candidate or older schema has no discussion table.
	// Do not turn errors from an installed table into missing metadata.
	var available bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.sqlite_schema
	 WHERE type='table' AND name='chat_discussion_events')`).Scan(&available); err != nil || !available {
		return err
	}

	args := make([]any, 1+len(messages))
	args[0] = groupID
	positions := make(map[string]int, len(messages))
	for i := range messages {
		id := messages[i].ID
		args[i+1], positions[id] = id, i
	}
	// Probe only the selected events and their exact invitation and parent keys.
	// Missing or NULL evidence drops the relation, not the message. Delivery
	// state and current membership status are not part of the recorded route.
	rows, err := tx.QueryContext(ctx, `WITH published AS NOT MATERIALIZED (
	 SELECT e.message_id,e.origin_message_id,e.group_id,e.topic_id,e.step,e.kind,e.actor_id,e.recipient_id,
	    e.delivery_id,e.related_invitation_id,e.expected_reply
	 FROM main.chat_discussion_events e
	 JOIN main.messages m ON m.id=e.message_id AND m.group_id=e.group_id AND m.topic_id=e.topic_id AND m.sender_id=e.actor_id
	 JOIN main.message_command_sources s ON s.message_id=m.id AND s.kind='member'
	 JOIN main.members a ON a.id=e.actor_id AND a.group_id=e.group_id
	 WHERE e.actor_id NOT IN ('','user')
	   AND (e.delivery_id IS NULL OR EXISTS(
	      SELECT 1 FROM main.deliveries d
	      JOIN main.conversations c ON c.id=d.conversation_id AND c.group_id=d.group_id AND c.topic_id=d.topic_id AND c.member_id=d.recipient_id
	      JOIN main.members r ON r.id=d.recipient_id AND r.group_id=d.group_id
	      WHERE d.id=e.delivery_id AND d.message_id=e.message_id AND d.group_id=e.group_id AND d.topic_id=e.topic_id AND d.recipient_id=e.recipient_id))
	   AND NOT EXISTS(SELECT 1 FROM main.deliveries d WHERE d.message_id=e.message_id AND d.id IS NOT e.delivery_id)
	   AND (e.kind<>'invite' OR (e.delivery_id IS NOT NULL AND e.recipient_id IS NOT NULL
	      AND e.recipient_id NOT IN ('','user',e.actor_id) AND e.expected_reply IS NOT NULL
	      AND ((e.step=1 AND e.source_message_id=e.origin_message_id AND e.related_invitation_id IS NULL)
	        OR (e.step>1 AND e.source_message_id<>e.origin_message_id AND e.related_invitation_id=e.source_message_id))))
	 )
	 SELECT e.message_id,e.kind,e.origin_message_id,i.message_id,
	    coalesce(i.related_invitation_id,''),i.actor_id,i.recipient_id,i.expected_reply
	 FROM published e
	 JOIN main.chat_discussions c ON c.origin_message_id=e.origin_message_id AND c.group_id=e.group_id AND c.topic_id=e.topic_id
	 JOIN main.messages o ON o.id=c.origin_message_id AND o.group_id=c.group_id AND o.topic_id=c.topic_id AND o.sender_id='user'
	 JOIN main.message_command_sources s ON s.message_id=o.id AND s.kind='local_user_chat' AND s.turn_id IS NULL AND s.claim_digest IS NULL
	 JOIN published i ON i.message_id=CASE WHEN e.kind='invite' THEN e.message_id ELSE e.related_invitation_id END
	    AND i.origin_message_id=e.origin_message_id AND i.group_id=e.group_id AND i.topic_id=e.topic_id AND i.kind='invite'
	 WHERE e.group_id=? AND e.message_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(messages)), ",")+`)
	   AND ((i.step=1 AND i.actor_id=c.root_inviter_id) OR (i.step>1 AND EXISTS(
	      SELECT 1 FROM published p WHERE p.message_id=i.related_invitation_id AND p.kind='invite'
	        AND p.origin_message_id=i.origin_message_id AND p.group_id=i.group_id AND p.topic_id=i.topic_id
	        AND p.step=i.step-1 AND p.recipient_id=i.actor_id AND (p.step<>1 OR p.actor_id=c.root_inviter_id))))
	   AND (e.kind='invite' OR (e.step>i.step AND (
	      (e.kind='reply' AND e.actor_id=i.recipient_id AND e.recipient_id=i.actor_id)
	      OR (e.kind='resolve' AND i.step=1 AND e.actor_id=i.actor_id AND e.recipient_id IS NULL AND e.delivery_id IS NULL))))`, args...)
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
