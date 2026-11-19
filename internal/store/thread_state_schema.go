package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// B registers this migration in merge order. There is no legacy backfill.
const threadStateSchema = `
CREATE TABLE thread_state_revisions (
    group_id TEXT NOT NULL REFERENCES groups(id),
    topic_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK(revision>0),
    slot INTEGER NOT NULL CHECK(slot BETWEEN 0 AND 7),
    kind TEXT NOT NULL CHECK(kind IN ('goal','constraint','confirmed_decision','plan')),
    content TEXT NOT NULL CHECK(length(CAST(content AS BLOB)) BETWEEN 1 AND 1024),
    source_id TEXT NOT NULL REFERENCES message_command_sources(id),
    source_position INTEGER NOT NULL REFERENCES message_log(sequence),
    confirmed_at TEXT NOT NULL,
    invalidates_revision INTEGER CHECK(invalidates_revision>0 AND invalidates_revision<revision),
    covered_through_message_id TEXT CHECK(covered_through_message_id<>''),
    covered_through_position INTEGER REFERENCES message_log(sequence) CHECK(covered_through_position>0),
    PRIMARY KEY(group_id, topic_id, task_id, task_revision, revision),
    CHECK((task_id='' AND task_revision=0) OR (task_id<>'' AND task_revision>0)),
    CHECK((covered_through_message_id IS NULL)=(covered_through_position IS NULL)),
    FOREIGN KEY(covered_through_message_id, group_id, topic_id) REFERENCES messages(id, group_id, topic_id),
    FOREIGN KEY(group_id, topic_id, task_id, task_revision, invalidates_revision)
        REFERENCES thread_state_revisions(group_id, topic_id, task_id, task_revision, revision)
) STRICT, WITHOUT ROWID;
CREATE INDEX thread_state_by_slot ON thread_state_revisions(group_id, topic_id, task_id, task_revision, slot, revision);
-- The independent head survives a lost revision row. Never cascade deletion.
CREATE TABLE thread_state_heads (
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK(revision>0),
    PRIMARY KEY(group_id, topic_id, task_id, task_revision),
    FOREIGN KEY(group_id, topic_id, task_id, task_revision, revision)
        REFERENCES thread_state_revisions(group_id, topic_id, task_id, task_revision, revision)
) STRICT, WITHOUT ROWID;
CREATE TRIGGER thread_state_head_insert BEFORE INSERT ON thread_state_heads
WHEN NEW.revision<>1 OR EXISTS(SELECT 1 FROM thread_state_heads WHERE (group_id,topic_id,task_id,task_revision)=
    (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision))
BEGIN SELECT RAISE(ABORT, '话题状态头不可替换'); END;
CREATE TRIGGER thread_state_head_update BEFORE UPDATE ON thread_state_heads
WHEN (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision) IS NOT (OLD.group_id,OLD.topic_id,OLD.task_id,OLD.task_revision)
    OR NEW.revision<>OLD.revision+1
BEGIN SELECT RAISE(ABORT, '话题状态头必须按序前进且范围不可变'); END;
CREATE TRIGGER thread_state_head_no_delete BEFORE DELETE ON thread_state_heads
BEGIN SELECT RAISE(ABORT, '话题状态头不可删除'); END;
CREATE TRIGGER thread_state_append BEFORE INSERT ON thread_state_revisions
BEGIN
    -- This also prevents REPLACE, independently of recursive_triggers.
    SELECT RAISE(ABORT, '状态修订必须连续且不可替换') WHERE NEW.revision<>1+coalesce((
        SELECT max(revision) FROM thread_state_revisions WHERE (group_id,topic_id,task_id,task_revision)=
        (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision)),0) OR NEW.revision<>1+coalesce((
        SELECT revision FROM thread_state_heads WHERE (group_id,topic_id,task_id,task_revision)=
        (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision)),0);
    SELECT RAISE(ABORT, '状态任务与话题或版本不匹配') WHERE NEW.task_id<>'' AND NOT EXISTS(
        SELECT 1` + threadStateTaskRelation + `
        WHERE t.id=NEW.task_id AND t.group_id=NEW.group_id AND link.topic_id=NEW.topic_id AND t.revision=NEW.task_revision);
    SELECT RAISE(ABORT, '状态缺少同话题的人类确认来源') WHERE NOT EXISTS(
        SELECT 1 FROM message_log l JOIN messages m ON (m.id,m.group_id,m.topic_id)=(l.message_id,l.group_id,l.topic_id)
        JOIN message_command_sources s ON s.message_id=m.id
        WHERE l.sequence=NEW.source_position AND (l.group_id,l.topic_id)=(NEW.group_id,NEW.topic_id)
        AND s.id=NEW.source_id AND s.kind='local_user_chat' AND s.turn_id IS NULL AND s.claim_digest IS NULL
        AND m.sender_id='user' AND m.content=NEW.content);
    SELECT RAISE(ABORT, '话题状态覆盖位置无效或回退') WHERE
        (NEW.covered_through_position IS NOT NULL AND NOT EXISTS(
            SELECT 1 FROM message_log l JOIN messages m ON (m.id,m.group_id,m.topic_id)=(l.message_id,l.group_id,l.topic_id)
            WHERE (l.sequence,l.message_id,l.group_id,l.topic_id)=
                (NEW.covered_through_position,NEW.covered_through_message_id,NEW.group_id,NEW.topic_id)))
        OR coalesce(NEW.covered_through_position,0)<coalesce((
            SELECT covered_through_position FROM thread_state_revisions WHERE (group_id,topic_id,task_id,task_revision,revision)=
                (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.revision-1)),0);
    SELECT RAISE(ABORT, '状态记录或失效来源不匹配') WHERE EXISTS(
        SELECT 1 FROM thread_state_revisions older WHERE (older.group_id,older.topic_id,older.task_id,older.task_revision,older.slot)=
            (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.slot)
        AND older.revision=(SELECT max(revision) FROM thread_state_revisions INDEXED BY thread_state_by_slot
            WHERE (group_id,topic_id,task_id,task_revision,slot)=(NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.slot))
        AND (older.kind<>NEW.kind OR older.source_position>=NEW.source_position
            OR (NEW.invalidates_revision IS NOT NULL AND (older.revision<>NEW.invalidates_revision OR older.invalidates_revision IS NOT NULL))
            OR (NEW.invalidates_revision IS NULL AND older.kind='plan')));
    SELECT RAISE(ABORT, '缺少被置为失效的状态') WHERE NEW.invalidates_revision IS NOT NULL AND NOT EXISTS(
        SELECT 1 FROM thread_state_revisions WHERE (group_id,topic_id,task_id,task_revision,revision,slot)=
        (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.invalidates_revision,NEW.slot));
END;
-- The revision, head and idempotency receipt commit in the same transaction.
CREATE TRIGGER thread_state_advance_head AFTER INSERT ON thread_state_revisions
BEGIN
    INSERT INTO thread_state_heads(group_id,topic_id,task_id,task_revision,revision)
        SELECT NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.revision WHERE NEW.revision=1;
    UPDATE thread_state_heads SET revision=NEW.revision WHERE NEW.revision>1 AND (group_id,topic_id,task_id,task_revision)=
        (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision);
END;
CREATE TRIGGER thread_state_no_update BEFORE UPDATE ON thread_state_revisions
BEGIN SELECT RAISE(ABORT, '状态修订不可修改'); END;
CREATE TRIGGER thread_state_no_delete BEFORE DELETE ON thread_state_revisions
BEGIN SELECT RAISE(ABORT, '状态修订不可删除'); END;
`

