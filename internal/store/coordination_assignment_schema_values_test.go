package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCoordinationAssignmentSchemaCounterfactualInitialShapeAndValues(t *testing.T) {
	s, _ := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	r := assignmentCandidate(t, s, g, c, w, "shape")
	assignmentCounterfactual(t, s)
	for _, column := range strings.Split(assignmentColumns, ",") {
		for _, value := range []any{nil, "", " padded", []byte("blob")} {
			bad := r
			bad.assignment = dispatchSet(assignmentColumns, r.assignment, column, value)
			dispatchReject(t, s, "", assignmentPair(bad)...)
		}
	}
	for column, values := range map[string][]any{
		"generation":    {0, -1, 2, 1.5, "9223372036854775808"},
		"status":        {"held", "enqueued", "decided", "released", ""},
		"attempt_count": {-1, 1, 1.5}, "last_error_code": {"sqlite_busy", "unknown"},
		"held_reason": {"worker_inactive"}, "next_attempt_at": {"2026-01-02T03:04:06.123456789Z"},
		"updated_at": {"2026-01-02T03:04:06.123456789Z"},
	} {
		for _, value := range values {
			bad := r
			bad.state = dispatchSet(assignmentStateColumns, r.state, column, value)
			dispatchReject(t, s, "", assignmentPair(bad)...)
		}
	}
	receipt := assignmentReceipt(t, s, r)
	for i := 11; i < 17; i++ {
		bad := r
		bad.state = slices.Clone(r.state)
		bad.state[i] = receipt[i]
		dispatchReject(t, s, "初始状态无效", assignmentPair(bad)...)
	}
	coordinatorMustCommit(t, s, assignmentPair(r)...)
}

