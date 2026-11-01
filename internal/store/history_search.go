package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// HistoryQuery searches literal content after strings.TrimSpace, without Unicode
// normalization. Query must be valid UTF-8 with no NUL: 1..256 runes, at most
// 1024 normalized bytes, and at most 4096 raw bytes.
// TopicID is exact (UTF-8, no NUL, at most 256 bytes): nil is trusted local-user
// access to all group topics; a pointer to "" is the main topic.
// Limit defaults to 50 and is capped at 200. Before is scoped pagination context,
// not authentication; it must not be used to authorize a member tool.
type HistoryQuery struct {
	Query   string  `json:"query"`
	TopicID *string `json:"topic_id"`
	Limit   int     `json:"limit"`
	Before  string  `json:"before"`
}

// HistoryPage carries the same opaque cursor through store, tool, and HTTP reads.
type HistoryPage struct {
	Items      []domain.Message `json:"items"`
	NextCursor string           `json:"next_cursor"`
}

func (p HistoryPage) MarshalJSON() ([]byte, error) {
	type page HistoryPage
	if p.Items == nil {
		p.Items = []domain.Message{}
	}
	return json.Marshal(page(p))
}

// SearchHistory uses stock SQLite LIKE: ASCII is case-insensitive, non-ASCII is
// case-sensitive, and Chinese literal substrings work without normalization.
// Contains-LIKE may scan the group's rows; LIMIT bounds materialization, not SQL work.
func (s *Store) SearchHistory(ctx context.Context, groupID string, query HistoryQuery) (HistoryPage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return HistoryPage{}, storeError("搜索历史失败", err)
	}
	defer tx.Rollback()
	page, err := searchHistoryInTx(ctx, tx, groupID, query)
	if err != nil {
		return HistoryPage{}, storeError("搜索历史失败", err)
	}
	if err := tx.Commit(); err != nil {
		return HistoryPage{}, storeError("搜索历史失败", err)
	}
	return page, nil
}

// searchHistoryInTx owns common validation and reads, not run capability checks.
// A member wrapper must authorize in this transaction and force the bound topic.
func searchHistoryInTx(ctx context.Context, tx *sql.Tx, groupID string, q HistoryQuery) (HistoryPage, error) {
	if !validHistoryText(q.Query, 4096) {
		return HistoryPage{}, fmt.Errorf("%w：搜索词须为有效 UTF-8、无 NUL 且原始长度不超过 4096 字节", ErrInvalid)
	}
	q.Query = strings.TrimSpace(q.Query)
	if len(q.Query) == 0 || len(q.Query) > 1024 || utf8.RuneCountInString(q.Query) > 256 {
		return HistoryPage{}, fmt.Errorf("%w：搜索词须为 1 到 256 个字符且不超过 1024 字节", ErrInvalid)
	}
	if err := validateHistoryTopic(q.TopicID); err != nil {
		return HistoryPage{}, err
	}
	limit, err := listLimit(ListOptions{Limit: q.Limit})
	if err != nil {
		return HistoryPage{}, err
	}
	scope := historySearchScope(groupID, q)
	before, err := decodeHistoryCursor(q.Before, scope)
	if err != nil {
		return HistoryPage{}, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM groups WHERE id=?", groupID).Scan(&exists); err != nil {
		return HistoryPage{}, err
	}
	statement := "SELECT " + messageColumns + " FROM messages WHERE group_id=?"
	args := []any{groupID}
	if q.TopicID != nil {
		statement += " AND topic_id=?"
		args = append(args, *q.TopicID)
	}
	statement += ` AND content LIKE ? ESCAPE '\'`
	args = append(args, "%"+historyLikeEscaper.Replace(q.Query)+"%")
	if before != nil {
		statement += " AND (created_at, id) < (?, ?)"
		args = append(args, before.CreatedAt.Format(timestampLayout), before.ID)
	}
	statement += " ORDER BY created_at DESC, id DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, statement, append(args, limit+1)...)
	if err != nil {
		return HistoryPage{}, err
	}
	defer rows.Close()
	page := HistoryPage{Items: make([]domain.Message, 0, limit+1)}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return HistoryPage{}, err
		}
		page.Items = append(page.Items, m)
	}
	if err := rows.Err(); err != nil {
		return HistoryPage{}, err
	}
	if err := rows.Close(); err != nil {
		return HistoryPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = encodeHistoryCursor(scope, page.Items[limit-1])
	}
	for i := range page.Items {
		if err := messageDeliveries(ctx, tx, &page.Items[i]); err != nil {
			return HistoryPage{}, err
		}
	}
	return page, nil
}