var threadStateSchemaVersion = slices.Index(migrations, threadStateSchema) + 1

var (
	ErrThreadStateUnavailable = errors.New("话题状态迁移尚未安装")
	errThreadStateIntegrity   = errors.New("话题状态记录或迁移不完整")
)

// An unregistered candidate may run on v30. Any partial footprint or missing
// registered marker under a later migration is damage, not a legacy fallback.
func threadStateInstalledInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var latest, found, valid int
	var marked bool
	err := tx.QueryRowContext(ctx, `SELECT (SELECT coalesce(max(version),0) FROM main.schema_migrations),
	 EXISTS(SELECT 1 FROM main.schema_migrations WHERE version=?), count(*), count(CASE WHEN (tbl_name='thread_state_revisions' AND (
	 (name='thread_state_revisions' AND type='table') OR (name='thread_state_by_slot' AND type='index') OR
	 (name IN ('thread_state_append','thread_state_advance_head','thread_state_no_update','thread_state_no_delete') AND type='trigger')))
	 OR (tbl_name='thread_state_heads' AND ((name='thread_state_heads' AND type='table') OR
	 (name IN ('thread_state_head_insert','thread_state_head_update','thread_state_head_no_delete') AND type='trigger'))) THEN 1 END)
	 FROM main.sqlite_schema WHERE name IN
	 ('thread_state_revisions','thread_state_by_slot','thread_state_append','thread_state_advance_head','thread_state_no_update','thread_state_no_delete',
	  'thread_state_heads','thread_state_head_insert','thread_state_head_update','thread_state_head_no_delete')`,
		threadStateSchemaVersion).Scan(&latest, &marked, &found, &valid)
	if err != nil {
		return false, err
	}
	if marked && valid == 10 {
		return true, nil
	}
	if !marked && found == 0 && (threadStateSchemaVersion == 0 || latest < threadStateSchemaVersion) {
		return false, nil
	}
	return false, errThreadStateIntegrity
}
