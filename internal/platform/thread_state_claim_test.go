package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func TestThreadStateSchedulerRepreparesBeforeClaim(t *testing.T) {
	for _, name := range []string{"first confirmation", "invalidate prepared plan"} {
		t.Run(name, func(t *testing.T) {
			f := newPrepareFixture(t)
			ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
			defer stop()
			thread := domain.ThreadRef{GroupID: f.group.ID, TopicID: f.trigger.TopicID}
			scope := domain.ThreadStateScope{ThreadRef: thread}
			local := NewLocalUser(f.s)
			update := func(revision int64, action, content string) error {
				m, err := local.CreateMessage(ctx, f.group.ID, MessageInput{TopicID: thread.TopicID, Content: content, RecipientIDs: []string{}}, action)
				if err != nil {
					return err
				}
				_, err = local.SetThreadState(ctx, scope, store.ThreadStateUpdate{ExpectedRevision: revision,
					Kind: domain.ThreadStatePlan, Action: action, Content: content, SourceMessage: m.ID}, action)
				return err
			}
			expected, action, content := int64(0), "set", "first confirmed plan"
			if !threadStateFixtureInstalled(t, f) {
				if err := update(expected, action, content); !errors.Is(err, store.ErrThreadStateUnavailable) {
					t.Fatalf("state update without E2 schema: %v", err)
				}
				got := assertLegacyThreadPreparation(t, f, f.pending)
				f.assertQueued(t)
				turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, MaxActive: 1,
					PreparedThreadState: got.PreparedThreadState, ResourceKeys: got.ResourceKeys})
				if err != nil || turn.ID == "" || turn.DeliveryID != f.pending.Delivery.ID || turn.State != domain.TurnStateStarting {
					t.Fatalf("legacy delivery could not be claimed: %+v, %v", turn, err)
				}
				t.Log("unregistered E2: ClaimTurn accepts legacy preparation after a rejected state update")
				return
			}
			if name == "invalidate prepared plan" {
				if err := update(0, "set", "old plan"); err != nil {
					t.Fatal(err)
				}
				expected, action, content = 1, "invalidate", "stop using the old plan"
			}
			p := f.preparer(t)
			executed := make(chan string, 1)
			finished := make(chan error, 1)
			attempts := 0 // Owned by the scheduling goroutine.
			go func() {
				finished <- scheduler.Run(ctx, f.s, scheduler.Options{MaxActive: 1, Recovered: true,
					RuntimeDir: f.opts.PrivateWorkspaceRoot, PollInterval: time.Millisecond}, func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
					attempts++
					got, err := p.Prepare(ctx, pending)
					if err != nil {
						return scheduler.PreparedExecution{}, err
					}
					if attempts == 1 {
						// Commit the change after rendering, before the existing claim.
						if err := update(expected, action, content); err != nil {
							return scheduler.PreparedExecution{}, err
						}
					} else {
						var clean bool
						err := f.db.QueryRowContext(ctx, `SELECT state='queued' AND turn_id='' AND NOT EXISTS(SELECT 1 FROM turns)
						 AND NOT EXISTS(SELECT 1 FROM resource_claims) FROM deliveries WHERE id=?`, pending.Delivery.ID).Scan(&clean)
						if err != nil || !clean {
							return scheduler.PreparedExecution{}, errors.New("stale claim left durable reservations")
						}
					}
					got.Executor = memberToolRuntimeExecutorFunc(func(_ context.Context, req provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
						executed <- req.Prompt
						return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
					})
					return got, nil
				})
			}()
			select {
			case prompt := <-executed:
				stop()
				if err := <-finished; !errors.Is(err, context.Canceled) {
					t.Fatalf("scheduler did not drain: %v", err)
				}
				if attempts != 2 || !strings.Contains(prompt, content) || (action == "invalidate" && !strings.Contains(prompt, `"active":false`)) {
					t.Fatalf("started before reprepare: attempts=%d prompt=%s", attempts, prompt)
				}
			case err := <-finished:
				t.Fatalf("scheduler stopped before a fresh preparation: %v", err)
			}
		})
	}
}
