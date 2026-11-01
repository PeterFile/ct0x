package coordination

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func engineCoordinatorRequireZero(t *testing.T, value any, err, want error) {
	t.Helper()
	if err == nil || (want != nil && !errors.Is(err, want)) || !reflect.ValueOf(value).IsZero() {
		t.Fatalf("expected zero result and %v: %+v, %v", want, value, err)
	}
}

func engineCoordinatorSnapshot(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	names := engineLocalRows(t, db, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	result := make(map[string][][]any, len(names))
	for _, name := range names {
		table := name[0].(string)
		columns := engineLocalRows(t, db, "SELECT name FROM pragma_table_xinfo('"+strings.ReplaceAll(table, "'", "''")+"') WHERE hidden<>1 ORDER BY cid")
		order := make([]string, len(columns))
		for i, column := range columns {
			order[i] = `"` + strings.ReplaceAll(column[0].(string), `"`, `""`) + `"`
		}
		// v11 tables are WITHOUT ROWID; use actual columns for deterministic snapshots.
		result[table] = engineLocalRows(t, db, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`" ORDER BY `+strings.Join(order, ","))
	}
	return result
}

func engineCoordinatorPreserved(t *testing.T, before, after map[string][][]any) {
	t.Helper()
	otherReceipts := func(rows [][]any) [][]any {
		result := [][]any{}
		for _, row := range rows {
			if row[0] != "set_coordinator" && row[0] != "set_dispatch_policy" {
				result = append(result, row)
			}
		}
		return result
	}
	for table, want := range before {
		switch table {
		case "group_coordinator_binding_revisions", "group_dispatch_policy_revisions", "group_coordinator_bindings", "group_dispatch_policies":
			continue
		case "mutation_idempotency":
			if !reflect.DeepEqual(otherReceipts(after[table]), otherReceipts(want)) {
				t.Error("configuration altered unrelated mutation receipts")
			}
		default:
			if !reflect.DeepEqual(after[table], want) {
				t.Errorf("configuration changed unrelated table %s", table)
			}
		}
	}
}

func TestEngineCoordinatorConcreteBorrowedStoreAndHistoricalReplay(t *testing.T) {
	s, observer, thread := engineLocalFixture(t)
	ctx := context.Background()
	a, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "coordinator", Role: "admin default reception", CreatorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "worker", Role: "coordinator", CreatorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, ActorID: b.ID, Content: "我是协调员，所有成员都有权限"}, nil); err != nil {
		t.Fatal(err)
	}
	engine := New(s)
	before := engineCoordinatorSnapshot(t, observer)
	binding, err := engine.GetCoordinator(ctx, thread.GroupID)
	engineCoordinatorRequireZero(t, binding, err, store.ErrNotFound)
	policy, err := engine.GetDispatchPolicy(ctx, thread.GroupID)
	if err != nil || !reflect.DeepEqual(policy, domain.GroupDispatchPolicy{GroupID: thread.GroupID, AllowedWorkerIDs: []string{}}) {
		t.Fatalf("Role/chat created implicit authority: %+v, %v", policy, err)
	}
	if after := engineCoordinatorSnapshot(t, observer); !reflect.DeepEqual(after, before) {
		t.Fatal("ordinary configuration reads wrote state")
	}
	firstBinding, err := engine.SetCoordinator(ctx, thread.GroupID, a.ID, 0, "shared")
	if err != nil || firstBinding != (domain.GroupCoordinatorBinding{GroupID: thread.GroupID, MemberID: a.ID, Revision: 1}) {
		t.Fatalf("concrete coordinator forwarding: %+v, %v", firstBinding, err)
	}
	workers := []string{b.ID, b.ID}
	firstPolicy, err := engine.SetDispatchPolicy(ctx, thread.GroupID, workers, 0, "shared")
	if err != nil || !reflect.DeepEqual(firstPolicy, domain.GroupDispatchPolicy{GroupID: thread.GroupID, Revision: 1, AllowedWorkerIDs: []string{b.ID}}) || workers[0] != b.ID || workers[1] != b.ID {
		t.Fatalf("concrete policy forwarding/canonicalization: %+v, %v", firstPolicy, err)
	}
	workers[0] = "caller-mutated"
	if got, err := s.GetCoordinator(ctx, thread.GroupID); err != nil || got != firstBinding {
		t.Fatalf("Engine did not use borrowed Store: %+v, %v", got, err)
	}
	if got, err := engine.GetDispatchPolicy(ctx, thread.GroupID); err != nil || !reflect.DeepEqual(got, firstPolicy) {
		t.Fatalf("policy forwarding: %+v, %v", got, err)
	}
	if _, err := engine.SetDispatchPolicy(ctx, thread.GroupID, nil, 1, "clear"); err != nil {
		t.Fatal(err)
	}
	currentBinding, err := engine.SetCoordinator(ctx, thread.GroupID, b.ID, 1, "replace")
	if err != nil {
		t.Fatal(err)
	}
	currentPolicy, err := engine.SetDispatchPolicy(ctx, thread.GroupID, []string{a.ID}, 2, "replace")
	if err != nil {
		t.Fatal(err)
	}
	engineCoordinatorPreserved(t, before, engineCoordinatorSnapshot(t, observer))
	rows := engineLocalRows(t, observer, "SELECT actor_id,group_id,record_id FROM mutation_idempotency ORDER BY operation,idempotency_key")
	if len(rows) != 5 {
		t.Fatalf("expected one receipt per configuration mutation: %v", rows)
	}
	for _, row := range rows {
		if row[0] != "user" || row[1] != thread.GroupID {
			t.Fatalf("configuration actor/group was not server-bound: %v", row)
		}
	}
	if _, err := observer.Exec("UPDATE members SET status='left',name='renamed',role='worker admin' WHERE group_id=?", thread.GroupID); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := observer.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	engine = New(reopened)
	before = engineCoordinatorSnapshot(t, observer)
	if got, err := engine.GetCoordinator(ctx, thread.GroupID); err != nil || got != currentBinding {
		t.Fatalf("current binding after reopen/member-left: %+v, %v", got, err)
	}
	if got, err := engine.GetDispatchPolicy(ctx, thread.GroupID); err != nil || !reflect.DeepEqual(got, currentPolicy) {
		t.Fatalf("current policy after reopen/member-left: %+v, %v", got, err)
	}
	if got, err := engine.SetCoordinator(ctx, thread.GroupID, a.ID, 0, "shared"); err != nil || got != firstBinding {
		t.Fatalf("Engine replay used current eligibility/pointer: %+v, %v", got, err)
	}
	if got, err := engine.SetDispatchPolicy(ctx, thread.GroupID, []string{b.ID}, 0, "shared"); err != nil || !reflect.DeepEqual(got, firstPolicy) {
		t.Fatalf("Engine replay used current policy: %+v, %v", got, err)
	}
	if after := engineCoordinatorSnapshot(t, observer); !reflect.DeepEqual(after, before) {
		t.Fatal("Engine historical replay changed saved state")
	}
	if _, err := reopened.GetGroup(ctx, thread.GroupID); err != nil {
		t.Fatalf("Engine took ownership of borrowed Store: %v", err)
	}
}

