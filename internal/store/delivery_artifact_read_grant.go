package store

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

var (
	ErrArtifactReadGrantUnavailable = errors.New("数据库尚未启用消息成果读取许可")
	ErrArtifactReadGrantIncomplete  = errors.New("消息成果读取许可的结构或证据不完整")
)

const artifactReadGrantMode = "explicit_artifact_read_grant/v1"

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

//go:embed delivery_artifact_read_grant.sql
var deliveryArtifactReadGrantSQL string

// Use the existing definitions for the shared guards and FK indexes. Find
// the latter by name in the canonical source, independent of registry order.
var artifactReadGrantPrerequisiteSQL = func() string {
	ddl := messageSequenceSchemaV8 + chatAuthoritySchemaV17 + artifactSchemaV6
	for _, migration := range migrations {
		if strings.Contains(migration, "\nCREATE UNIQUE INDEX deliveries_turn_identity ") ||
			strings.Contains(migration, "\nCREATE UNIQUE INDEX conversations_delivery_identity ") {
			ddl += "\n" + migration + "\n"
		}
	}
	return ddl
}()

// Detect the private fragment in the transaction that uses it. Match the
// CREATE statements as stored by SQLite (no leading comments or final ';'),
// not just object names. This caches only the expected source, not the check.
func artifactReadGrantStorage(ctx context.Context, tx *sql.Tx) (installed, hasSource bool, err error) {
	var present bool
	err = tx.QueryRowContext(ctx, artifactReadGrantSchemaCheck, deliveryArtifactReadGrantSQL, artifactReadGrantPrerequisiteSQL).
		Scan(&present, &hasSource, &installed)
	if err == nil && present && !installed {
		err = ErrArtifactReadGrantIncomplete
	}
	return
}

const artifactReadGrantSchemaCheck = `WITH required(type,tbl_name,name) AS (VALUES
 ('table','delivery_artifact_read_grants','delivery_artifact_read_grants'),
 ('trigger','delivery_artifact_read_grants','delivery_artifact_read_grants_insert'),
 ('trigger','delivery_artifact_read_grants','delivery_artifact_read_grants_update'),
 ('trigger','delivery_artifact_read_grants','delivery_artifact_read_grants_delete'),
 ('trigger','deliveries','delivery_artifact_read_grant_deliveries_insert'),
 ('trigger','deliveries','delivery_artifact_read_grant_deliveries_identity'),
 ('trigger','deliveries','delivery_artifact_read_grant_deliveries_delete'),
 ('index','deliveries','deliveries_turn_identity'),
 ('index','conversations','conversations_delivery_identity'),
 ('trigger','messages','messages_insert'),
 ('trigger','messages','messages_identity'),
 ('trigger','messages','messages_no_delete'),
 ('trigger','message_command_sources','message_command_sources_insert'),
 ('trigger','message_command_sources','message_command_sources_update'),
 ('trigger','message_command_sources','message_command_sources_delete'),
 ('trigger','artifact_versions','artifact_versions_insert'),
 ('trigger','artifact_versions','artifact_versions_no_update'),
 ('trigger','artifact_versions','artifact_versions_no_delete')
 ), storage(present,source) AS (SELECT
    EXISTS(SELECT 1 FROM main.sqlite_schema WHERE name GLOB 'delivery_artifact_read_grant*'),
    EXISTS(SELECT 1 FROM main.sqlite_schema WHERE type='table' AND name='message_command_sources'))
 SELECT present,source,CASE WHEN present THEN source AND (SELECT foreign_keys FROM pragma_foreign_keys)=1
   AND NOT EXISTS(SELECT 1 FROM required r LEFT JOIN main.sqlite_schema s USING(type,tbl_name,name)
     WHERE s.sql IS NULL OR instr(char(10)||CASE WHEN r.name GLOB 'delivery_artifact_read_grant*' THEN ?1 ELSE ?2 END||char(10),
       char(10)||s.sql||';'||char(10))=0)
   AND EXISTS(SELECT 1 FROM pragma_table_list('delivery_artifact_read_grants') WHERE schema='main' AND wr=1 AND ncol=8)
   AND (SELECT count(*) FROM pragma_table_xinfo('delivery_artifact_read_grants','main')
     WHERE type='TEXT' AND "notnull"=1 AND hidden=0 AND pk=(name='delivery_id')
     AND name IN ('delivery_id','message_id','group_id','topic_id','conversation_id','recipient_id','artifact_id','version_id'))=8
   AND EXISTS(SELECT 1 FROM pragma_index_list('delivery_artifact_read_grants','main') i
     WHERE i."unique"=1 AND i.partial=0 AND (SELECT count(*)=1 AND min(name)='message_id' FROM pragma_index_info(i.name,'main')))
   AND (SELECT count(*) FROM (SELECT DISTINCT target,child,parent FROM (
     SELECT "table" AS target, group_concat("from" ORDER BY seq) AS child, group_concat("to" ORDER BY seq) AS parent
     FROM pragma_foreign_key_list('delivery_artifact_read_grants','main')
     GROUP BY id HAVING on_update='NO ACTION' AND on_delete='NO ACTION')
     WHERE (target,child,parent) IN (
       ('deliveries','delivery_id,group_id,conversation_id,recipient_id','id,group_id,conversation_id,recipient_id'),
       ('messages','message_id,group_id,topic_id','id,group_id,topic_id'),
       ('message_command_sources','message_id','message_id'),
       ('artifact_versions','version_id,artifact_id,group_id','id,artifact_id,group_id'))))=4
 ELSE 0 END FROM storage`

