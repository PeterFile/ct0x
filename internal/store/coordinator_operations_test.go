package store

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func coordinatorOperationRequireZero(t *testing.T, value any, err, want error) {
	t.Helper()
	if err == nil || (want != nil && !errors.Is(err, want)) || !reflect.ValueOf(value).IsZero() {
		t.Fatalf("expected zero result and %v: %+v, %v", want, value, err)
	}
}

func coordinatorOperationWrite(s *Store, groupID, memberID string, expected int64, key string, policy bool) (any, error) {
	if policy {
		return s.SetDispatchPolicy(ctx, groupID, []string{memberID}, expected, key)
	}
	return s.SetCoordinator(ctx, groupID, memberID, expected, key)
}

func coordinatorOperationRead(s *Store, groupID string, policy bool) (any, error) {
	if policy {
		return s.GetDispatchPolicy(ctx, groupID)
	}
	return s.GetCoordinator(ctx, groupID)
}

func coordinatorOperationOtherRows(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	rows := coordinatorOtherRows(t, s)
	rows["mutation_idempotency"] = messageSequenceRows(t, s.db, `SELECT * FROM mutation_idempotency
	 WHERE operation NOT IN ('set_coordinator','set_dispatch_policy') ORDER BY operation,actor_id,group_id,idempotency_key`)
	return rows
}

func coordinatorOperationCounts(t *testing.T, s *Store, bindings, policies, keys int) {
	t.Helper()
	for table, want := range map[string]int{
		"group_coordinator_binding_revisions":                                               bindings,
		"group_dispatch_policy_revisions":                                                   policies,
		"mutation_idempotency WHERE operation IN ('set_coordinator','set_dispatch_policy')": keys,
	} {
		var got int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: count=%d, %v; want %d", table, got, err, want)
		}
	}
}

func TestCoordinatorOperationsCurrentDefaultsAndReplay(t *testing.T) {
	f := setupMessages(t)
	before := messageSequenceSnapshot(t, f.s)
	if got, err := f.s.GetCoordinator(ctx, f.groupID); !errors.Is(err, ErrNotFound) || got != (domain.GroupCoordinatorBinding{}) {
		t.Fatalf("absent binding: %+v, %v", got, err)
	}
	wantDefault := domain.GroupDispatchPolicy{GroupID: f.groupID, AllowedWorkerIDs: []string{}}
	if got, err := f.s.GetDispatchPolicy(ctx, f.groupID); err != nil || !reflect.DeepEqual(got, wantDefault) {
		t.Fatalf("default policy: %+v, %v", got, err)
	}
	messageSequencePreserved(t, f.s, before)
	binding, err := f.s.SetCoordinator(ctx, f.groupID, f.a, 0, "binding")
	if err != nil || binding != (domain.GroupCoordinatorBinding{GroupID: f.groupID, MemberID: f.a, Revision: 1}) {
		t.Fatalf("save binding: %+v, %v", binding, err)
	}
	if got, err := f.s.GetCoordinator(ctx, f.groupID); err != nil || got != binding {
		t.Fatalf("current binding: %+v, %v", got, err)
	}
	if got, err := f.s.SetCoordinator(ctx, f.groupID, f.a, 0, "binding"); err != nil || got != binding {
		t.Fatalf("binding replay: %+v, %v", got, err)
	}
	policy, err := f.s.SetDispatchPolicy(ctx, f.groupID, nil, 0, "policy")
	wantDefault.Revision = 1
	if err != nil || !reflect.DeepEqual(policy, wantDefault) {
		t.Fatalf("save empty policy: %+v, %v", policy, err)
	}
	if got, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{}, 0, "policy"); err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("empty policy replay: %+v, %v", got, err)
	}
	if got, err := f.s.GetDispatchPolicy(ctx, f.groupID); err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("current empty policy: %+v, %v", got, err)
	}
	coordinatorOperationCounts(t, f.s, 1, 1, 2)
}

