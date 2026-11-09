package store

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestCoordinationAssignmentV13MigrationFrozenHistory(t *testing.T) {
	want := []string{
		"0c4b8cb8a1b1dde7a98596f45e8c6996d892392deac0481a83bc9f73f8eca64c",
		"15f9694d11b3e60c893dd633e815a97ada9a1b219c621c157eb7eab76dff1b28",
		"6161295e75c2dcd7435d451f2a8359fb1aff445da0209aa2e42f599f25bb8399",
		"a5e0d499caaa68a606af016f72b1022679639eab38594a12759976075a761714",
		"f843e64ea2d1b7611c781f513a102e3d52fbb67317c7a8e23953a2d28a9538e4",
		"d73117e5319acdd6884ff57df747dd67cd2f2958efac5318799cd1677a1ab32c",
		"f68370d3140ed156c84567f4cd99f82381293b656486ba1dd18691cfdf08a77f",
		"f8f82d4a122e630608f5e78760882a1ac025ed38bd6f76edb7013cff1c6f8e5f",
		"efa3930f60093437d002bf41c748f987066821fa6036aacaec60de78e30ee659",
		"4b6c8c8252d7968a083fdffb19fa0216a17813e90716b485d6b64c78eb3d27fe",
		"899b3bbe85055d44ecefbb5176c8c98d157ed1edbba10815ba2765bc61f1cc41",
		"9dd7c579f374cf961802aea6066e6c62a787186de10ecd8c285010439607bf7c",
	}
	if len(migrations) < 13 {
		t.Fatalf("v13 Assignment migration missing: %d migrations", len(migrations))
	}
	if migrations[12] != coordinationAssignmentSchemaV13 {
		t.Fatal("v13 must remain the Assignment schema at index 12")
	}
	for i, hash := range want {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations[i]))); got != hash {
			t.Errorf("frozen v%d SQL: %s; want %s", i+1, got, hash)
		}
		t.Logf("v%d SQL sha256=%s", i+1, hash)
	}
	files := map[string]string{
		"artifact_schema.go":               "7b6e35217f804b13f9c8ca270884c691c62371b1ada3e1a4a8fa6aaf392604f2",
		"task_artifact_schema.go":          "dcd0302eede4bb5902110942ea59260ab2c0573982cf04cb0f73c47b986a4ad1",
		"message_sequence_schema.go":       "34cad45d4aa97735700fc94de7d28a12b7a401358053b5d6a0fae9f45a1ee5d8",
		"coordination_trigger_schema.go":   "43f2ea03b95b15d78d82159d3be1e2b73057bafaf91fb4b96404e3ad0ad304ac",
		"coordination_admission_schema.go": "e486be398009f0113ef5a7ad79645ca4467d25294ec55cfb385d5ec5509ef2c1",
		"coordinator_schema.go":            "2bdb618c28e9061741f61867661a25df603fab1e28d70bb8b7a019f0f1e4fd4a",
		"coordination_dispatch_schema.go":  "edb16604950a583eff5bbbc3e6adc884e91026c593deaa780390cd1f60e8833b",
	}
	for path, hash := range files {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(source)); got != hash {
			t.Errorf("frozen source %s: %s; want %s", path, got, hash)
		}
	}
	source, err := os.ReadFile("migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := assignmentCheckMigrationSource(source); err != nil {
		t.Fatal(err)
	}
	t.Log("runner sha256=289b8c8bf09c1f8cb4e59a43b10eeb75020791a00759d9617b57cc400a84e4c5; all v1-v12 SQL source bytes unchanged")
}

