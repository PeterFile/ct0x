package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"agentworkspace/internal/domain"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("未找到记录")
	ErrInvalid  = errors.New("参数无效")
	ErrConflict = errors.New("幂等键对应的请求内容冲突")
)

// Fixed-width UTC timestamps preserve time ordering in SQLite TEXT indexes.
const timestampLayout = "2006-01-02T15:04:05.000000000Z"

const (
	groupColumns        = "id, name, description, created_at"
	memberColumns       = "id, group_id, name, role, provider_id, model, creator_id, owner_id, status, workspace, created_at"
	conversationColumns = "id, group_id, member_id, topic_id, created_at"
)

type Store struct{ db *sql.DB }

type CreateGroupInput struct {
	Name, Description string
	ActorID           string `json:"-"` // Bound by trusted platform/session code.
	IdempotencyKey    string
}

type CreateMemberInput struct {
	GroupID, Name, Role, ProviderID, Model, CreatorID, OwnerID, Workspace string
	ActorID                                                               string `json:"-"` // Bound by trusted platform/session code.
	IdempotencyKey                                                        string
}

// Cursor is an exclusive timestamp/ID position; transport encoding is external.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

type ListOptions struct {
	Limit  int // Zero selects 50; valid explicit limits are 1 through 200.
	Before *Cursor
}

type Page[T any] struct {
	Items      []T
	NextCursor *Cursor // Nil means the end, including an exactly full final page.
}