func TestCoordinatorOperationsCanonicalSetAndSliceIsolation(t *testing.T) {
	f := setupMessages(t)
	c := member(t, f.s, f.groupID).ID
	input := []string{c, f.b, f.a, c, f.b}
	original := slices.Clone(input)
	policy, err := f.s.SetDispatchPolicy(ctx, f.groupID, input, 0, "set")
	want := recipientSet(input)
	if err != nil || policy.Revision != 1 || !slices.Equal(policy.AllowedWorkerIDs, want) || !slices.Equal(input, original) {
		t.Fatalf("canonical set/caller mutation: %+v, %v; input=%v", policy, err, input)
	}
	input[0], policy.AllowedWorkerIDs[0] = "caller-mutated", "result-mutated"
	if got, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.a, c, f.b, f.a}, 0, "set"); err != nil || !slices.Equal(got.AllowedWorkerIDs, want) {
		t.Fatalf("canonical replay aliases caller memory: %+v, %v", got, err)
	}
	if got, err := f.s.GetDispatchPolicy(ctx, f.groupID); err != nil || !slices.Equal(got.AllowedWorkerIDs, want) {
		t.Fatalf("current policy aliases response memory: %+v, %v", got, err)
	}
	before := messageSequenceSnapshot(t, f.s)
	got, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.a, f.b}, 0, "set")
	coordinatorOperationRequireZero(t, got, err, ErrConflict)
	got, err = f.s.SetDispatchPolicy(ctx, f.groupID, want, 1, "set")
	coordinatorOperationRequireZero(t, got, err, ErrConflict)
	messageSequencePreserved(t, f.s, before)
}