func TestCoordinationAssignmentSchemaCounterfactualBackoffAndFrozenStates(t *testing.T) {
	s, _ := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	r := assignmentCandidate(t, s, g, c, w, "backoff")
	assignmentCounterfactual(t, s)
	coordinatorMustCommit(t, s, assignmentPair(r)...)
	at, err := time.Parse(timestampLayout, "2024-02-28T23:59:59.999999999Z")
	if err != nil {
		t.Fatal(err)
	}
	for i, seconds := range []int{2, 4, 8, 16, 32, 60, 60} {
		v := slices.Clone(r.state)
		v[4], v[6], v[7], v[8], v[9] = int64(i+2), int64(i+1), at.Add(time.Duration(seconds)*time.Second).Format(timestampLayout), at.Format(timestampLayout), "sqlite_busy"
		if i%2 != 0 {
			v[9] = "sqlite_locked"
		}
		for column, values := range map[string][]any{
			"generation": {int64(i + 1), int64(i + 3), 1.5}, "attempt_count": {int64(i), int64(i + 2), -1},
			"next_attempt_at": {at.Add(time.Duration(seconds)*time.Second + time.Nanosecond).Format(timestampLayout)},
			"last_error_code": {"", "timeout", []byte("sqlite_busy")}, "held_reason": {"worker_inactive"},
			"assignment_id": {"changed"}, "task_id": {"changed"}, "group_id": {"changed"}, "worker_member_id": {c},
		} {
			for _, value := range values {
				statement := assignmentStateUpdate(dispatchSet(assignmentStateColumns, v, column, value))
				statement.args[len(statement.args)-1] = r.assignment[0]
				dispatchReject(t, s, "", statement)
			}
		}
		coordinatorMustCommit(t, s, assignmentStateUpdate(v))
		r.state = v
		at = at.Add(-time.Minute) // Backwards wall time is permitted; arithmetic remains exact.
	}
	for _, code := range []string{"task_stale", "authority_stale", "binding_stale", "policy_stale", "coordinator_inactive", "worker_inactive", "payload_too_large", "payload_not_encodable"} {
		v := slices.Clone(r.state)
		v[4], v[5], v[9], v[10] = v[4].(int64)+1, "held", "", code
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		statement := assignmentStateUpdate(v)
		if _, err := tx.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	// Frozen held/enqueued include every receipt field and no-op writes.
	for _, status := range []string{"held", "enqueued"} {
		t.Run(status, func(t *testing.T) {
			v := assignmentReceipt(t, s, r)
			if status == "held" {
				v = slices.Clone(r.state)
				v[4], v[5], v[9], v[10] = v[4].(int64)+1, "held", "", "worker_inactive"
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			statement := assignmentStateUpdate(v)
			if _, err := tx.Exec(statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
			for _, column := range strings.Split(assignmentStateColumns, ",") {
				if _, err := tx.Exec("UPDATE coordination_assignment_dispatch SET "+column+"="+column+" WHERE assignment_id=?", r.assignment[0]); err == nil || !strings.Contains(err.Error(), "冻结状态") {
					t.Fatalf("%s must freeze %s including no-op updates: %v", status, column, err)
				}
			}
		})
	}
}

func TestCoordinationAssignmentSchemaCounterfactualReceiptCompletenessAndExactParents(t *testing.T) {
	s, _ := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	other, oc, ow := dispatchConfiguredGroup(t, s)
	r := assignmentCandidate(t, s, g, c, w, "receipt")
	assignmentCounterfactual(t, s)
	coordinatorMustCommit(t, s, assignmentPair(r)...)
	valid := assignmentReceipt(t, s, r)
	for mask := 0; mask < 63; mask++ {
		v := slices.Clone(valid)
		for i := range 6 {
			if mask&(1<<i) == 0 {
				v[11+i] = nil
			}
		}
		dispatchReject(t, s, "", assignmentStateUpdate(v))
	}
	for _, parent := range []struct{ group, sender, recipient, topic string }{
		{g, c, w, "other-topic"}, {g, c, c, "receipt"}, {g, "user", w, "receipt"}, {other, oc, ow, "receipt"},
	} {
		m, err := s.CreateMessage(ctx, MessageRequest{GroupID: parent.group, ActorID: parent.sender, TopicID: parent.topic, Content: "different identity"}, []string{parent.recipient})
		if err != nil {
			t.Fatal(err)
		}
		d := m.Deliveries[0]
		v := slices.Clone(valid)
		v[11], v[12], v[13], v[14] = d.MessageID, d.ID, d.ConversationID, d.RecipientID
		dispatchReject(t, s, "回执关联无效", assignmentStateUpdate(v))
	}
	for column, values := range map[string][]any{
		"message_id": {"missing", "", []byte("blob")}, "delivery_id": {"missing"},
		"conversation_id": {"missing"}, "delivered_member_id": {c, ow},
		"materialized_from_generation": {0, int64(2), 1.5, "9223372036854775808"},
		"materialized_at":              {"2026-01-02T03:04:06.123456789Z", []byte(dispatchStamp)},
		"attempt_count":                {int64(1)}, "next_attempt_at": {"2026-01-02T03:04:06.123456789Z"},
	} {
		for _, value := range values {
			dispatchReject(t, s, "", assignmentStateUpdate(dispatchSet(assignmentStateColumns, valid, column, value)))
		}
	}
	for _, status := range []string{"pending", "held"} {
		v := dispatchSet(assignmentStateColumns, valid, "status", status)
		if status == "held" {
			v[10] = "worker_inactive"
		}
		dispatchReject(t, s, "", assignmentStateUpdate(v))
	}
	coordinatorMustCommit(t, s, assignmentStateUpdate(valid))
	dispatchAssertFK(t, s)
}

func TestCoordinationAssignmentSchemaCounterfactualCanonicalTimestamps(t *testing.T) {
	s, _ := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	r := assignmentCandidate(t, s, g, c, w, "timestamps")
	assignmentCounterfactual(t, s)
	for _, stamp := range []string{"0001-01-01T00:00:00.000000000Z", "2000-02-29T23:59:59.999999999Z", "9999-12-31T23:59:59.999999999Z"} {
		v := r
		v.assignment, v.state = slices.Clone(r.assignment), slices.Clone(r.state)
		v.assignment[6], v.state[7], v.state[8] = stamp, stamp, stamp
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range assignmentPair(v) {
			if _, err := tx.Exec(statement.query, statement.args...); err != nil {
				t.Fatalf("canonical %s: %v", stamp, err)
			}
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	invalid := []string{"0000-01-01T00:00:00.000000000Z", "1900-02-29T00:00:00.000000000Z", "2026-04-31T00:00:00.000000000Z", "2026-01-02T24:00:00.000000000Z", "2026-01-02T03:04:60.000000000Z", "2026-01-02T03:04:05.12345678Z", "2026-01-02T03:04:05.123456789+00:00", dispatchStamp + "\x00", "now", ""}
	for _, stamp := range invalid {
		bad := r
		bad.assignment, bad.state = slices.Clone(r.assignment), slices.Clone(r.state)
		bad.assignment[6], bad.state[7], bad.state[8] = stamp, stamp, stamp
		dispatchReject(t, s, "", assignmentPair(bad)...)
	}
	coordinatorMustCommit(t, s, assignmentPair(r)...)
	valid := assignmentReceipt(t, s, r)
	for _, stamp := range invalid {
		v := slices.Clone(valid)
		v[8], v[16] = stamp, stamp
		dispatchReject(t, s, "", assignmentStateUpdate(v))
	}
	coordinatorMustCommit(t, s, assignmentStateUpdate(valid))
	r.state = valid
	binding := assignmentBinding(t, s, r)
	for _, stamp := range invalid {
		dispatchReject(t, s, "", coordinatorSQL{assignmentBindingInsert, dispatchSet(assignmentBindingColumns, binding, "created_at", stamp)})
	}
	coordinatorMustCommit(t, s, coordinatorSQL{assignmentBindingInsert, binding})
}

func TestCoordinationAssignmentSchemaCounterfactualCounterOverflow(t *testing.T) {
	s, _ := assignmentV13Store(t)
	g, c, w := dispatchConfiguredGroup(t, s)
	r := assignmentCandidate(t, s, g, c, w, "overflow")
	assignmentCounterfactual(t, s)
	coordinatorMustCommit(t, s, assignmentPair(r)...)
	var ddl string
	if err := s.db.QueryRow("SELECT sql FROM sqlite_master WHERE name='coordination_assignment_dispatch_update'").Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	seed := func(generation, attempts int64) {
		t.Helper()
		// Unreachable boundary fixture only; restore the exact production guard before every tested write.
		coordinatorMustCommit(t, s,
			coordinatorSQL{"DROP TRIGGER coordination_assignment_dispatch_update", nil},
			coordinatorSQL{"UPDATE coordination_assignment_dispatch SET generation=?,attempt_count=?,last_error_code='sqlite_busy' WHERE assignment_id=?", []any{generation, attempts, r.assignment[0]}},
			coordinatorSQL{ddl, nil},
		)
	}
	for _, counters := range [][2]int64{{9223372036854775807, 9223372036854775806}, {1, 9223372036854775807}} {
		seed(counters[0], counters[1])
		dispatchReject(t, s, "", coordinatorSQL{`UPDATE coordination_assignment_dispatch SET generation=generation+1,
		 attempt_count=attempt_count+1,last_error_code='sqlite_busy',next_attempt_at='2026-01-02T03:05:05.123456789Z' WHERE assignment_id=?`, []any{r.assignment[0]}})
	}
	seed(9223372036854775806, 9223372036854775805)
	for _, status := range []string{"pending", "held", "enqueued"} {
		v := slices.Clone(r.state)
		v[4], v[5], v[6], v[9] = int64(9223372036854775807), status, int64(9223372036854775805), ""
		switch status {
		case "pending":
			v[6], v[7], v[9] = int64(9223372036854775806), "2026-01-02T03:05:05.123456789Z", "sqlite_busy"
		case "held":
			v[10] = "worker_inactive"
		case "enqueued":
			v = assignmentReceipt(t, s, r)
			v[4], v[6], v[15] = int64(9223372036854775807), int64(9223372036854775805), int64(9223372036854775806)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		statement := assignmentStateUpdate(v)
		if _, err := tx.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("last int64 generation %s: %v", status, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	dispatchAssertFK(t, s)
}

func TestCoordinationAssignmentSchemaCounterfactualInjectedPairRollback(t *testing.T) {
	for _, failure := range []string{"abort", "ignore", "commit"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := assignmentV13Store(t)
			g, c, w := dispatchConfiguredGroup(t, s)
			r := assignmentCandidate(t, s, g, c, w, "fault")
			assignmentCounterfactual(t, s)
			body, timing := "SELECT RAISE(ABORT,'故障注入');", "BEFORE"
			if failure == "ignore" {
				body = "SELECT RAISE(IGNORE);"
			}
			if failure == "commit" {
				coordinatorMustCommit(t, s, coordinatorSQL{"CREATE TABLE assignment_commit_fixture(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)", nil})
				body, timing = "INSERT INTO assignment_commit_fixture VALUES('missing');", "AFTER"
			}
			coordinatorMustCommit(t, s, coordinatorSQL{fmt.Sprintf("CREATE TRIGGER assignment_fault %s INSERT ON coordination_assignment_dispatch BEGIN %s END", timing, body), nil})
			dispatchReject(t, s, "", assignmentPair(r)...)
			assignmentEmpty(t, s)
			coordinatorMustCommit(t, s, coordinatorSQL{"DROP TRIGGER assignment_fault", nil})
			coordinatorMustCommit(t, s, assignmentPair(r)...)
			dispatchAssertFK(t, s)
		})
	}
}
