package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"agentworkspace/internal/domain"
)

// The v27 source gate must already have checked typed discussion evidence.
// Here only the incoming body may match a clue. The verified publisher chain
// can veto it, never supply a missing phrase or act as a new authorization.
func chatInvestigationClueBodyInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, checkRecency bool) (string, error) {
	kind, id, err := messageSourceInTx(ctx, tx, c.MessageID)
	if err != nil {
		return "", err
	}
	if id == "" || kind != c.SourceKind || id != c.SourceID {
		return "", ErrMemberToolDenied
	}

	const maxBodyBytes = 1 << 20 // The public JSON body cap; never match a truncated prefix.
	const maxRelayHops = 64      // The structured discussion policy's maximum targeted wakes.
	var incoming string
	for hops := 0; c.SourceKind == "local_user_chat" || c.SourceKind == "member"; hops++ {
		var body sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(m.content AS BLOB))<=? THEN m.content END
		 FROM messages m JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(m.id,m.group_id,m.topic_id)
		 WHERE (m.id,m.group_id,m.topic_id,l.sequence)=(?,?,?,?)`, maxBodyBytes, c.MessageID, c.GroupID, c.TopicID, c.Generation).Scan(&body)
		if err != nil {
			return "", err
		}
		if !body.Valid || !memberToolTextValid(body.String, maxBodyBytes) {
			return "", chatDeny("preparation_scope_mismatch", fmt.Sprintf("消息 %s（来源 %s）的原文超出核对上限或不是有效文本。", c.MessageID, c.SourceID),
				"请用一条简短的新消息说明线索，可以吗？")
		}
		for _, limit := range [...]string{"只讨论", "仅讨论"} {
			if strings.Contains(body.String, limit) {
				return "", chatDeny("discussion_only", fmt.Sprintf("已核验的消息 %s（来源 %s）原文含「%s」，本次仅作讨论。", c.MessageID, c.SourceID, limit),
					"是否发出一条新消息，明确现在需要开展哪项工作？")
			}
		}
		if hops == 0 {
			incoming = body.String
		}
		if c.SourceKind == "local_user_chat" {
			if checkRecency && hops > 0 {
				stale, err := chatSourceSupersededInTx(ctx, tx, c.ThreadRef, c.Generation)
				if err != nil {
					return "", err
				}
				if stale {
					return "", chatStaleSource()
				}
			}
			break
		}
		if hops == maxRelayHops {
			return "", ErrMemberToolDenied
		}
		c, err = chatDiscussionMemberParentInTx(ctx, tx, c)
		if err != nil {
			return "", err
		}
	}
	return incoming, nil
}