// The fourth length-prefixed field is a mode, never text searched for a
// marker. Only one stored command is needed, including sends without a key.
func hasArtifactReadGrantMode(payload []byte) bool {
	for range 3 {
		if len(payload) < 8 {
			return false
		}
		n := binary.BigEndian.Uint64(payload[:8])
		payload = payload[8:]
		if n > uint64(len(payload)) {
			return false
		}
		payload = payload[n:]
	}
	return len(payload) >= 8+len(artifactReadGrantMode) && binary.BigEndian.Uint64(payload[:8]) == uint64(len(artifactReadGrantMode)) &&
		string(payload[8:8+len(artifactReadGrantMode)]) == artifactReadGrantMode
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
	result, err := tx.ExecContext(ctx, `INSERT INTO delivery_artifact_read_grants
	 (delivery_id,message_id,group_id,topic_id,conversation_id,recipient_id,artifact_id,version_id)
	 VALUES(?,?,?,?,?,?,?,?)`, d.ID, m.ID, m.GroupID, m.TopicID, d.ConversationID, d.RecipientID, pin.ArtifactID, pin.VersionID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrArtifactReadGrantIncomplete
	}
	return err
}

// Rejoin the grant to the concrete delivery and its immutable local-user
// command source, not to sender text or a same-topic conversation's latest send.
const deliveryArtifactReadGrantSelect = `SELECT g.artifact_id,g.version_id,m.content,s.payload FROM delivery_artifact_read_grants g
 JOIN deliveries d ON (d.id,d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)=
    (g.delivery_id,g.message_id,g.group_id,g.topic_id,g.conversation_id,g.recipient_id)
 JOIN messages m ON (m.id,m.group_id,m.topic_id)=(d.message_id,d.group_id,d.topic_id) AND m.sender_id='user'
 JOIN message_command_sources s ON s.message_id=m.id AND s.kind='local_user_chat' AND s.turn_id IS NULL AND s.claim_digest IS NULL
 JOIN artifact_versions v ON (v.id,v.artifact_id,v.group_id)=(g.version_id,g.artifact_id,g.group_id)`

