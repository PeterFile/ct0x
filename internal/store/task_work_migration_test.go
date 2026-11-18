package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskWorkFrozenPrefixAndRegistration(t *testing.T) {
	// v1–22 are checked by the existing V23 frozen-prefix test. The tail
	// uses the actual B2/B1 registry in 138ac12, not the staged pilot order.
	frozen := []string{
		"5c6e7109292e04fb9a1e9ddffd806335fe82034e20b56adb8ee982dc668249f8",
		"1f222b7416870fb91b69655cb21eff30596835dc939b755ecebdc066a74a6416",
		"b396002751673ff784499c6f4d6810c2b4fe545c64bef7f891765d9c06c45946",
		"8551c57ed3e74829db7f5ccbdb3138e708b9da34a1854ab2679f4d5ca0c35273",
		"787f414c06ff802d82c6ba56c503cfd8c4575da9faad34d876a3f1c102919e0f",
		"e88227a0666bb09ad6f67c60579eec4f910cefd47fe60b3135c03a6f183e25f8",
		"a3d41f35bbf678377e79fd36b5ec2d088fe98f376f51323b82ad4877af5676bb",
	}
	if len(migrations) < 30 || taskWorkReportVersion != 30 || migrations[27] != taskWorkInputSchema ||
		migrations[28] != taskWorkDeliverySchema || migrations[29] != taskWorkReportSchema {
		t.Fatalf("expected B1 input/delivery at v28/v29 and B3 reports at v30, migrations=%d", len(migrations))
	}
	for i, want := range frozen {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations[22+i]))); got != want {
			t.Fatalf("staged migration v%d changed: %s", 23+i, got)
		}
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
	// Stop at the B1 delivery migration; v30 is exercised separately with live v29 work.
	const b1Version = 29
	upgraded := func() *Store {
		prior := migrations
		migrations = prior[:b1Version]
		defer func() { migrations = prior }()
		return queueHandle(t, f.path, false)
	}()
	messageSequenceLedger(t, upgraded, b1Version)
	messageSequencePreserved(t, upgraded, before)
	if got, err := upgraded.GetChatAdmission(ctx, p.GroupID, p.TaskID); err != nil || !reflect.DeepEqual(got, history) {
		t.Fatalf("B1 rewrote original work source: %+v %v", got, err)
	}
	dispatchAssertFK(t, upgraded)
}