// Open accepts a filesystem path, never a caller-supplied SQLite DSN.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w：数据库路径不能为空", ErrInvalid)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, storeError("解析数据库路径失败", err)
	}
	dsn := url.URL{Scheme: "file", Path: absolute, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, storeError("打开数据库失败", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := migrate(db); err != nil {
		return nil, storeError("初始化数据库失败", errors.Join(err, db.Close()))
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return storeError("关闭数据库失败", s.db.Close()) }

func (s *Store) CreateGroup(ctx context.Context, input CreateGroupInput) (domain.Group, error) {
	scope := mutationScope{operation: "create_group", actorID: input.ActorID, key: input.IdempotencyKey}
	tx, id, replay, err := s.beginMutation(ctx, scope, mutationPayload(input.Name, input.Description))
	if err != nil {
		return domain.Group{}, storeError("创建群组失败", err)
	}
	defer tx.Rollback()
	if strings.TrimSpace(input.Name) == "" {
		return domain.Group{}, fmt.Errorf("%w：群组名称不能为空", ErrInvalid)
	}
	var g domain.Group
	if replay {
		g, err = scanGroup(tx.QueryRowContext(ctx, "SELECT "+groupColumns+" FROM groups WHERE id = ?", id))
	} else {
		g, err = scanGroup(tx.QueryRowContext(ctx,
			"INSERT INTO groups(id, name, description, created_at) VALUES(?, ?, ?, ?) RETURNING "+groupColumns,
			id, input.Name, input.Description, time.Now().UTC().Format(timestampLayout)))
	}
	if err != nil {
		return domain.Group{}, storeError("创建群组失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Group{}, storeError("保存群组失败", err)
	}
	return g, nil
}

func (s *Store) GetGroup(ctx context.Context, id string) (domain.Group, error) {
	g, err := scanGroup(s.db.QueryRowContext(ctx, "SELECT "+groupColumns+" FROM groups WHERE id = ?", id))
	return g, storeError("读取群组失败", err)
}

func (s *Store) CreateMember(ctx context.Context, input CreateMemberInput) (domain.Member, error) {
	scope := mutationScope{operation: "create_member", actorID: input.ActorID, groupID: input.GroupID, key: input.IdempotencyKey}
	payload := mutationPayload(input.GroupID, input.Name, input.Role, input.ProviderID, input.Model, input.CreatorID, input.OwnerID, input.Workspace)
	tx, id, replay, err := s.beginMutation(ctx, scope, payload)
	if err != nil {
		return domain.Member{}, storeError("创建成员失败", err)
	}
	defer tx.Rollback()
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Role) == "" {
		return domain.Member{}, fmt.Errorf("%w：成员名称和职责不能为空", ErrInvalid)
	}
	var m domain.Member
	if replay {
		m, err = scanMember(tx.QueryRowContext(ctx,
			"SELECT "+memberColumns+" FROM members WHERE group_id = ? AND id = ?", input.GroupID, id))
	} else {
		m, err = scanMember(tx.QueryRowContext(ctx,
			`INSERT INTO members(`+memberColumns+`) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			 WHERE EXISTS(SELECT 1 FROM groups WHERE id = ?) RETURNING `+memberColumns,
			id, input.GroupID, input.Name, input.Role, input.ProviderID, input.Model,
			input.CreatorID, input.OwnerID, domain.MemberStatusActive, input.Workspace,
			time.Now().UTC().Format(timestampLayout), input.GroupID))
	}
	if err != nil {
		return domain.Member{}, storeError("创建成员失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Member{}, storeError("保存成员失败", err)
	}
	return m, nil
}

func (s *Store) GetMember(ctx context.Context, groupID, memberID string) (domain.Member, error) {
	m, err := scanMember(s.db.QueryRowContext(ctx,
		"SELECT "+memberColumns+" FROM members WHERE group_id = ? AND id = ?", groupID, memberID))
	return m, storeError("读取成员失败", err)
}

func (s *Store) GetOrCreateConversation(ctx context.Context, groupID, memberID, topicID string) (domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Conversation{}, storeError("创建会话失败", err)
	}
	defer tx.Rollback()
	c, err := conversationInTx(ctx, tx, groupID, memberID, topicID)
	if err != nil {
		return domain.Conversation{}, storeError("读取会话失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Conversation{}, storeError("保存会话失败", err)
	}
	return c, nil
}

func conversationInTx(ctx context.Context, tx *sql.Tx, groupID, memberID, topicID string) (domain.Conversation, error) {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO conversations(`+conversationColumns+`) SELECT ?, ?, ?, ?, ?
		 WHERE EXISTS(SELECT 1 FROM members WHERE group_id = ? AND id = ?)
		 ON CONFLICT(group_id, member_id, topic_id) DO NOTHING`,
		rand.Text(), groupID, memberID, topicID, time.Now().UTC().Format(timestampLayout), groupID, memberID)
	if err != nil {
		return domain.Conversation{}, err
	}
	return scanConversation(tx.QueryRowContext(ctx,
		"SELECT "+conversationColumns+" FROM conversations WHERE group_id = ? AND member_id = ? AND topic_id = ?",
		groupID, memberID, topicID))
}

func (s *Store) GetConversation(ctx context.Context, groupID, conversationID string) (domain.Conversation, error) {
	c, err := scanConversation(s.db.QueryRowContext(ctx,
		"SELECT "+conversationColumns+" FROM conversations WHERE group_id = ? AND id = ?", groupID, conversationID))
	return c, storeError("读取会话失败", err)
}

type scanner interface{ Scan(...any) error }

func scanGroup(row scanner) (g domain.Group, err error) {
	var stamp string
	if err = row.Scan(&g.ID, &g.Name, &g.Description, &stamp); err == nil {
		g.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func scanMember(row scanner) (m domain.Member, err error) {
	var stamp string
	if err = row.Scan(&m.ID, &m.GroupID, &m.Name, &m.Role, &m.ProviderID, &m.Model,
		&m.CreatorID, &m.OwnerID, &m.Status, &m.Workspace, &stamp); err == nil {
		m.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func scanConversation(row scanner) (c domain.Conversation, err error) {
	var stamp string
	if err = row.Scan(&c.ID, &c.GroupID, &c.MemberID, &c.TopicID, &stamp); err == nil {
		c.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func storeError(action string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return fmt.Errorf("%s：%w", action, err)
}
