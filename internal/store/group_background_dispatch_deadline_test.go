package store_test

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestGroupBackgroundDispatchReadyUntilClamp(t *testing.T) {
	const ttl = 30 * time.Second
	for _, tc := range []struct {
		name              string
		ceiling, lifetime time.Duration
	}{
		{"earlier ceiling", time.Second, time.Second},
		{"later ceiling", time.Minute, ttl},
		{"zero retains TTL", 0, ttl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newBackgroundFixture(t, "", true)
				cfg := backgroundOptions()
				if tc.ceiling != 0 {
					cfg.ReadyUntil = time.Now().UTC().Add(tc.ceiling)
				}
				f.pass(t, cfg)
				// Both timers advance only synctest's virtual clock.
				<-time.After(tc.lifetime - time.Nanosecond)
				if !f.db.BackgroundDispatchProfileReady(discussionProfile) {
					t.Fatal("profile lost readiness before the installed expiry")
				}
				<-time.After(time.Nanosecond)
				if f.db.BackgroundDispatchProfileReady(discussionProfile) {
					t.Fatal("profile still ready at the exclusive expiry")
				}
			})
		})
	}
}

func TestGroupBackgroundDispatchReadyUntilExpired(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newBackgroundFixture(t, "", true)
		g := f.group(t, "")
		f.pass(t, backgroundOptions())
		if !f.db.BackgroundDispatchProfileReady(discussionProfile) {
			t.Fatal("fixture did not publish readiness before the expired pass")
		}
		m := f.post(t, g.ID, "", platform.MessageInput{Content: "讨论"})
		cfg := backgroundOptions()
		cfg.ReadyUntil = time.Now().UTC()
		f.pass(t, cfg)
		if f.db.BackgroundDispatchProfileReady(discussionProfile) {
			t.Error("expired assertion did not withdraw prior readiness")
		}
		assertBackgroundDeadlineParked(t, f, m)

		f.pass(t, backgroundOptions())
		backgroundDelivery(t, f, m)
	})
}

func TestGroupBackgroundDispatchReadyUntilRouteRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newBackgroundFixture(t, "", true)
		g := f.group(t, "")
		m := f.post(t, g.ID, "", platform.MessageInput{Content: "讨论"})
		cfg := backgroundOptions()
		cfg.ReadyUntil = time.Now().UTC().Add(time.Second)
		// Expire after the join and delivery writes, at the final intent write.
		store.BeforeGroupBackgroundFixtureWrite(t, f.db, "group_background_intents", func() {
			<-time.After(time.Until(cfg.ReadyUntil))
		})
		f.pass(t, cfg)
		if !time.Now().Equal(cfg.ReadyUntil) {
			t.Fatal("fixture did not reach the final route write and expiry")
		}
		assertBackgroundDeadlineParked(t, f, m)
	})
}

func assertBackgroundDeadlineParked(t *testing.T, f backgroundFixture, m domain.Message) {
	t.Helper()
	f.assertCount(t, 1, `SELECT count(*) FROM group_background_intents WHERE message_id=? AND source_id=? AND delivery_id IS NULL`, m.ID, m.SourceID)
	for _, table := range []string{"members", "group_background_joins", "conversations", "deliveries"} {
		f.assertCount(t, 0, `SELECT count(*) FROM `+table)
	}
	f.assertCount(t, 1, `SELECT count(*) FROM messages`)
}

func TestGroupBackgroundDispatchReadyUntilClaimRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		f := newBackgroundFixture(t, "", true)
		g := f.group(t, "")
		m := f.post(t, g.ID, "", platform.MessageInput{Content: "讨论"})
		cfg := backgroundOptions()
		cfg.ReadyUntil = time.Now().UTC().Add(time.Second)
		f.pass(t, cfg)
		pending := backgroundDelivery(t, f, m)
		p, _ := backgroundPreparer(t, f, discussionProfile.ProviderID, provider.Pi, "1.4.0", false)
		prepared, err := p.Prepare(ctx, pending)
		if err != nil || len(prepared.ResourceKeys) == 0 {
			t.Fatalf("want a real preparation with resource keys: %v, %v", prepared.ResourceKeys, err)
		}
		// The preparation is still valid when ClaimTurn starts. Expire only
		// after it has already reserved the delivery and written the turn.
		store.BeforeGroupBackgroundFixtureWrite(t, f.db, "resource_claims", func() {
			<-time.After(time.Until(cfg.ReadyUntil))
		})
		turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: pending.Delivery.ID,
			MaxActive: 1, ResourceKeys: prepared.ResourceKeys, BackgroundProfile: prepared.BackgroundProfile})
		if !time.Now().Equal(cfg.ReadyUntil) {
			t.Fatal("fixture did not reach the resource write and expiry")
		}
		if !errors.Is(err, store.ErrTurnBusy) || turn.ID != "" {
			t.Errorf("stale preparation claimed a turn after expiry: turn %q, error %v", turn.ID, err)
		}
		f.assertCount(t, 1, `SELECT count(*) FROM deliveries WHERE id=? AND turn_id='' AND state='queued'`, pending.Delivery.ID)
		f.assertCount(t, 0, `SELECT count(*) FROM turns`)
		f.assertCount(t, 0, `SELECT count(*) FROM resource_claims`)
	})
}