func TestCoordinatorOperationsBoundaryValidation(t *testing.T) {
	f := setupMessages(t)
	before := messageSequenceSnapshot(t, f.s)
	for _, policy := range []bool{false, true} {
		for _, key := range []string{"", " ", "\t\r\n", "\u2003"} {
			got, err := coordinatorOperationWrite(f.s, f.groupID, f.a, 0, key, policy)
			coordinatorOperationRequireZero(t, got, err, ErrInvalid)
		}
		for _, revision := range []int64{-1, math.MinInt64, math.MaxInt64} {
			got, err := coordinatorOperationWrite(f.s, f.groupID, f.a, revision, "invalid-revision", policy)
			coordinatorOperationRequireZero(t, got, err, ErrInvalid)
		}
		for _, id := range []string{"", " ", " leading", "trailing ", "bad\nline", "bad\rline", "bad\x00id", string([]byte{0xff}), strings.Repeat("x", 4097)} {
			got, err := coordinatorOperationWrite(f.s, id, f.a, 0, "invalid-group", policy)
			coordinatorOperationRequireZero(t, got, err, ErrInvalid)
			got, err = coordinatorOperationWrite(f.s, f.groupID, id, 0, "invalid-member", policy)
			coordinatorOperationRequireZero(t, got, err, ErrInvalid)
		}
		got, err := coordinatorOperationWrite(f.s, f.groupID, f.a, math.MaxInt64-1, "valid-large-revision", policy)
		coordinatorOperationRequireZero(t, got, err, ErrConflict)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestCoordinatorOperationsCASAndReusableFailedKeys(t *testing.T) {
	for _, policy := range []bool{false, true} {
		t.Run(strconv.FormatBool(policy), func(t *testing.T) {
			f := setupMessages(t)
			got, err := coordinatorOperationWrite(f.s, f.groupID, f.a, 1, "create", policy)
			coordinatorOperationRequireZero(t, got, err, ErrConflict)
			first, err := coordinatorOperationWrite(f.s, f.groupID, f.a, 0, "create", policy)
			if err != nil {
				t.Fatal(err)
			}
			for _, attempt := range []struct {
				stale, expected int64
				key             string
			}{{0, 1, "update"}, {1, 2, "retry"}} {
				before := messageSequenceSnapshot(t, f.s)
				got, err := coordinatorOperationWrite(f.s, f.groupID, f.b, attempt.stale, attempt.key, policy)
				coordinatorOperationRequireZero(t, got, err, ErrConflict)
				messageSequencePreserved(t, f.s, before)
				got, err = coordinatorOperationWrite(f.s, f.groupID, f.b, attempt.expected, attempt.key, policy)
				want := any(domain.GroupCoordinatorBinding{GroupID: f.groupID, MemberID: f.b, Revision: attempt.expected + 1})
				if policy {
					want = domain.GroupDispatchPolicy{GroupID: f.groupID, Revision: attempt.expected + 1, AllowedWorkerIDs: []string{f.b}}
				}
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("failed CAS burned key: %+v, %v; want %+v", got, err, want)
				}
			}
			if got, err := coordinatorOperationWrite(f.s, f.groupID, f.a, 0, "create", policy); err != nil || !reflect.DeepEqual(got, first) {
				t.Fatalf("historical replay was subjected to current CAS: %+v, %v", got, err)
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err = coordinatorOperationWrite(f.s, f.groupID, f.b, 0, "create", policy)
			coordinatorOperationRequireZero(t, got, err, ErrConflict)
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestCoordinatorOperationsActiveSameGroupOnly(t *testing.T) {
	for _, policy := range []bool{false, true} {
		t.Run(strconv.FormatBool(policy), func(t *testing.T) {
			f := setupMessages(t)
			left := member(t, f.s, f.groupID).ID
			if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", left); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			var safeError string
			for _, id := range []string{"missing", f.foreign, left} {
				got, err := coordinatorOperationWrite(f.s, f.groupID, id, 0, "eligible", policy)
				coordinatorOperationRequireZero(t, got, err, ErrNotFound)
				if safeError != "" && err.Error() != safeError {
					t.Fatalf("membership error disclosed missing/foreign/left distinction: %v vs %s", err, safeError)
				}
				safeError = err.Error()
				messageSequencePreserved(t, f.s, before)
			}
			if policy {
				got, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.a, f.foreign}, 0, "eligible")
				coordinatorOperationRequireZero(t, got, err, ErrNotFound)
				messageSequencePreserved(t, f.s, before)
			}
			if _, err := coordinatorOperationWrite(f.s, f.groupID, f.a, 0, "eligible", policy); err != nil {
				t.Fatalf("membership rejection burned key: %v", err)
			}
		})
	}
}

func TestCoordinatorOperationsBothDirectionExclusion(t *testing.T) {
	f := setupMessages(t)
	if _, err := f.s.SetCoordinator(ctx, f.groupID, f.a, 0, "binding"); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	policy, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.a, f.b}, 0, "policy")
	coordinatorOperationRequireZero(t, policy, err, ErrConflict)
	messageSequencePreserved(t, f.s, before)
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	before = messageSequenceSnapshot(t, f.s)
	binding, err := f.s.SetCoordinator(ctx, f.groupID, f.b, 1, "replace")
	coordinatorOperationRequireZero(t, binding, err, ErrConflict)
	messageSequencePreserved(t, f.s, before)
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, nil, 1, "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetCoordinator(ctx, f.groupID, f.b, 1, "replace"); err != nil {
		t.Fatalf("clearing policy did not unblock binding or key was burned: %v", err)
	}
	before = messageSequenceSnapshot(t, f.s)
	policy, err = f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 2, "reverse")
	coordinatorOperationRequireZero(t, policy, err, ErrConflict)
	messageSequencePreserved(t, f.s, before)
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.a}, 2, "reverse"); err != nil {
		t.Fatal(err)
	}
	coordinatorOperationCounts(t, f.s, 2, 3, 5)
}

