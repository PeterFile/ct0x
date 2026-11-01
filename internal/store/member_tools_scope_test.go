package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"agentworkspace/internal/provider"
)

func requireMemberToolDenied(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrMemberToolDenied) || err.Error() != ErrMemberToolDenied.Error() {
		t.Fatalf("expected sanitized authorization denial, got %v", err)
	}
}

func TestMemberToolScopeBindsTrustedIdentity(t *testing.T) {
	for _, topic := range []string{"", "bound-topic"} {
		t.Run(fmt.Sprintf("topic=%q", topic), func(t *testing.T) {
			f := setupMessages(t)
			d := queueDelivery(t, f.s, f.groupID, f.a, topic)
			unclaimed := TurnClaim{GroupID: f.groupID, TurnID: d.ID, Token: "not-a-claim"}
			if scope, err := f.s.BindMemberTool(ctx, unclaimed); scope != (MemberToolScope{}) {
				t.Fatal("unclaimed delivery returned an identity")
			} else {
				requireMemberToolDenied(t, err)
			}
			turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: 1})
			if err != nil {
				t.Fatal(err)
			}
			claim := TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
			scope, err := f.s.BindMemberTool(ctx, claim)
			if err != nil || scope.Claim != claim || scope.MemberID != f.a || scope.ConversationID != d.ConversationID || scope.TopicID != topic {
				t.Fatalf("binding was not derived from the claimed conversation: %+v, %v", scope, err)
			}
			encoded, err := json.Marshal(scope)
			if err != nil || string(encoded) != "{}" {
				t.Fatalf("scope exposed trusted identity: %s, %v", encoded, err)
			}
			var decoded MemberToolScope
			if err := json.Unmarshal([]byte(`{"Claim":{"Token":"forged"},"MemberID":"forged","ConversationID":"forged","TopicID":"forged"}`), &decoded); err != nil || decoded != (MemberToolScope{}) {
				t.Fatalf("JSON selected trusted identity: %+v, %v", decoded, err)
			}
			if err := f.s.ValidateMemberTool(ctx, scope); err != nil {
				t.Fatal(err)
			}
			queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			if err := f.s.ValidateMemberTool(ctx, scope); err != nil {
				t.Fatalf("running claim was denied: %v", err)
			}
		})
	}
}

func TestMemberToolScopeRejectsForgedBindings(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	scope, err := f.s.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	otherTopic, err := f.s.GetOrCreateConversation(ctx, f.groupID, f.a, "other-topic")
	if err != nil {
		t.Fatal(err)
	}
	otherMember, err := f.s.GetOrCreateConversation(ctx, f.groupID, f.b, scope.TopicID)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.s.GetOrCreateConversation(ctx, f.otherID, f.foreign, scope.TopicID)
	if err != nil {
		t.Fatal(err)
	}
	for name, forge := range map[string]func(*MemberToolScope){
		"empty-group":        func(s *MemberToolScope) { s.Claim.GroupID = "" },
		"empty-turn":         func(s *MemberToolScope) { s.Claim.TurnID = "" },
		"empty-token":        func(s *MemberToolScope) { s.Claim.Token = "" },
		"wrong-token":        func(s *MemberToolScope) { s.Claim.Token = "PRIVATE-TOKEN" },
		"missing-turn":       func(s *MemberToolScope) { s.Claim.TurnID = "missing" },
		"cross-group":        func(s *MemberToolScope) { s.Claim.GroupID = f.otherID },
		"padded-group":       func(s *MemberToolScope) { s.Claim.GroupID += " " },
		"empty-member":       func(s *MemberToolScope) { s.MemberID = "" },
		"different-member":   func(s *MemberToolScope) { s.MemberID = f.b },
		"foreign-member":     func(s *MemberToolScope) { s.MemberID = f.foreign },
		"empty-conversation": func(s *MemberToolScope) { s.ConversationID = "" },
		"different-topic":    func(s *MemberToolScope) { s.TopicID = "other-topic" },
		"empty-topic":        func(s *MemberToolScope) { s.TopicID = "" },
		"other-topic-conversation": func(s *MemberToolScope) {
			s.ConversationID, s.TopicID = otherTopic.ID, otherTopic.TopicID
		},
		"other-member-conversation": func(s *MemberToolScope) {
			s.MemberID, s.ConversationID = f.b, otherMember.ID
		},
		"foreign-conversation": func(s *MemberToolScope) {
			s.MemberID, s.ConversationID = f.foreign, foreign.ID
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := scope
			forge(&bad)
			requireMemberToolDenied(t, f.s.ValidateMemberTool(ctx, bad))
			if bad.Claim != claim {
				got, err := f.s.BindMemberTool(ctx, bad.Claim)
				requireMemberToolDenied(t, err)
				if got != (MemberToolScope{}) {
					t.Fatal("failed binding returned trusted identity")
				}
			}
		})
	}
	requireMemberToolDenied(t, f.s.ValidateMemberTool(ctx, MemberToolScope{}))
}

