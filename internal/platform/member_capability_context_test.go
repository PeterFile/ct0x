package platform

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"agentworkspace/internal/store"
)

func TestMemberCapabilityRequestContextClassification(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	claim := memberCapabilityClaim(t, f, f.groupID)
	tools := NewMemberTools(f.db)
	grants := MemberToolGrants{Operations: []string{"send_message"}}
	revoked, err := tools.Issue(ctx, claim, grants)
	if err != nil {
		t.Fatal(err)
	}
	tools.Revoke(revoked)
	token, err := tools.Issue(ctx, claim, grants)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"canceled", "deadline"} {
		t.Run(state, func(t *testing.T) {
			requestCtx, cancel := context.WithCancel(ctx)
			if state == "deadline" {
				cancel()
				requestCtx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			}
			cancel()
			for _, test := range []struct {
				name, token, operation string
				want                   error
			}{
				{"valid", token, "send_message", requestCtx.Err()},
				{"invalid", "unknown-capability", "send_message", store.ErrMemberToolDenied},
				{"revoked", revoked, "send_message", store.ErrMemberToolDenied},
				{"missing-grant", token, "read_channel", store.ErrMemberToolDenied},
			} {
				t.Run(test.name, func(t *testing.T) {
					called := false
					err := tools.withAuthorized(requestCtx, test.token, test.operation, func(store.MemberToolScope, MemberToolGrants) error {
						called = true
						return nil
					})
					if !errors.Is(err, test.want) {
						t.Errorf("context classification = %v; want %v", err, test.want)
					}
					if test.want == store.ErrMemberToolDenied {
						requireCapabilityDenied(t, err)
					}
					if called {
						t.Error("failed request context or registry authorization reached the callback")
					}
					f.counts(t, 1, 1, 1, 0)
				})
			}
		})
	}
	if err := capabilityUse(ctx, tools, token, "send_message"); err != nil {
		t.Fatalf("context failure invalidated a live registration or leaked its lock: %v", err)
	}
}

func TestMemberCapabilityRequestContextDuringCallback(t *testing.T) {
	for _, state := range []string{"live", "canceled", "deadline"} {
		for _, outcome := range []struct {
			name string
			err  error
		}{
			{"denied", store.ErrMemberToolDenied},
			{"committed", nil},
		} {
			t.Run(state+"/"+outcome.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := setupLocalMessages(t)
					ctx := context.Background()
					claim := memberCapabilityClaim(t, f, f.groupID)
					tools := NewMemberTools(f.db)
					token, err := tools.Issue(ctx, claim, MemberToolGrants{Operations: []string{"send_message"}})
					if err != nil {
						t.Fatal(err)
					}
					requestCtx, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()
					called, messageID := false, ""
					err = tools.withAuthorized(requestCtx, token, "send_message", func(scope store.MemberToolScope, _ MemberToolGrants) error {
						called = true
						if err := requestCtx.Err(); err != nil {
							t.Fatalf("request context expired before the real preflight succeeded: %v", err)
						}
						if outcome.err == nil {
							message, err := f.db.SendMessageForTool(requestCtx, scope, store.MemberToolMessageInput{Content: "committed before context completion"}, "callback-commit")
							if err != nil {
								t.Fatal(err)
							}
							messageID = message.ID
						}
						switch state {
						case "canceled":
							cancel()
						case "deadline":
							// Advance virtual time only after validation (and any commit).
							time.Sleep(time.Second)
							<-requestCtx.Done()
						}
						return outcome.err
					})
					want := outcome.err
					if outcome.err != nil && state != "live" {
						want = context.Canceled
						if state == "deadline" {
							want = context.DeadlineExceeded
						}
					}
					if !called || !errors.Is(err, want) {
						t.Errorf("callback entered=%t, result=%v; want %v", called, err, want)
					}
					if outcome.err == nil {
						message, err := f.db.GetMessage(ctx, f.groupID, messageID)
						if err != nil || message.ID == "" || message.Content != "committed before context completion" {
							t.Fatalf("successful callback lost its committed result: %v", err)
						}
						f.counts(t, 2, 1, 1, 1)
					} else {
						f.counts(t, 1, 1, 1, 0)
					}
				})
			})
		}
	}
}