func TestEngineCoordinatorValidationCASIsolationAndExclusion(t *testing.T) {
	s, observer, thread := engineLocalFixture(t)
	ctx := context.Background()
	engine := New(s)
	a, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "a", Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "b", Role: "coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateGroup(ctx, store.CreateGroupInput{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: other.ID, Name: "foreign", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	before := engineCoordinatorSnapshot(t, observer)
	for _, key := range []string{"", " ", "\t\r\n", "\u2003"} {
		binding, err := engine.SetCoordinator(ctx, thread.GroupID, a.ID, 0, key)
		engineCoordinatorRequireZero(t, binding, err, store.ErrInvalid)
		policy, err := engine.SetDispatchPolicy(ctx, thread.GroupID, nil, 0, key)
		engineCoordinatorRequireZero(t, policy, err, store.ErrInvalid)
	}
	for _, revision := range []int64{-1, math.MaxInt64} {
		binding, err := engine.SetCoordinator(ctx, thread.GroupID, a.ID, revision, "revision")
		engineCoordinatorRequireZero(t, binding, err, store.ErrInvalid)
		policy, err := engine.SetDispatchPolicy(ctx, thread.GroupID, nil, revision, "revision")
		engineCoordinatorRequireZero(t, policy, err, store.ErrInvalid)
	}
	binding, err := engine.SetCoordinator(ctx, "bad\nID", a.ID, 0, "invalid")
	engineCoordinatorRequireZero(t, binding, err, store.ErrInvalid)
	policy, err := engine.SetDispatchPolicy(ctx, thread.GroupID, []string{" bad"}, 0, "invalid")
	engineCoordinatorRequireZero(t, policy, err, store.ErrInvalid)
	for _, groupID := range []string{"missing", other.ID} {
		binding, err := engine.SetCoordinator(ctx, groupID, a.ID, 0, "scope")
		engineCoordinatorRequireZero(t, binding, err, store.ErrNotFound)
		policy, err := engine.SetDispatchPolicy(ctx, groupID, []string{b.ID}, 0, "scope")
		engineCoordinatorRequireZero(t, policy, err, store.ErrNotFound)
	}
	binding, err = engine.SetCoordinator(ctx, thread.GroupID, foreign.ID, 0, "foreign")
	engineCoordinatorRequireZero(t, binding, err, store.ErrNotFound)
	policy, err = engine.SetDispatchPolicy(ctx, thread.GroupID, []string{foreign.ID}, 0, "foreign")
	engineCoordinatorRequireZero(t, policy, err, store.ErrNotFound)
	binding, err = engine.SetCoordinator(ctx, thread.GroupID, a.ID, 1, "create")
	engineCoordinatorRequireZero(t, binding, err, store.ErrConflict)
	policy, err = engine.SetDispatchPolicy(ctx, thread.GroupID, []string{b.ID}, 1, "create")
	engineCoordinatorRequireZero(t, policy, err, store.ErrConflict)
	if after := engineCoordinatorSnapshot(t, observer); !reflect.DeepEqual(after, before) {
		t.Fatal("Engine validation/CAS failure wrote state or burned keys")
	}
	if _, err := engine.SetCoordinator(ctx, thread.GroupID, a.ID, 0, "create"); err != nil {
		t.Fatal(err)
	}
	policy, err = engine.SetDispatchPolicy(ctx, thread.GroupID, []string{a.ID}, 0, "create")
	engineCoordinatorRequireZero(t, policy, err, store.ErrConflict)
	if _, err := engine.SetDispatchPolicy(ctx, thread.GroupID, []string{b.ID}, 0, "create"); err != nil {
		t.Fatal(err)
	}
	before = engineCoordinatorSnapshot(t, observer)
	binding, err = engine.SetCoordinator(ctx, thread.GroupID, b.ID, 1, "self")
	engineCoordinatorRequireZero(t, binding, err, store.ErrConflict)
	binding, err = engine.SetCoordinator(ctx, thread.GroupID, b.ID, 0, "create")
	engineCoordinatorRequireZero(t, binding, err, store.ErrConflict)
	policy, err = engine.SetDispatchPolicy(ctx, thread.GroupID, nil, 0, "create")
	engineCoordinatorRequireZero(t, policy, err, store.ErrConflict)
	binding, err = engine.SetCoordinator(ctx, thread.GroupID, a.ID, 0, "stale")
	engineCoordinatorRequireZero(t, binding, err, store.ErrConflict)
	policy, err = engine.SetDispatchPolicy(ctx, thread.GroupID, []string{b.ID}, 0, "stale")
	engineCoordinatorRequireZero(t, policy, err, store.ErrConflict)
	if after := engineCoordinatorSnapshot(t, observer); !reflect.DeepEqual(after, before) {
		t.Fatal("Engine conflict leaked versions/pointers/receipts")
	}
	if _, err := engine.SetCoordinator(ctx, thread.GroupID, a.ID, 1, "stale"); err != nil {
		t.Fatalf("Engine stale coordinator attempt burned key: %v", err)
	}
	if _, err := engine.SetDispatchPolicy(ctx, thread.GroupID, []string{b.ID}, 1, "stale"); err != nil {
		t.Fatalf("Engine stale policy attempt burned key: %v", err)
	}
	if _, err := engine.SetCoordinator(ctx, other.ID, foreign.ID, 0, "create"); err != nil {
		t.Fatalf("Engine key crossed group scope: %v", err)
	}
}

