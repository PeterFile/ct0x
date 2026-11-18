package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestB3OwnerWorkMutationUncertainty(t *testing.T) {
	for _, tc := range []struct {
		operation string
		category  error
	}{
		{"delegate_work", ErrConflict},
		{"handback_work", ErrMemberToolDenied},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			f, owner, child, _ := workReportOwner(t, 1)
			p, scope := owner.Provenance, owner.Member
			before := messageSequenceSnapshot(t, f.s)
			const key = "Owner/Exact:1"
			delegate := domain.DelegateWorkInput{MemberID: child.ID, Question: "核对摘要。"}
			handback := domain.HandbackWorkInput{Summary: "请复核摘要。"}
			delegating := tc.operation == "delegate_work"
			invoke := func(s *Store) (any, error) {
				if delegating {
					return s.DelegateWork(ctx, scope, delegate, key)
				}
				return s.HandbackWork(ctx, scope, handback, key)
			}

			cause := fmt.Errorf("commit acknowledgement lost: %w", tc.category)
			fault, observed := coordinationDecideWrapped(t, f.path, "ack-error", cause)
			fault.EnableTaskWorkExecution()
			fault.EnableInvestigationExecution()
			out, callErr := invoke(fault)
			if len(observed.begins) != 1 {
				t.Fatalf("expected one transaction, got %d", len(observed.begins))
			}
			for _, s := range []*Store{fault, f.s} {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}

			// Prove the write committed before checking the caller's outcome or replaying.
			s := queueHandle(t, f.path, false)
			s.EnableTaskWorkExecution()
			s.EnableInvestigationExecution()
			var id string
			var payload []byte
			if err := s.db.QueryRow(`SELECT record_id,payload FROM mutation_idempotency WHERE (operation,actor_id,group_id,idempotency_key)=(?,?,?,?)`,
				tc.operation, p.MemberID, p.GroupID, key).Scan(&id, &payload); err != nil {
				t.Fatalf("missing durable exact-key receipt: %v", err)
			}
			var want any
			var wantPayload []byte
			added := []string{"messages", "message_log", "mutation_idempotency"}
			if delegating {
				d, err := s.GetTaskWorkDelegation(ctx, p.GroupID, id)
				if err != nil || d.ID != id || d.AssignmentID != p.AssignmentID || d.AuthorTurnID != scope.Claim.TurnID || d.AuthorAttemptID != owner.AttemptID ||
					d.MemberID != delegate.MemberID || d.Question != delegate.Question || d.Ordinal != 1 || d.MaxDelegations != 1 {
					t.Fatalf("wrong durable delegation: %+v %v", d, err)
				}
				wake, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, d.DeliveryID)
				if err != nil || wake.SourceKind != domain.TaskWorkSourceDelegation || wake.SourceID != id || wake.MessageID != d.MessageID ||
					wake.MemberID != child.ID || wake.Blocked != "" {
					t.Fatalf("wrong durable delegation wake: %+v %v", wake, err)
				}
				want = d
				wantPayload = mutationPayload(p.AssignmentID, scope.Claim.TurnID, owner.AttemptID, child.ID, delegate.Question, "false", "[]")
				added = append(added, "task_work_delegations", "task_work_deliveries", "deliveries", "conversations")
			} else {
				var h domain.TaskWorkHandback
				var stamp string
				if err := s.db.QueryRow(`SELECT `+taskWorkParentColumns+`,p.id,p.author_turn_id,p.author_attempt_id,p.coordinator_member_id,p.summary,p.notice_message_id,p.created_at
 FROM task_work_handbacks p JOIN coordination_assignment_dispatch origin ON origin.assignment_id=p.assignment_id WHERE p.group_id=? AND p.id=?`, p.GroupID, id).
					Scan(append(taskWorkParentDest(&h.TaskWorkParent), &h.ID, &h.AuthorTurnID, &h.AuthorAttemptID, &h.CoordinatorID, &h.Summary, &h.NoticeMessageID, &stamp)...); err != nil {
					t.Fatalf("missing durable handback: %v", err)
				}
				created, err := time.Parse(timestampLayout, stamp)
				if err != nil || h.AssignmentID != p.AssignmentID || h.AuthorTurnID != scope.Claim.TurnID || h.AuthorAttemptID != owner.AttemptID ||
					h.HandbackWorkInput != handback || h.CoordinatorID != f.a {
					t.Fatalf("wrong durable handback: %+v %v", h, err)
				}
				h.CreatedAt = created
				m, err := s.GetMessage(ctx, p.GroupID, h.NoticeMessageID)
				if err != nil || m.TopicID != p.TopicID || m.SourceKind != "platform" || m.SourceID != "work:handback:"+id || len(m.Deliveries) != 0 {
					t.Fatalf("wrong durable handback notice: %+v %v", m, err)
				}
				want = h
				wantPayload = mutationPayload(p.AssignmentID, scope.Claim.TurnID, owner.AttemptID, handback.Summary)
				added = append(added, "task_work_handbacks")
			}
			if !reflect.DeepEqual(payload, wantPayload) {
				t.Fatal("durable key has the wrong payload")
			}
			after := messageSequenceSnapshot(t, s)
			for _, table := range added {
				if len(after[table]) != len(before[table])+1 {
					t.Fatalf("expected exactly one new %s row", table)
				}
				delete(before, table)
			}
			messageSequencePreserved(t, s, before)
			t.Logf("one durable effect and its exact key after reopen; conflict=%t denied=%t unknown=%t",
				errors.Is(callErr, ErrConflict), errors.Is(callErr, ErrMemberToolDenied), errors.Is(callErr, ErrTaskWorkMutationUnknown))

			requestDispatchZero(t, out, callErr, ErrTaskWorkMutationUnknown)
			var evidence interface {
				Phase() string
				CommitOutcomeUnknown() bool
				RollbackConfirmed() bool
			}
			if !errors.As(callErr, &evidence) || evidence.Phase() != "commit" || !evidence.CommitOutcomeUnknown() || evidence.RollbackConfirmed() ||
				!errors.Is(callErr, cause) || !errors.Is(callErr, tc.category) || callErr.Error() != ErrTaskWorkMutationUnknown.Error() {
				t.Fatalf("wrong commit evidence: %v", callErr)
			}
			if replay, err := invoke(s); err != nil || !reflect.DeepEqual(replay, want) {
				t.Fatalf("same-key reconciliation changed the durable receipt: %+v %v; want %+v", replay, err, want)
			}
			messageSequencePreserved(t, s, after)
		})
	}
}