func assignmentCheckMigrationSource(source []byte) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "migrations.go", source, 0)
	if err != nil {
		return err
	}
	var literal *ast.CompositeLit
	for _, declaration := range file.Decls {
		decl, ok := declaration.(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR {
			continue
		}
		for _, declaration := range decl.Specs {
			spec := declaration.(*ast.ValueSpec)
			for _, name := range spec.Names {
				if name.Name != "migrations" {
					continue
				}
				if literal != nil || len(spec.Names) != 1 || len(spec.Values) != 1 {
					return fmt.Errorf("migration history must have one slice declaration")
				}
				literal, ok = spec.Values[0].(*ast.CompositeLit)
				if !ok {
					return fmt.Errorf("migration history must remain a slice literal")
				}
			}
		}
	}
	if literal == nil || len(literal.Elts) < 13 {
		return fmt.Errorf("migration source must retain v1-v13")
	}
	offset := fset.File(file.Pos()).Offset
	for i := 12; i < len(literal.Elts); i++ {
		identifier, ok := literal.Elts[i].(*ast.Ident)
		if !ok || (i == 12 && identifier.Name != "coordinationAssignmentSchemaV13") {
			return fmt.Errorf("migration v%d must append a schema identifier; v13 must name coordinationAssignmentSchemaV13", i+1)
		}
		gap := source[offset(literal.Elts[i-1].End()):offset(identifier.Pos())]
		if strings.TrimSpace(string(gap)) != "," {
			return fmt.Errorf("migration v%d changed source outside its appended identifier", i+1)
		}
	}
	gap := strings.TrimSpace(string(source[offset(literal.Elts[len(literal.Elts)-1].End()):offset(literal.Rbrace)]))
	if gap != "" && gap != "," {
		return fmt.Errorf("unexpected source after the appended schema identifiers")
	}
	// Remove only the identifier tail; every historical byte, including the runner, stays hashed.
	original := string(source[:offset(literal.Elts[11].End())]) + string(source[offset(literal.Rbrace):])
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(original))); got != "9250e1428695d914886a18329f163df84c04438d9b437495fb9d8675c3eb46b9" {
		return fmt.Errorf("migrations.go changed beyond appended schema identifiers: %s", got)
	}
	_, runner, found := strings.Cut(string(source), "// Migration history")
	if !found {
		return fmt.Errorf("runner marker missing")
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte("// Migration history"+runner))); got != "289b8c8bf09c1f8cb4e59a43b10eeb75020791a00759d9617b57cc400a84e4c5" {
		return fmt.Errorf("frozen runner: %s", got)
	}
	return nil
}

func TestCoordinationAssignmentV13MigrationSourceHistoryBoundaries(t *testing.T) {
	source, err := os.ReadFile("migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, old, replacement string
		valid                  bool
	}{
		{"append-one", "coordinationAssignmentSchemaV13", "coordinationAssignmentSchemaV13, assignmentFutureSchemaV14", true},
		{"append-multiline", "coordinationAssignmentSchemaV13", "coordinationAssignmentSchemaV13,\n assignmentFutureSchemaV14,\n assignmentFutureSchemaV15", true},
		{"old-sql", "CREATE TABLE groups (", "CREATE  TABLE groups (", false},
		{"old-constant", "coordinationDispatchSchemaV12", "changedDispatchSchemaV12", false},
		{"v13-identity", "coordinationAssignmentSchemaV13", "changedAssignmentSchemaV13", false},
		{"tail-call", "coordinationAssignmentSchemaV13", "coordinationAssignmentSchemaV13, futureSchema()", false},
		{"tail-literal", "coordinationAssignmentSchemaV13", "coordinationAssignmentSchemaV13, `SELECT 1;`", false},
		{"tail-comment", "coordinationAssignmentSchemaV13", "coordinationAssignmentSchemaV13 /* unowned source */", false},
		{"runner", "tx, err := db.Begin()", "tx, err := db.BeginTx(nil, nil)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(string(source), tc.old) {
				t.Fatalf("source fixture boundary missing: %s", tc.old)
			}
			err := assignmentCheckMigrationSource([]byte(strings.Replace(string(source), tc.old, tc.replacement, 1)))
			if (err == nil) != tc.valid {
				t.Fatalf("source valid=%t, want %t: %v", err == nil, tc.valid, err)
			}
		})
	}
}

type assignmentV12Snapshot struct {
	Request   domain.DispatchRequest
	State     domain.DispatchState
	UpdatedAt time.Time
}

