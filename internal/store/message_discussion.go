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
	// The invitation may be outside the page. A summary with no invitation
	// remains a public message without a handoff relation.
	rows, err := tx.QueryContext(ctx, `SELECT e.message_id,e.kind,e.origin_message_id,i.message_id,
	 coalesce(p.message_id,''),i.actor_id,i.recipient_id,i.expected_reply
	 FROM main.chat_discussion_events e
	 JOIN messages m ON m.id=e.message_id AND m.group_id=e.group_id AND m.topic_id=e.topic_id AND m.sender_id=e.actor_id
	 JOIN main.chat_discussion_events i ON i.message_id=CASE WHEN e.kind='invite' THEN e.message_id ELSE e.related_invitation_id END
	    AND i.origin_message_id=e.origin_message_id AND i.group_id=e.group_id AND i.topic_id=e.topic_id AND i.kind='invite'
	 LEFT JOIN main.chat_discussion_events p ON p.message_id=i.related_invitation_id
	    AND p.origin_message_id=i.origin_message_id AND p.group_id=i.group_id AND p.topic_id=i.topic_id AND p.kind='invite'
	 WHERE e.group_id=? AND e.message_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(messages)), ",")+`)
	    AND (e.kind='invite'
	      OR (e.kind='reply' AND e.actor_id=i.recipient_id AND e.recipient_id=i.actor_id)
	      OR (e.kind='resolve' AND i.step=1 AND e.actor_id=i.actor_id))`, args...)
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