// ReadHistory preserves GetMessage visibility for nil TopicID and otherwise
// filters the exact topic in SQL before materializing content or deliveries.
func (s *Store) ReadHistory(ctx context.Context, groupID string, topicID *string, messageID string) (domain.Message, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.Message{}, storeError("读取历史失败", err)
	}
	defer tx.Rollback()
	m, err := readHistoryInTx(ctx, tx, groupID, topicID, messageID)
	if err != nil {
		return domain.Message{}, storeError("读取历史失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Message{}, storeError("读取历史失败", err)
	}
	return m, nil
}

// readHistoryInTx shares the caller's transaction, without authorizing a run.
func readHistoryInTx(ctx context.Context, tx *sql.Tx, groupID string, topicID *string, messageID string) (domain.Message, error) {
	if err := validateHistoryTopic(topicID); err != nil {
		return domain.Message{}, err
	}
	if topicID == nil {
		return messageInTx(ctx, tx, groupID, messageID)
	}
	m, err := scanMessage(tx.QueryRowContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE group_id=? AND topic_id=? AND id=?", groupID, *topicID, messageID))
	if err == nil {
		err = messageDeliveries(ctx, tx, &m)
	}
	return m, err
}

var historyLikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func validHistoryText(value string, maximum int) bool {
	return len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validateHistoryTopic(topic *string) error {
	if topic != nil && !validHistoryText(*topic, 256) {
		return fmt.Errorf("%w：话题须为有效 UTF-8、无 NUL 且不超过 256 字节", ErrInvalid)
	}
	return nil
}

// Length-prefixed fields bind exact bytes and distinguish all topics from "".
func historySearchScope(groupID string, q HistoryQuery) string {
	mode, topic := "all", ""
	if q.TopicID != nil {
		mode, topic = "exact", *q.TopicID
	}
	sum := sha256.Sum256(mutationPayload("history-search-v1", groupID, q.Query, mode, topic))
	return hex.EncodeToString(sum[:])
}

type historyCursor struct {
	Version   int    `json:"v"`
	Scope     string `json:"scope"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func encodeHistoryCursor(scope string, last domain.Message) string {
	// This fixed string/int structure cannot fail JSON marshaling.
	raw, _ := json.Marshal(historyCursor{1, scope, last.CreatedAt.UTC().Format(timestampLayout), last.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeHistoryCursor(encoded, scope string) (*Cursor, error) {
	if encoded == "" {
		return nil, nil
	}
	invalid := fmt.Errorf("%w：历史分页游标无效或不属于当前搜索范围", ErrInvalid)
	if len(encoded) > 2048 {
		return nil, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, invalid
	}
	var c historyCursor
	if json.Unmarshal(raw, &c) != nil {
		return nil, invalid
	}
	// Re-encoding rejects unknown/duplicate keys, noncanonical JSON and base64,
	// alternate escapes, and invalid UTF-8 without copying transport parsers.
	canonical, _ := json.Marshal(c)
	if base64.RawURLEncoding.EncodeToString(canonical) != encoded {
		return nil, invalid
	}
	stamp, err := time.Parse(timestampLayout, c.CreatedAt)
	if err != nil || stamp.IsZero() || stamp.Year() < 1 || stamp.Year() > 9999 || stamp.Format(timestampLayout) != c.CreatedAt ||
		c.Version != 1 || c.Scope != scope || !validHistoryText(c.ID, 256) || strings.TrimSpace(c.ID) == "" {
		return nil, invalid
	}
	return &Cursor{CreatedAt: stamp, ID: c.ID}, nil
}
