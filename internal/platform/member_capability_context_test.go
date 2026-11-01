package platform

import (
	"context"
	"errors"
	"testing"
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
