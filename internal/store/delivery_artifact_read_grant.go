package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

var ErrArtifactReadGrantUnavailable = errors.New("数据库尚未启用消息成果读取许可")

// ValidArtifactReadPin is the exact fresh Pi transport's ID contract. No
// trimming, normalization, or fallback to the current version is permitted.
func ValidArtifactReadPin(ref domain.ArtifactVersionRef) bool {
	for _, id := range [...]string{ref.ArtifactID, ref.VersionID} {
		if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.TrimSpace(id) != id || strings.ContainsAny(id, `/\`) ||
			strings.ContainsFunc(id, unicode.IsControl) {
			return false
		}
	}
	return true
}

// The additive fragment has no public ledger number here. Absent storage is
// compatible with old sends and lookup, but not with creating or using a pin.
func artifactReadGrantInstalled(ctx context.Context, tx *sql.Tx) (bool, error) {
	var installed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.sqlite_schema
	 WHERE type='table' AND name='delivery_artifact_read_grants')`).Scan(&installed)
	return installed, err
}

func insertDeliveryArtifactReadGrant(ctx context.Context, tx *sql.Tx, m domain.Message, pin domain.ArtifactVersionRef) error {
	if len(m.Deliveries) != 1 {
		return ErrInvalid
	}
	// Check the actual group and exact version in the message transaction. Do
	// not touch the blob during a send or seek a version by Artifact head.
	if _, err := artifactVersionInTx(ctx, tx, m.GroupID, pin.ArtifactID, pin.VersionID); err != nil {
		return err
	}
	d := m.Deliveries[0]
	_, err := tx.ExecContext(ctx, `INSERT INTO delivery_artifact_read_grants
	 (delivery_id,message_id,group_id,topic_id,conversation_id,recipient_id,artifact_id,version_id)
	 VALUES(?,?,?,?,?,?,?,?)`, d.ID, m.ID, m.GroupID, m.TopicID, d.ConversationID, d.RecipientID, pin.ArtifactID, pin.VersionID)
	return err
}

// Rejoin the grant to the concrete delivery and its immutable local-user
// command source, not to sender text or a same-topic conversation's latest send.
const deliveryArtifactReadGrantSelect = `SELECT g.artifact_id,g.version_id,m.content,s.payload FROM delivery_artifact_read_grants g
 JOIN deliveries d ON (d.id,d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)=
    (g.delivery_id,g.message_id,g.group_id,g.topic_id,g.conversation_id,g.recipient_id)
 JOIN messages m ON (m.id,m.group_id,m.topic_id)=(d.message_id,d.group_id,d.topic_id) AND m.sender_id='user'
 JOIN message_command_sources s ON s.message_id=m.id AND s.kind='local_user_chat'`

const singleGrantDelivery = `d.state='queued' AND NOT EXISTS(
 SELECT 1 FROM deliveries other WHERE other.message_id=d.message_id AND other.id<>d.id)`

func scanDeliveryArtifactReadGrant(row scanner, groupID, topicID, recipientID string) (domain.ArtifactVersionRef, error) {
	var pin domain.ArtifactVersionRef
	var body string
	var payload []byte
	if err := row.Scan(&pin.ArtifactID, &pin.VersionID, &body, &payload); err != nil {
		return domain.ArtifactVersionRef{}, err
	}
	// A local-user sender and a single actual delivery alone are insufficient:
	// the immutable command must explicitly approve this recipient and version.
	proof := MessageRequest{GroupID: groupID, TopicID: topicID, Content: body,
		RecipientIDs: []string{recipientID}, ArtifactReadGrant: &pin}
	if !ValidArtifactReadPin(pin) || !bytes.Equal(payload, proof.payload()) {
		return domain.ArtifactVersionRef{}, ErrMemberToolDenied
	}
	return pin, nil
}

// QueuedDeliveryArtifactReadGrant is a preparation snapshot only. A zero ref
// means no authority; it never confers a capability before a real claim.
func (s *Store) QueuedDeliveryArtifactReadGrant(ctx context.Context, groupID string, delivery domain.Delivery, topicID string) (domain.ArtifactVersionRef, error) {
	var pin domain.ArtifactVersionRef
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return pin, err
	}
	defer tx.Rollback()
	if installed, err := artifactReadGrantInstalled(ctx, tx); err != nil || !installed {
		return pin, err
	}
	row := tx.QueryRowContext(ctx, deliveryArtifactReadGrantSelect+`
	 WHERE (d.id,d.group_id,d.message_id,d.conversation_id,d.recipient_id,d.topic_id)=(?,?,?,?,?,?)
	 AND d.turn_id='' AND `+singleGrantDelivery,
		delivery.ID, groupID, delivery.MessageID, delivery.ConversationID, delivery.RecipientID, topicID)
	pin, err = scanDeliveryArtifactReadGrant(row, groupID, topicID, delivery.RecipientID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ArtifactVersionRef{}, nil
	}
	if err != nil {
		return domain.ArtifactVersionRef{}, err
	}
	return pin, tx.Commit()
}

// ValidateClaimedDeliveryArtifactReadGrant re-proves the prepared pin inside a
// normal member claim fence, including the still-fresh turn's delivery backlink.
// This checks durable approval, not bytes or a request made by the model.
func (s *Store) ValidateClaimedDeliveryArtifactReadGrant(ctx context.Context, scope MemberToolScope, deliveryID string, pin domain.ArtifactVersionRef) error {
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return ErrMemberToolDenied
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, deliveryArtifactReadGrantSelect+`
	 JOIN turns t ON (t.id,t.delivery_id,t.group_id,t.conversation_id,t.member_id)=
	    (d.turn_id,d.id,d.group_id,d.conversation_id,d.recipient_id)
	 WHERE (t.id,d.id,d.group_id,d.conversation_id,d.recipient_id,d.topic_id)=(?,?,?,?,?,?)
	 AND t.state='starting' AND t.native_session_id='' AND t.accepted_at IS NULL AND t.candidate_text=''
	 AND `+singleGrantDelivery,
		scope.Claim.TurnID, deliveryID, scope.Claim.GroupID, scope.ConversationID, scope.MemberID, scope.TopicID)
	current, err := scanDeliveryArtifactReadGrant(row, scope.Claim.GroupID, scope.TopicID, scope.MemberID)
	if err != nil || current != pin || tx.Commit() != nil {
		return ErrMemberToolDenied
	}
	return nil
}
