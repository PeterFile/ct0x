package store

import (
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
)

func TestMigrationsV45V46TaskImpactAndDecisionOrder(t *testing.T) {
	if got := slices.Index(migrations, taskImpactSchema) + 1; got != 45 {
		t.Fatalf("taskImpactSchema migration is v%d, want v45", got)
	}
	if got := slices.Index(migrations, workInputDecisionSchema) + 1; got != 46 {
		t.Fatalf("workInputDecisionSchema migration is v%d, want v46", got)
	}
}

func TestMigrationsV45V46UpgradeKeepsEarlierReturn(t *testing.T) {
	public := migrations
	if len(public) < 46 {
		t.Fatalf("public migrations stop at v%d, want at least v46", len(public))
	}
	t.Cleanup(func() { migrations = public })
	migrations = public[:44]
	f := chatSetup(t, "user", false)
	owner := chatAuthorityWorker(t, f)
	historical, _ := impactInput(t, f, "另需写入目录。", "")
	pending, notice := impactInput(t, f, "另需更新第二份目录。", "")
	const reason, key = "目录写入不在原授权内。", "pre-v45-work-input"
	old, err := f.s.HandBackWorkInputWithKey(ctx, owner.Member, historical.WorkInputID, reason, key)
	if err != nil || old.ID != historical.WorkInputID || old.Return == nil ||
		old.Return.Disposition != domain.TaskWorkInputReturnSeparateAuthorizationRequired {
		t.Fatalf("pre-v45 WorkInput return failed: %+v, %v", old, err)
	}
	messageSequenceLedger(t, f.s, 44)

	migrations = public
	upgraded := queueHandle(t, f.path, false)
	messageSequenceLedger(t, upgraded, len(public))

	replay, err := upgraded.HandBackWorkInputWithKey(ctx, owner.Member, old.ID, reason, key)
	if err != nil || !reflect.DeepEqual(replay, old) {
		t.Fatalf("v44 return changed after v46 upgrade: %+v, %v", replay, err)
	}
	page, err := upgraded.ListPendingTaskDecisions(ctx, ListOptions{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("upgrade or replay enrolled a historical return: %+v, %v", page, err)
	}
	if impacts := messageSequenceTableRows(t, upgraded, "task_constraint_impacts"); len(impacts) != 0 {
		t.Fatalf("upgrade or replay backfilled historical impacts: %v", impacts)
	}

	current, err := upgraded.HandBackWorkInputWithKey(ctx, owner.Member, pending.WorkInputID, reason, "post-v46-work-input")
	if err != nil || current.ID != pending.WorkInputID || current.Return == nil {
		t.Fatalf("post-v46 WorkInput return failed: %+v, %v", current, err)
	}
	page, err = upgraded.ListPendingTaskDecisions(ctx, ListOptions{})
	if err != nil || len(page.Items) != 1 || page.NextCursor != nil {
		t.Fatalf("expected only the post-v46 decision: %+v, %v", page, err)
	}
	p := owner.Provenance
	dispatch, err := upgraded.GetDispatchRequest(ctx, p.GroupID, p.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	assertReturnedDecisionScope(t, page.Items[0], current, notice, dispatch.Request.AuthorityGeneration)
	dispatchAssertFK(t, upgraded)
	assertSQLiteQuickCheckOK(t, upgraded)
}
