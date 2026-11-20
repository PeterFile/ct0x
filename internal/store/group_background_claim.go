package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Compare durable source order, not delivery creation time: deferred A1
// deliveries are usually newer than explicitly addressed follow-ups. Only
// order local-user chat claims in the same bound topic when at least one route
// is A1. In particular, a work wake with sender_id=user isn't chat. Claimed
// turns keep the existing member, conversation and resource locks.
// The caller aliases the current delivery as d.
const groupBackgroundEarlierSQL = `EXISTS(
 SELECT 1 FROM group_background_bindings b
 JOIN message_log cur ON cur.message_id=d.message_id
 JOIN message_command_sources chat ON chat.message_id=cur.message_id AND chat.kind='local_user_chat'
 WHERE b.group_id=d.group_id AND (
   EXISTS(SELECT 1 FROM group_background_intents i WHERE i.group_id=d.group_id
     AND i.topic_id=d.topic_id AND i.sequence<cur.sequence AND i.delivery_id IS NULL)
   OR EXISTS(SELECT 1 FROM deliveries older
     JOIN message_log src ON src.message_id=older.message_id
     JOIN message_command_sources older_chat ON older_chat.message_id=src.message_id AND older_chat.kind='local_user_chat'
     WHERE older.group_id=d.group_id AND older.topic_id=d.topic_id
       AND older.state='queued' AND older.turn_id='' AND src.sequence<cur.sequence
       AND (EXISTS(SELECT 1 FROM group_background_intents i WHERE i.delivery_id=older.id)
         OR EXISTS(SELECT 1 FROM group_background_intents i WHERE i.delivery_id=d.id)))
 ))`

// BackgroundDispatchProfileReady is a preparation hint, not a claim. ClaimTurn
// rechecks the current snapshot and its expiry at final admission.
func (s *Store) BackgroundDispatchProfileReady(profile BackgroundDispatchProfile) bool {
	s.background.readyMu.RLock()
	defer s.background.readyMu.RUnlock()
	return s.background.profileReadyLocked(profile)
}

func (b *groupBackgroundDispatch) profileReadyLocked(profile BackgroundDispatchProfile) bool {
	return b.ready != nil && time.Now().Before(b.ready.expires) && b.ready.profiles[profile]
}

// The existing claim transaction owns the write lock. A member's provider/model
// is set at creation; workspace can change, so use the live row here, not the
// preparation's earlier workspace observation.
func backgroundClaimProfileInTx(ctx context.Context, tx *sql.Tx, input ClaimTurnInput) (bool, error) {
	var live BackgroundDispatchProfile
	var workspace string
	err := tx.QueryRowContext(ctx, `SELECT m.provider_id,m.model,m.workspace
	 FROM group_background_intents i JOIN members m ON (m.group_id,m.id)=(i.group_id,i.recipient_id)
	 WHERE i.delivery_id=? AND i.group_id=?`, input.DeliveryID, input.GroupID).
		Scan(&live.ProviderID, &live.Model, &workspace)
	if errors.Is(err, sql.ErrNoRows) && input.BackgroundProfile == (BackgroundDispatchProfile{}) {
		return false, nil
	}
	if errors.Is(err, sql.ErrNoRows) || err == nil && (input.BackgroundProfile != live || workspace != "") {
		return false, ErrTurnBusy
	}
	return err == nil, err
}
