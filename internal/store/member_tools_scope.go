package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

var ErrMemberToolDenied = errors.New("成员工具授权无效或已失效")

// MemberToolScope is server-bound identity, never model or transport input.
// TopicID may be empty for the main channel; all other identities are required.
// Possessing a scope does not replace checking it inside the operation's transaction.
type MemberToolScope struct {
	Claim          TurnClaim `json:"-"`
	MemberID       string    `json:"-"`
	ConversationID string    `json:"-"`
	TopicID        string    `json:"-"`
}

const memberToolBinding = ` FROM turns t
 JOIN conversations c ON c.id=t.conversation_id AND c.group_id=t.group_id AND c.member_id=t.member_id
 JOIN members m ON m.id=t.member_id AND m.group_id=t.group_id AND m.status='active'
 WHERE t.group_id=? AND t.id=? AND t.claim_token=?
 AND t.state IN ('starting','running') AND t.cancel_requested_at IS NULL AND t.candidate_terminal=''`

// BindMemberTool derives identity only from an actual, currently authorized claim.
func (s *Store) BindMemberTool(ctx context.Context, claim TurnClaim) (MemberToolScope, error) {
	if !memberToolIDsValid(claim.GroupID, claim.TurnID, claim.Token) {
		return MemberToolScope{}, ErrMemberToolDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemberToolScope{}, ErrMemberToolDenied
	}
	defer tx.Rollback()
	scope := MemberToolScope{Claim: claim}
	err = tx.QueryRowContext(ctx, "SELECT t.member_id,t.conversation_id,c.topic_id"+memberToolBinding,
		claim.GroupID, claim.TurnID, claim.Token).Scan(&scope.MemberID, &scope.ConversationID, &scope.TopicID)
	if err != nil || !memberToolIDsValid(scope.MemberID, scope.ConversationID) {
		return MemberToolScope{}, ErrMemberToolDenied
	}
	if err := tx.Commit(); err != nil {
		return MemberToolScope{}, ErrMemberToolDenied
	}
	return scope, nil
}

// ValidateMemberTool is a current preflight check, not authorization for later work.
func (s *Store) ValidateMemberTool(ctx context.Context, scope MemberToolScope) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrMemberToolDenied
	}
	defer tx.Rollback()
	if err := memberToolScopeInTx(ctx, tx, scope); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return ErrMemberToolDenied
	}
	return nil
}

// memberToolScopeInTx must be the first statement in the transaction that performs
// the actual tool read/mutation, including receipt replays. The conditional write
// serializes lifecycle changes even on deferred SQLite transactions.
func memberToolScopeInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope) error {
	claim := scope.Claim
	if !memberToolIDsValid(claim.GroupID, claim.TurnID, claim.Token, scope.MemberID, scope.ConversationID) {
		return ErrMemberToolDenied
	}
	var allowed int
	err := tx.QueryRowContext(ctx, `UPDATE turns SET claim_token=claim_token
	 WHERE id IN (SELECT t.id`+memberToolBinding+` AND t.member_id=? AND t.conversation_id=? AND c.topic_id=?)
	 RETURNING 1`, claim.GroupID, claim.TurnID, claim.Token, scope.MemberID, scope.ConversationID, scope.TopicID).Scan(&allowed)
	if err != nil {
		return ErrMemberToolDenied
	}
	return nil
}

func memberToolIDsValid(ids ...string) bool {
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\x00\r\n") {
			return false
		}
	}
	return true
}
