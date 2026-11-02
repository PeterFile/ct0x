package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCoordinationDispatchSchemaCanonicalTimestamps(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "time")
	valid := []string{
		"0001-01-01T00:00:00.000000000Z", "0004-02-29T23:59:59.999999999Z",
		"1900-02-28T23:59:59.999999999Z", "2000-02-29T12:34:56.000000001Z",
		"2024-02-29T23:59:59.999999999Z", "9999-12-31T23:59:59.999999999Z",
	}
	for _, stamp := range valid {
		t.Run("valid/"+stamp, func(t *testing.T) {
			values := dispatchSQLRows{slices.Clone(rows.request), slices.Clone(rows.state)}
			values.request[12], values.state[6], values.state[7] = stamp, stamp, stamp
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, statement := range dispatchPair(values) {
				if _, err := tx.Exec(statement.query, statement.args...); err != nil {
					t.Fatalf("valid canonical time rejected: %v", err)
				}
			}
			var got string
			if err := tx.QueryRow("SELECT created_at FROM coordination_dispatch_requests WHERE id=?", rows.request[0]).Scan(&got); err != nil || got != stamp {
				t.Fatalf("nanoseconds were lost: %q, %v", got, err)
			}
		})
	}
	invalid := []string{
		"0000-01-01T00:00:00.000000000Z", "10000-01-01T00:00:00.000000000Z",
		"0001-02-29T00:00:00.000000000Z", "1900-02-29T00:00:00.000000000Z", "2100-02-29T00:00:00.000000000Z",
		"2025-02-29T00:00:00.000000000Z", "2024-02-30T00:00:00.000000000Z", "2026-04-31T00:00:00.000000000Z",
		"2026-00-01T00:00:00.000000000Z", "2026-13-01T00:00:00.000000000Z", "2026-01-00T00:00:00.000000000Z",
		"2026-01-32T00:00:00.000000000Z", "2026-01-02T24:00:00.000000000Z", "2026-01-02T23:60:00.000000000Z",
		"2026-01-02T23:59:60.000000000Z", "2026-01-02T03:04:05.12345678Z", "2026-01-02T03:04:05.1234567890Z",
		"2026-01-02T03:04:05.123456789z", "2026-01-02t03:04:05.123456789Z", "2026-01-02 03:04:05.123456789Z",
		"2026-01-02T03:04:05.123456789+00:00", "２０２６-01-02T03:04:05.123456789Z", "202a-01-02T03:04:05.123456789Z",
		"2026-01-02T03:04:05.12345678aZ", "2026-01-02T03:04:05.123456789Z\x00", "", "now", "not-a-date",
	}
	for _, stamp := range invalid {
		t.Run("invalid-request/"+stamp, func(t *testing.T) {
			bad := dispatchSQLRows{slices.Clone(rows.request), slices.Clone(rows.state)}
			bad.request[12], bad.state[6], bad.state[7] = stamp, stamp, stamp
			dispatchReject(t, s, "", dispatchPair(bad)...)
		})
	}
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
	receipt := dispatchReceipt(t, s, rows)
	for _, stamp := range invalid {
		for _, column := range []string{"next_attempt_at", "updated_at", "materialized_at"} {
			t.Run("invalid-state/"+column+"/"+stamp, func(t *testing.T) {
				values := dispatchSet(dispatchStateColumns, receipt, column, stamp)
				if column != "next_attempt_at" {
					values[7], values[15] = stamp, stamp
				}
				dispatchReject(t, s, "", dispatchUpdate(values))
			})
		}
	}
}