func assignmentTypedV12(t *testing.T, s *Store) []assignmentV12Snapshot {
	t.Helper()
	var columns []string
	for _, column := range strings.Split(dispatchRequestColumns, ",") {
		columns = append(columns, "r."+column)
	}
	rows, err := s.db.Query(`SELECT ` + strings.Join(columns, ",") + `,s.generation,s.status,s.attempt_count,s.next_attempt_at,s.updated_at,s.last_error_code,s.held_reason,
	 s.message_id,s.delivery_id,s.conversation_id,s.delivered_member_id,s.materialized_from_generation,s.materialized_at
	 FROM coordination_dispatch_requests r JOIN coordination_dispatch_state s
	 ON (s.request_id,s.task_id,s.group_id)=(r.id,r.task_id,r.group_id) ORDER BY r.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []assignmentV12Snapshot
	parse := func(v string) time.Time {
		t.Helper()
		stamp, err := time.Parse(timestampLayout, v)
		if err != nil {
			t.Fatal(err)
		}
		return stamp
	}
	for rows.Next() {
		var v assignmentV12Snapshot
		r, state := &v.Request, &v.State
		var created, next, updated string
		var message, delivery, conversation, member, materialized sql.NullString
		var generation sql.NullInt64
		if err := rows.Scan(&r.ID, &r.GroupID, &r.TopicID, &r.TaskID, &r.TaskRevision, &r.AuthorityGeneration, &r.MandateID,
			&r.CoordinatorMemberID, &r.BindingRevision, &r.PolicyRevision, &r.RequestedBy, &r.Reason, &created,
			&state.Generation, &state.Status, &state.AttemptCount, &next, &updated, &state.LastErrorCode, &state.HeldReason,
			&message, &delivery, &conversation, &member, &generation, &materialized); err != nil {
			t.Fatal(err)
		}
		r.CreatedAt, state.RequestID, state.NextAttemptAt, v.UpdatedAt = parse(created), r.ID, parse(next), parse(updated)
		if message.Valid {
			if !delivery.Valid || !conversation.Valid || !member.Valid || !generation.Valid || !materialized.Valid {
				t.Fatal("partial v12 receipt")
			}
			state.Delivery = &domain.DispatchDeliveryReceipt{MessageID: message.String, DeliveryID: delivery.String, ConversationID: conversation.String,
				CoordinatorMemberID: member.String, MaterializedFromGeneration: generation.Int64, CreatedAt: parse(materialized.String)}
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func assignmentV12Fixture(t *testing.T) (*Store, string, func(*Store)) {
	t.Helper()
	s, path, g, c, check := dispatchV11Fixture(t)
	original := migrations
	migrations = migrations[:12]
	err := migrate(s.db)
	migrations = original
	if err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, s, 12)
	for _, status := range []string{"pending", "backoff", "held-no-receipt", "enqueued"} {
		r := dispatchCandidate(t, s, g, c, "v12-"+status)
		coordinatorMustCommit(t, s, dispatchPair(r)...)
		switch status {
		case "backoff":
			r.state[3], r.state[5], r.state[6], r.state[8] = int64(2), int64(1), "2026-01-02T03:04:07.123456789Z", "sqlite_busy"
			coordinatorMustCommit(t, s, dispatchUpdate(r.state))
		case "held-no-receipt":
			r.state[3], r.state[4], r.state[9] = int64(2), "held", "no_eligible_worker"
			coordinatorMustCommit(t, s, dispatchUpdate(r.state))
		case "enqueued":
			coordinatorMustCommit(t, s, dispatchUpdate(dispatchReceipt(t, s, r)))
		}
	}
	var heldTask string
	if err := s.db.QueryRow("SELECT task_id FROM coordination_dispatch_state WHERE status='held'").Scan(&heldTask); err != nil {
		t.Fatal(err)
	}
	task, err := s.GetTask(ctx, g, heldTask)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
	if err != nil {
		t.Fatal(err)
	}
	submit := taskSubmissionInput(attempt)
	submit.IdempotencyKey = "assignment-v12-submit"
	submission, err := s.SubmitTask(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	review := taskReviewInput(submission)
	review.Outcome, review.IdempotencyKey = domain.ReviewOutcomeRejected, "assignment-v12-review"
	if _, err := s.ReviewTask(ctx, review); err != nil {
		t.Fatal(err)
	}
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{g, 2, c}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g, 2, "[]"}},
		coordinatorSQL{"UPDATE group_coordinator_bindings SET revision=2 WHERE group_id=?", []any{g}},
		coordinatorSQL{"UPDATE group_dispatch_policies SET revision=2 WHERE group_id=?", []any{g}},
		coordinatorSQL{"UPDATE members SET status='left' WHERE id=?", []any{c}},
	)
	typed := assignmentTypedV12(t, s)
	if len(typed) != 4 {
		t.Fatalf("populated v12 fixture: %+v", typed)
	}
	return s, path, func(current *Store) {
		t.Helper()
		check(current) // Typed replays, real unknown/cancel_requested, receipts and resource claims.
		if got := assignmentTypedV12(t, current); !reflect.DeepEqual(got, typed) {
			t.Fatalf("typed v12 requests/state/receipts changed: %+v; want %+v", got, typed)
		}
	}
}

type assignmentOriginalTableSnapshot struct {
	columns [][]any // Original names and declared types, in column order.
	rows    [][]any // Every original value paired with its SQLite storage type.
}

func assignmentOriginalColumns(t *testing.T, s *Store, table string) [][]any {
	t.Helper()
	return messageSequenceRows(t, s.db, "SELECT name,type FROM pragma_table_xinfo('"+strings.ReplaceAll(table, "'", "''")+"') WHERE hidden<>1 ORDER BY cid")
}

func assignmentOriginalRows(t *testing.T, s *Store, table string, columns [][]any) [][]any {
	t.Helper()
	var projection, order []string
	for _, column := range columns {
		quoted := `"` + strings.ReplaceAll(column[0].(string), `"`, `""`) + `"`
		projection = append(projection, "typeof("+quoted+")", quoted)
		// Match the shared snapshot's type-aware, collation-independent ordering.
		order = append(order, "typeof("+quoted+")", quoted+" COLLATE BINARY")
	}
	return messageSequenceRows(t, s.db, `SELECT `+strings.Join(projection, ",")+` FROM "`+strings.ReplaceAll(table, `"`, `""`)+`" ORDER BY `+strings.Join(order, ","))
}

