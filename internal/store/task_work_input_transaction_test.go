package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskWorkInputMutationUncertainty(t *testing.T) {
	for _, tc := range []struct{ operation, fault string }{
		{workInputAckOperation, "ack-error"}, {workInputAckOperation, "refuse"}, {workInputAckOperation, "rollback-tx-done"},
		{workInputReturnOperation, "ack-error"}, {workInputReturnOperation, "refuse"}, {workInputReturnOperation, "rollback-ack-error"},
	} {
		t.Run(tc.operation+"/"+tc.fault, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			f.s.EnableTaskWorkExecution()
			owner := chatAuthorityWorker(t, f)
			p, scope := owner.Provenance, owner.Member
			queueEvent(t, f.s, scope.Claim, provider.AcceptedEvent, "", "")
			const quote = "同时写入项目目录"
			assessor, c := newWorkInputCandidate(t, f, quote)
			proposal := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "补充原任务要求",
				Followup: &domain.ChatFollowupAssessment{Constraint: quote}}
			r, err := f.s.ApplyChatAssessment(ctx, assessor, c.ID, proposal, "记录要求。", "input-before-mutation")
			if err != nil || r.WorkInputID == "" || r.WorkDeliveryID == "" {
				t.Fatalf("missing durable input/wake: %+v %v", r, err)
			}
			original, err := f.s.GetWorkInputForMember(ctx, scope, r.WorkInputID)
			if err != nil || original.AfterConstraint != quote || original.Consumption != nil || original.Return != nil {
				t.Fatalf("missing original input projection: %+v %v", original, err)
			}
			var claims int
			if err := f.s.db.QueryRow(`SELECT count(*) FROM resource_claims WHERE turn_id=?`, scope.Claim.TurnID).Scan(&claims); err != nil || claims == 0 {
				t.Fatalf("fixture lacks original occupancy: %d %v", claims, err)
			}
			before := messageSequenceSnapshot(t, f.s)
			const key, reason = "Input/Exact:1", "原授权不允许修改目录。"
			id := original.ID
			handBack := tc.operation == workInputReturnOperation
			invoke := func(s *Store, changed bool) (domain.TaskWorkInput, error) {
				inputID, why := id, reason
				if changed {
					if handBack {
						why += "另有依据。"
					} else {
						inputID += "-different"
					}
				}
				if handBack {
					return s.HandBackWorkInputWithKey(ctx, scope, inputID, why, key)
				}
				return s.AcknowledgeWorkInputWithKey(ctx, scope, inputID, key)
			}
			var gateway *Store
			var checkFence func()
			phase := "commit"
			var cause error = fmt.Errorf("PRIVATE commit fault: %w", ErrConflict)
			const fence = "UPDATE turns SET claim_token=claim_token"
			if strings.HasPrefix(tc.fault, "rollback-") {
				// Make the real in-transaction key readback fail after the effect.
				coordinationDecideExec(t, f.s, fmt.Sprintf(`CREATE TRIGGER work_input_key_fault AFTER INSERT ON mutation_idempotency
	 WHEN NEW.operation='%s' BEGIN UPDATE mutation_idempotency SET record_id='wrong' WHERE
	 (operation,actor_id,group_id,idempotency_key)=(NEW.operation,NEW.actor_id,NEW.group_id,NEW.idempotency_key); END`, tc.operation))
				phase, cause = "mutation", errors.New("PRIVATE rollback acknowledgement lost")
				if tc.fault == "rollback-tx-done" {
					cause = sql.ErrTxDone
				}
				s, observed := assignmentBoundaryStore(t, f.path, cause)
				gateway, checkFence = s, func() {
					if len(observed.events) != 1 || observed.events[0].rollbackCalls != 1 || observed.events[0].rollbackErr != nil ||
						!strings.HasPrefix(observed.events[0].firstSQL, fence) {
						t.Fatalf("expected the real scope fence and SQLite rollback, got %+v", observed.events)
					}
				}
			} else {
				s, observed := coordinationDecideWrapped(t, f.path, tc.fault, cause)
				gateway, checkFence = s, func() {
					if len(observed.begins) != 1 || len(observed.queries) == 0 || !strings.HasPrefix(observed.queries[0], fence) {
						t.Fatal("expected exactly one transaction beginning with the real scope fence")
					}
				}
			}
			out, err := invoke(gateway, false)
			requestDispatchZero(t, out, err, ErrTaskWorkMutationUnknown)
			wrapped := fmt.Errorf("work input: %w", err)
			if !errors.Is(wrapped, ErrTaskWorkMutationUnknown) || !errors.Is(wrapped, ErrConflict) || !errors.Is(wrapped, cause) || err.Error() != ErrTaskWorkMutationUnknown.Error() {
				t.Fatalf("unknown must take precedence over the preserved business cause: %v", wrapped)
			}
			var evidence interface {
				Phase() string
				CommitOutcomeUnknown() bool
				RollbackConfirmed() bool
			}
			if !errors.As(wrapped, &evidence) || evidence.Phase() != phase || evidence.CommitOutcomeUnknown() != (phase == "commit") || evidence.RollbackConfirmed() {
				t.Fatalf("wrong transaction evidence: %+v", evidence)
			}
			checkFence()
			for _, s := range []*Store{gateway, f.s} {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}

			observer := queueHandle(t, f.path, false)
			persisted := tc.fault == "ack-error"
			if !persisted {
				messageSequencePreserved(t, observer, before)
				if phase == "mutation" {
					coordinationDecideExec(t, observer, "DROP TRIGGER work_input_key_fault")
				}
				if retry, err := invoke(observer, false); err != nil || retry.ID != id {
					t.Fatalf("explicit same-key retry after no commit: %+v %v", retry, err)
				}
			}
			page, err := observer.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{})
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != id {
				t.Fatalf("wrong durable input history: %+v %v", page, err)
			}
			want := page.Items[0]
			projection := want
			projection.Consumption, projection.Return = nil, nil
			if !reflect.DeepEqual(projection, original) {
				t.Fatalf("effect changed the projected input: %+v", projection)
			}
			added := []string{"mutation_idempotency", "task_work_input_consumptions", "task_constraint_impacts", "messages", "message_log"}
			payload := mutationPayload(id, p.AssignmentID)
			if handBack {
				added = []string{"mutation_idempotency", "task_work_input_returns", "task_constraint_impacts", "messages", "message_log"}
				payload = mutationPayload(id, p.AssignmentID, reason)
				h := want.Return
				if h == nil || h.Reason != reason || h.AttemptID != owner.AttemptID || h.TurnID != scope.Claim.TurnID || want.Consumption != nil ||
					h.Disposition != domain.TaskWorkInputReturnSeparateAuthorizationRequired || h.PolicyRevision != f.policy.Revision || h.PolicySourceID != f.policy.ConfigurationSourceID {
					t.Fatalf("wrong return: %+v", want)
				}
				if message, err := observer.GetMessage(ctx, p.GroupID, h.NoticeMessageID); err != nil || message.TopicID != p.TopicID || len(message.Deliveries) != 0 {
					t.Fatalf("return changed topic or woke work: %+v %v", message, err)
				}
			} else if ack := want.Consumption; ack == nil || ack.AttemptID != owner.AttemptID || ack.TurnID != scope.Claim.TurnID || want.Return != nil {
				t.Fatalf("wrong acknowledgement: %+v", want)
			}
			var recordID string
			var savedPayload []byte
			if err := observer.db.QueryRow(`SELECT record_id,payload FROM mutation_idempotency WHERE (operation,actor_id,group_id,idempotency_key)=(?,?,?,?)`,
				tc.operation, p.MemberID, p.GroupID, key).Scan(&recordID, &savedPayload); err != nil || recordID != id || !reflect.DeepEqual(savedPayload, payload) {
				t.Fatalf("wrong durable key or payload: record=%q %v", recordID, err)
			}
			after := messageSequenceSnapshot(t, observer)
			for _, table := range added {
				if len(after[table]) != len(before[table])+1 {
					t.Fatalf("expected exactly one new %s row", table)
				}
				delete(before, table)
			}
			messageSequencePreserved(t, observer, before)
			if replay, err := invoke(observer, false); err != nil || !reflect.DeepEqual(replay, want) {
				t.Fatalf("same-key replay changed the durable receipt: %+v %v", replay, err)
			}
			out, err = invoke(observer, true)
			requestDispatchZero(t, out, err, ErrConflict)
			if errors.Is(err, ErrTaskWorkMutationUnknown) || !errors.As(err, &evidence) || evidence.Phase() != "mutation" || !evidence.RollbackConfirmed() {
				t.Fatalf("confirmed changed-payload conflict lost its classification: %v", err)
			}
			messageSequencePreserved(t, observer, after)
			impact := impacts(t, observer, p.GroupID, p.TaskID)
			if len(impact) != 1 || impact[0].Source.InputID != id || handBack != (impact[0].Decision != nil) {
				t.Fatalf("reconciliation did not preserve precisely one scoped effect: %+v", impact)
			}
			t.Logf("phase=%s unknown before reconciliation; effect already durable=%t; exact key and original occupancy preserved", phase, persisted)
		})
	}
}
