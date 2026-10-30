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
`, `
CREATE UNIQUE INDEX deliveries_turn_identity ON deliveries(id, group_id, conversation_id, recipient_id);
CREATE INDEX deliveries_pending_created_id ON deliveries(created_at, id)
    WHERE state='queued' AND turn_id='';
CREATE TABLE turns (
    id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    delivery_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    claim_token TEXT NOT NULL CHECK(claim_token <> ''),
    state TEXT NOT NULL CHECK(state IN ('starting','running','cancel_requested','unknown',
        'not_started','succeeded','failed','aborted','truncated')),
    native_session_id TEXT NOT NULL DEFAULT '',
    accepted_at TEXT,
    cancel_requested_at TEXT,
    candidate_text TEXT NOT NULL DEFAULT '',
    candidate_terminal TEXT NOT NULL DEFAULT '' CHECK(candidate_terminal IN ('','succeeded','failed','aborted','truncated')),
    reply_message_id TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    receipt_json TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(delivery_id, group_id, conversation_id, member_id) REFERENCES deliveries(id, group_id, conversation_id, recipient_id)
);
CREATE UNIQUE INDEX turns_active_conversation ON turns(conversation_id)
    WHERE state IN ('starting','running','cancel_requested','unknown');
CREATE UNIQUE INDEX turns_active_member ON turns(member_id)
    WHERE state IN ('starting','running','cancel_requested','unknown');
CREATE TABLE resource_claims (
    resource_key TEXT PRIMARY KEY NOT NULL CHECK(resource_key <> ''),
    turn_id TEXT NOT NULL REFERENCES turns(id)
);
CREATE INDEX resource_claims_turn ON resource_claims(turn_id);
`, `
CREATE UNIQUE INDEX turns_task_identity ON turns(id, group_id);
CREATE UNIQUE INDEX turns_task_actor_identity ON turns(id, group_id, member_id);
CREATE TABLE tasks (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    group_id TEXT NOT NULL REFERENCES groups(id),
    title TEXT NOT NULL CHECK(trim(title) <> ''),
    description TEXT NOT NULL DEFAULT '',
    acceptance TEXT NOT NULL CHECK(trim(acceptance) <> ''),
    assignee_id TEXT,
    status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','submitted','accepted')),
    revision INTEGER NOT NULL DEFAULT 1 CHECK(typeof(revision)='integer' AND revision > 0),
    active_attempt_id TEXT,
    current_submission_id TEXT,
    created_at TEXT NOT NULL,
    UNIQUE(id, group_id),
    FOREIGN KEY(group_id, assignee_id) REFERENCES members(group_id, id),
    FOREIGN KEY(active_attempt_id, id, group_id, revision)
        REFERENCES task_attempts(id, task_id, group_id, task_revision),
    FOREIGN KEY(current_submission_id, id, group_id, revision)
        REFERENCES task_submissions(id, task_id, group_id, task_revision),
    FOREIGN KEY(current_submission_id, active_attempt_id) REFERENCES task_submissions(id, attempt_id),
    CHECK((status='open' AND current_submission_id IS NULL) OR
        (status='submitted' AND active_attempt_id IS NOT NULL AND current_submission_id IS NOT NULL) OR
        (status='accepted' AND active_attempt_id IS NULL AND current_submission_id IS NOT NULL))
);
CREATE INDEX tasks_group_created_id ON tasks(group_id, created_at DESC, id DESC);
CREATE TABLE task_dependencies (
    group_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    dependency_id TEXT NOT NULL,
    PRIMARY KEY(group_id, task_id, dependency_id),
    CHECK(task_id <> dependency_id),
    FOREIGN KEY(task_id, group_id) REFERENCES tasks(id, group_id),
    FOREIGN KEY(dependency_id, group_id) REFERENCES tasks(id, group_id)
);
CREATE TRIGGER task_dependencies_cycle BEFORE INSERT ON task_dependencies
WHEN EXISTS (
    WITH RECURSIVE reachable(id) AS (
        SELECT NEW.dependency_id UNION
        SELECT d.dependency_id FROM task_dependencies d JOIN reachable r ON d.task_id=r.id
        WHERE d.group_id=NEW.group_id
    ) SELECT 1 FROM reachable WHERE id=NEW.task_id
)
BEGIN SELECT RAISE(ABORT, '任务依赖不能形成环'); END;
CREATE TRIGGER task_dependencies_no_update BEFORE UPDATE ON task_dependencies
BEGIN SELECT RAISE(ABORT, '依赖边必须在事务中删除后重新插入'); END;
CREATE TABLE task_attempts (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    task_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(typeof(task_revision)='integer' AND task_revision > 0),
    actor_id TEXT NOT NULL CHECK(trim(actor_id) <> ''),
    turn_id TEXT CHECK(turn_id IS NULL OR turn_id <> ''),
    -- Human references skip only the member component, never the Turn's group.
    turn_member_id TEXT GENERATED ALWAYS AS (nullif(actor_id,'user')) VIRTUAL,
    created_at TEXT NOT NULL,
    CHECK(actor_id='user' OR turn_id IS NOT NULL),
    UNIQUE(id, task_id, group_id, task_revision),
    UNIQUE(id, task_id, group_id, task_revision, actor_id),
    -- Historical rows reference stable task identity, not its mutable revision.
    FOREIGN KEY(task_id, group_id) REFERENCES tasks(id, group_id),
    FOREIGN KEY(turn_id, group_id) REFERENCES turns(id, group_id),
    FOREIGN KEY(turn_id, group_id, turn_member_id) REFERENCES turns(id, group_id, member_id)
);
CREATE TRIGGER task_attempts_insert BEFORE INSERT ON task_attempts
WHEN EXISTS(SELECT 1 FROM task_attempts WHERE id=NEW.id) OR NOT EXISTS(
    SELECT 1 FROM tasks WHERE id=NEW.task_id AND group_id=NEW.group_id
        AND revision=NEW.task_revision AND status='open')
