package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceOperationSQLIntentGuards(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	o := workspaceOperationDone(t, f.s, role, "original")
	for name, change := range map[string]map[string]string{
		"valid":               nil,
		"traversal":           {"relative_path": "'../escape'"},
		"empty-path":          {"relative_path": "''"},
		"wrong-resource":      {"resource_key": "'workspace:other-physical'"},
		"wrong-grant":         {"grant_id": "'foreign-grant'"},
		"wrong-task":          {"task_id": "'foreign-task'"},
		"wrong-attempt":       {"attempt_id": "'old-attempt'"},
		"wrong-revision":      {"task_revision": "task_revision+1"},
		"unsaved-preimage":    {"preimage_blob_hash": "''"},
		"wrong-preimage-blob": {"preimage_blob_hash": "after_hash"},
		"wrong-expected":      {"expected_hash": "after_hash"},
		"create-with-file":    {"kind": "'create'", "expected_hash": "''"},
		"invalid-after":       {"after_hash": "'bare-model-hash'"},
		"negative-size":       {"after_size": "-1"},
		"oversize":            {"preimage_size": "16777217"},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			message, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: f.groupID, TopicID: role.Member.TopicID, ActorID: f.worker, Content: "平台 SQL 合同夹具", RecipientIDs: []string{}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := tx.Query("SELECT name FROM pragma_table_info('workspace_operations') ORDER BY cid")
			if err != nil {
				t.Fatal(err)
			}
			var columns, expressions []string
			var args []any
			for rows.Next() {
				var col string
				if err := rows.Scan(&col); err != nil {
					t.Fatal(err)
				}
				columns = append(columns, col)
				expr := col
				switch col {
				case "id":
					expr = "?"
					args = append(args, "sql-"+name)
				case "idempotency_key":
					expr = "?"
					args = append(args, "sql-key-"+name)
				case "notice_message_id":
					expr = "?"
					args = append(args, message.ID)
				case "created_at":
					expr = "?"
					args = append(args, time.Now().UTC().Format(timestampLayout))
				}
				if changed, ok := change[col]; ok {
					expr = changed
				}
				expressions = append(expressions, expr)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			query := "INSERT INTO workspace_operations(" + strings.Join(columns, ",") + ") SELECT " + strings.Join(expressions, ",") + " FROM workspace_operations WHERE id=?"
			_, err = tx.Exec(query, append(args, o.ID)...)
			if (err == nil) != (name == "valid") {
				t.Fatalf("SQL intent guard %s: %v", name, err)
			}
		})
	}
}

func TestWorkspaceOperationSQLReceiptGuards(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	o := workspaceOperationBegin(t, f.s, role, "pending")
	for _, tc := range []struct {
		name, outcome, hash, code string
		size                      int64
	}{
		{"valid", "done", o.Input.AfterHash, "", o.Input.AfterSize},
		{"wrong-after", "done", o.Preimage.Hash, "", o.Preimage.Size},
		{"wrong-size", "done", o.Input.AfterHash, "", o.Input.AfterSize + 1},
		{"false-unchanged", "unchanged", o.Input.AfterHash, "", o.Input.AfterSize},
		{"hidden-sync-error", "done", o.Input.AfterHash, "sync_failed", o.Input.AfterSize},
		{"unknown-no-evidence", "unknown", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			message, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: f.groupID, TopicID: role.Member.TopicID, ActorID: f.worker, Content: "平台结果 SQL 合同夹具", RecipientIDs: []string{}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec("INSERT INTO workspace_operation_receipts(operation_id,outcome,observed_hash,observed_size,error_code,notice_message_id,finished_at) VALUES(?,?,?,?,?,?,?)", o.ID, tc.outcome, tc.hash, tc.size, tc.code, message.ID, time.Now().UTC().Format(timestampLayout))
			if (err == nil) != (tc.name == "valid") {
				t.Fatalf("SQL receipt guard %s: %v", tc.name, err)
			}
		})
	}
}

func workspaceOperationRequireLocalRollback(t *testing.T, err error) {
	t.Helper()
	code, local := ClassifyDispatchTransient(err)
	var certainty interface {
		CommitOutcomeUnknown() bool
		RollbackConfirmed() bool
	}
	if !local || !errors.As(err, &certainty) || certainty.CommitOutcomeUnknown() || !certainty.RollbackConfirmed() {
		t.Fatalf("unexpected concurrent error: %v; cause=%v", err, errors.Unwrap(err))
	}
	t.Logf("default-budget local contention: code=%s, rollback confirmed, cause=%v; reconcile original receipt only", code, errors.Unwrap(err))
}

func TestWorkspaceOperationConcurrentOriginalKey(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	second := queueHandle(t, f.path, false)
	type result struct {
		o       WorkspaceOperation
		created bool
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, s := range []*Store{f.s, second} {
		go func(s *Store) {
			<-start
			o, created, err := s.BeginWorkspaceOperation(ctx, role, workspaceOperationInput(), workspaceOperationPreimage(), "same-key")
			results <- result{o, created, err}
		}(s)
	}
	close(start)
	a, b := <-results, <-results
	// Under race instrumentation the existing coordination SQL planner can hold
	// the write lock beyond the production 5s busy budget. A confirmed local
	// rollback grants no IO; reconcile that caller by the original key once.
	for _, candidate := range []*result{&a, &b} {
		if candidate.err == nil {
			continue
		}
		if candidate.created || candidate.o != (WorkspaceOperation{}) {
			t.Fatalf("failed Begin returned IO permission: %+v", candidate)
		}
		workspaceOperationRequireLocalRollback(t, candidate.err)
		o, created, err := f.s.BeginWorkspaceOperation(ctx, role, workspaceOperationInput(), workspaceOperationPreimage(), "same-key")
		if err != nil || created {
			t.Fatalf("original-key reconciliation requested IO again: %+v,%t,%v", o, created, err)
		}
		candidate.o, candidate.err = o, nil
	}
	if a.o.ID != b.o.ID || a.created == b.created {
		t.Fatalf("concurrent pending key duplicated or lost intent: %+v / %+v", a, b)
	}
	finishes := make(chan error, 2)
	for _, s := range []*Store{f.s, second} {
		go func(s *Store) {
			_, err := s.FinishWorkspaceOperation(ctx, role, a.o.ID, WorkspaceOperationResult{Outcome: "done", ObservedHash: a.o.Input.AfterHash, ObservedSize: a.o.Input.AfterSize})
			finishes <- err
		}(s)
	}
	for range 2 {
		if err := <-finishes; err != nil {
			workspaceOperationRequireLocalRollback(t, err)
			if _, err := f.s.FinishWorkspaceOperation(ctx, role, a.o.ID, WorkspaceOperationResult{Outcome: "done", ObservedHash: a.o.Input.AfterHash, ObservedSize: a.o.Input.AfterSize}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for table, want := range map[string]int{"workspace_operations": 1, "workspace_operation_receipts": 1} {
		var count int
		if err := f.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d,%v", table, count, err)
		}
	}
	page, err := f.s.ListWorkspaceOperationsForTurn(ctx, f.groupID, a.o.TurnID, ListOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor != nil {
		t.Fatalf("audit exact bounded history: %+v,%v", page, err)
	}
	t.Log(fmt.Sprintf("one immutable intent and terminal receipt for Turn %s", a.o.TurnID))
}
