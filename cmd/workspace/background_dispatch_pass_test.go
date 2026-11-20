package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func newBackgroundPreviewPassFixture(t *testing.T) (config, *store.Store, *platform.Preparer, []provider.Provider) {
	t.Helper()
	f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dbPath, approvalPath := filepath.Join(dir, "workspace.db"), filepath.Join(dir, "a1.json")
	installBackgroundPreviewFixture(t, dbPath)
	db := openBackgroundPreviewTestDB(t, dbPath)
	t.Cleanup(func() { db.Close() })
	if err := db.EnableGroupBackgroundDispatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	providers := provider.Discover(provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: f.path}}})
	providers[0].Version = "1.4.0"
	p, err := platform.NewPreparer(db, platform.PrepareOptions{Providers: providers, PrivateWorkspaceRoot: dir})
	if err != nil {
		t.Fatal(err)
	}
	c := config{enableExecution: true, piPath: f.path, groupBackgroundDispatchPreview: approvalPath}
	return c, db, p, providers
}

func TestBackgroundDispatchPreviewClearsReadinessOnNextPass(t *testing.T) {
	ctx := context.Background()
	c, db, p, providers := newBackgroundPreviewPassFixture(t)
	piPath, approvalPath := c.piPath, c.groupBackgroundDispatchPreview
	writeBackgroundPreviewFixtureApproval(t, approvalPath, backgroundPreviewFixtureApproval(t, piPath))
	var output bytes.Buffer
	var previousCalls int
	pass, err := c.withBackgroundDispatchPass(db, p, providers, func(runCtx context.Context) {
		if runCtx != ctx {
			t.Error("existing intent callback did not receive the scheduler context")
		}
		previousCalls++
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	local := platform.NewLocalUser(db)
	group, err := local.CreateGroup(ctx, platform.GroupInput{Name: "撤回测试"}, "")
	if err != nil {
		t.Fatal(err)
	}
	message, err := local.CreateMessage(ctx, group.ID, platform.MessageInput{Content: "讨论"}, "")
	if err != nil {
		t.Fatal(err)
	}
	writeReady := func(t *testing.T) map[string]any {
		t.Helper()
		current := backgroundPreviewFixtureApproval(t, piPath)
		current["qualification"] = backgroundPreviewQualification(current, piPath, time.Now())
		writeBackgroundPreviewFixtureApproval(t, approvalPath, current)
		return current
	}
	writeReady(t)
	pass(ctx)
	message, err = db.GetMessage(ctx, group.ID, message.ID)
	if err != nil || len(message.Deliveries) != 1 {
		t.Fatalf("fixture should already have routed its only intent: %+v, %v", message, err)
	}
	pending := store.PendingDelivery{GroupID: group.ID, Delivery: message.Deliveries[0]}
	prepared, err := p.Prepare(ctx, pending)
	if err != nil || prepared.BackgroundProfile != (store.BackgroundDispatchProfile{ProviderID: providers[0].ID, Model: backgroundFixtureModel}) {
		t.Fatalf("fixture needs a genuine A1 prepared profile: %+v, %v", prepared.BackgroundProfile, err)
	}
	claim := store.ClaimTurnInput{GroupID: group.ID, DeliveryID: pending.Delivery.ID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys, BackgroundProfile: prepared.BackgroundProfile}

	for _, tc := range []struct {
		name   string
		change func(map[string]any, map[string]any)
		log    bool
	}{
		{"paused", func(a, _ map[string]any) { a["paused"] = true }, false},
		{"expired approval", func(a, _ map[string]any) { a["approval_expires_at"] = time.Now().Add(-time.Minute) }, false},
		{"invalid capacity", func(a, _ map[string]any) { a["max_active_members"] = 0 }, true},
		{"invalid preset", func(a, _ map[string]any) { delete(a, "preset") }, true},
		{"invalid approval provider", func(a, _ map[string]any) { a["provider_id"] = "other-installation" }, true},
		{"no evidence", func(a, _ map[string]any) { delete(a, "qualification") }, false},
		{"expired evidence", func(_, q map[string]any) { q["observed_at"] = time.Now().Add(-30 * time.Second) }, false},
		{"future evidence", func(_, q map[string]any) { q["observed_at"] = time.Now().Add(time.Minute) }, false},
		{"different model", func(_, q map[string]any) { q["model"] = "fixture-provider/other-model" }, false},
		{"different provider", func(_, q map[string]any) { q["provider_id"] = "other-installation" }, false},
		{"different path", func(_, q map[string]any) { q["path"] = piPath + ".other" }, false},
		{"different version", func(_, q map[string]any) { q["version"] = "1.4.1" }, false},
		{"different transport", func(_, q map[string]any) { q["transport"] = provider.CodexOneShotAdapterID }, false},
		{"withdrawn approval file", nil, false},
		// Last: replace the selected Pi even though the path/version in the
		// catalog and qualification still match and the new file is executable.
		{"replaced Pi", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approval := writeReady(t)
			pass(ctx) // No unresolved intents; a fresh sample must still republish.
			if !db.BackgroundDispatchProfileReady(prepared.BackgroundProfile) {
				t.Fatal("qualified live pair not republished on the idle pass")
			}
			switch {
			case tc.change != nil:
				tc.change(approval, approval["qualification"].(map[string]any))
				writeBackgroundPreviewFixtureApproval(t, approvalPath, approval)
			case tc.name == "withdrawn approval file":
				if err := os.Remove(approvalPath); err != nil {
					t.Fatal(err)
				}
			case tc.name == "replaced Pi":
				if err := os.Rename(piPath, piPath+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(piPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			output.Reset()
			calls := previousCalls
			pass(ctx)
			if previousCalls != calls+1 {
				t.Fatal("existing intent callback was dropped or repeated")
			}
			if tc.log != strings.Contains(output.String(), "群组后台调度扫描失败") {
				t.Fatalf("unexpected intent error report: %q", &output)
			}
			if db.BackgroundDispatchProfileReady(prepared.BackgroundProfile) {
				t.Fatal("previous Store readiness survived an observed loss of approval/evidence")
			}
			if _, err := p.Prepare(ctx, pending); !errors.Is(err, scheduler.ErrNotReady) {
				t.Fatalf("unqualified ready pair still prepared: %v", err)
			}
			if _, err := db.ClaimTurn(ctx, claim); !errors.Is(err, store.ErrTurnBusy) {
				t.Fatalf("stale prepared profile could still claim: %v", err)
			}
		})
	}
}

func TestBackgroundDispatchPreviewClockRollback(t *testing.T) {
	for _, atExpiry := range []string{"unchanged", "withdrawn", "replaced", "paused"} {
		t.Run(atExpiry, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				c, db, p, providers := newBackgroundPreviewPassFixture(t)
				path := c.groupBackgroundDispatchPreview
				grant := backgroundPreviewFixtureApproval(t, c.piPath)
				writeBackgroundPreviewFixtureApproval(t, path, grant)
				start := time.Now()
				var wallOffset time.Duration
				sample := func() (time.Time, time.Duration) {
					now := time.Now()
					return now.UTC().Add(wallOffset), now.Sub(start)
				}
				var output bytes.Buffer
				pass, err := c.withBackgroundDispatchPassClock(db, p, providers, nil, &output, sample)
				if err != nil {
					t.Fatal(err)
				}
				profile := store.BackgroundDispatchProfile{ProviderID: providers[0].ID, Model: backgroundFixtureModel}
				check := func(stage string, want bool) {
					t.Helper()
					if got := db.BackgroundDispatchProfileReady(profile); got != want {
						t.Errorf("%s: profile ready = %t, want %t", stage, got, want)
					}
				}
				run := func(stage string, want bool) {
					t.Helper()
					pass(ctx)
					if output.Len() != 0 {
						t.Fatalf("%s: unexpected pass error: %s", stage, &output)
					}
					check(stage, want)
				}
				attest := func(at time.Time) {
					grant["qualification"] = backgroundPreviewQualification(grant, c.piPath, at)
					writeBackgroundPreviewFixtureApproval(t, path, grant)
				}

				const step = 10 * time.Second
				wallOffset = step // Calibrate again when the wall jumps forward from startup.
				attest(time.Now().Add(wallOffset))
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				run("initial qualification", true)
				time.Sleep(2 * step)
				run("before wall rollback", true)
				wallOffset = 0 // Align the rolled-back sample with the Store's virtual wall.
				run("qualified with less time remaining than raw wall suggests", true)
				time.Sleep(step - time.Nanosecond)
				check("one nanosecond before the effective expiry", true)
				time.Sleep(time.Nanosecond)
				check("at the exclusive effective expiry", false)
				time.Sleep(time.Nanosecond)

				// A full qualification lifetime has elapsed, even when no pass
				// sees the original grant expire against the raw wall.
				switch atExpiry {
				case "unchanged":
					wallOffset = step // Both wall and elapsed clocks have passed expiry.
				case "withdrawn":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "replaced":
					attest(time.Now()) // A different valid observation, not the old one.
				case "paused":
					grant["paused"] = true
					writeBackgroundPreviewFixtureApproval(t, path, grant)
					delete(grant, "paused")
				}
				run("pass after a full qualification lifetime elapsed", atExpiry == "replaced")
				wallOffset = 0
				if err := os.WriteFile(path+".replay", original, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".replay", path); err != nil {
					t.Fatal(err)
				}
				run("expired bytes restored under rolled-back wall", false)

				attest(time.Now().Add(step / 2))
				run("observation in the raw wall's future but not the effective clock's", false)
				wallOffset = step
				attest(time.Now().Add(wallOffset))
				run("fresh observation after wall recovery", true)
			})
		})
	}
}
