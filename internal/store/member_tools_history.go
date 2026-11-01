package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrToolResultTooLarge rejects the whole encoded result, including a final newline.
var ErrToolResultTooLarge = errors.New("工具结果超过 65536 字节限制")

// MemberHistorySearchInput has no authority selectors. Limit defaults to 20, maximum 50.
type MemberHistorySearchInput struct {
	Query  string `json:"query"`
	Limit  int    `json:"limit"`
	Before string `json:"before"`
}

// MemberHistoryReadInput uses rune offsets; zero Length selects 1024, maximum 4096.
type MemberHistoryReadInput struct {
	MessageID string `json:"message_id"`
	Offset    int    `json:"offset"`
	Length    int    `json:"length"`
}

// MemberHistoryFragment is an exact Unicode rune range [Offset, NextOffset).
// ContentComplete is true only for a fragment starting at zero and reaching message end.
type MemberHistoryFragment struct {
	Offset          int    `json:"offset"`
	NextOffset      int    `json:"next_offset"`
	End             bool   `json:"end"`
	TotalRunes      int    `json:"total_runes"`
	Content         string `json:"content"`
	ContentComplete bool   `json:"content_complete"`
}

type MemberHistoryHit struct {
	MessageID string                `json:"message_id"`
	SenderID  string                `json:"sender_id"`
	CreatedAt time.Time             `json:"created_at"`
	Excerpt   MemberHistoryFragment `json:"excerpt"`
}

type MemberHistorySearchResult struct {
	Items      []MemberHistoryHit `json:"items"`
	NextCursor string             `json:"next_cursor"`
}

func (p MemberHistorySearchResult) MarshalJSON() ([]byte, error) {
	type page MemberHistorySearchResult
	if p.Items == nil {
		p.Items = []MemberHistoryHit{}
	}
	return json.Marshal(page(p))
}

type MemberHistoryReadResult struct {
	MessageID string `json:"message_id"`
	MemberHistoryFragment
}

// SearchHistoryForTool returns prefix excerpts, not match-centered snippets.
func (s *Store) SearchHistoryForTool(ctx context.Context, scope MemberToolScope, input MemberHistorySearchInput) (MemberHistorySearchResult, error) {
	const action = "搜索成员历史失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return MemberHistorySearchResult{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	if input.Limit < 0 || input.Limit > 50 {
		return MemberHistorySearchResult{}, fmt.Errorf("%w：搜索数量须在 1 到 50 之间", ErrInvalid)
	}
	if input.Limit == 0 {
		input.Limit = 20
	}
	page, err := searchHistoryInScopeTx(ctx, tx, scope.Claim.GroupID, HistoryQuery{
		Query: input.Query, TopicID: &scope.TopicID, Limit: input.Limit, Before: input.Before,
	})
	if err != nil {
		return MemberHistorySearchResult{}, memberToolOperationError(action, err)
	}
	result := MemberHistorySearchResult{Items: make([]MemberHistoryHit, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, message := range page.Items {
		if !utf8.ValidString(message.ID) || !utf8.ValidString(message.SenderID) {
			return MemberHistorySearchResult{}, fmt.Errorf("%w：历史消息标识不是有效 UTF-8", ErrInvalid)
		}
		excerpt, err := memberHistoryFragment(message.Content, 0, 256)
		if err != nil {
			return MemberHistorySearchResult{}, err
		}
		result.Items = append(result.Items, MemberHistoryHit{
			MessageID: message.ID, SenderID: message.SenderID, CreatedAt: message.CreatedAt, Excerpt: excerpt,
		})
	}
	if err := memberHistoryResultBudget(result); err != nil {
		return MemberHistorySearchResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return MemberHistorySearchResult{}, memberToolOperationError(action, err)
	}
	return result, nil
}

func (s *Store) ReadHistoryForTool(ctx context.Context, scope MemberToolScope, input MemberHistoryReadInput) (MemberHistoryReadResult, error) {
	const action = "读取成员历史失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return MemberHistoryReadResult{}, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	if !validHistoryText(input.MessageID, 256) || strings.TrimSpace(input.MessageID) == "" {
		return MemberHistoryReadResult{}, fmt.Errorf("%w：历史消息标识无效", ErrInvalid)
	}
	if input.Offset < 0 || input.Length < 0 || input.Length > 4096 {
		return MemberHistoryReadResult{}, fmt.Errorf("%w：偏移须非负，读取长度须在 1 到 4096 之间", ErrInvalid)
	}
	if input.Length == 0 {
		input.Length = 1024
	}
	message, err := readHistoryInScopeTx(ctx, tx, scope.Claim.GroupID, &scope.TopicID, input.MessageID)
	if err != nil {
		return MemberHistoryReadResult{}, memberToolOperationError(action, err)
	}
	fragment, err := memberHistoryFragment(message.Content, input.Offset, input.Length)
	if err != nil {
		return MemberHistoryReadResult{}, err
	}
	result := MemberHistoryReadResult{MessageID: message.ID, MemberHistoryFragment: fragment}
	if err := memberHistoryResultBudget(result); err != nil {
		return MemberHistoryReadResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return MemberHistoryReadResult{}, memberToolOperationError(action, err)
	}
	return result, nil
}

func memberHistoryFragment(content string, offset, length int) (MemberHistoryFragment, error) {
	// Validate the entire body before rune conversion to avoid silent replacement.
	if !utf8.ValidString(content) {
		return MemberHistoryFragment{}, fmt.Errorf("%w：历史消息正文不是有效 UTF-8", ErrInvalid)
	}
	runes := []rune(content)
	if offset > len(runes) {
		return MemberHistoryFragment{}, fmt.Errorf("%w：读取偏移超过消息字符总数", ErrInvalid)
	}
	next := offset + min(length, len(runes)-offset)
	end := next == len(runes)
	return MemberHistoryFragment{
		Offset: offset, NextOffset: next, End: end, TotalRunes: len(runes),
		Content: string(runes[offset:next]), ContentComplete: offset == 0 && end,
	}, nil
}

func memberHistoryResultBudget(result any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return errors.New("历史工具结果编码失败")
	}
	// Reserve the final newline used by JSON encoders, including at the boundary.
	if len(encoded)+1 > 65536 {
		return ErrToolResultTooLarge
	}
	return nil
}
