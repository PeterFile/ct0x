package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

var ErrMemberCapacity = errors.New("群组成员容量已满，请稍后重试")

type MemberToolCreateInput struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Reason string `json:"reason"`
}

// MemberToolCreatePolicy is supplied only by trusted runtime grants.
type MemberToolCreatePolicy struct {
	ProviderID string `json:"-"`
	Model      string `json:"-"`
	MaxMembers int    `json:"-"`
}

type MemberToolCreateResult struct {
	Member      domain.Member   `json:"member"`
	Reused      bool            `json:"reused"`
	JoinMessage *domain.Message `json:"join_message,omitempty"`
}

// CreateMemberForTool reuses exact active role/provider/model/empty-workspace matches
// before checking capacity. This is deterministic reuse, not semantic skill search.
func (s *Store) CreateMemberForTool(ctx context.Context, scope MemberToolScope, input MemberToolCreateInput, policy MemberToolCreatePolicy, key string) (MemberToolCreateResult, error) {
	const action = "自动增员失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return MemberToolCreateResult{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	if !memberToolKeyValid(key) || policy.MaxMembers < 1 || !memberToolIDsValid(policy.ProviderID, policy.Model) || !utf8.ValidString(policy.ProviderID) || !utf8.ValidString(policy.Model) {
		return MemberToolCreateResult{}, ErrInvalid
	}
	mutation := mutationScope{operation: "member_tool_create_member", actorID: scope.MemberID, groupID: scope.Claim.GroupID, key: key}
	// Topic is part of the request context; capacity is not part of receipt identity.
	payload := mutationPayload(input.Name, input.Role, input.Reason, policy.ProviderID, policy.Model, scope.TopicID)
	id, replay, err := reserveMutation(ctx, tx, mutation, payload)
	if err != nil {
		return MemberToolCreateResult{}, memberToolOperationError(action, err)
	}
	var result MemberToolCreateResult
	if replay {
		result, err = memberToolCreateReceiptInTx(ctx, tx, scope.Claim.GroupID, id)
	} else {
		if !memberToolTextValid(input.Name, 160) || !memberToolTextValid(input.Role, 8192) || !memberToolTextValid(input.Reason, 16384) {
			return MemberToolCreateResult{}, ErrInvalid
		}
		result, err = automaticMemberInTx(ctx, tx, id, scope, input, policy)
		if err == nil {
			joinID := ""
			if result.JoinMessage != nil {
				joinID = result.JoinMessage.ID
			}
			// The operation-scoped receipt is [memberID, joinMessageID]; empty join means reuse.
			receipt, _ := json.Marshal([2]string{result.Member.ID, joinID})
			_, err = tx.ExecContext(ctx, `UPDATE mutation_idempotency SET record_id=?
			 WHERE operation=? AND actor_id=? AND group_id=? AND idempotency_key=?`,
				string(receipt), mutation.operation, mutation.actorID, mutation.groupID, mutation.key)
		}
	}
	if err != nil {
		return MemberToolCreateResult{}, memberToolOperationError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return MemberToolCreateResult{}, memberToolOperationError(action, err)
	}
	return result, nil
}

func automaticMemberInTx(ctx context.Context, tx *sql.Tx, id string, scope MemberToolScope, input MemberToolCreateInput, policy MemberToolCreatePolicy) (MemberToolCreateResult, error) {
	member, err := scanMember(tx.QueryRowContext(ctx, `SELECT `+memberColumns+` FROM members
	 WHERE group_id=? AND status='active' AND role=? AND provider_id=? AND model=? AND workspace=''
	 ORDER BY created_at,id LIMIT 1`, scope.Claim.GroupID, input.Role, policy.ProviderID, policy.Model))
	if err == nil {
		return MemberToolCreateResult{Member: member, Reused: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MemberToolCreateResult{}, err
	}
	var active int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM members WHERE group_id=? AND status='active'", scope.Claim.GroupID).Scan(&active); err != nil {
		return MemberToolCreateResult{}, err
	}
	if active >= policy.MaxMembers {
		return MemberToolCreateResult{}, ErrMemberCapacity
	}
	member, err = insertMemberInTx(ctx, tx, id, CreateMemberInput{
		GroupID: scope.Claim.GroupID, Name: input.Name, Role: input.Role, ProviderID: policy.ProviderID, Model: policy.Model,
		CreatorID: scope.MemberID, OwnerID: "user",
	})
	if err != nil {
		return MemberToolCreateResult{}, err
	}
	recipients := []string{member.ID}
	join, err := createMessageInTx(ctx, tx, MessageRequest{
		GroupID: scope.Claim.GroupID, TopicID: scope.TopicID, ActorID: scope.MemberID, RecipientIDs: recipients,
		Content: fmt.Sprintf("新成员「%s」加入讨论。\n背景与问题：\n%s", member.Name, input.Reason),
	}, recipients)
	if err != nil {
		return MemberToolCreateResult{}, err
	}
	return MemberToolCreateResult{Member: member, JoinMessage: &join}, nil
}

// insertMemberInTx only inserts a validated, trusted member. The caller owns
// authorization, selection, capacity, idempotency and the surrounding transaction.
func insertMemberInTx(ctx context.Context, tx *sql.Tx, id string, input CreateMemberInput) (domain.Member, error) {
	return scanMember(tx.QueryRowContext(ctx,
		`INSERT INTO members(`+memberColumns+`) SELECT ?,?,?,?,?,?,?,?,?,?,?
		 WHERE EXISTS(SELECT 1 FROM groups WHERE id=?) RETURNING `+memberColumns,
		id, input.GroupID, input.Name, input.Role, input.ProviderID, input.Model,
		input.CreatorID, input.OwnerID, domain.MemberStatusActive, input.Workspace,
		time.Now().UTC().Format(timestampLayout), input.GroupID))
}

func memberToolCreateReceiptInTx(ctx context.Context, tx *sql.Tx, groupID, receipt string) (MemberToolCreateResult, error) {
	var refs []string
	if json.Unmarshal([]byte(receipt), &refs) != nil || len(refs) != 2 || refs[0] == "" {
		return MemberToolCreateResult{}, errors.New("增员回执无效")
	}
	member, err := scanMember(tx.QueryRowContext(ctx, "SELECT "+memberColumns+" FROM members WHERE group_id=? AND id=?", groupID, refs[0]))
	if err != nil {
		return MemberToolCreateResult{}, err
	}
	result := MemberToolCreateResult{Member: member, Reused: refs[1] == ""}
	if refs[1] != "" {
		join, err := messageInTx(ctx, tx, groupID, refs[1])
		if err != nil {
			return MemberToolCreateResult{}, err
		}
		result.JoinMessage = &join
	}
	return result, nil
}

func memberToolTextValid(text string, maximum int) bool {
	return len(text) <= maximum && strings.TrimSpace(text) != "" && utf8.ValidString(text) && !strings.ContainsRune(text, '\x00')
}