func assignmentSnapshotOriginalTables(t *testing.T, s *Store, original map[string][][]any) map[string]assignmentOriginalTableSnapshot {
	t.Helper()
	snapshot := make(map[string]assignmentOriginalTableSnapshot, len(original))
	for table := range original {
		columns := assignmentOriginalColumns(t, s, table)
		snapshot[table] = assignmentOriginalTableSnapshot{columns: columns, rows: assignmentOriginalRows(t, s, table, columns)}
	}
	return snapshot
}

func assignmentCheckOriginalTables(t *testing.T, s *Store, snapshot map[string]assignmentOriginalTableSnapshot) error {
	t.Helper()
	tables := messageSequenceRows(t, s.db, "SELECT name FROM sqlite_master WHERE type='table'")
	for table, want := range snapshot {
		if !slices.ContainsFunc(tables, func(row []any) bool { return row[0] == table }) {
			return fmt.Errorf("future migration removed original %s table", table)
		}
		columns := assignmentOriginalColumns(t, s, table)
		for _, column := range want.columns {
			if !slices.ContainsFunc(columns, func(got []any) bool { return reflect.DeepEqual(got, column) }) {
				return fmt.Errorf("future migration changed or removed original %s column: %v", table, column)
			}
		}
		if got := assignmentOriginalRows(t, s, table, want.columns); !reflect.DeepEqual(got, want.rows) {
			return fmt.Errorf("future migration changed original %s rows (including SQLite types): %+v; want %+v", table, got, want.rows)
		}
	}
	return nil
}