BEGIN SELECT RAISE(ABORT, '任务尝试身份或当前版本冲突'); END;
CREATE TABLE task_submissions (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    task_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(typeof(task_revision)='integer' AND task_revision > 0),
    attempt_id TEXT NOT NULL UNIQUE,
    summary TEXT NOT NULL CHECK(trim(summary) <> ''),
    submitted_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(id, task_id, group_id, task_revision),
    UNIQUE(id, attempt_id),
    FOREIGN KEY(attempt_id, task_id, group_id, task_revision, submitted_by)
        REFERENCES task_attempts(id, task_id, group_id, task_revision, actor_id)
);
CREATE TABLE task_review_decisions (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    submission_id TEXT NOT NULL UNIQUE,
    task_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(typeof(task_revision)='integer' AND task_revision > 0),
    outcome TEXT NOT NULL CHECK(outcome IN ('accepted','rejected')),
    reviewed_by TEXT NOT NULL CHECK(trim(reviewed_by) <> ''),
    reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    FOREIGN KEY(submission_id, task_id, group_id, task_revision)
        REFERENCES task_submissions(id, task_id, group_id, task_revision)
);
CREATE TRIGGER task_submissions_insert BEFORE INSERT ON task_submissions
WHEN EXISTS(SELECT 1 FROM task_submissions WHERE id=NEW.id OR attempt_id=NEW.attempt_id)
BEGIN SELECT RAISE(ABORT, '提交证据不可替换'); END;
CREATE TRIGGER task_reviews_insert BEFORE INSERT ON task_review_decisions
WHEN EXISTS(SELECT 1 FROM task_review_decisions WHERE id=NEW.id OR submission_id=NEW.submission_id)
BEGIN SELECT RAISE(ABORT, '评审记录不可替换'); END;
CREATE TRIGGER task_attempts_no_update BEFORE UPDATE ON task_attempts
BEGIN SELECT RAISE(ABORT, '任务尝试不可修改'); END;
CREATE TRIGGER task_attempts_no_delete BEFORE DELETE ON task_attempts
BEGIN SELECT RAISE(ABORT, '任务尝试不可删除'); END;
CREATE TRIGGER task_submissions_no_update BEFORE UPDATE ON task_submissions
BEGIN SELECT RAISE(ABORT, '提交证据不可修改'); END;
CREATE TRIGGER task_submissions_no_delete BEFORE DELETE ON task_submissions
BEGIN SELECT RAISE(ABORT, '提交证据不可删除'); END;
CREATE TRIGGER task_reviews_no_update BEFORE UPDATE ON task_review_decisions
BEGIN SELECT RAISE(ABORT, '评审记录不可修改'); END;
CREATE TRIGGER task_reviews_no_delete BEFORE DELETE ON task_review_decisions
BEGIN SELECT RAISE(ABORT, '评审记录不可删除'); END;
CREATE TRIGGER tasks_accept_insert BEFORE INSERT ON tasks
WHEN NEW.status='accepted' AND NOT EXISTS(SELECT 1 FROM task_review_decisions
    WHERE submission_id=NEW.current_submission_id AND task_id=NEW.id AND group_id=NEW.group_id
        AND task_revision=NEW.revision AND outcome='accepted')
BEGIN SELECT RAISE(ABORT, '接纳任务必须有明确接纳的评审'); END;
CREATE TRIGGER tasks_accept_update BEFORE UPDATE ON tasks
WHEN NEW.status='accepted' AND NOT EXISTS(SELECT 1 FROM task_review_decisions
    WHERE submission_id=NEW.current_submission_id AND task_id=NEW.id AND group_id=NEW.group_id
        AND task_revision=NEW.revision AND outcome='accepted')
BEGIN SELECT RAISE(ABORT, '接纳任务必须有明确接纳的评审'); END;
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
