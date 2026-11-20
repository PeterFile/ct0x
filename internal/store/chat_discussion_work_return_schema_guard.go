package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const chatDiscussionWorkReturnVersion = 31

var chatDiscussionWorkReturnDDL = func() string {
	// In immutable v31, column-one CREATEs start statements and column-one
	// comments separate them. SQLite stores only the statement, without ';'.
	var objects [][4]string
	for _, body := range strings.Split(chatDiscussionWorkReturnSchema, "\nCREATE ")[1:] {
		body, _, _ = strings.Cut(body, "\n--")
		ddl := "CREATE " + strings.TrimSuffix(strings.TrimSpace(body), ";")
		header := strings.Fields(ddl)
		kind, name, table := strings.ToLower(header[1]), header[2], header[2]
		if kind == "trigger" {
			_, target, _ := strings.Cut(ddl, " ON ")
			table = strings.Fields(target)[0]
		}
		objects = append(objects, [4]string{kind, name, table, ddl})
	}
	encoded, _ := json.Marshal(objects) // Strings always marshal.
	return string(encoded)
}()

const chatDiscussionWorkReturnSchemaSQL = `WITH footprint AS (
 SELECT type,name,tbl_name,sql FROM main.sqlite_schema
 WHERE name='chat_discussion_work_returns' COLLATE NOCASE
 OR (type='trigger' AND lower(name) GLOB 'chat_discussion_work_return[s_]*')
)
SELECT CASE
 WHEN ?1<1 OR NOT EXISTS(SELECT 1 FROM main.sqlite_schema WHERE type='table' AND name='schema_migrations'
   AND sql='CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY CHECK(version > 0))') THEN -1
 WHEN (SELECT count(*) FROM main.schema_migrations WHERE version BETWEEN 1 AND ?1)<>?1
   OR EXISTS(SELECT 1 FROM main.schema_migrations WHERE version<1 OR version>?1) THEN -1
 WHEN ?1>=?2-1 AND (SELECT count(*) FROM main.sqlite_schema WHERE type='table' AND name IN (
   'chat_discussion_events','chat_discussions','task_work_handbacks','messages','deliveries',
   'members','mandates','coordination_triggers','message_command_sources','turns',
   'chat_intake_executions','chat_candidates','groups','conversations'))<>14 THEN -1
 WHEN ?1<?2 THEN CASE WHEN EXISTS(SELECT 1 FROM footprint) THEN -1 ELSE 0 END
 WHEN EXISTS(
   SELECT value->>0,value->>1,value->>2,value->>3 FROM json_each(?3)
   EXCEPT SELECT type,name,tbl_name,sql FROM footprint
 ) THEN -1
 ELSE 1 END`

// Only the schema version this handle opened can authorize absence. Bound the
// ledger checks by its integer primary key and the DDL checks by the catalog;
// no application history is inspected. At v30 and later, also require the v31
// DDL's preexisting parents/guard dependencies and the queue identity tables.
func (s *Store) chatDiscussionWorkReturnInstalledInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var state int
	err := tx.QueryRowContext(ctx, chatDiscussionWorkReturnSchemaSQL,
		s.schemaVersion, chatDiscussionWorkReturnVersion, chatDiscussionWorkReturnDDL).Scan(&state)
	if err != nil {
		return false, err
	}
	if state == -1 {
		return false, fmt.Errorf("%w：讨论工作回传迁移记录与数据库结构不一致", ErrConflict)
	}
	return state == 1, nil
}
