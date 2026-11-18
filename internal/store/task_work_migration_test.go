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

func TestTaskWorkLegacyV27UpgradePreservesLiveHistory(t *testing.T) {
	f := func() chatFixture {
		prior := migrations
		migrations = prior[:27]
		defer func() { migrations = prior }()
		return chatSetup(t, "user", false)
	}()
	messageSequenceLedger(t, f.s, 27)
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