func TestCoordinationDispatchSchemaBackoffNanosecondCalendarArithmetic(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "calendar-backoff")
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
	for _, at := range []string{
		"0001-01-01T00:00:00.000000001Z", "0004-02-28T23:59:59.999999999Z",
		"1900-02-28T23:59:59.999999999Z", "1999-12-31T23:59:59.999999999Z",
		"2000-02-29T23:59:59.123456789Z", "2024-02-28T23:59:59.999999999Z",
		"2026-01-02T03:04:59.999999999Z", "9999-12-31T23:59:57.999999999Z",
	} {
		t.Run(at, func(t *testing.T) {
			parsed, err := time.Parse(timestampLayout, at)
			if err != nil {
				t.Fatal(err)
			}
			values := slices.Clone(rows.state)
			values[3], values[5], values[6], values[7], values[8] = int64(2), int64(1), parsed.Add(2*time.Second).Format(timestampLayout), at, "sqlite_busy"
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			statement := dispatchUpdate(values)
			if _, err := tx.Exec(statement.query, statement.args...); err != nil {
				t.Fatalf("exact nanos/calendar backoff rejected: %v", err)
			}
			var next string
			if err := tx.QueryRow("SELECT next_attempt_at FROM coordination_dispatch_state WHERE request_id=?", rows.request[0]).Scan(&next); err != nil || next != values[6] {
				t.Fatalf("backoff lost precision: %q, %v; want %s", next, err, values[6])
			}
		})
	}
	for _, at := range []string{"9999-12-31T23:59:58.000000000Z", "9999-12-31T23:59:59.999999999Z"} {
		values := slices.Clone(rows.state)
		values[3], values[5], values[6], values[7], values[8] = int64(2), int64(1), "0001-01-01T00:00:00.000000000Z", at, "sqlite_busy"
		dispatchReject(t, s, "", dispatchUpdate(values))
	}
}