func TestB3HistoricalV29UpgradePreservesTypedWork(t *testing.T) {
	f := func() chatFixture {
		prior := migrations
		migrations = prior[:29]
		defer func() { migrations = prior }()
		return chatSetup(t, "user", false)
	}()
	old := f.s
	messageSequenceLedger(t, old, 29)
	old.EnableTaskWorkExecution()
	worker := chatAuthorityWorker(t, f)
	p := worker.Provenance
	refusal := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
	co := taskWorkTestCoordinator(t, old, f.groupID, p.AssignmentID)
	taskWorkTestReceipt(t, old, co, refusal, domain.TurnStateNotStarted)
	r1, d1 := taskWorkTestFollowup(t, f, "续办原任务，保留旧格式。")
	taskWorkTestReceipt(t, old, worker.Member, refusal, domain.TurnStateNotStarted)
	prior := taskWorkTestClaim(t, old, d1, "v29-owner-held")
	a1, err := old.BindTaskWorkContinuation(ctx, prior)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := old.AcknowledgeWorkInputWithKey(ctx, prior, r1.WorkInputID, "v29-ack")
	if err != nil || ack.Consumption == nil || ack.Consumption.AttemptID != a1.ID {
		t.Fatalf("v29 handling evidence: %+v %v", ack, err)
	}
	_, d2 := taskWorkTestFollowup(t, f, "还有一项补充。")
	queueEvent(t, old, prior.Claim, provider.AcceptedEvent, "", "")
	unknown := queueReceipt(provider.OutcomeUnknown)
	unknown.DirectChildReaped = false
	taskWorkTestReceipt(t, old, prior, unknown, domain.TurnStateUnknown)
	foreign := queueDelivery(t, old, f.otherID, f.foreign, "v29-resource-competitor")
	before := messageSequenceSnapshot(t, old)
	const tables = `SELECT name,sql FROM sqlite_schema WHERE type='table' AND name IN ('task_work_deliveries','task_work_continuations') ORDER BY name`
	oldDDL := messageSequenceRows(t, old.db, tables)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s := queueHandle(t, f.path, false)
	messageSequenceLedger(t, s, taskWorkReportVersion)
	messageSequencePreserved(t, s, before)
	if got := messageSequenceRows(t, s.db, tables); !reflect.DeepEqual(got, oldDDL) {
		t.Fatalf("v30 rebuilt v29 tables: %+v", got)
	}
	taskWorkTestPeek(t, s, d1, "already_claimed")
	taskWorkTestPeek(t, s, d2, "task_work_execution_disabled")
	s.EnableTaskWorkExecution()
	taskWorkTestPeek(t, s, d2, "prior_turn_unsettled")
	claim := ClaimTurnInput{GroupID: f.otherID, DeliveryID: foreign.ID, MaxActive: 8, ResourceKeys: []string{"v29-owner-held"}}
	if turn, err := s.ClaimTurn(ctx, claim); !errors.Is(err, ErrTurnBusy) || turn.ID != "" {
		t.Fatalf("v30 lost unknown v29 resource: %+v %v", turn, err)
	}
	messageSequencePreserved(t, s, before)
	taskWorkTestReceipt(t, s, prior, queueReceipt(provider.OutcomeAborted), domain.TurnStateAborted)
	next := taskWorkTestClaim(t, s, d2)
	a2, err := s.BindTaskWorkContinuation(ctx, next)
	if err != nil || a2.ID == a1.ID || a2.TaskID != p.TaskID || a2.TurnID != next.Claim.TurnID {
		t.Fatalf("lost v29 continuation ownership: %+v %v", a2, err)
	}
	after := messageSequenceSnapshot(t, s)
	if got, err := s.AcknowledgeWorkInputWithKey(ctx, next, r1.WorkInputID, "v29-ack"); err != nil || !reflect.DeepEqual(got, ack) {
		t.Fatalf("v29 key/receipt no longer replayable: %+v %v", got, err)
	}
	if page, err := s.ListTaskWorkReports(ctx, p.GroupID, p.TaskID, ListOptions{}); err != nil || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("history invented B3 reports: %+v %v", page, err)
	}
	messageSequencePreserved(t, s, after)
	dispatchAssertFK(t, s)
}

func TestTaskWorkAppliedSchemaAndResolverPlans(t *testing.T) {
	f := taskWorkChatAtPrefix(t, 29)
	p := chatAuthorityWorker(t, f).Provenance
	for _, tc := range []struct {
		name, query string
		args        []any
		searches    []string
	}{
		{"prefix", workInputPrefixCount, []any{taskWorkInputVersion}, []string{"schema_migrations USING INTEGER PRIMARY KEY (rowid>? AND rowid<?)"}},
		{"shape", workInputShapeProbe, nil, []string{
			"task_coordination_followup_thread (group_id=? AND topic_id=? AND task_id=?)",
			"task_work_inputs_constraints (group_id=? AND task_id=? AND task_revision=?)",
			"task_work_inputs_history (group_id=? AND task_id=? AND created_at=? AND id=?)",
			"task_work_inputs_inbox (assignment_id=? AND task_revision=?)",
		}},
		{"thread", `SELECT a.id` + chatFollowupAssignments + ` ORDER BY tc.task_id`, []any{p.GroupID, p.TopicID, int64(999)}, []string{
			"task_coordination_followup_thread (group_id=? AND topic_id=?)",
		}},
		{"target", `SELECT a.id` + chatFollowupAssignments + ` AND tc.task_id=?`, []any{p.GroupID, p.TopicID, int64(999), p.TaskID}, []string{
			"task_coordination_followup_thread (group_id=? AND topic_id=? AND task_id=?)",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := resolutionPlan(t, f.s, tc.query, tc.args...)
			for line := range strings.SplitSeq(plan, "\n") {
				// The scalar output row and the keys of one named index are
				// bounded; neither is a scan of application/catalog tables.
				if line == "SCAN CONSTANT ROW" || strings.HasPrefix(line, "SCAN pragma_index_info VIRTUAL TABLE INDEX ") {
					continue
				}
				if strings.Contains(line, "sqlite_schema") || strings.Contains(line, "SCAN ") {
					t.Fatalf("expected keyed applied-path searches: %s", plan)
				}
			}
			for _, search := range tc.searches {
				if !strings.Contains(plan, search) {
					t.Fatalf("expected keyed lookup %q in %s", search, plan)
				}
			}
		})
	}
}

