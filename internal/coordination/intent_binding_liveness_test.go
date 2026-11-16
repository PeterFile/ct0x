//go:build !race

package coordination

import (
	"context"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestTrustedIntentPassCallbackUsesEngineStore(t *testing.T) {
	f := intentConfigured(t)
	a := f.assignment(t)
	r := f.request(t)
	initialRequest, err := f.s.GetDispatchRequest(context.Background(), r.GroupID, r.ID)
	if err != nil || initialRequest.Request != r || initialRequest.State.Status != domain.DispatchStatusPending ||
		initialRequest.State.Generation != 1 || initialRequest.State.AttemptCount != 0 || initialRequest.State.Delivery != nil {
		t.Fatalf("genuine pending request setup: %+v, %v", initialRequest, err)
	}
	initialAssignment, err := f.s.GetAssignment(context.Background(), a.GroupID, a.ID)
	if err != nil || initialAssignment.Assignment != a || initialAssignment.Dispatch.Status != domain.AssignmentDispatchStatusPending ||
		initialAssignment.Dispatch.Generation != 1 || initialAssignment.Dispatch.AttemptCount != 0 || initialAssignment.Dispatch.Delivery != nil {
		t.Fatalf("genuine pending assignment setup: %+v, %v", initialAssignment, err)
	}
	source, err := f.s.GetDispatchRequest(context.Background(), a.GroupID, a.RequestID)
	if err != nil || source.Request.ID != a.RequestID || source.Request.TaskID != a.TaskID || source.Request.GroupID != a.GroupID ||
		source.Request.TopicID != f.thread.TopicID || source.Request.CoordinatorMemberID != f.coord || source.State.Delivery == nil ||
		a.WorkerMemberID != f.worker || a.DecisionID == "" || r.ID == a.RequestID {
		t.Fatalf("independent assignment source provenance: %+v, %v", source, err)
	}
	at := time.Now().UTC()
	requests, err := f.s.ListDueDispatchRequests(context.Background(), at, store.DispatchDueOptions{})
	if err != nil || len(requests.Items) != 1 || requests.Items[0].RequestID != r.ID || requests.Items[0].Generation != initialRequest.State.Generation {
		t.Fatalf("request must be due beside the assignment: %+v, %v", requests, err)
	}
	assignments, err := f.s.ListDueAssignments(context.Background(), at, store.AssignmentDueOptions{})
	if err != nil || len(assignments.Items) != 1 || assignments.Items[0].AssignmentID != a.ID || assignments.Items[0].Generation != initialAssignment.Dispatch.Generation {
		t.Fatalf("assignment must be due beside the request: %+v, %v", assignments, err)
	}
	before := engineCoordinatorSnapshot(t, f.db)
	engine := New(f.s)
	callback := trustedIntentCallback(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var requestReceipt *domain.DispatchDeliveryReceipt
	var assignmentReceipt *domain.AssignmentDeliveryReceipt
	calls := 0
	// This external watchdog does not enlarge the production run's shared 1s.
	for {
		if ctx.Err() != nil {
			t.Fatalf("dual-kind receipt watchdog: calls=%d request=%+v assignment=%+v", calls, requestReceipt, assignmentReceipt)
		}
		hadReceipts := requestReceipt != nil && assignmentReceipt != nil
		reported := callback(ctx)
		calls++
		// Read durable state independently, even after a reported deadline.
		d, err := f.s.GetDispatchRequest(context.Background(), r.GroupID, r.ID)
		if err != nil || d.Request != r || d.State.RequestID != r.ID {
			t.Fatalf("callback changed the original request identity: %+v, %v", d, err)
		}
		v, err := f.s.GetAssignment(context.Background(), a.GroupID, a.ID)
		if err != nil || v.Assignment != a || v.Dispatch.AssignmentID != a.ID {
			t.Fatalf("callback changed the original assignment identity: %+v, %v", v, err)
		}
		sourceAfter, err := f.s.GetDispatchRequest(context.Background(), a.GroupID, a.RequestID)
		if err != nil || !reflect.DeepEqual(source, sourceAfter) {
			t.Fatalf("callback changed the assignment's original source receipt: %+v, %v", sourceAfter, err)
		}
		t.Logf("callback=%d parent-context=%v request-state=%+v request-receipt=%+v assignment-state=%+v assignment-receipt=%+v reported=%v",
			calls, ctx.Err(), d.State, d.State.Delivery, v.Dispatch, v.Dispatch.Delivery, reported)
		if requestReceipt != nil && !reflect.DeepEqual(requestReceipt, d.State.Delivery) {
			t.Fatalf("request receipt changed across callbacks: %+v", d)
		}
		if assignmentReceipt != nil && !reflect.DeepEqual(assignmentReceipt, v.Dispatch.Delivery) {
			t.Fatalf("assignment receipt changed across callbacks: %+v", v)
		}
		outputs := 0
		switch d.State.Status {
		case domain.DispatchStatusPending:
			if !reflect.DeepEqual(initialRequest.State, d.State) {
				t.Fatalf("pending request lost its original generation or was deferred: %+v", d)
			}
		case domain.DispatchStatusEnqueued:
			receipt := d.State.Delivery
			if receipt == nil || receipt.CoordinatorMemberID != r.CoordinatorMemberID || receipt.MaterializedFromGeneration != initialRequest.State.Generation || receipt.CreatedAt.IsZero() ||
				d.State.Generation != initialRequest.State.Generation+1 || d.State.AttemptCount != 0 || d.State.LastErrorCode != "" || d.State.HeldReason != "" ||
				!d.State.NextAttemptAt.Equal(initialRequest.State.NextAttemptAt) {
				t.Fatalf("invalid original-generation request receipt: %+v", d)
			}
			trustedIntentRequireOutput(t, f, receipt.MessageID, receipt.DeliveryID, receipt.ConversationID, "user", r.CoordinatorMemberID,
				"系统根据本地用户调度命令", "DispatchRequest: "+r.ID, "Task: "+r.TaskID, "Mandate: "+r.MandateID)
			requestReceipt = receipt
			outputs++
		default:
			t.Fatalf("fresh eligible request has an undeclared disposition: %+v", d)
		}
		switch v.Dispatch.Status {
		case domain.AssignmentDispatchStatusPending:
			if !reflect.DeepEqual(initialAssignment.Dispatch, v.Dispatch) {
				t.Fatalf("pending assignment lost its original generation or was deferred: %+v", v)
			}
		case domain.AssignmentDispatchStatusEnqueued:
			receipt := v.Dispatch.Delivery
			if receipt == nil || receipt.WorkerMemberID != a.WorkerMemberID || receipt.MaterializedFromGeneration != initialAssignment.Dispatch.Generation || receipt.CreatedAt.IsZero() ||
				v.Dispatch.Generation != initialAssignment.Dispatch.Generation+1 || v.Dispatch.AttemptCount != 0 || v.Dispatch.LastErrorCode != "" || v.Dispatch.HeldReason != "" ||
				!v.Dispatch.NextAttemptAt.Equal(initialAssignment.Dispatch.NextAttemptAt) {
				t.Fatalf("invalid original-generation assignment receipt: %+v", v)
			}
			trustedIntentRequireOutput(t, f, receipt.MessageID, receipt.DeliveryID, receipt.ConversationID, source.Request.CoordinatorMemberID, a.WorkerMemberID,
				"平台根据已保存的协调决定", "Assignment: "+a.ID, "DispatchRequest: "+a.RequestID, "Task: "+a.TaskID, "Mandate: "+source.Request.MandateID)
			assignmentReceipt = receipt
			outputs++
		default:
			t.Fatalf("fresh eligible assignment has an undeclared disposition: %+v", v)
		}
		if requestReceipt != nil && assignmentReceipt != nil &&
			(requestReceipt.MessageID == assignmentReceipt.MessageID || requestReceipt.DeliveryID == assignmentReceipt.DeliveryID) {
			t.Fatal("the two kinds reused one output identity")
		}
		trustedIntentRequireEffects(t, before, engineCoordinatorSnapshot(t, f.db), outputs)
		if reported != nil {
			// Inspect every leaf; a deadline must not hide any mixed failure.
			trustedIntentRequireCancellationTree(t, reported, false)
		}
		if hadReceipts && reported == nil {
			break // Durable completion was already observed; this callback preserved it.
		}
		// Neither a first nil run nor an exhaustion-only nil run proves global
		// completion. Keep observing the real original-generation durable states.
	}
	after := engineCoordinatorSnapshot(t, f.db)
	// These calls only replay receipts already produced by the real callback.
	dReplay, err := f.s.MaterializeDispatchRequest(context.Background(), r.ID, initialRequest.State.Generation, time.Now().Add(time.Hour))
	if err != nil {
		trustedIntentLogError(t, err)
		t.Fatalf("request original-generation receipt replay: %v", err)
	}
	if dReplay.Disposition != string(domain.DispatchStatusEnqueued) || dReplay.Generation != initialRequest.State.Generation+1 ||
		!reflect.DeepEqual(dReplay.Delivery, requestReceipt) || dReplay.HeldReason != "" {
		t.Fatalf("request original-generation receipt replay: %+v", dReplay)
	}
	aReplay, err := f.s.MaterializeAssignment(context.Background(), a.ID, initialAssignment.Dispatch.Generation, time.Now().Add(time.Hour))
	if err != nil {
		trustedIntentLogError(t, err)
		t.Fatalf("assignment original-generation receipt replay: %v", err)
	}
	if aReplay.Disposition != string(domain.AssignmentDispatchStatusEnqueued) || aReplay.Generation != initialAssignment.Dispatch.Generation+1 ||
		!reflect.DeepEqual(aReplay.Delivery, assignmentReceipt) || aReplay.HeldReason != "" {
		t.Fatalf("assignment original-generation receipt replay: %+v", aReplay)
	}
	d, err := f.s.GetDispatchRequest(context.Background(), r.GroupID, r.ID)
	if err != nil || d.Request != r || !reflect.DeepEqual(requestReceipt, d.State.Delivery) {
		t.Fatalf("request receipt readback after replay: %+v, %v", d, err)
	}
	v, err := f.s.GetAssignment(context.Background(), a.GroupID, a.ID)
	if err != nil || v.Assignment != a || !reflect.DeepEqual(assignmentReceipt, v.Dispatch.Delivery) {
		t.Fatalf("assignment receipt readback after replay: %+v, %v", v, err)
	}
	if !reflect.DeepEqual(after, engineCoordinatorSnapshot(t, f.db)) {
		t.Fatal("original-generation receipt replay duplicated or changed persisted rows")
	}
	trustedIntentRequireEffects(t, before, after, 2)
	t.Logf("both original-generation receipts remained immutable across %d real callbacks and explicit receipt replay", calls)
}
