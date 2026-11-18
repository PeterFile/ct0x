package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestTaskWorkInputEffectKeys(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	scope, p := worker.Member, worker.Provenance
	coordinator := taskWorkTestCoordinator(t, f.s, p.GroupID, p.AssignmentID)
	taskWorkTestReceipt(t, f.s, coordinator, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	var ids []string
	var assessor MemberToolScope
	for _, text := range []string{"注明摘要来源", "同时写入项目目录", "也请参考原文附注"} {
		s, c := newWorkInputCandidate(t, f, text)
		r, err := f.s.ApplyChatAssessmentDormant(ctx, s, c.ID, domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork,
			Reason: "补充原任务"}, "已记录后续要求。", "effect-fixture")
		if err != nil || r.WorkInputID == "" {
			t.Fatalf("record input: %+v %v", r, err)
		}
		ids = append(ids, r.WorkInputID)
		assessor = s
	}
	ackID, returnID, pendingID := ids[0], ids[1], ids[2]
	const key = "Effect/Exact:1"
	const reason = "原授权只允许在本话题交付，不允许写入目录"
	conflict := func(got domain.TaskWorkInput, err error) {
		t.Helper()
		if !errors.Is(err, ErrConflict) || got != (domain.TaskWorkInput{}) {
			t.Fatalf("expected conflict without a receipt, got %+v %v", got, err)
		}
	}

	before := messageSequenceSnapshot(t, f.s)
	if got, err := f.s.AcknowledgeWorkInputWithKey(ctx, scope, ackID, ""); !errors.Is(err, ErrInvalid) || got.ID != "" {
		t.Fatalf("WithKey accepted an empty key: %+v %v", got, err)
	}
	if got, err := f.s.HandBackWorkInputWithKey(ctx, scope, returnID, reason, "x,y"); !errors.Is(err, ErrInvalid) || got.ID != "" {
		t.Fatalf("WithKey accepted a malformed key: %+v %v", got, err)
	}
	coordinationDecideExec(t, f.s, `CREATE TEMP TRIGGER ignore_work_ack BEFORE INSERT ON task_work_input_consumptions BEGIN SELECT RAISE(IGNORE); END`)
	conflict(f.s.AcknowledgeWorkInputWithKey(ctx, scope, ackID, key))
	coordinationDecideExec(t, f.s, `DROP TRIGGER ignore_work_ack`)
	messageSequencePreserved(t, f.s, before)

	ack, err := f.s.AcknowledgeWorkInputWithKey(ctx, scope, ackID, key)
	if err != nil || ack.ID != ackID || ack.Consumption == nil || ack.Consumption.AttemptID != worker.AttemptID || ack.Consumption.TurnID != scope.Claim.TurnID {
		t.Fatalf("explicit keyed acknowledgement: %+v %v", ack, err)
	}
	afterAck := messageSequenceSnapshot(t, f.s)
	if again, err := f.s.AcknowledgeWorkInputWithKey(ctx, scope, ackID, key); err != nil || !reflect.DeepEqual(again, ack) {
		t.Fatalf("confirmation replay changed its receipt: %+v %v", again, err)
	}
	conflict(f.s.AcknowledgeWorkInputWithKey(ctx, scope, returnID, key))
	conflict(f.s.HandBackWorkInputWithKey(ctx, scope, ackID, reason, key))
	messageSequencePreserved(t, f.s, afterAck)

	// Missing notice coverage or a mismatched ledger row must roll back both.
	for _, fault := range []string{
		`BEFORE INSERT ON message_log BEGIN SELECT RAISE(IGNORE); END`,
		`AFTER INSERT ON mutation_idempotency WHEN NEW.operation='hand_back_work_input'
		 BEGIN UPDATE mutation_idempotency SET record_id='other' WHERE operation=NEW.operation; END`,
	} {
		coordinationDecideExec(t, f.s, `CREATE TEMP TRIGGER work_return_fault `+fault)
		conflict(f.s.HandBackWorkInputWithKey(ctx, scope, returnID, reason, key))
		coordinationDecideExec(t, f.s, `DROP TRIGGER work_return_fault`)
		messageSequencePreserved(t, f.s, afterAck)
	}

	returned, err := f.s.HandBackWorkInputWithKey(ctx, scope, returnID, reason, key)
	if err != nil || returned.ID != returnID || returned.Consumption != nil || returned.Return == nil || returned.Return.Reason != reason ||
		returned.Return.MemberID != p.MemberID || returned.Return.TurnID != scope.Claim.TurnID || returned.Return.AttemptID != worker.AttemptID || returned.Return.NoticeMessageID == "" {
		t.Fatalf("explicit keyed handback: %+v %v", returned, err)
	}
	afterReturn := messageSequenceSnapshot(t, f.s)
	if again, err := f.s.HandBackWorkInputWithKey(ctx, scope, returnID, reason, key); err != nil || !reflect.DeepEqual(again, returned) {
		t.Fatalf("handback replay changed its receipt: %+v %v", again, err)
	}
	conflict(f.s.HandBackWorkInputWithKey(ctx, scope, returnID, "不同依据", key))
	conflict(f.s.HandBackWorkInputWithKey(ctx, scope, returnID, "不同依据", "alias"))
	conflict(f.s.AcknowledgeWorkInputWithKey(ctx, scope, returnID, "alias"))
	if got, err := f.s.AcknowledgeWorkInputWithKey(ctx, assessor, ackID, key); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
		t.Fatalf("an unrelated live claim could rebind the effect: %+v %v", got, err)
	}
	messageSequencePreserved(t, f.s, afterReturn)
	if again, err := f.s.HandBackWorkInputWithKey(ctx, scope, returnID, reason, "alias"); err != nil || !reflect.DeepEqual(again, returned) {
		t.Fatalf("new key replaced the source's sole handback: %+v %v", again, err)
	}
	delete(afterReturn, "mutation_idempotency") // The alias may add only a key, not a second notice or fact.
	messageSequencePreserved(t, f.s, afterReturn)
	rows := messageSequenceRows(t, f.s.db, `SELECT operation,actor_id,group_id,idempotency_key,payload,record_id FROM mutation_idempotency
	 WHERE operation IN ('acknowledge_work_input','hand_back_work_input') ORDER BY operation,idempotency_key`)
	want := [][]any{
		{workInputAckOperation, p.MemberID, p.GroupID, key, mutationPayload(ackID, p.AssignmentID), ackID},
		{workInputReturnOperation, p.MemberID, p.GroupID, key, mutationPayload(returnID, p.AssignmentID, reason), returnID},
		{workInputReturnOperation, p.MemberID, p.GroupID, "alias", mutationPayload(returnID, p.AssignmentID, reason), returnID},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("wrong effect key scopes or payloads: %+v", rows)
	}

	result := AssignmentResultInput{Summary: "在原话题交付摘要，未写入目录。"}
	if got, err := f.s.SubmitAssignmentResult(ctx, worker, result, "original-scope"); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("handback bypassed a different unresolved input: %+v %v", got, err)
	}
	if _, err := f.s.AcknowledgeWorkInput(ctx, scope, pendingID); err != nil {
		t.Fatal(err)
	}
	sub, err := f.s.SubmitAssignmentResult(ctx, worker, result, "original-scope")
	if err != nil || sub.ID == "" || sub.TaskID != p.TaskID || sub.TaskRevision != p.TaskRevision || sub.AttemptID != worker.AttemptID {
		t.Fatalf("a returned input blocked submission of the original scope: %+v %v", sub, err)
	}
	page, err := f.s.ListTaskWorkInputs(ctx, p.GroupID, p.TaskID, ListOptions{})
	if err != nil || len(page.Items) != 3 || !slices.ContainsFunc(page.Items, func(i domain.TaskWorkInput) bool { return reflect.DeepEqual(i, returned) }) {
		t.Fatalf("submission changed the declined input's handling fact: %+v %v", page, err)
	}
}
