package store

import (
	"database/sql"
	"fmt"
)

var migrations = []string{`
CREATE TABLE groups (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX groups_created_id ON groups(created_at DESC, id DESC);
CREATE TABLE members (
    id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL REFERENCES groups(id),
    name TEXT NOT NULL,
    role TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    -- Actor identities do not imply membership or cascading lifecycle ownership.
    creator_id TEXT NOT NULL DEFAULT '',
    owner_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active', 'left')),
    workspace TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE(group_id, id)
);
CREATE INDEX members_group_created_id ON members(group_id, created_at DESC, id DESC);
CREATE TABLE conversations (
    id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    topic_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    FOREIGN KEY(group_id, member_id) REFERENCES members(group_id, id),
    UNIQUE(group_id, member_id, topic_id)
);
`, `
CREATE TABLE mutation_idempotency (
    operation TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK(idempotency_key <> ''),
    payload BLOB NOT NULL,
    record_id TEXT NOT NULL,
    PRIMARY KEY(operation, actor_id, group_id, idempotency_key)
);
`, `
CREATE UNIQUE INDEX conversations_delivery_identity ON conversations(id, group_id, member_id, topic_id);
CREATE TABLE messages (
    id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL REFERENCES groups(id),
    topic_id TEXT NOT NULL DEFAULT '',
    sender_id TEXT NOT NULL,
    content TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(id, group_id, topic_id)
);
CREATE INDEX messages_group_created_id ON messages(group_id, created_at DESC, id DESC);
CREATE TABLE deliveries (
    id TEXT PRIMARY KEY NOT NULL,
    message_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    recipient_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued' CHECK(state IN ('queued', 'accepted', 'failed')),
    turn_id TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    FOREIGN KEY(message_id, group_id, topic_id) REFERENCES messages(id, group_id, topic_id),
    FOREIGN KEY(conversation_id, group_id, recipient_id, topic_id) REFERENCES conversations(id, group_id, member_id, topic_id),
    UNIQUE(message_id, recipient_id)
);
`}

// Migration history is explicit and contiguous; never infer it from table shape.
func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY CHECK(version > 0))"); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return err
	}
	defer rows.Close()
	applied := 0
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return err
		}
		applied++
		if version != applied || version > len(migrations) {
			return fmt.Errorf("数据库迁移历史不受支持：版本 %d", version)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for i := applied; i < len(migrations); i++ {
		if _, err := tx.Exec(migrations[i]); err != nil {
			return fmt.Errorf("迁移版本 %d 失败：%w", i+1, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_migrations(version) VALUES(?)", i+1); err != nil {
			return err
		}
	}
	return tx.Commit()
}
