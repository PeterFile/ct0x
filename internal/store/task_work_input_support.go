package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const taskWorkInputVersion = 28

// Message provenance projection still uses its display-only marker check.
const workInputSchemaExists = `EXISTS(SELECT 1 FROM schema_migrations WHERE version=?)`

// version is an integer primary key: this range can only have 28 rows if
// every required version is present, even on a store already opened at v29.
const workInputPrefixCount = `SELECT count(*) FROM main.schema_migrations WHERE version BETWEEN 1 AND ?`

const workInputThreadIndex = "task_coordination_followup_thread"

// Probe the required relations with sentinel keys, including the immutable
// keys created by v28: a same-named view or a keyless table is not enough.
// INDEXED BY alone can still scan a same-named index over the wrong columns.
// Check that one named index's keys directly, not by walking the catalog.
const workInputShapeProbe = `SELECT CASE WHEN (
 SELECT group_concat(name, ',' ORDER BY seqno)='group_id,topic_id,task_id'
 FROM pragma_index_info('` + workInputThreadIndex + `','main')
 ) THEN
 EXISTS(SELECT 1 FROM main.task_coordination INDEXED BY ` + workInputThreadIndex + ` WHERE (group_id,topic_id,task_id)=('','',''))
 OR EXISTS(SELECT 1 FROM main.chat_followup_routes INDEXED BY sqlite_autoindex_chat_followup_routes_5
     WHERE (source_id,group_id,topic_id)=('','','') AND trigger_id='')
 OR EXISTS(SELECT 1 FROM main.task_work_inputs INDEXED BY task_work_inputs_constraints
     WHERE (group_id,task_id,task_revision)=('','',0) AND after_revision>before_revision AND source_id='')
 OR EXISTS(SELECT 1 FROM main.task_work_inputs INDEXED BY task_work_inputs_history
     WHERE (group_id,task_id,created_at,id)=('','','',''))
 OR EXISTS(SELECT 1 FROM main.task_work_inputs INDEXED BY task_work_inputs_inbox WHERE (assignment_id,task_revision)=('',0))
 OR EXISTS(SELECT 1 FROM main.task_work_input_consumptions INDEXED BY sqlite_autoindex_task_work_input_consumptions_1
     WHERE input_id='' AND turn_id='' AND attempt_id='')
 OR EXISTS(SELECT 1 FROM main.task_work_input_returns INDEXED BY sqlite_autoindex_task_work_input_returns_1
     WHERE input_id='' AND member_id='' AND notice_message_id='')
 END`

var errWorkInputSchemaMismatch = errors.New("工作输入迁移前缀与表结构不一致")

func workInputSchemaInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, workInputPrefixCount, taskWorkInputVersion).Scan(&count); err != nil {
		return false, err
	}
	if count == taskWorkInputVersion {
		var shape sql.NullBool // False is fine (no data at the sentinel keys); NULL is not.
		if err := tx.QueryRowContext(ctx, workInputShapeProbe).Scan(&shape); err != nil {
			var sqliteErr *sqlite.Error
			if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_ERROR {
				err = fmt.Errorf("%w：%w", errWorkInputSchemaMismatch, err)
			}
			return false, err
		}
		if !shape.Valid {
			return false, errWorkInputSchemaMismatch
		}
		return true, nil
	}

	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version),0) FROM main.schema_migrations`).Scan(&latest); err != nil {
		return false, err
	}
	if latest >= taskWorkInputVersion || count != latest {
		return false, errWorkInputSchemaMismatch
	}
	// Only an older contiguous prefix with no v28/v29 footprint may omit
	// the work-input/source fences. This also detects a lost trailing marker.
	var partial bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.sqlite_schema WHERE name IN
	 ('chat_followup_routes','task_work_inputs','task_work_input_consumptions','task_work_input_returns',
	 'task_coordination_followup_thread','task_work_deliveries','task_work_continuations'))`).Scan(&partial)
	if err == nil && partial {
		err = errWorkInputSchemaMismatch
	}
	return false, err
}