func TestCoordinationAssignmentV13MigrationOriginalProjectionRejectsCorruption(t *testing.T) {
	for _, tc := range []struct {
		name, sql, want string
	}{
		{"value", "UPDATE assignment_projection_fixture SET label='changed' WHERE id=1", "original assignment_projection_fixture rows"},
		{"missing-row", "DELETE FROM assignment_projection_fixture WHERE id=1", "original assignment_projection_fixture rows"},
		{"extra-row", "INSERT INTO assignment_projection_fixture VALUES(6,1,'extra')", "original assignment_projection_fixture rows"},
		{"storage-type", "UPDATE assignment_projection_fixture SET value=CAST(value AS REAL) WHERE id=1", "original assignment_projection_fixture rows"},
		{"declared-type-empty-table", "DROP TABLE assignment_projection_empty; CREATE TABLE assignment_projection_empty(id TEXT,value TEXT)", "original assignment_projection_empty column"},
		{"missing-column", "ALTER TABLE assignment_projection_fixture DROP COLUMN label", "original assignment_projection_fixture column"},
		{"missing-table", "DROP TABLE assignment_projection_fixture", "original assignment_projection_fixture table"},
		{"empty-table-row", "INSERT INTO assignment_projection_empty VALUES('unexpected',X'31')", "original assignment_projection_empty rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := assignmentV13Store(t)
			coordinatorMustCommit(t, s, coordinatorSQL{`CREATE TABLE assignment_projection_fixture(id INTEGER PRIMARY KEY,value,label TEXT COLLATE NOCASE);
			 INSERT INTO assignment_projection_fixture VALUES(1,1,'A'),(2,1.0,'a'),(3,'1','A'),(4,X'31','a'),(5,NULL,'z');
			 CREATE TABLE assignment_projection_empty(id TEXT,value BLOB);`, nil})
			before := assignmentSnapshotOriginalTables(t, s, messageSequenceSnapshot(t, s))
			if err := assignmentCheckOriginalTables(t, s, before); err != nil {
				t.Fatal(err)
			}
			coordinatorMustCommit(t, s, coordinatorSQL{tc.sql, nil})
			err := assignmentCheckOriginalTables(t, s, before)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("did not detect %s corruption: %v; want %q", tc.name, err, tc.want)
			}
			t.Logf("rejected %s: %v", tc.name, err)
		})
	}
}

const assignmentSchemaQuery = "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name"

func TestCoordinationAssignmentV13MigrationPopulatedUpgradeReopenAndAppend(t *testing.T) {
	assignmentV13PopulatedUpgradeReopenAndAppend(t)
}

func TestCoordinationAssignmentV13MigrationFutureNullableColumn(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	// This synthetic extension proves compatibility, not a real N3a2 migration.
	migrations = append(slices.Clone(original), `ALTER TABLE coordination_dispatch_state ADD COLUMN assignment_test_nullable TEXT;`)
	assignmentV13PopulatedUpgradeReopenAndAppend(t)
}

