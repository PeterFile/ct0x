package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/provider"
)

func TestMemberToolOperationCreateConcurrentAcrossStores(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, mode := range []string{"matching", "capacity", "same-key", "conflicting-key"} {
			t.Run(fmt.Sprintf("deferred=%t/%s", deferred, mode), func(t *testing.T) {
				f, scope := memberOperationFixture(t)
				handles := []*Store{queueHandle(t, f.path, deferred), queueHandle(t, f.path, deferred)}
				type outcome struct {
					result MemberToolCreateResult
					err    error
				}
				start, results := make(chan struct{}), make(chan outcome, 8)
				for i := range 8 {
					go func() {
						<-start
						input, key := memberOperationInput(), fmt.Sprint(i)
						switch mode {
						case "matching":
							input.Name += key
						case "capacity":
							input.Role += key
						case "same-key":
							key = "shared"
						case "conflicting-key":
							key = "shared"
							input.Name += fmt.Sprint(i % 2)
						}
						result, err := handles[i%2].CreateMemberForTool(ctx, scope, input, memberOperationPolicy(), key)
						results <- outcome{result, err}
					}()
				}
				close(start)
				successes, reused, denied := 0, 0, 0
				var winner MemberToolCreateResult
				for range 8 {
					r := <-results
					if r.err != nil {
						want := ErrMemberCapacity
						if mode == "conflicting-key" {
							want = ErrConflict
						}
						if !errors.Is(r.err, want) || r.result.Member.ID != "" || r.result.JoinMessage != nil {
							t.Errorf("unexpected concurrent failure or partial object: %v", r.err)
						}
						denied++
						continue
					}
					successes++
					if r.result.Reused {
						reused++
					}
					if winner.Member.ID != "" && winner.Member.ID != r.result.Member.ID {
						t.Error("concurrent matching requests created different members")
					}
					if mode == "same-key" || mode == "conflicting-key" {
						if winner.Member.ID != "" && !reflect.DeepEqual(winner, r.result) {
							t.Error("same-key receipt did not converge")
						}
					}
					winner = r.result
				}
				wantSuccess, wantReuse, wantKeys := 8, 0, 1
				switch mode {
				case "matching":
					wantReuse, wantKeys = 7, 8
				case "capacity":
					wantSuccess = 1
				case "conflicting-key":
					wantSuccess = 4
				}
				if successes != wantSuccess || reused != wantReuse || denied != 8-wantSuccess {
					t.Fatalf("unexpected success/reuse/denial counts: %d/%d/%d", successes, reused, denied)
				}
				if counts := memberOperationCounts(t, f.s); counts != ([6]int{4, 2, 2, 2, wantKeys, 1}) {
					t.Fatalf("concurrent creation duplicated side effects or leaked reservations: %v", counts)
				}
			})
		}
	}
}

func TestMemberToolOperationKeysAreActorGroupAndOperationScoped(t *testing.T) {
	f, scope := memberOperationFixture(t)
	scopes := []MemberToolScope{scope}
	for _, identity := range [][2]string{{f.groupID, f.b}, {f.otherID, f.foreign}} {
		delivery := queueDelivery(t, f.s, identity[0], identity[1], scope.TopicID)
		turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: identity[0], DeliveryID: delivery.ID, MaxActive: 3})
		if err != nil {
			t.Fatal(err)
		}
		bound, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
		if err != nil {
			t.Fatal(err)
		}
		scopes = append(scopes, bound)
	}
	key := strings.Repeat("k", 128)
	for i, bound := range scopes {
		input, policy := memberOperationInput(), memberOperationPolicy()
		input.Role += fmt.Sprint(i)
		policy.MaxMembers = 4
		result, err := queueHandle(t, f.path, true).CreateMemberForTool(ctx, bound, input, policy, key)
		if err != nil || result.Reused || result.Member.CreatorID != bound.MemberID || result.Member.GroupID != bound.Claim.GroupID {
			t.Fatalf("receipt crossed actor/group scope: %+v, %v", result, err)
		}
	}
	message, err := f.s.SendMessageForTool(ctx, scope, MemberToolMessageInput{Content: "same key, different operation"}, key)
	if err != nil || message.SenderID != scope.MemberID || len(message.Deliveries) != 0 {
		t.Fatalf("creation receipt collided with send_message: %+v, %v", message, err)
	}
	if got := memberOperationCounts(t, f.s); got != ([6]int{6, 7, 6, 6, 4, 3}) {
		t.Fatalf("scoped mutations lost or duplicated effects: %v", got)
	}
}

