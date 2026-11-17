//go:build !race

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func coordinationCompositionToolBudget() time.Duration { return 3 * time.Second }

// Ordinary runs must use the real production one-second intent pass for both kinds.
func coordinationCompositionAssignmentPrerequisite(*testing.T, *coordinationCompositionFixture, coordinationCompositionReport) {
}

func TestCoordinationCompositionSchedulerRoleReceipts(t *testing.T) {
	for _, thinking := range []string{"", "max"} {
		t.Run("thinking="+thinking, func(t *testing.T) {
			f := newCoordinationCompositionFixture(t, "normal", thinking)
			base, stop, _, _ := startCoordinationComposition(t, 10*time.Second, f.args(t, thinking)...)
			// The configured coordinator also receives an ordinary delivery; its
			// member identity must not turn that capability into a role grant.
			if _, err := f.db.CreateMessage(context.Background(), store.MessageRequest{GroupID: f.group.ID, ActorID: "user", TopicID: "ordinary", Content: "普通协调成员投递", RecipientIDs: []string{f.lead.ID}}, []string{f.lead.ID}); err != nil {
				t.Fatal(err)
			}
			ordinary := f.report(t, "ordinary")
			f.turn(t, ordinary.RunID, domain.TurnStateSucceeded)
			request := f.dispatchHTTP(t, base)
			coordinator := f.report(t, "coordinator")
			assignment := f.workerQueuedAtCapacity(t, base, coordinator)
			if assignment.Request.ID != request.ID || assignment.Decision.CoordinatorTurnID != coordinator.RunID || assignment.Assignment.WorkerMemberID != f.worker.ID {
				t.Fatal("real coordinator receipt did not bind the HTTP request and selected worker")
			}
			f.release(t, "coordinator")
			f.turn(t, coordinator.RunID, domain.TurnStateSucceeded)
			worker := f.report(t, "worker")
			f.release(t, "worker")
			f.turn(t, worker.RunID, domain.TurnStateSucceeded)
			task, err := f.db.GetTask(context.Background(), f.group.ID, request.TaskID)
			if err != nil || task.Status != domain.TaskStatusSubmitted || task.CurrentSubmissionID != worker.SubmissionID {
				t.Fatalf("worker HTTP submission was not durable evidence separate from acceptance: %v", err)
			}
			submission, err := f.db.GetTaskSubmission(context.Background(), f.group.ID, task.ID, worker.SubmissionID)
			if err != nil || submission.SubmittedBy != f.worker.ID || submission.AttemptID != task.ActiveAttemptID || submission.Summary != "本地角色工具提交" {
				t.Fatalf("submission lost the actual bound worker/attempt: %v", err)
			}
			binding, err := f.db.GetAssignment(context.Background(), f.group.ID, coordinator.AssignmentID)
			if err != nil || binding.Binding == nil || binding.Binding.TurnID != worker.RunID || binding.Binding.AttemptID != submission.AttemptID {
				t.Fatal("worker execution did not bind the authoritative Assignment attempt")
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			for _, run := range []string{coordinator.RunID, worker.RunID} {
				receipt := f.receipt(t, run)
				if receipt.Outcome != provider.OutcomeSucceeded || !receipt.Accepted || !receipt.DirectChildReaped || !receipt.OutputComplete || filepath.Dir(receipt.StdoutPath) != filepath.Join(f.dir, "runtime", run) {
					t.Fatal("role native receipt or actual scheduler RuntimeDir was lost")
				}
			}
			if coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM coordination_decisions") != 1 || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM task_submissions") != 1 || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM task_review_decisions") != 0 || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM resource_claims") != 0 {
				t.Fatal("composition duplicated role writes, accepted a submission, or failed to drain receipts")
			}
		})
	}
}
