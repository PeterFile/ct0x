package store

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestWorkspaceOperationV20ToV21AtomicMigration(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	if len(original) < 21 || original[20] != workspaceOperationSchemaV21 {
		t.Fatal("missing appended v21")
	}
	migrations = original[:20]
	f := chatSetup(t, "user", false)
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, chatWorkspacePolicyInput(t, f.policy.ChatPolicyRequest), 1, "workspace-policy"); err != nil {
		t.Fatal(err)
	}
	// Close the original intake before capturing a new local-user command
	// under the workspace policy. Admission itself still uses the live gates.
	seedPreV23TurnReceipt(t, f.s, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	message := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: "请按刚才约定在本群已授权目录落实文件变更，交付可核验的文件版本。", RecipientIDs: []string{f.a}}, []string{f.a})
	intake, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: intake.ID, Token: intake.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.api.CaptureChatCandidate(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	assessment := f.input
	assessment.Action = domain.ChatActionWorkspaceChange
	assessment.Title, assessment.Description, assessment.Acceptance = "更新已授权目录", "按讨论落实变更", "提交可核验的冻结文件版本"
	chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
	receipt, err := f.api.ApplyChatAssessment(ctx, scope, candidate.ID, assessment, "按授权范围落实。", "workspace-admit")
	if err != nil || receipt.TaskID == "" || receipt.WorkspaceScope == nil {
		t.Fatalf("workspace admission: %+v, %v", receipt, err)
	}
	delivery := queueDelivery(t, f.s, f.groupID, f.b, "migration-unknown")
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 8, ResourceKeys: []string{receipt.WorkspaceScope.ResourceKey}})
	if err != nil {
		t.Fatal(err)
	}
	claim := TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
	seedPreV23TurnReceipt(t, f.s, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown})
	before := messageSequenceSnapshot(t, f.s)
	objects := messageSequenceRows(t, f.s.db, assignmentSchemaQuery)
	migrations = slices.Clone(original[:21])
	migrations[20] += "\nINSERT INTO f4_missing_migration_fixture VALUES(1);"
	if err := migrate(f.s.db); err == nil || !strings.Contains(err.Error(), "迁移版本 21") {
		t.Fatalf("partial v21 migration committed: %v", err)
	}
	messageSequenceLedger(t, f.s, 20)
	messageSequencePreserved(t, f.s, before)
	if got := messageSequenceRows(t, f.s.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
		t.Fatal("failed v21 leaked schema changes")
	}
	migrations = original[:21]
	if err := migrate(f.s.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, f.s, 21)
	messageSequencePreserved(t, f.s, before)
	for _, table := range []string{"workspace_operations", "workspace_operation_receipts", "worker_artifact_publications"} {
		var n int
		if err := f.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("v21 inferred execution/publication from historical scope: %s=%d,%v", table, n, err)
		}
	}
	current := messageSequenceRows(t, f.s.db, assignmentSchemaQuery)
	for _, old := range objects {
		if !slices.ContainsFunc(current, func(got []any) bool { return reflect.DeepEqual(got, old) }) {
			t.Fatalf("v21 rewrote frozen v1–20 object: %v", old)
		}
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	messageSequenceLedger(t, reopened, 21)
	messageSequencePreserved(t, reopened, before)
	if scope, err := reopened.GetTaskWorkspaceScope(ctx, f.groupID, receipt.TaskID); err != nil || !reflect.DeepEqual(scope, *receipt.WorkspaceScope) {
		t.Fatalf("v21/reopen changed F3 scope: %+v,%v", scope, err)
	}
	if held, err := reopened.HasResourceClaim(ctx, receipt.WorkspaceScope.ResourceKey); err != nil || !held {
		t.Fatalf("v21 released unknown Turn claim: %t,%v", held, err)
	}
	dispatchAssertFK(t, reopened)
}