const singleGrantDelivery = `NOT EXISTS(
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
	if !ValidArtifactReadPin(pin) || !utf8.ValidString(body) || !bytes.Equal(payload, proof.payload()) {
		return domain.ArtifactVersionRef{}, ErrMemberToolDenied
	}
	return pin, nil
}

// Grant replays use the same message/recipient/pin and canonical request as the
// original write, not just the receipt. A settled delivery can still replay.
func verifyArtifactReadGrantReplay(ctx context.Context, tx *sql.Tx, message domain.Message, request MessageRequest, payload []byte) error {
	if len(message.Deliveries) != 1 {
		return ErrArtifactReadGrantIncomplete
	}
	d := message.Deliveries[0]
	row := tx.QueryRowContext(ctx, deliveryArtifactReadGrantSelect+`
	 WHERE (d.id,d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)=(?,?,?,?,?,?)
	 AND s.payload=? AND s.idempotency_key=? AND `+singleGrantDelivery,
		d.ID, message.ID, request.GroupID, request.TopicID, d.ConversationID, request.RecipientIDs[0], payload, request.IdempotencyKey)
	pin, err := scanDeliveryArtifactReadGrant(row, request.GroupID, request.TopicID, request.RecipientIDs[0])
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrMemberToolDenied) || err == nil && pin != *request.ArtifactReadGrant {
		return ErrArtifactReadGrantIncomplete
	}
	return err
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
	installed, hasSource, err := artifactReadGrantStorage(ctx, tx)
	if err != nil {
		return pin, err
	}
	if installed {
		row := tx.QueryRowContext(ctx, deliveryArtifactReadGrantSelect+`
		 WHERE (d.id,d.group_id,d.message_id,d.conversation_id,d.recipient_id,d.topic_id)=(?,?,?,?,?,?)
		 AND d.state='queued' AND d.turn_id='' AND `+singleGrantDelivery,
			delivery.ID, groupID, delivery.MessageID, delivery.ConversationID, delivery.RecipientID, topicID)
		pin, err = scanDeliveryArtifactReadGrant(row, groupID, topicID, delivery.RecipientID)
		if !errors.Is(err, sql.ErrNoRows) {
			if err != nil {
				return domain.ArtifactVersionRef{}, err
			}
			return pin, tx.Commit()
		}
	}
	// A failed join is not necessarily an ordinary send. Distinguish a
	// mismatched grant or a command whose promised row/fragment is missing.
	var payload []byte
	var hasGrant bool
	switch {
	case installed:
		err = tx.QueryRowContext(ctx, `SELECT (SELECT payload FROM message_command_sources WHERE message_id=?),
		 EXISTS(SELECT 1 FROM delivery_artifact_read_grants WHERE message_id=? OR delivery_id=?)`,
			delivery.MessageID, delivery.MessageID, delivery.ID).Scan(&payload, &hasGrant)
	case hasSource:
		err = tx.QueryRowContext(ctx, `SELECT (SELECT payload FROM message_command_sources WHERE message_id=?)`, delivery.MessageID).Scan(&payload)
	}
	if err != nil {
		return pin, err
	}
	if hasGrant || hasArtifactReadGrantMode(payload) {
		return pin, ErrArtifactReadGrantIncomplete
	}
	// If offline damage erases both the grant and its immutable command
	// evidence, this Delivery is indistinguishable here from a legacy send.
	// A keyed replay still has to re-prove the grant; absence never grants it.
	return pin, tx.Commit()
}

// ValidateClaimedDeliveryArtifactReadGrant re-proves the prepared pin inside a
// normal member claim fence, including the still-fresh turn's delivery backlink.
func (s *Store) ValidateClaimedDeliveryArtifactReadGrant(ctx context.Context, scope MemberToolScope, deliveryID string, pin domain.ArtifactVersionRef) error {
	return s.validateDeliveryArtifactReadGrant(ctx, scope, deliveryID, pin, true)
}

// ValidateDeliveryArtifactReadGrantForTool is the final fence after blob I/O.
// Unlike issuance, it also admits a running turn on its accepted delivery.
func (s *Store) ValidateDeliveryArtifactReadGrantForTool(ctx context.Context, scope MemberToolScope, deliveryID string, pin domain.ArtifactVersionRef) error {
	return s.validateDeliveryArtifactReadGrant(ctx, scope, deliveryID, pin, false)
}

func (s *Store) validateDeliveryArtifactReadGrant(ctx context.Context, scope MemberToolScope, deliveryID string, pin domain.ArtifactVersionRef, fresh bool) error {
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return ErrMemberToolDenied
	}
	defer tx.Rollback()
	if requireDeliveryArtifactReadGrantInTx(ctx, tx, scope, deliveryID, pin, fresh) != nil || tx.Commit() != nil {
		return ErrMemberToolDenied
	}
	return nil
}

// Caller must already hold the live member claim fence in this transaction.
func requireDeliveryArtifactReadGrantInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, deliveryID string, pin domain.ArtifactVersionRef, fresh bool) error {
	installed, _, err := artifactReadGrantStorage(ctx, tx)
	if err != nil || !installed || deliveryID == "" || !ValidArtifactReadPin(pin) {
		return ErrMemberToolDenied
	}
	state := ` AND d.state IN ('queued','accepted')`
	if fresh {
		state = ` AND d.state='queued' AND t.state='starting' AND t.native_session_id='' AND t.accepted_at IS NULL AND t.candidate_text=''`
	}
	row := tx.QueryRowContext(ctx, deliveryArtifactReadGrantSelect+`
	 JOIN turns t ON (t.id,t.delivery_id,t.group_id,t.conversation_id,t.member_id)=
	    (d.turn_id,d.id,d.group_id,d.conversation_id,d.recipient_id)
	 WHERE (t.id,d.id,d.group_id,d.conversation_id,d.recipient_id,d.topic_id)=(?,?,?,?,?,?)
	 AND `+singleGrantDelivery+state,
		scope.Claim.TurnID, deliveryID, scope.Claim.GroupID, scope.ConversationID, scope.MemberID, scope.TopicID)
	current, err := scanDeliveryArtifactReadGrant(row, scope.Claim.GroupID, scope.TopicID, scope.MemberID)
	if err != nil || current != pin {
		return ErrMemberToolDenied
	}
	return nil
}
