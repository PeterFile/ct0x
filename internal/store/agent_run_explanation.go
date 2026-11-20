package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"
)

const AgentRunExplanationMaxBytes = 1024

var (
	ErrAgentRunExplanationUnavailable = errors.New("当前数据库不支持公开运行说明")
	ErrAgentRunExplanationUnknown     = errors.New("运行说明保存结果未确认；请保留原幂等键和原文用于对账")
)

// AgentRunExplanation is a member-published statement, not evidence of tool
// execution or settlement. It contains no storage or invocation metadata.
type AgentRunExplanation struct {
	Text string `json:"text"`
}

// AgentRunExplanationSupported checks storage capability, not caller authority.
// The append migration's version depends on landing order.
func (s *Store) AgentRunExplanationSupported(ctx context.Context) (bool, error) {
	supported, err := agentRunExplanationSupported(ctx, s.db)
	if err != nil {
		return false, queueError("检查公开运行说明支持失败", err)
	}
	return supported, nil
}

func agentRunExplanationSupported(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (bool, error) {
	var supported bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.sqlite_schema
	 WHERE type='table' AND name='agent_run_explanations')`).Scan(&supported)
	return supported, err
}

// PublishAgentRunExplanation accepts only deliberately public text and a
// trusted request key. The native caller supplies the bound scope, not model
// arguments. There is one immutable statement per Turn and no chat effect.
func (s *Store) PublishAgentRunExplanation(ctx context.Context, scope MemberToolScope, text, key string) (AgentRunEntry, error) {
	const action = "发布公开运行说明失败"
	// Reuse the member-scope write fence and transaction outcome handling.
	entry, err := taskWorkMutationTx(ctx, s, scope, action, func(tx *sql.Tx) (AgentRunEntry, error) {
		if !memberToolTextValid(text, AgentRunExplanationMaxBytes) || !memberToolKeyValid(key) {
			return AgentRunEntry{}, ErrInvalid
		}
		supported, err := agentRunExplanationSupported(ctx, tx)
		if err != nil {
			return AgentRunEntry{}, err
		}
		if !supported {
			return AgentRunEntry{}, ErrAgentRunExplanationUnavailable
		}

		claim := scope.Claim
		digest := sha256.Sum256([]byte(claim.Token))
		// A competing key for this Turn or an original key for another Turn
		// must not allocate another row. Serialize before this probe, even
		// on deferred SQLite transactions, via the member scope fence.
		_, err = tx.ExecContext(ctx, `INSERT INTO main.agent_run_explanations
		 (id,group_id,member_id,conversation_id,topic_id,claim_digest,idempotency_key,text,created_at)
		 SELECT ?,?,?,?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM main.agent_run_explanations
		 WHERE id=? OR (group_id,member_id,idempotency_key)=(?,?,?))`, claim.TurnID, claim.GroupID,
			scope.MemberID, scope.ConversationID, scope.TopicID, digest[:], key, text, time.Now().UTC().Format(timestampLayout),
			claim.TurnID, claim.GroupID, scope.MemberID, key)
		if err != nil {
			return AgentRunEntry{}, err
		}
		// Read back only the exact fact for this claim, key and text. A
		// visible Run timeline does not establish ownership of its key.
		entry, err := scanAgentRunExplanation(tx.QueryRowContext(ctx, agentRunExplanationSQL+`
		 AND (e.claim_digest,e.idempotency_key,e.text)=(?,?,?)`, claim.TurnID, claim.GroupID, scope.MemberID,
			scope.ConversationID, scope.TopicID, digest[:], key, text))
		if errors.Is(err, sql.ErrNoRows) {
			return AgentRunEntry{}, ErrConflict
		}
		if err != nil {
			return AgentRunEntry{}, err
		}
		if err := coordinationDecideLiveInTx(ctx, tx, scope); err != nil {
			return AgentRunEntry{}, err
		}
		return entry, nil
	})
	// An uncertain commit or rollback is not a conflict or a successful
	// publication. Reconcile only with the same key and unchanged text.
	switch {
	case errors.Is(err, ErrTaskWorkMutationUnknown):
		return AgentRunEntry{}, ErrAgentRunExplanationUnknown
	case errors.Is(err, ErrAgentRunExplanationUnavailable):
		return AgentRunEntry{}, ErrAgentRunExplanationUnavailable
	default:
		return entry, err
	}
}

var agentRunExplanationSQL = `SELECT e.id,` + agentRunPreviewSQL("e.text", AgentRunExplanationMaxBytes) + `,e.created_at
 FROM main.agent_run_explanations e WHERE (e.id,e.group_id,e.member_id,e.conversation_id,e.topic_id)=(?,?,?,?,?)`

func scanAgentRunExplanation(row scanner) (AgentRunEntry, error) {
	var id, text, stamp string
	if err := row.Scan(&id, &text, &stamp); err != nil {
		return AgentRunEntry{}, err
	}
	at, err := time.Parse(timestampLayout, stamp)
	if err != nil {
		return AgentRunEntry{}, err
	}
	if !memberToolTextValid(text, AgentRunExplanationMaxBytes) {
		return AgentRunEntry{}, ErrInvalid
	}
	return AgentRunEntry{ID: runExplanationPrefix + id, Kind: "public_explanation", CreatedAt: at,
		PublicExplanation: &AgentRunExplanation{Text: text}}, nil
}

func (w agentRunWindow) explanation(ctx context.Context, tx *sql.Tx, ref agentRunRef) (*AgentRunEntry, error) {
	supported, err := agentRunExplanationSupported(ctx, tx)
	if err != nil || !supported {
		return nil, err
	}
	bound, args := w.bound("e", runExplanationPrefix, []any{ref.runID, ref.groupID, ref.memberID, ref.conversationID, ref.topicID})
	entry, err := scanAgentRunExplanation(tx.QueryRowContext(ctx, agentRunExplanationSQL+bound, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &entry, err
}
