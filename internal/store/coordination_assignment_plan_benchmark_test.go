package store

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

// This is a planner baseline, not an authorization test. Use -benchtime=1x:
// an empty, unanalyzed Store still compiles the real gate with nonempty bindings.
func BenchmarkCoordinationAssignmentStartPlan(b *testing.B) {
	b.StopTimer()
	started := time.Now()
	s, err := Open(filepath.Join(b.TempDir(), "assignment-plan.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	b.Logf("open_schema_elapsed=%s", time.Since(started))

	var version, sourceID string
	var schemaVersion, assignments, statsTables int
	if err := s.db.QueryRowContext(ctx, `SELECT sqlite_version(),sqlite_source_id(),
 (SELECT max(version) FROM schema_migrations),
 (SELECT count(*) FROM coordination_assignments),
 (SELECT count(*) FROM sqlite_schema WHERE name IN ('sqlite_stat1','sqlite_stat4'))`).Scan(
		&version, &sourceID, &schemaVersion, &assignments, &statsTables); err != nil {
		b.Fatal(err)
	}
	b.Logf("go=%s driver=%T sqlite_version=%s sqlite_source_id=%s schema_version=%d assignment_rows=%d stats_tables=%d",
		runtime.Version(), s.db.Driver(), version, sourceID, schemaVersion, assignments, statsTables)
	if assignments != 0 || statsTables != 0 {
		b.Fatal("planner baseline requires a fresh, unanalyzed Store")
	}

	query := "SELECT EXISTS(SELECT 1" + coordinationAssignmentRelations + coordinationAssignmentStart + ")"
	v := assignmentSnapshot{
		a: domain.Assignment{ID: "plan-assignment", RequestID: "plan-request", DecisionID: "plan-decision",
			TaskID: "plan-task", GroupID: "plan-group", WorkerMemberID: "plan-worker",
			CreatedAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)},
		r: domain.DispatchRequest{TopicID: "plan-topic", TaskRevision: 1, AuthorityGeneration: 1,
			MandateID: "plan-mandate", CoordinatorMemberID: "plan-coordinator", BindingRevision: 1, PolicyRevision: 1},
		coordinatorTurn: "plan-coordinator-turn",
	}
	args := append(v.args(), sql.Named("own", "plan-attempt"), sql.Named("turn", "plan-worker-turn"))
	b.Logf("sql_bytes=%d sql_sha256=%x named_args=%#v", len(query), sha256.Sum256([]byte(query)), args)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			b.Error(err)
		}
	}()

	b.Run("PrepareExecute", func(b *testing.B) {
		b.StopTimer()
		var prepareElapsed, executeElapsed time.Duration
		b.ResetTimer()
		for i := range b.N {
			b.Logf("sample=%d phase=prepare_begin", i+1)
			b.StartTimer()
			started := time.Now()
			stmt, err := tx.PrepareContext(ctx, query)
			elapsed := time.Since(started)
			prepareElapsed += elapsed
			b.StopTimer()
			if err != nil {
				b.Fatalf("prepare_elapsed=%s: %v", elapsed, err)
			}
			b.Logf("sample=%d prepare_elapsed=%s phase=bound_execute_begin", i+1, elapsed)

			// Prepare has no bindings in database/sql. This phase includes binding,
			// possible sqlite3Reprepare in step, and the scalar Scan, not just row IO.
			var allowed bool
			b.StartTimer()
			started = time.Now()
			err = stmt.QueryRowContext(ctx, args...).Scan(&allowed)
			elapsed = time.Since(started)
			executeElapsed += elapsed
			closeErr := stmt.Close()
			b.StopTimer()
			if err != nil || closeErr != nil {
				b.Fatalf("bind_step_scan_elapsed=%s query_error=%v close_error=%v", elapsed, err, closeErr)
			}
			b.Logf("sample=%d bind_step_scan_elapsed=%s result_rows=1 allowed=%t (empty-store diagnostic only)", i+1, elapsed, allowed)
		}
		b.ReportMetric(float64(prepareElapsed.Nanoseconds())/float64(b.N), "prepare-ns/op")
		b.ReportMetric(float64(executeElapsed.Nanoseconds())/float64(b.N), "bind-step-scan-ns/op")
	})

	b.Run("Explain", func(b *testing.B) {
		b.StopTimer()
		var planElapsed time.Duration
		var plan []string
		b.ResetTimer()
		for i := range b.N {
			b.Logf("sample=%d phase=explain_begin", i+1)
			b.StartTimer()
			started := time.Now()
			rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				b.Fatalf("plan_elapsed=%s: %v", time.Since(started), err)
			}
			plan = plan[:0]
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					rows.Close()
					b.Fatal(err)
				}
				plan = append(plan, fmt.Sprintf("id=%d parent=%d detail=%s", id, parent, detail))
			}
			err = rows.Err()
			closeErr := rows.Close()
			elapsed := time.Since(started)
			planElapsed += elapsed
			b.StopTimer()
			if err != nil || closeErr != nil {
				b.Fatalf("plan_elapsed=%s rows_error=%v close_error=%v", elapsed, err, closeErr)
			}
			b.Logf("sample=%d plan_elapsed=%s plan_rows=%d", i+1, elapsed, len(plan))
		}
		b.ReportMetric(float64(planElapsed.Nanoseconds())/float64(b.N), "plan-ns/op")
		b.ReportMetric(float64(len(plan)), "plan-rows/op")
		for _, step := range plan {
			b.Log(step)
		}
	})
}
