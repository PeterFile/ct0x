package store

import (
	"context"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// VerifyDiscussionWorkReturnStartup checks the live claim for an already
// resolved A7 route in one read. It neither issues tools nor changes the claim.
func (s *Store) VerifyDiscussionWorkReturnStartup(ctx context.Context, claim TurnClaim, route domain.ChatDiscussionWorkReturnReceipt,
	providerID, model, resourceKey string) error {
	if claim.GroupID != route.GroupID || !utf8.ValidString(route.TopicID) || len(model) > 256 {
		return ErrMemberToolDenied
	}
	for _, value := range []string{claim.GroupID, claim.TurnID, claim.Token, route.DeliveryID, route.MessageID,
		route.RecipientID, route.ConversationID, providerID, model, resourceKey} {
		if !memberToolIDsValid(value) || !utf8.ValidString(value) {
			return ErrMemberToolDenied
		}
	}
	_, err := scanMember(s.db.QueryRowContext(ctx, `SELECT m.id,m.group_id,m.name,m.role,m.provider_id,m.model,
	 m.creator_id,m.owner_id,m.status,m.workspace,m.created_at`+memberToolBinding+`
	 AND t.state='starting' AND t.accepted_at IS NULL AND t.native_session_id=''
	 AND t.candidate_text='' AND t.reply_message_id='' AND t.receipt_json=''
	 AND (t.delivery_id,t.member_id,t.conversation_id,c.topic_id,m.provider_id,m.model)=(?,?,?,?,?,?)
	 AND EXISTS(SELECT 1 FROM deliveries d
	   WHERE (d.id,d.group_id,d.recipient_id,d.conversation_id,d.topic_id,d.turn_id)=
	     (t.delivery_id,t.group_id,t.member_id,t.conversation_id,c.topic_id,t.id)
	   AND d.message_id=? AND d.state='queued' AND d.error='')
	 AND EXISTS(SELECT 1 FROM resource_claims held WHERE held.resource_key=? AND held.turn_id=t.id)
	 AND (SELECT count(*) FROM resource_claims WHERE turn_id=t.id)=1`,
		claim.GroupID, claim.TurnID, claim.Token, route.DeliveryID, route.RecipientID, route.ConversationID, route.TopicID,
		providerID, model, route.MessageID, resourceKey))
	if err != nil || ctx.Err() != nil {
		return ErrMemberToolDenied
	}
	return nil
}