func assignmentV13PopulatedUpgradeReopenAndAppend(t *testing.T) {
	t.Helper()
	allMigrations := migrations
	defer func() { migrations = allMigrations }()
	migrations = allMigrations[:13]
	old, path, check := assignmentV12Fixture(t)
	before := messageSequenceSnapshot(t, old)
	originalTables := assignmentSnapshotOriginalTables(t, old, before)
	objects := messageSequenceRows(t, old.db, assignmentSchemaQuery)
	if err := migrate(old.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, old, 13)
	messageSequencePreserved(t, old, before)
	check(old)
	assignmentEmpty(t, old)
	current := messageSequenceRows(t, old.db, assignmentSchemaQuery)
	for _, original := range objects {
		if !slices.ContainsFunc(current, func(row []any) bool { return reflect.DeepEqual(original, row) }) {
			t.Fatalf("v13 changed old schema: %v", original)
		}
	}
	var tables []string
	for _, object := range current {
		if slices.ContainsFunc(objects, func(row []any) bool { return reflect.DeepEqual(object, row) }) {
			continue
		}
		if !slices.Contains([]any{"coordination_assignments", "coordination_assignment_dispatch", "coordination_assignment_attempts"}, object[2]) {
			t.Fatalf("v13 added an object on an old/unowned table: %v", object)
		}
		if object[0] == "table" {
			tables = append(tables, object[1].(string))
		}
	}
	if !slices.Equal(tables, []string{"coordination_assignment_attempts", "coordination_assignment_dispatch", "coordination_assignments"}) {
		t.Fatalf("v13 must add exactly three tables: %v", tables)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s := assignmentV13Open(t, path)
	messageSequencePreserved(t, s, before)
	check(s)
	assignmentEmpty(t, s)
	if got := messageSequenceRows(t, s.db, assignmentSchemaQuery); !reflect.DeepEqual(got, current) {
		t.Fatal("reopen changed v13 schema")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	const futureFixture = `CREATE TABLE assignment_future_fixture(id TEXT PRIMARY KEY, value TEXT NOT NULL);
	 INSERT INTO assignment_future_fixture VALUES('applied','future fixture ran');`
	// Keep every real future migration, and prove a failure at the appended tail rolls them all back.
	migrations = append(slices.Clone(allMigrations), futureFixture+"INSERT INTO assignment_missing_future_fixture VALUES(1);")
	failed, err := Open(path)
	if err == nil {
		_ = failed.Close()
		t.Fatal("failing future migration was swallowed")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("迁移版本 %d", len(migrations))) || !strings.Contains(err.Error(), "assignment_missing_future_fixture") {
		t.Fatalf("did not reach the appended future fault: %v", err)
	}
	t.Logf("actual future append rollback: %v", err)
	rolledBack := assignmentV13Open(t, path)
	messageSequencePreserved(t, rolledBack, before)
	check(rolledBack)
	assignmentEmpty(t, rolledBack)
	if got := messageSequenceRows(t, rolledBack.db, assignmentSchemaQuery); !reflect.DeepEqual(got, current) {
		t.Fatal("failed future append leaked schema or changed v13 objects")
	}
	if err := rolledBack.Close(); err != nil {
		t.Fatal(err)
	}
	migrations = append(slices.Clone(allMigrations), futureFixture)
	var future [][]any
	for i := range 2 {
		reopened := queueHandle(t, path, false)
		messageSequenceLedger(t, reopened, len(migrations))
		if err := assignmentCheckOriginalTables(t, reopened, originalTables); err != nil {
			t.Fatal(err)
		}
		check(reopened)
		assignmentEmpty(t, reopened)
		if got := messageSequenceTableRows(t, reopened, "assignment_future_fixture"); !reflect.DeepEqual(got, [][]any{{"applied", "future fixture ran"}}) {
			t.Fatalf("appended future migration did not persist its sentinel: %v", got)
		}
		got := messageSequenceRows(t, reopened.db, assignmentSchemaQuery)
		if i == 0 {
			future = got
		} else if !reflect.DeepEqual(got, future) {
			t.Fatal("future append/reopen was not idempotent")
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoordinationAssignmentV13MigrationFailureRollbackAndRetry(t *testing.T) {
	for _, failure := range []string{"late-ddl", "commit"} {
		t.Run(failure, func(t *testing.T) {
			old, path, check := assignmentV12Fixture(t)
			original := migrations
			defer func() { migrations = original }()
			v13Migrations := original[:13]
			migrations = v13Migrations
			if failure == "late-ddl" {
				coordinatorMustCommit(t, old, coordinatorSQL{`CREATE TRIGGER coordination_assignment_attempts_no_delete BEFORE DELETE ON groups WHEN 0 BEGIN SELECT 1; END`, nil})
			} else {
				coordinatorMustCommit(t, old, coordinatorSQL{"CREATE TABLE assignment_migration_commit_fixture(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)", nil})
				migrations = slices.Clone(v13Migrations)
				migrations[12] += "INSERT INTO assignment_migration_commit_fixture VALUES('missing');"
			}
			before := messageSequenceSnapshot(t, old)
			objects := messageSequenceRows(t, old.db, assignmentSchemaQuery)
			err := migrate(old.db)
			if err == nil {
				t.Fatal("injected migration fault committed")
			}
			if failure == "late-ddl" && !strings.Contains(err.Error(), "迁移版本 13") {
				t.Fatalf("did not reach v13 late DDL: %v", err)
			}
			if failure == "commit" {
				coordinationAdmissionRequireFK(t, err)
			}
			t.Logf("actual v13 %s rollback: %v", failure, err)
			messageSequenceLedger(t, old, 12)
			messageSequencePreserved(t, old, before)
			check(old)
			if got := messageSequenceRows(t, old.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
				t.Fatalf("failed v13 leaked schema or modified old objects: %v", got)
			}
			migrations = v13Migrations
			if failure == "late-ddl" {
				coordinatorMustCommit(t, old, coordinatorSQL{"DROP TRIGGER coordination_assignment_attempts_no_delete", nil})
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := assignmentV13Open(t, path)
			messageSequenceLedger(t, reopened, 13)
			messageSequencePreserved(t, reopened, before)
			check(reopened)
			assignmentEmpty(t, reopened)
		})
	}
}
