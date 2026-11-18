package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskWorkFrozenPrefixAndRegistration(t *testing.T) {
	// v1–22 are checked by the existing V23 frozen-prefix test. These are
	// the remaining SQL hashes from staged B2 (3ce69a0), not the pilot order.
	frozen := []string{
		"5c6e7109292e04fb9a1e9ddffd806335fe82034e20b56adb8ee982dc668249f8",
		"1f222b7416870fb91b69655cb21eff30596835dc939b755ecebdc066a74a6416",
		"b396002751673ff784499c6f4d6810c2b4fe545c64bef7f891765d9c06c45946",
		"8551c57ed3e74829db7f5ccbdb3138e708b9da34a1854ab2679f4d5ca0c35273",
		"787f414c06ff802d82c6ba56c503cfd8c4575da9faad34d876a3f1c102919e0f",
	}
	if len(migrations) < 29 || migrations[27] != taskWorkInputSchema || migrations[28] != taskWorkDeliverySchema {
		t.Fatalf("expected B1 input/delivery at v28/v29, migrations=%d", len(migrations))
	}
	for i, want := range frozen {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations[22+i]))); got != want {
			t.Fatalf("staged migration v%d changed: %s", 23+i, got)
		}
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations[28]))); got != "a3d41f35bbf678377e79fd36b5ec2d088fe98f376f51323b82ad4877af5676bb" {
		t.Fatalf("typed delivery migration v29 changed: %s", got)
	}
}

func taskWorkChatAtPrefix(t *testing.T, version int) chatFixture {
	t.Helper()
	prior := migrations
	migrations = prior[:version]
	defer func() { migrations = prior }()
	f := chatSetup(t, "user", false)
	messageSequenceLedger(t, f.s, version)
	return f
}

func TestTaskWorkLegacyV27UpgradePreservesLiveHistory(t *testing.T) {
	f := taskWorkChatAtPrefix(t, 27)
	worker := chatAuthorityWorker(t, f)
	p := worker.Provenance
	history, err := f.s.GetChatAdmission(ctx, p.GroupID, p.TaskID)
	if err != nil || history.TaskID != p.TaskID || !history.AuthorityEvidenceVerified {
		t.Fatalf("original admission proof: %+v %v", history, err)
	}
	if got, err := f.s.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{}); err != nil || got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("legacy prefix did not expose an empty input history: %+v %v", got, err)
	}
	if _, err := f.s.ResolveTaskWorkDelivery(ctx, p.GroupID, p.DeliveryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy prefix classified old work as a wake: %v", err)
	}
	scope, candidate := newWorkInputCandidate(t, f, "请继续原任务。")
	proposal := f.input // Even a legacy request disposition with explicit followup must not create a Task.
	proposal.Followup = &domain.ChatFollowupAssessment{}
	r, err := f.s.ApplyChatAssessment(ctx, scope, candidate.ID, proposal, "核对原任务。", "legacy-input")
	chatAssertNotAdmitted(t, r, err, "followup_unavailable")
	queueEvent(t, f.s, worker.Member.Claim, provider.AcceptedEvent, "", "")
	unknown := queueReceipt(provider.OutcomeUnknown)
	unknown.DirectChildReaped = false
	taskWorkTestReceipt(t, f.s, worker.Member, unknown, domain.TurnStateUnknown)
	before := messageSequenceSnapshot(t, f.s)
	upgraded := queueHandle(t, f.path, false)
	messageSequenceLedger(t, upgraded, len(migrations))
	messageSequencePreserved(t, upgraded, before)
	if got, err := upgraded.GetChatAdmission(ctx, p.GroupID, p.TaskID); err != nil || !reflect.DeepEqual(got, history) {
		t.Fatalf("B1 rewrote original work source: %+v %v", got, err)
	}
	dispatchAssertFK(t, upgraded)
}

func TestTaskWorkInputSchemaDisagreementFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		damage  string
	}{
		{"missing-v28-with-v29", 29, "DELETE FROM schema_migrations WHERE version=28"},
		{"missing-v28-with-tables", 28, "DELETE FROM schema_migrations WHERE version=28"},
		{"missing-mandatory-table", 28, "DROP TABLE task_work_input_returns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := taskWorkChatAtPrefix(t, tc.version)
			worker := chatAuthorityWorker(t, f)
			p := worker.Provenance
			scope, c := newWorkInputCandidate(t, f, "继续任务 <"+p.TaskID+">，注明来源。")
			r, err := f.s.ApplyChatAssessmentDormant(ctx, scope, c.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
				Reason: "记录待处理要求", Followup: &domain.ChatFollowupAssessment{TaskID: p.TaskID, Constraint: "注明来源"}}, "记录补充。", "pending")
			if err != nil || r.WorkInputID == "" || r.TaskID != p.TaskID || !r.WorkInputPending {
				t.Fatalf("expected a genuine pending input: %+v %v", r, err)
			}
			result := AssignmentResultInput{Summary: "文本交付。"}
			if _, err := f.s.SubmitAssignmentResult(ctx, worker, result, "fresh-result"); !errors.Is(err, ErrConflict) {
				t.Fatalf("fixture lacks an outstanding-input fence: %v", err)
			}
			coordinationDecideExec(t, f.s, tc.damage)
			before := messageSequenceSnapshot(t, f.s)
			t.Run("history", func(t *testing.T) {
				got, err := f.s.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{})
				if !errors.Is(err, errWorkInputSchemaMismatch) || got.Items != nil || got.NextCursor != nil {
					t.Fatalf("history did not detect inconsistent schema: %+v %v", got, err)
				}
			})
			t.Run("submit", func(t *testing.T) {
				got, err := f.s.SubmitAssignmentResult(ctx, worker, result, "fresh-result")
				if !errors.Is(err, errWorkInputSchemaMismatch) || got.ID != "" {
					t.Fatalf("fresh submission did not detect inconsistent schema: %+v %v", got, err)
				}
			})
			t.Run("request-work", func(t *testing.T) {
				request := domain.RequestWorkRequest{TopicID: c.TopicID, MessageID: c.MessageID, Mode: domain.RoutingDecisionCreateNew,
					Reason: "同一来源", NewTask: &domain.NewTaskRequest{Title: f.input.Title, Acceptance: f.input.Acceptance}}
				got, err := f.s.RequestWork(ctx, p.GroupID, request, "same-source")
				if !errors.Is(err, errWorkInputSchemaMismatch) || got.Task.ID != "" {
					t.Fatalf("same-source RequestWork did not detect inconsistent schema: %+v %v", got, err)
				}
			})
			messageSequencePreserved(t, f.s, before)
		})
	}

	t.Run("later-marker-alone", func(t *testing.T) {
		f := taskWorkChatAtPrefix(t, 29)
		p := chatAuthorityWorker(t, f).Provenance
		// Leave the real v29 registry entry but remove all v28/v29 structures.
		coordinationDecideExec(t, f.s, `DROP TABLE task_work_continuations;
		 DROP TABLE task_work_deliveries;
		 DROP TABLE task_work_input_returns;
		 DROP TABLE task_work_input_consumptions;
		 DROP TABLE task_work_inputs;
		 DROP TABLE chat_followup_routes;
		 DROP INDEX task_coordination_followup_thread;
		 DELETE FROM schema_migrations WHERE version=28`)
		if got, err := f.s.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{}); !errors.Is(err, errWorkInputSchemaMismatch) || got.Items != nil {
			t.Fatalf("later migration marker was treated as a genuine v27 prefix: %+v %v", got, err)
		}
	})
}