func TestTaskWorkLegacyV27OrdinarySubmission(t *testing.T) {
	f := taskWorkChatAtPrefix(t, 27)
	worker := chatAuthorityWorker(t, f)
	p := worker.Provenance
	protected := map[string][][]any{
		"turns":           messageSequenceTableRows(t, f.s, "turns"),
		"resource_claims": messageSequenceTableRows(t, f.s, "resource_claims"),
	}
	got, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "文本交付。"}, "legacy-result")
	if err != nil || got.ID == "" || got.TaskID != p.TaskID || got.AttemptID != worker.AttemptID {
		t.Fatalf("v27 failed to persist an ordinary result: %+v %v", got, err)
	}
	if saved, err := f.s.GetTaskSubmission(ctx, p.GroupID, p.TaskID, got.ID); err != nil || !reflect.DeepEqual(saved, got) {
		t.Fatalf("legacy result did not survive readback: %+v %v", saved, err)
	}
	messageSequencePreserved(t, f.s, protected)
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
		{"missing-v27-in-v29", 29, "DELETE FROM schema_migrations WHERE version=27"},
		{"missing-thread-index", 28, "DROP INDEX task_coordination_followup_thread"},
		{"wrong-thread-index-keys", 28, `DROP INDEX task_coordination_followup_thread;
			CREATE INDEX task_coordination_followup_thread ON task_coordination(current_mandate_id)`},
		{"wrong-return-shape", 28, "DROP TABLE task_work_input_returns; CREATE TABLE task_work_input_returns(input_id TEXT PRIMARY KEY NOT NULL)"},
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
			freshScope, fresh := newWorkInputCandidate(t, f, "整理独立的会议议程。")
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
			t.Run("chat-admission", func(t *testing.T) {
				got, err := f.s.ApplyChatAssessment(ctx, freshScope, fresh.ID, f.input, "登记独立工作。", "fresh-admission")
				if tc.name == "missing-v27-in-v29" {
					// The earlier candidate policy read fails on v27 and the
					// tool boundary intentionally hides its internal cause.
					if err == nil || err.Error() != "验证聊天候选失败" || got.ID != "" {
						t.Fatalf("missing v27 did not fail at candidate policy read: %+v %v", got, err)
					}
					return
				}
				if !errors.Is(err, errWorkInputSchemaMismatch) || got.ID != "" {
					t.Fatalf("fresh chat admission did not detect inconsistent schema: %+v %v", got, err)
				}
			})
			messageSequencePreserved(t, f.s, before)
		})
	}

	t.Run("legacy-gap", func(t *testing.T) {
		f := taskWorkChatAtPrefix(t, 27)
		p := chatAuthorityWorker(t, f).Provenance
		coordinationDecideExec(t, f.s, "DELETE FROM schema_migrations WHERE version=26")
		if got, err := f.s.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{}); !errors.Is(err, errWorkInputSchemaMismatch) || got.Items != nil {
			t.Fatalf("incomplete ledger was treated as a genuine v27 prefix: %+v %v", got, err)
		}
	})

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