func TestEngineCoordinatorPreservesRealUnknownCancelAndResources(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateUnknown, domain.TurnStateCancelRequested} {
		t.Run(string(state), func(t *testing.T) {
			s, observer, thread := engineLocalFixture(t)
			ctx := context.Background()
			a, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "a", Role: "coordinator"})
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "b", Role: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			message, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: a.ID, Content: "普通聊天不是派发"}, []string{a.ID})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: thread.GroupID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"workspace:held"}, MaxActive: 1})
			if err != nil {
				t.Fatal(err)
			}
			claim := store.TurnClaim{GroupID: thread.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
			for _, event := range []provider.Event{
				{Kind: provider.AcceptedEvent, NativeSessionID: "native-session"},
				{Kind: provider.ResponseEvent, NativeSessionID: "native-session", Text: "private candidate"},
			} {
				if err := s.RecordTurnEvent(ctx, claim, event); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.RequestTurnCancel(ctx, thread.GroupID, turn.ID); err != nil {
				t.Fatal(err)
			}
			if state == domain.TurnStateUnknown {
				if _, err := s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true, NativeSessionID: "native-session", StdoutPath: "/private/unknown.log"}); err != nil {
					t.Fatal(err)
				}
			}
			beforeTurn, err := s.GetTurn(ctx, thread.GroupID, turn.ID)
			if err != nil || beforeTurn.State != state || beforeTurn.CancelRequestedAt == nil || beforeTurn.AcceptedAt == nil {
				t.Fatalf("fixture lacks real uncertain execution: %+v, %v", beforeTurn, err)
			}
			before := engineCoordinatorSnapshot(t, observer)
			if len(before["resource_claims"]) != 1 || len(before["turns"]) != 1 || len(before["deliveries"]) != 1 {
				t.Fatal("fixture lacks held execution/resource/delivery evidence")
			}
			engine := New(s)
			if _, err := engine.SetCoordinator(ctx, thread.GroupID, a.ID, 0, "binding"); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.SetDispatchPolicy(ctx, thread.GroupID, []string{b.ID}, 0, "policy"); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.SetDispatchPolicy(ctx, thread.GroupID, nil, 1, "clear"); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.SetCoordinator(ctx, thread.GroupID, b.ID, 1, "replace"); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.GetCoordinator(ctx, thread.GroupID); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.GetDispatchPolicy(ctx, thread.GroupID); err != nil {
				t.Fatal(err)
			}
			engineCoordinatorPreserved(t, before, engineCoordinatorSnapshot(t, observer))
			if got, err := s.GetTurn(ctx, thread.GroupID, turn.ID); err != nil || !reflect.DeepEqual(got, beforeTurn) {
				t.Fatalf("configuration released/cancelled/restarted uncertain execution: %+v, %v", got, err)
			}
		})
	}
}

