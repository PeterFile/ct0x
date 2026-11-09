package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func queueClaimInsertFixture(t *testing.T, coordinator bool) (*Store, string, string, domain.Delivery) {
	t.Helper()
	if coordinator {
		f, request := materializePending(t)
		receipt, err := f.s.MaterializeDispatchRequest(ctx, request.ID, 1, materializeAt)
		if err != nil {
			t.Fatal(err)
		}
		materializeAssertReceipt(t, f.s, request, receipt, 1, materializeAt)
		message, err := f.s.GetMessage(ctx, f.groupID, receipt.Delivery.MessageID)
		if err != nil {
			t.Fatal(err)
		}
		return f.s, f.path, f.groupID, message.Deliveries[0]
	}
	f := setupMessages(t)
	message, err := f.s.CreateMessage(ctx, MessageRequest{
		GroupID: f.groupID, ActorID: "user", TopicID: "claim-insert", Content: "待执行输入", IdempotencyKey: "claim-insert",
	}, []string{f.a})
	if err != nil {
		t.Fatal(err)
	}
	return f.s, f.path, f.groupID, message.Deliveries[0]
}

func queueClaimInsertSnapshot(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	snapshot := messageSequenceSnapshot(t, s)
	for _, table := range []string{"schema_migrations", "sqlite_sequence"} {
		snapshot[table] = messageSequenceTableRows(t, s, table)
	}
	return snapshot
}

func queueClaimInsertPreserved(t *testing.T, s *Store, before map[string][][]any) {
	t.Helper()
	after := queueClaimInsertSnapshot(t, s)
	if len(after) != len(before) {
		t.Errorf("committed table set changed: got %d, want %d", len(after), len(before))
	}
	for table, rows := range before {
		if !reflect.DeepEqual(after[table], rows) {
			t.Errorf("failed claim changed committed %s: got %#v, want %#v", table, after[table], rows)
		}
	}
}

func TestQueueClaimInsertIgnoredRollsBackAndRetries(t *testing.T) {
	for _, coordinator := range []bool{false, true} {
		for _, keys := range [][]string{nil, {"claim-insert-target"}} {
			t.Run(fmt.Sprintf("coordinator=%t/keys=%d", coordinator, len(keys)), func(t *testing.T) {
				s, path, groupID, delivery := queueClaimInsertFixture(t, coordinator)
				var held []domain.Turn
				for _, state := range []domain.TurnState{domain.TurnStateUnknown, domain.TurnStateCancelRequested} {
					d := queueDelivery(t, s, groupID, member(t, s, groupID).ID, string(state))
					turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: groupID, DeliveryID: d.ID, ResourceKeys: []string{string(state)}, MaxActive: 3})
					if err != nil {
						t.Fatal(err)
					}
					claim := TurnClaim{GroupID: groupID, TurnID: turn.ID, Token: turn.ClaimToken}
					queueEvent(t, s, claim, provider.AcceptedEvent, "", "")
					if state == domain.TurnStateUnknown {
						turn, err = s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeUnknown))
					} else {
						turn, err = s.RequestTurnCancel(ctx, groupID, turn.ID)
					}
					if err != nil || turn.State != state {
						t.Fatalf("invalid held control: %+v, %v", turn, err)
					}
					held = append(held, turn)
				}
				observer := queueHandle(t, path, false)
				if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER queue_claim_insert_ignore BEFORE INSERT ON turns
				 WHEN NEW.delivery_id='%s' BEGIN
				  UPDATE groups SET description='PRIVATE-CLAIM-INSERT' WHERE id=NEW.group_id;
				  SELECT RAISE(IGNORE);
				 END`, delivery.ID)); err != nil {
					t.Fatal(err)
				}
				before := queueClaimInsertSnapshot(t, observer)
				input := ClaimTurnInput{GroupID: groupID, DeliveryID: delivery.ID, ResourceKeys: keys, MaxActive: 3}
				got, err := s.ClaimTurn(ctx, input)
				if err == nil || !reflect.DeepEqual(got, domain.Turn{}) {
					t.Errorf("ignored INSERT must return zero Turn and error: %+v, %v", got, err)
				}
				if err != nil && (!strings.HasPrefix(err.Error(), "认领轮次失败") || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "INSERT") || strings.Contains(err.Error(), delivery.ID)) {
					t.Errorf("claim error is not safe Chinese: %v", err)
				}
				t.Logf("ignored INSERT: zero Turn=%t, error=%v, conflict=%t; checking %d committed tables", reflect.DeepEqual(got, domain.Turn{}), err, errors.Is(err, ErrConflict), len(before))
				queueClaimInsertPreserved(t, observer, before)
				queueClaimInsertPreserved(t, s, before)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if err := observer.Close(); err != nil {
					t.Fatal(err)
				}
				s = queueHandle(t, path, false)
				queueClaimInsertPreserved(t, s, before)
				if t.Failed() {
					return
				}
				if _, err := s.db.Exec("DROP TRIGGER queue_claim_insert_ignore"); err != nil {
					t.Fatal(err)
				}
				turn, err := s.ClaimTurn(ctx, input)
				if err != nil || turn.ID == "" || turn.ClaimToken == "" || turn.GroupID != groupID || turn.DeliveryID != delivery.ID || turn.ConversationID != delivery.ConversationID || turn.MemberID != delivery.RecipientID || turn.State != domain.TurnStateStarting || turn.AcceptedAt != nil || turn.CancelRequestedAt != nil {
					t.Fatalf("legal retry did not claim exactly: %+v, %v", turn, err)
				}
				if duplicate, err := s.ClaimTurn(ctx, input); !errors.Is(err, ErrTurnBusy) || !reflect.DeepEqual(duplicate, domain.Turn{}) {
					t.Fatalf("delivery claimed twice: %+v, %v", duplicate, err)
				}
				committed := queueClaimInsertSnapshot(t, s)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = queueHandle(t, path, false)
				queueClaimInsertPreserved(t, s, committed)
				persisted, err := s.GetTurn(ctx, groupID, turn.ID)
				if err != nil || !reflect.DeepEqual(persisted, turn) {
					t.Fatalf("retry Turn is not exact and durable: %+v, %v", persisted, err)
				}
				queueDeliveryState(t, s, turn, domain.DeliveryStateQueued)
				queueCounts(t, s, 3, 3, 2+len(keys))
				for _, previous := range held {
					persisted, err := s.GetTurn(ctx, groupID, previous.ID)
					if err != nil || !reflect.DeepEqual(persisted, previous) {
						t.Fatalf("claim changed held execution: %+v, %v", persisted, err)
					}
				}
				wantLocks := append([][]any{}, before["resource_claims"]...)
				for _, key := range keys {
					wantLocks = append(wantLocks, []any{key, turn.ID})
				}
				slices.SortFunc(wantLocks, func(a, b []any) int { return strings.Compare(a[0].(string), b[0].(string)) })
				if gotLocks := messageSequenceTableRows(t, s, "resource_claims"); !reflect.DeepEqual(gotLocks, wantLocks) {
					t.Fatalf("retry lost or invented resource locks: got %#v, want %#v", gotLocks, wantLocks)
				}
				t.Log("rollback and successful retry snapshots survived close/reopen; exact Turn and held locks verified")
			})
		}
	}
}
