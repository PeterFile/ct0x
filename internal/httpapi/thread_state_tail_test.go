package httpapi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestThreadStateTailLossFailsClosed(t *testing.T) {
	for _, name := range []string{"only revision", "last retirement"} {
		t.Run(name, func(t *testing.T) {
			a := openMessageAPI(t, "")
			g := newGroup(t, a, "tail loss")
			const topic = "export"
			const initialPlan = "use the old exporter"
			url := threadStateURL(g.ID, topic)
			if !threadStateInstalled(t, a) {
				m := threadStateMessage(t, a, g.ID, topic, initialPlan)
				assertThreadStateUnavailable(t, a, url, stateUpdate(0, 0, "plan", "set", m.Content, m.ID))
				return
			}
			post := func(expected int, action, text string) {
				t.Helper()
				m := threadStateMessage(t, a, g.ID, topic, text)
				decoded[domain.ThreadStateSnapshot](t, a.request("PUT", url, stateUpdate(expected, 0, "plan", action, text, m.ID), action), 200)
			}
			expected, action, text := 0, "set", initialPlan
			if name == "last retirement" {
				post(expected, action, text)
				expected, action, text = 1, "invalidate", "retire the old exporter"
			}
			m := decoded[domain.Member](t, a.request("POST", threadStateBase+g.ID+"/members",
				`{"name":"handoff","role":"inspect","provider_id":"fixture","model":"fixture/model"}`), 201)
			trigger := threadStateMessage(t, a, g.ID, topic, "inspect the current plan", m.ID)
			cli := filepath.Join(t.TempDir(), "never-run")
			if err := os.WriteFile(cli, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
				t.Fatal(err)
			}
			p, err := platform.NewPreparer(a.db, platform.PrepareOptions{PrivateWorkspaceRoot: t.TempDir(),
				Providers: []provider.Provider{{ID: "fixture", Name: provider.Codex, Version: "0.162.1", Installation: provider.Installed, Path: cli}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			pending := store.PendingDelivery{GroupID: g.ID, Delivery: trigger.Deliveries[0]}
			old, err := p.Prepare(ctx, pending)
			if err != nil || old.PreparedThreadState == nil || old.PreparedThreadState.Revision != int64(expected) {
				t.Fatalf("prepare before the last change: %+v, %v", old.PreparedThreadState, err)
			}
			post(expected, action, text)

			// Simulate loss of the committed revision on a fixture-only connection
			// without foreign keys. Restore the trigger so the schema shape is intact.
			raw := messageSQL(t, a)
			tx, err := raw.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var ddl string
			if err := tx.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='thread_state_no_delete'`).Scan(&ddl); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`DROP TRIGGER thread_state_no_delete`); err != nil {
				t.Fatal(err)
			}
			result, err := tx.Exec(`DELETE FROM thread_state_revisions WHERE group_id=? AND topic_id=? AND task_id='' AND task_revision=0 AND revision=?`, g.ID, topic, expected+1)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				t.Fatalf("delete fixture's latest revision: %d, %v", n, err)
			}
			if _, err := tx.Exec(ddl); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}

			if w := a.request("GET", url, ""); w.Code != 500 {
				t.Errorf("GET accepted the shortened state: %d %s", w.Code, w.Body)
			} else {
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
			}
			if got, err := p.Prepare(ctx, pending); err == nil || got.Executor != nil || got.Request.Prompt != "" || got.PreparedThreadState != nil {
				t.Errorf("Prepare accepted the shortened state: %+v, %v", got.PreparedThreadState, err)
			}
			// The old stamp matches the shortened prefix, not the committed head.
			if turn, err := a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: pending.Delivery.ID, MaxActive: 1,
				PreparedThreadState: old.PreparedThreadState, ResourceKeys: old.ResourceKeys}); err == nil || errors.Is(err, store.ErrTurnBusy) || turn.ID != "" {
				t.Errorf("ClaimTurn did not reject the damaged timeline: turn=%s, %v", turn.ID, err)
			}
			var untouched bool
			if err := raw.QueryRow(`SELECT state='queued' AND turn_id='' AND NOT EXISTS(SELECT 1 FROM turns)
			 AND NOT EXISTS(SELECT 1 FROM resource_claims) FROM deliveries WHERE id=?`, pending.Delivery.ID).Scan(&untouched); err != nil || !untouched {
				t.Errorf("failed claim left durable reservations: %v, %v", untouched, err)
			}
		})
	}
}