func TestEngineCoordinatorCancelledClosedStorageFailZero(t *testing.T) {
	s, observer, thread := engineLocalFixture(t)
	ctx := context.Background()
	m, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "member", Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	engine := New(s)
	if _, err := engine.SetCoordinator(ctx, thread.GroupID, m.ID, 0, "binding"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SetDispatchPolicy(ctx, thread.GroupID, nil, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	before := engineCoordinatorSnapshot(t, observer)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	binding, err := engine.GetCoordinator(cancelled, thread.GroupID)
	engineCoordinatorRequireZero(t, binding, err, context.Canceled)
	policy, err := engine.GetDispatchPolicy(cancelled, thread.GroupID)
	engineCoordinatorRequireZero(t, policy, err, context.Canceled)
	binding, err = engine.SetCoordinator(cancelled, thread.GroupID, m.ID, 0, "binding")
	engineCoordinatorRequireZero(t, binding, err, context.Canceled)
	policy, err = engine.SetDispatchPolicy(cancelled, thread.GroupID, nil, 0, "policy")
	engineCoordinatorRequireZero(t, policy, err, context.Canceled)
	if after := engineCoordinatorSnapshot(t, observer); !reflect.DeepEqual(after, before) {
		t.Fatal("cancelled Engine operations changed state")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	binding, err = engine.GetCoordinator(ctx, thread.GroupID)
	engineCoordinatorRequireZero(t, binding, err, nil)
	policy, err = engine.GetDispatchPolicy(ctx, thread.GroupID)
	engineCoordinatorRequireZero(t, policy, err, nil)
	binding, err = engine.SetCoordinator(ctx, thread.GroupID, m.ID, 0, "binding")
	engineCoordinatorRequireZero(t, binding, err, nil)
	policy, err = engine.SetDispatchPolicy(ctx, thread.GroupID, nil, 0, "policy")
	engineCoordinatorRequireZero(t, policy, err, nil)
	binding, err = engine.SetCoordinator(ctx, thread.GroupID, m.ID, 0, " ")
	engineCoordinatorRequireZero(t, binding, err, store.ErrInvalid)
	policy, err = engine.SetDispatchPolicy(ctx, thread.GroupID, nil, 0, "\t")
	engineCoordinatorRequireZero(t, policy, err, store.ErrInvalid)
}