func TestCoordinationDispatchSchemaDeliveryGuardLookupPlans(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	rows := dispatchCandidate(t, s, g, coordinator, "guard-plan")
	coordinatorMustCommit(t, s, dispatchPair(rows)...)
	receipt := dispatchReceipt(t, s, rows)
	coordinatorMustCommit(t, s, dispatchUpdate(receipt))
	for _, statement := range []coordinatorSQL{
		{"SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=?", []any{receipt[11]}},
		{"SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=? AND delivery_id<>?", []any{receipt[11], "other"}},
		{`SELECT 1 FROM deliveries d JOIN coordination_dispatch_state s ON s.delivery_id=d.id
		 WHERE d.message_id=? AND d.recipient_id=?`, []any{receipt[10], receipt[13]}},
		{`SELECT 1 FROM deliveries d JOIN coordination_dispatch_state s ON s.delivery_id=d.id
		 WHERE d.message_id=? AND d.recipient_id=? AND d.id<>?`, []any{receipt[10], receipt[13], "other"}},
	} {
		plan, err := s.db.Query("EXPLAIN QUERY PLAN "+statement.query, statement.args...)
		if err != nil {
			t.Fatal(err)
		}
		var details []string
		for plan.Next() {
			var id, parent, unused int
			var detail string
			if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		if err := plan.Err(); err != nil {
			t.Fatal(err)
		}
		plan.Close()
		detail := strings.Join(details, "\n")
		t.Log(detail)
		if strings.Contains(detail, "SCAN ") || !strings.Contains(detail, "(delivery_id=?)") {
			t.Fatalf("receipt parent guards must use state.delivery_id UNIQUE: %s", detail)
		}
		if strings.Contains(statement.query, "JOIN") && !strings.Contains(detail, "(message_id=? AND recipient_id=?)") {
			t.Fatalf("replacement victims must use the existing delivery UNIQUE: %s", detail)
		}
	}
}

func TestCoordinationDispatchSchemaPendingDueAndExactScopePlans(t *testing.T) {
	s, _ := openTestStore(t)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	other, foreign, _ := dispatchConfiguredGroup(t, s)
	var candidates []dispatchSQLRows
	for i := range 55 {
		rows := dispatchCandidate(t, s, g, coordinator, "due")
		rows.request[0], rows.state[0] = fmt.Sprintf("due-%03d", i), fmt.Sprintf("due-%03d", i)
		coordinatorMustCommit(t, s, dispatchPair(rows)...)
		candidates = append(candidates, rows)
	}
	foreignRows := dispatchCandidate(t, s, other, foreign, "due")
	foreignRows.request[0], foreignRows.state[0] = "zz-foreign", "zz-foreign"
	coordinatorMustCommit(t, s, dispatchPair(foreignRows)...)
	// Exclude held and enqueued despite matching due timestamps.
	held := candidates[53]
	held.state[3], held.state[4], held.state[9] = int64(2), "held", "task_stale"
	coordinatorMustCommit(t, s, dispatchUpdate(held.state))
	enqueued := dispatchReceipt(t, s, candidates[54])
	coordinatorMustCommit(t, s, dispatchUpdate(enqueued))
	const first = `SELECT request_id,generation,next_attempt_at FROM coordination_dispatch_state
	 WHERE status='pending' AND next_attempt_at<=? ORDER BY next_attempt_at,request_id LIMIT ?`
	const following = `SELECT request_id,generation,next_attempt_at FROM coordination_dispatch_state
	 WHERE status='pending' AND next_attempt_at<=? AND (next_attempt_at,request_id)>(?,?)
	 ORDER BY next_attempt_at,request_id LIMIT ?`
	for _, statement := range []coordinatorSQL{
		{first, []any{dispatchStamp, 50}}, {following, []any{dispatchStamp, dispatchStamp, "due-049", 50}},
	} {
		plan, err := s.db.Query("EXPLAIN QUERY PLAN "+statement.query, statement.args...)
		if err != nil {
			t.Fatal(err)
		}
		var descriptions []string
		for plan.Next() {
			var id, parent, unused int
			var detail string
			if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			descriptions = append(descriptions, detail)
		}
		if err := plan.Err(); err != nil {
			t.Fatal(err)
		}
		plan.Close()
		detail := strings.Join(descriptions, "\n")
		t.Log(detail)
		if !strings.Contains(detail, "USING INDEX coordination_dispatch_pending_due") || strings.Contains(detail, "TEMP B-TREE") || strings.Contains(detail, "SCAN ") {
			t.Fatalf("due scan must use the bounded partial range index without sorting: %s", detail)
		}
	}
	read := func(query string, args ...any) [][]any {
		t.Helper()
		result, err := s.db.Query(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer result.Close()
		items := [][]any{}
		for result.Next() {
			var id, next string
			var generation int64
			if err := result.Scan(&id, &generation, &next); err != nil {
				t.Fatal(err)
			}
			items = append(items, []any{id, generation, next})
		}
		if err := result.Err(); err != nil {
			t.Fatal(err)
		}
		return items
	}
	page := read(first, dispatchStamp, 50)
	if len(page) != 50 || page[0][0] != "due-000" || page[49][0] != "due-049" {
		t.Fatalf("SQL LIMIT/original key order: %v", page)
	}
	last := page[len(page)-1]
	page = read(following, dispatchStamp, last[2], last[0], 50)
	if !reflect.DeepEqual(page, [][]any{{"due-050", int64(1), dispatchStamp}, {"due-051", int64(1), dispatchStamp}, {"due-052", int64(1), dispatchStamp}, {"zz-foreign", int64(1), dispatchStamp}}) {
		t.Fatalf("keyset/held exclusion: %v", page)
	}
	// This tests SQL shape/index scope only, not an N2c API or production pass fairness.
	for _, statement := range []coordinatorSQL{
		{`SELECT s.request_id,s.generation,s.next_attempt_at FROM coordination_dispatch_state s
		 WHERE s.group_id=? AND s.task_id=? AND s.status IN ('pending','enqueued','held')`, []any{g, candidates[0].request[3]}},
		{`SELECT s.request_id,s.generation,s.next_attempt_at FROM coordination_dispatch_requests r
		 JOIN coordination_dispatch_state s ON s.request_id=r.id AND s.task_id=r.task_id AND s.group_id=r.group_id
		 WHERE r.group_id=? AND r.id=?`, []any{g, candidates[0].request[0]}},
	} {
		if got := read(statement.query, statement.args...); len(got) != 1 || got[0][0] != candidates[0].request[0] {
			t.Fatalf("exact scope read: %v", got)
		}
		wrong := slices.Clone(statement.args)
		wrong[0] = other
		if got := read(statement.query, wrong...); len(got) != 0 {
			t.Fatalf("cross-group exact read leaked: %v", got)
		}
		plan, err := s.db.Query("EXPLAIN QUERY PLAN "+statement.query, statement.args...)
		if err != nil {
			t.Fatal(err)
		}
		for plan.Next() {
			var id, parent, unused int
			var detail string
			if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			t.Log(detail)
			if strings.Contains(detail, "SCAN ") {
				t.Fatalf("exact task/group query scanned history: %s", detail)
			}
		}
		if err := plan.Err(); err != nil {
			t.Fatal(err)
		}
		plan.Close()
	}
	dispatchAssertFK(t, s)
}