func TestCoordinatorOperationsExactActorGroupOperationScopeFirst(t *testing.T) {
	f := setupMessages(t)
	for _, policy := range []bool{false, true} {
		operation := "set_coordinator"
		if policy {
			operation = "set_dispatch_policy"
		}
		for _, payload := range [][]byte{mutationPayload("missing-group", "0", f.a), []byte("different-private-payload")} {
			if _, err := f.s.db.Exec(`INSERT OR REPLACE INTO mutation_idempotency VALUES(?,?,?,?,?,?)`, operation, "user", "missing-group", "forged", payload, "999"); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err := coordinatorOperationWrite(f.s, "missing-group", f.a, 0, "forged", policy)
			coordinatorOperationRequireZero(t, got, err, ErrNotFound)
			messageSequencePreserved(t, f.s, before)
		}
		if _, err := f.s.db.Exec(`INSERT INTO mutation_idempotency VALUES(?,?,?,?,?,?)`, operation, f.a, f.groupID, "actor-only", []byte("foreign-actor"), "999"); err != nil {
			t.Fatal(err)
		}
		id := f.a
		if policy {
			id = f.b
		}
		if _, err := coordinatorOperationWrite(f.s, f.groupID, id, 0, "actor-only", policy); err != nil {
			t.Fatalf("caller was not bound to user or operation scope collided: %v", err)
		}
	}
	if _, err := f.s.SetCoordinator(ctx, f.otherID, f.foreign, 0, "actor-only"); err != nil {
		t.Fatalf("key leaked across groups: %v", err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.otherID, nil, 0, "actor-only"); err != nil {
		t.Fatalf("key leaked across operations: %v", err)
	}
	rows := messageSequenceRows(t, f.s.db, `SELECT operation,actor_id,group_id,record_id FROM mutation_idempotency
	 WHERE actor_id='user' AND group_id<>'missing-group' ORDER BY operation,group_id`)
	if len(rows) != 4 {
		t.Fatalf("expected exactly four user/group/operation receipts: %v", rows)
	}
	for _, row := range rows {
		if row[3] != "1" {
			t.Fatalf("receipt is not a historical revision: %v", row)
		}
	}
}

func TestCoordinatorOperationsOpaqueIDsAndExactKeyBytes(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	groupID, memberID := "group:opaque|群\tinside", "member:opaque|员\tinside"
	if _, err := s.db.Exec("UPDATE groups SET id=? WHERE id=?", groupID, g.ID); err != nil {
		t.Fatal(err)
	}
	m := member(t, s, groupID)
	if _, err := s.db.Exec("UPDATE members SET id=? WHERE id=?", memberID, m.ID); err != nil {
		t.Fatal(err)
	}
	key := " \topaque\x00\r\n" + strings.Repeat("k", 5000) + " "
	binding, err := s.SetCoordinator(ctx, groupID, memberID, 0, key)
	if err != nil || binding.GroupID != groupID || binding.MemberID != memberID {
		t.Fatalf("opaque identifiers were rewritten/restricted: %+v, %v", binding, err)
	}
	if got, err := s.SetCoordinator(ctx, groupID, memberID, 0, key); err != nil || got != binding {
		t.Fatalf("opaque key replay: %+v, %v", got, err)
	}
	if _, err := s.SetDispatchPolicy(ctx, groupID, nil, 0, key); err != nil {
		t.Fatalf("policy imposed transport-specific key restrictions: %v", err)
	}
	rows := messageSequenceRows(t, s.db, "SELECT idempotency_key,payload FROM mutation_idempotency ORDER BY operation")
	if len(rows) != 2 || rows[0][0] != key || rows[1][0] != key {
		t.Fatalf("key bytes were trimmed: %v", rows)
	}
	before := messageSequenceSnapshot(t, s)
	got, err := s.SetCoordinator(ctx, groupID, memberID, 0, strings.TrimSpace(key))
	coordinatorOperationRequireZero(t, got, err, ErrConflict)
	messageSequencePreserved(t, s, before)
}

func TestCoordinatorOperationsHistoricalReceiptCannotCrossGroupOrType(t *testing.T) {
	f := setupMessages(t)
	if _, err := f.s.SetCoordinator(ctx, f.otherID, f.foreign, 0, "other-binding"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 0, "own-policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.otherID, nil, 0, "other-policy"); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []bool{false, true} {
		operation, groupID, id := "set_coordinator", f.groupID, f.a
		if policy {
			operation, groupID = "set_dispatch_policy", group(t, f.s).ID
		}
		payload := mutationPayload(groupID, "0", id)
		if _, err := f.s.db.Exec("INSERT INTO mutation_idempotency VALUES(?,?,?,?,?,?)", operation, "user", groupID, "forged-history", payload, "1"); err != nil {
			t.Fatal(err)
		}
		before := messageSequenceSnapshot(t, f.s)
		got, err := coordinatorOperationWrite(f.s, groupID, id, 0, "forged-history", policy)
		coordinatorOperationRequireZero(t, got, err, ErrNotFound)
		messageSequencePreserved(t, f.s, before)
	}
}