func TestMemberToolScopeDurableLifecycleInvalidation(t *testing.T) {
	for _, state := range []string{"cancel-starting", "cancel-running", "candidate-terminal", "unknown", "recovered", "left", "not_started", "succeeded", "failed", "aborted", "truncated"} {
		t.Run(state, func(t *testing.T) {
			f, _, claim := queueClaimFixture(t)
			scope, err := f.s.BindMemberTool(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "cancel-starting", "cancel-running":
				if state == "cancel-running" {
					queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
				}
				_, err = f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
			case "candidate-terminal":
				queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
				queueEvent(t, f.s, claim, provider.TerminalEvent, "", provider.OutcomeSucceeded)
			case "recovered":
				_, err = f.s.RecoverInFlight(ctx)
			case "left":
				_, err = f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", scope.MemberID)
			default:
				receipt := queueReceipt(provider.Outcome(state))
				if state == "not_started" || state == "unknown" {
					receipt = provider.ExecutionReceipt{Outcome: provider.Outcome(state)}
				}
				_, err = f.s.ApplyTurnReceipt(ctx, claim, receipt)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range []*Store{f.s, queueHandle(t, f.path, false)} {
				requireMemberToolDenied(t, s.ValidateMemberTool(ctx, scope))
				got, err := s.BindMemberTool(ctx, claim)
				requireMemberToolDenied(t, err)
				if got != (MemberToolScope{}) {
					t.Fatal("invalidated claim returned a scope")
				}
				current := queueTurn(t, s, claim)
				freshClaim := TurnClaim{GroupID: current.GroupID, TurnID: current.ID, Token: current.ClaimToken}
				_, err = s.BindMemberTool(ctx, freshClaim)
				requireMemberToolDenied(t, err)
			}
		})
	}
}

func TestMemberToolScopePreflightDoesNotAuthorizeLaterTransaction(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	scope, err := f.s.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.ValidateMemberTool(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, f.path, true)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	requireMemberToolDenied(t, memberToolScopeInTx(ctx, tx, scope))
}

func TestMemberToolScopeFencesOperationInSameTransaction(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, transition := range []string{"cancel", "recovery"} {
			t.Run(fmt.Sprintf("deferred=%t/%s", deferred, transition), func(t *testing.T) {
				f, _, claim := queueClaimFixture(t)
				scope, err := f.s.BindMemberTool(ctx, claim)
				if err != nil {
					t.Fatal(err)
				}
				a, b := queueHandle(t, f.path, deferred), queueHandle(t, f.path, deferred)
				start, used, changed := make(chan struct{}), make(chan error, 1), make(chan error, 1)
				go func() {
					<-start
					used <- func() error {
						tx, err := a.db.BeginTx(ctx, nil)
						if err != nil {
							return err
						}
						defer tx.Rollback()
						if err := memberToolScopeInTx(ctx, tx, scope); err != nil {
							return err
						}
						_, err = createMessageInTx(ctx, tx, MessageRequest{GroupID: scope.Claim.GroupID, ActorID: scope.MemberID, TopicID: scope.TopicID, Content: "fenced operation"}, nil)
						if err != nil {
							return err
						}
						return tx.Commit()
					}()
				}()
				go func() {
					<-start
					var err error
					if transition == "cancel" {
						_, err = b.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID)
					} else {
						_, err = b.RecoverInFlight(ctx)
					}
					changed <- err
				}()
				close(start)
				err = <-used
				if err != nil && !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("operation failed to serialize: %v", err)
				}
				if err := <-changed; err != nil {
					t.Fatal(err)
				}
				var count int
				if queryErr := f.s.db.QueryRow("SELECT count(*) FROM messages WHERE sender_id=? AND group_id=? AND topic_id=?", f.a, f.groupID, scope.TopicID).Scan(&count); queryErr != nil {
					t.Fatal(queryErr)
				}
				if (count == 1) != (err == nil) || count > 1 {
					t.Fatalf("denied operation wrote, or successful operation was lost: count=%d, error=%v", count, err)
				}
				requireMemberToolDenied(t, f.s.ValidateMemberTool(ctx, scope))
			})
		}
	}
}

func TestMemberToolScopeStorageAndContextFailuresFailClosed(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	scope, err := f.s.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	requireMemberToolDenied(t, f.s.ValidateMemberTool(cancelled, scope))
	_, err = f.s.BindMemberTool(cancelled, claim)
	requireMemberToolDenied(t, err)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.BindMemberTool(ctx, claim)
	requireMemberToolDenied(t, err)
	requireMemberToolDenied(t, f.s.ValidateMemberTool(ctx, scope))
}
