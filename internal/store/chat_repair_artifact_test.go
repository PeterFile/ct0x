package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

type chatRepairLinkAPI interface {
	GetChatRepairLink(context.Context, string, string) (domain.ChatRepairLink, error)
}

func TestChatRepairLinkAndDetachedArtifactGuard(t *testing.T) {
	f, parent, review := chatRepairReviewCandidate(t, false)
	input := chatRepairInput(t, f.input, review.ID, "review_request")
	receipt, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "同范围新任务。", "repair-link")
	if err != nil || receipt.RepairLink == nil {
		t.Fatalf("missing formal repair link: %+v, %v", receipt, err)
	}
	api, ok := any(f.s).(chatRepairLinkAPI)
	if !ok {
		t.Fatal("Store lacks historical repair link reader")
	}
	for _, reader := range []chatRepairLinkAPI{api, any(queueHandle(t, f.path, false)).(chatRepairLinkAPI)} {
		got, err := reader.GetChatRepairLink(ctx, f.groupID, receipt.TaskID)
		if err != nil || !reflect.DeepEqual(got, *receipt.RepairLink) {
			t.Fatalf("repair link did not survive reopen: %+v, %v", got, err)
		}
		got, err = reader.GetChatRepairLink(ctx, f.otherID, receipt.TaskID)
		if !errors.Is(err, ErrNotFound) || got.ID != "" {
			t.Fatalf("repair link crossed group: %+v, %v", got, err)
		}
	}
	for _, query := range []string{
		"UPDATE chat_repair_links SET authorization='original_agreement' WHERE repair_task_id=?",
		"DELETE FROM chat_repair_links WHERE repair_task_id=?",
		"INSERT OR REPLACE INTO chat_repair_links SELECT * FROM chat_repair_links WHERE repair_task_id=?",
	} {
		if _, err := f.s.db.Exec(query, receipt.TaskID); err == nil {
			t.Fatalf("repair source history was mutable: %s", query)
		}
	}
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := chatRepairArtifactBaseInTx(ctx, tx, f.groupID, parent.TaskID, "", ""); err != nil {
		t.Fatalf("non-repair publication changed: %v", err)
	}
	if err := chatRepairArtifactBaseInTx(ctx, tx, f.groupID, receipt.TaskID, "new-detached-artifact", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("repair allowed detached artifact or empty expected base: %v", err)
	}
}