func TestMemberToolOperationCreateFailureRollsBackEverySideEffect(t *testing.T) {
	for _, failure := range []string{"message", "delivery", "receipt", "commit"} {
		t.Run(failure, func(t *testing.T) {
			f, scope := memberOperationFixture(t)
			before := memberOperationCounts(t, f.s)
			trigger := "AFTER INSERT ON messages"
			body := "SELECT RAISE(ABORT,'PRIVATE-SQL-ERROR');"
			switch failure {
			case "delivery":
				trigger = "AFTER INSERT ON deliveries"
			case "receipt":
				trigger = "BEFORE UPDATE ON mutation_idempotency"
			case "commit":
				if _, err := f.s.db.Exec("CREATE TABLE member_operation_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					t.Fatal(err)
				}
				trigger = "AFTER INSERT ON deliveries"
				body = "INSERT INTO member_operation_guard VALUES('PRIVATE-SQL-ERROR');"
			}
			if _, err := f.s.db.Exec("CREATE TRIGGER member_operation_fail " + trigger + " BEGIN " + body + " END"); err != nil {
				t.Fatal(err)
			}
			result, err := f.s.CreateMemberForTool(ctx, scope, memberOperationInput(), memberOperationPolicy(), "retry")
			if err == nil || result.Member.ID != "" || result.JoinMessage != nil || strings.Contains(err.Error(), "PRIVATE") || memberOperationCounts(t, f.s) != before {
				t.Fatalf("failed create leaked SQL or partially committed: %v", err)
			}
			if _, err := f.s.db.Exec("DROP TRIGGER member_operation_fail"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.CreateMemberForTool(ctx, scope, memberOperationInput(), memberOperationPolicy(), "retry"); err != nil {
				t.Fatalf("rollback did not permit a clean retry: %v", err)
			}
		})
	}
}

func TestMemberToolOperationLifecycleOrdering(t *testing.T) {
	for _, transition := range []string{"cancel", "recovery", "terminal"} {
		for _, operation := range []string{"list", "read", "send", "create"} {
			for _, ordering := range []string{"lost-first", "concurrent"} {
				t.Run(transition+"/"+operation+"/"+ordering, func(t *testing.T) {
					f, scope := memberOperationFixture(t)
					a, b := queueHandle(t, f.path, true), queueHandle(t, f.path, true)
					invoke := func() error {
						switch operation {
						case "list":
							_, err := a.ListMembersForTool(ctx, scope, ListOptions{})
							return err
						case "read":
							_, err := a.ReadChannelForTool(ctx, scope, ListOptions{})
							return err
						case "send":
							_, err := a.SendMessageForTool(ctx, scope, MemberToolMessageInput{Content: "fenced"}, "fenced")
							return err
						default:
							_, err := a.CreateMemberForTool(ctx, scope, memberOperationInput(), memberOperationPolicy(), "fenced")
							return err
						}
					}
					invalidate := func() error {
						switch transition {
						case "cancel":
							_, err := b.RequestTurnCancel(ctx, scope.Claim.GroupID, scope.Claim.TurnID)
							return err
						case "recovery":
							_, err := b.RecoverInFlight(ctx)
							return err
						default:
							_, err := b.ApplyTurnReceipt(ctx, scope.Claim, queueReceipt(provider.OutcomeSucceeded))
							return err
						}
					}
					var err error
					if ordering == "lost-first" {
						if err := invalidate(); err != nil {
							t.Fatal(err)
						}
						err = invoke()
						requireMemberToolDenied(t, err)
					} else {
						start, used, invalidated := make(chan struct{}), make(chan error, 1), make(chan error, 1)
						go func() { <-start; used <- invoke() }()
						go func() { <-start; invalidated <- invalidate() }()
						close(start)
						err = <-used
						if err != nil && !errors.Is(err, ErrMemberToolDenied) {
							t.Fatalf("operation did not serialize: %v", err)
						}
						if err := <-invalidated; err != nil {
							t.Fatal(err)
						}
					}
					want := [6]int{3, 1, 1, 1, 0, 1}
					if err == nil && operation == "send" {
						want[1], want[4] = 2, 1
					}
					if err == nil && operation == "create" {
						want = [6]int{4, 2, 2, 2, 1, 1}
					}
					if got := memberOperationCounts(t, f.s); got != want {
						t.Fatalf("operation side effects did not match before-or-denied outcome: got %v, want %v", got, want)
					}
					requireMemberToolDenied(t, invoke()) // Even receipt replays must check the current claim.
				})
			}
		}
	}
}
