package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Copy only trusted fixture metadata from a real admitted, completed operation.
// Explicit IDs and clock observations make both wall-clock failure modes
// deterministic; all production FKs, notices and authorization triggers run.
const workspaceOperationClockIntentSQL = `INSERT INTO workspace_operations
 (id,group_id,task_id,task_revision,assignment_id,attempt_id,turn_id,member_id,grant_id,resource_key,node_id,claim_digest,idempotency_key,
 relative_path,kind,expected_hash,after_hash,after_size,preimage_exists,preimage_hash,preimage_blob_hash,preimage_size,notice_message_id,created_at)
 SELECT ?,group_id,task_id,task_revision,assignment_id,attempt_id,turn_id,member_id,grant_id,resource_key,node_id,claim_digest,?,
 ?,kind,expected_hash,after_hash,after_size,preimage_exists,preimage_hash,preimage_blob_hash,preimage_size,?,?
 FROM workspace_operations WHERE id=?`

func workspaceOperationClockIntent(t *testing.T, s *Store, role CoordinationToolScope, template WorkspaceOperation, id string, at time.Time) WorkspaceOperation {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	m, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: template.GroupID, TopicID: role.Member.TopicID, ActorID: template.MemberID, Content: "平台时钟观测夹具", RecipientIDs: []string{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(workspaceOperationClockIntentSQL, id, id, "clock/order.txt", m.ID, at.Format(timestampLayout), template.ID); err != nil {
		t.Fatal(err)
	}
	o, err := workspaceOperationInTx(ctx, tx, template.GroupID, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestWorkerArtifactPathUsesCommittedIntentOrder(t *testing.T) {
	for _, clock := range []string{"equal", "backward"} {
		t.Run(clock, func(t *testing.T) {
			f, _, role := workspaceOperationFixture(t)
			template := workspaceOperationDone(t, f.s, role, "template")
			firstAt := time.Now().UTC().Add(-time.Hour)
			laterAt := firstAt
			if clock == "backward" {
				laterAt = firstAt.Add(-time.Hour)
			}
			var operations []WorkspaceOperation
			for i, at := range []time.Time{firstAt, laterAt} {
				// Lexical ID order deliberately opposes insertion/commit order.
				o := workspaceOperationClockIntent(t, f.s, role, template, []string{"z-earlier", "a-later"}[i], at)
				done, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, WorkspaceOperationResult{Outcome: "done", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize})
				if err != nil {
					t.Fatalf("finish ordered intent: %v; cause=%v", err, errors.Unwrap(err))
				}
				operations = append(operations, done)
			}
			first, latest := operations[0], operations[1]
			if first.CreatedAt != firstAt || latest.CreatedAt != laterAt {
				t.Fatal("clock observations were rewritten")
			}
			t.Run("sql-publication-guard", func(t *testing.T) {
				for i, o := range operations {
					t.Run(o.ID, func(t *testing.T) {
						tx, err := f.s.db.BeginTx(ctx, nil)
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						v, err := publishArtifactInTx(ctx, tx, PublishArtifactInput{GroupID: o.GroupID, ActorID: o.MemberID, SourceTaskID: o.TaskID, Name: "SQL 顺序证明", BlobHash: o.Input.AfterHash, Size: o.Input.AfterSize})
						if err != nil {
							t.Fatal(err)
						}
						_, err = tx.Exec(`INSERT INTO worker_artifact_publications
 (version_id,operation_id,group_id,member_id,idempotency_key,requested_operation_id,requested_relative_path,requested_artifact_id,requested_name,requested_expected_version_id,blob_hash,size,created_at)
 VALUES(?,?,?,?,'sql-order','',?,'',?,'',?,?,?)`, v.ID, o.ID, o.GroupID, o.MemberID, o.Input.RelativePath, "SQL 顺序证明", v.BlobHash, v.Size, v.CreatedAt.Format(timestampLayout))
						if (err == nil) != (i == 1) {
							t.Fatalf("latest committed intent guard: operation=%s, shouldAccept=%t, err=%v", o.ID, i == 1, err)
						}
					})
				}
			})
			t.Run("go-publication", func(t *testing.T) {
				v, err := f.s.PublishWorkerArtifact(ctx, role, WorkerArtifactInput{RelativePath: latest.Input.RelativePath, Name: "按提交顺序发布"}, WorkerArtifactBlob{Hash: latest.Input.AfterHash, Size: latest.Input.AfterSize}, "ordered-path")
				if err != nil {
					t.Fatalf("publish latest intent: %v; cause=%v", err, errors.Unwrap(err))
				}
				var linked string
				if err := f.s.db.QueryRow("SELECT operation_id FROM worker_artifact_publications WHERE version_id=?", v.ID).Scan(&linked); err != nil || linked != latest.ID {
					t.Fatalf("path publication selected %q, want last committed %q: %v", linked, latest.ID, err)
				}
			})
			t.Run("durable-sequence", func(t *testing.T) {
				var ddl string
				if err := f.s.db.QueryRow("SELECT sql FROM sqlite_schema WHERE name='workspace_operations'").Scan(&ddl); err != nil || !strings.Contains(ddl, "sequence INTEGER PRIMARY KEY AUTOINCREMENT") {
					t.Fatalf("intent order must use a declared durable SQLite ordinal, not implicit rowid: %v", err)
				}
				var before [2]int64
				for i, o := range operations {
					if err := f.s.db.QueryRow("SELECT sequence FROM workspace_operations WHERE id=?", o.ID).Scan(&before[i]); err != nil {
						t.Fatal(err)
					}
				}
				if before[0] <= 0 || before[1] <= before[0] {
					t.Fatalf("commit sequence did not advance: %v", before)
				}
				if _, err := f.s.db.Exec("VACUUM"); err != nil {
					t.Fatal(err)
				}
				if err := f.s.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := Open(f.path)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				for i, o := range operations {
					var got int64
					if err := reopened.db.QueryRow("SELECT sequence FROM workspace_operations WHERE id=?", o.ID).Scan(&got); err != nil || got != before[i] {
						t.Fatalf("VACUUM/reopen changed durable order: got=%d want=%d err=%v", got, before[i], err)
					}
				}
				v, err := reopened.PublishWorkerArtifact(ctx, role, WorkerArtifactInput{RelativePath: latest.Input.RelativePath, Name: "重开后按提交顺序发布"}, WorkerArtifactBlob{Hash: latest.Input.AfterHash, Size: latest.Input.AfterSize}, "reopened-path")
				if err != nil {
					t.Fatalf("reopened path publication: %v; cause=%v", err, errors.Unwrap(err))
				}
				var linked string
				if err := reopened.db.QueryRow("SELECT operation_id FROM worker_artifact_publications WHERE version_id=?", v.ID).Scan(&linked); err != nil || linked != latest.ID {
					t.Fatalf("reopened path lost durable order: %q,%v", linked, err)
				}
				dispatchAssertFK(t, reopened)
			})
		})
	}
}

func TestWorkspaceOperationSQLSequenceIntegrity(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	template := workspaceOperationDone(t, f.s, role, "template")
	var sequence int64
	if err := f.s.db.QueryRow("SELECT sequence FROM workspace_operations WHERE id=?", template.ID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	m, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: f.groupID, TopicID: role.Member.TopicID, ActorID: f.worker, Content: "平台序号约束夹具", RecipientIDs: []string{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	query := strings.Replace(workspaceOperationClockIntentSQL, "(id,", "(sequence,id,", 1)
	query = strings.Replace(query, "SELECT ?,", "SELECT ?,?,", 1)
	for _, ordinal := range []any{0, -1, 1.5, sequence} {
		if _, err := tx.Exec("SAVEPOINT sequence_case"); err != nil {
			t.Fatal(err)
		}
		insert := query
		if ordinal == sequence {
			insert = strings.Replace(insert, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
		}
		_, err := tx.Exec(insert, ordinal, "sequence-fixture", "sequence-fixture", "clock/order.txt", m.ID, time.Now().UTC().Format(timestampLayout), template.ID)
		if err == nil || ordinal == sequence && !strings.Contains(err.Error(), "文件操作意图不可替换") {
			t.Errorf("invalid/replaced sequence %v escaped its SQL guard: %v", ordinal, err)
		}
		if _, err := tx.Exec("ROLLBACK TO sequence_case; RELEASE sequence_case"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceOperationFinishPreservesBackwardClockObservation(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	template := workspaceOperationDone(t, f.s, role, "template")
	claims := messageSequenceTableRows(t, f.s, "resource_claims")
	for _, outcome := range []string{"unchanged", "done"} {
		o := workspaceOperationClockIntent(t, f.s, role, template, "backward-"+outcome, time.Now().UTC().Add(time.Hour))
		result := WorkspaceOperationResult{Outcome: outcome, ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize}
		if outcome == "unchanged" {
			result.ObservedHash, result.ObservedSize = o.Preimage.Hash, o.Preimage.Size
		}
		before := time.Now().UTC()
		done, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, result)
		after := time.Now().UTC()
		if err != nil {
			t.Fatalf("trusted %s result rejected after clock correction: %v; cause=%v", outcome, err, errors.Unwrap(err))
		}
		if done.CreatedAt != o.CreatedAt || done.FinishedAt == nil || done.FinishedAt.Before(before) || done.FinishedAt.After(after) || !done.FinishedAt.Before(done.CreatedAt) || done.Result == nil || *done.Result != result {
			t.Fatalf("real clock observation/result was changed: %+v, window=[%v,%v]", done, before, after)
		}
		var stored string
		if err := f.s.db.QueryRow("SELECT finished_at FROM workspace_operation_receipts WHERE operation_id=?", o.ID).Scan(&stored); err != nil || stored != done.FinishedAt.Format(timestampLayout) {
			t.Fatalf("SQLite lost observed Finish time: %q,%v", stored, err)
		}
		if replay, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, result); err != nil || !reflect.DeepEqual(replay, done) {
			t.Fatalf("immutable backward-clock receipt replay: %+v,%v; cause=%v", replay, err, errors.Unwrap(err))
		}
		if unresolved, err := f.s.HasUnresolvedWorkspaceOperations(ctx, f.groupID, o.TurnID); err != nil || unresolved {
			t.Fatalf("trusted %s result remained pending: %t,%v", outcome, unresolved, err)
		}
	}
	if !reflect.DeepEqual(claims, messageSequenceTableRows(t, f.s, "resource_claims")) {
		t.Fatal("recording trusted clock observations released resource claims")
	}
	dispatchAssertFK(t, f.s)
}

func TestWorkspaceOperationUnicodeControlCc(t *testing.T) {
	var controls []rune
	for c := rune(0); c <= 0x9f; c++ {
		if c <= 0x1f || c >= 0x7f {
			controls = append(controls, c)
		}
	}
	t.Run("go", func(t *testing.T) {
		for _, c := range controls {
			input := workspaceOperationInput()
			input.RelativePath = "a" + string(c) + "b"
			if err := validateWorkspaceOperationInput(input, "key"); !errors.Is(err, ErrInvalid) {
				t.Errorf("Unicode Cc U+%04X admitted in path: %v", c, err)
			}
			if workspaceOperationText("a"+string(c)+"b", 256, false) {
				t.Errorf("Unicode Cc U+%04X admitted in identifier/key", c)
			}
		}
		for _, c := range []rune{0x00a0, 0x200b, 0x2028} {
			input := workspaceOperationInput()
			input.RelativePath = "a" + string(c) + "b"
			if err := validateWorkspaceOperationInput(input, "key"); err != nil {
				t.Errorf("non-Cc U+%04X was over-rejected: %v", c, err)
			}
		}
	})
	t.Run("sql", func(t *testing.T) {
		f, _, role := workspaceOperationFixture(t)
		template := workspaceOperationDone(t, f.s, role, "template")
		tx, err := f.s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		m, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: f.groupID, TopicID: role.Member.TopicID, ActorID: f.worker, Content: "平台路径校验夹具", RecipientIDs: []string{}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.Prepare(workspaceOperationClockIntentSQL)
		if err != nil {
			t.Fatal(err)
		}
		defer stmt.Close()
		for _, c := range append(controls, 'x', 0x00a0, 0x200b, 0x2028) {
			if _, err := tx.Exec("SAVEPOINT path_case"); err != nil {
				t.Fatal(err)
			}
			_, err := stmt.Exec("sql-control", "sql-control", "a"+string(c)+"b", m.ID, time.Now().UTC().Format(timestampLayout), template.ID)
			want := c > 0x9f || c > 0x1f && c < 0x7f
			if (err == nil) != want {
				t.Errorf("real SQL path CHECK U+%04X: shouldAccept=%t, err=%v", c, want, err)
			}
			if _, err := tx.Exec("ROLLBACK TO path_case; RELEASE path_case"); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("real v21 INSERT guards checked all %d Unicode Cc codepoints and non-Cc samples", len(controls))
	})
}
