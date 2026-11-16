//go:build race

package main

import (
	"context"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

// This is a race safety prerequisite, not evidence of one-second pump throughput.
// The production scheduler and its real intent callback continue running normally.
func coordinationCompositionAssignmentPrerequisite(t *testing.T, f *coordinationCompositionFixture, report coordinationCompositionReport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before, err := f.db.GetAssignment(ctx, f.group.ID, report.AssignmentID)
	if err != nil {
		t.Fatalf("race safety prerequisite could not read the durable assignment: %v", err)
	}
	if before.Assignment.ID != report.AssignmentID || before.Assignment.GroupID != f.group.ID || before.Assignment.WorkerMemberID != f.worker.ID || before.Decision.CoordinatorTurnID != report.RunID || before.Decision.CoordinatorMemberID != f.lead.ID || before.Decision.Kind != domain.CoordinatorDecisionKindAssign {
		t.Fatal("race safety prerequisite lost the actual coordinator decision/assignment identity")
	}
	generation := before.Dispatch.Generation
	if before.Dispatch.Delivery != nil {
		generation = before.Dispatch.Delivery.MaterializedFromGeneration
	}
	if generation < 1 {
		t.Fatal("race safety prerequisite has no recorded original generation")
	}
	var materialized domain.AssignmentMaterialization
	var materializeErr error
	if before.Dispatch.Delivery == nil {
		if before.Dispatch.Status != domain.AssignmentDispatchStatusPending {
			t.Fatal("race safety prerequisite requires the original pending assignment")
		}
		materialized, materializeErr = f.db.MaterializeAssignment(ctx, before.Assignment.ID, generation, time.Now().UTC())
	}
	// GetAssignment validates the real decision and message/delivery relationships.
	// A concurrent winner is accepted only with the same identity and original receipt generation.
	after, err := f.db.GetAssignment(ctx, f.group.ID, report.AssignmentID)
	if err != nil {
		t.Fatalf("race safety prerequisite has no durable reconciliation: read=%v materialize=%v", err, materializeErr)
	}
	d := after.Dispatch.Delivery
	if after.Assignment != before.Assignment || after.Decision != before.Decision || after.Dispatch.AssignmentID != before.Assignment.ID || after.Dispatch.Status != domain.AssignmentDispatchStatusEnqueued || after.Dispatch.Generation != generation+1 || d == nil || d.MaterializedFromGeneration != generation || d.WorkerMemberID != f.worker.ID || d.MessageID == "" || d.DeliveryID == "" || d.ConversationID == "" || d.CreatedAt.IsZero() {
		t.Fatalf("race safety prerequisite did not reconcile the same original assignment generation/receipt: materialize=%v", materializeErr)
	}
	if before.Dispatch.Delivery != nil {
		if before.Dispatch.Status != domain.AssignmentDispatchStatusEnqueued || before.Dispatch.Generation != after.Dispatch.Generation || *before.Dispatch.Delivery != *d {
			t.Fatal("race safety prerequisite changed an already-enqueued receipt")
		}
	} else if materializeErr == nil {
		if materialized.Disposition != string(domain.AssignmentDispatchStatusEnqueued) || materialized.Generation != after.Dispatch.Generation || materialized.Delivery == nil || *materialized.Delivery != *d || materialized.HeldReason != "" {
			t.Fatal("race safety prerequisite materialization result differs from its durable receipt")
		}
	}
	if materializeErr != nil {
		t.Logf("race safety prerequisite reconciled assignment %s original generation %d to delivery %s after: %v", before.Assignment.ID, generation, d.DeliveryID, materializeErr)
	}
}
