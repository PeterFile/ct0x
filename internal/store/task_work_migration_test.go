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
