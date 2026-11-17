package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func chatWorkspaceAdmitted(t *testing.T) (chatFixture, domain.ChatAssessmentReceipt) {
	t.Helper()
	f := chatWorkspaceSetup(t, "user", false)
	chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
	r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "按授权范围落实。", "workspace-admit")
	if err != nil || r.TaskID == "" || r.WorkspaceScope == nil {
		t.Fatalf("workspace admission: %+v %v", r, err)
	}
	return f, r
}

func TestChatWorkspaceAtomicFailuresRollbackAllWrites(t *testing.T) {
	for _, fault := range []string{"task", "scope-abort", "scope-ignore", "grant-ignore", "dispatch", "receipt", "commit"} {
		t.Run(fault, func(t *testing.T) {
			f := chatWorkspaceSetup(t, "user", false)
			chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
			table, effect := "chat_workspace_scopes", "SELECT RAISE(ABORT,'injected fault');"
			switch fault {
			case "task":
				table = "tasks"
			case "scope-ignore":
				effect = "SELECT RAISE(IGNORE);"
			case "grant-ignore":
				table, effect = "chat_workspace_scope_grants", "SELECT RAISE(IGNORE);"
			case "dispatch":
				table = "coordination_dispatch_state"
			case "receipt":
				table = "chat_assessments"
			case "commit":
				coordinationDecideExec(t, f.s, `CREATE TABLE workspace_commit_parent(id TEXT PRIMARY KEY);
				 CREATE TABLE workspace_commit_fault(id TEXT REFERENCES workspace_commit_parent(id) DEFERRABLE INITIALLY DEFERRED);`)
				table, effect = "chat_assessments", "INSERT INTO workspace_commit_fault VALUES('missing');"
			}
			coordinationDecideExec(t, f.s, "CREATE TRIGGER workspace_atomic_fault BEFORE INSERT ON "+table+" BEGIN "+effect+" END")
			before := messageSequenceSnapshot(t, f.s)
			r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "未完成不应残留授权。", "atomic-workspace")
			if err == nil || !reflect.ValueOf(r).IsZero() {
				t.Fatalf("fault admitted partial work: %+v %v", r, err)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestChatWorkspaceScopeImmutableAndClosedMemberSet(t *testing.T) {
	f, r := chatWorkspaceAdmitted(t)
	for table, column := range map[string]string{"chat_workspace_policy_revisions": "revision", "chat_workspace_scopes": "task_id", "chat_workspace_scope_grants": "grant_id"} {
		for _, query := range []string{"UPDATE " + table + " SET " + column + "=" + column, "DELETE FROM " + table, "INSERT OR REPLACE INTO " + table + " SELECT * FROM " + table + " LIMIT 1"} {
			if _, err := f.s.db.Exec(query); err == nil {
				t.Fatalf("immutable scope changed: %s", query)
			}
		}
	}
	other := member(t, f.s, f.groupID)
	grant := chatWorkspaceAuthorize(t, f, f.groupID, other.ID, r.WorkspaceScope.Grants[0].Root, r.WorkspaceScope.ResourceKey, "read_write")
	if _, err := f.s.db.Exec(`INSERT INTO chat_workspace_scope_grants VALUES(?,?,?,?,?,?,?,?,19)`, r.TaskID, f.groupID, grant.ResourceKey, "local", grant.ID, grant.MemberID, grant.Root, grant.ResolvedRoot); err == nil {
		t.Fatal("committed scope gained a new grant/member after its receipt")
	}
}

// Build real legacy admission/dispatch references, then exercise v20 SQL guards
// directly. This never invents local-user provenance or commits partial evidence.
func chatWorkspacePendingSQL(t *testing.T, f chatFixture) (domain.ChatAssessmentReceipt, *sql.Tx) {
	t.Helper()
	grant, err := f.s.GetWorkspaceGrant(ctx, f.groupID, f.b)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := f.s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{TopicID: f.scope.TopicID, MessageID: f.candidate.MessageID, Mode: domain.RoutingDecisionCreateNew, Reason: "direct SQL guard fixture", NewTask: &domain.NewTaskRequest{Title: "目录变更", Acceptance: "冻结文件版本"}}, "sql-work")
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := f.s.RequestDispatch(ctx, f.groupID, admitted.Task.ID, 1, 1, "SQL guard fixture", "sql-dispatch")
	if err != nil {
		t.Fatal(err)
	}
	scope := &domain.TaskWorkspaceScope{ThreadRef: f.candidate.ThreadRef, TaskID: admitted.Task.ID, TaskRevision: 1, MandateID: admitted.Mandate.ID, CandidateID: f.candidate.ID, AssessmentID: "sql-assessment", DispatchRequestID: dispatch.ID, PolicyRevision: f.policy.Revision, ResourceKey: grant.ResourceKey, NodeID: "local", Grants: []domain.TaskWorkspaceGrant{{GrantID: grant.ID, MemberID: grant.MemberID, Root: grant.Root, ResolvedRoot: grant.ResolvedRoot, GrantSchemaVersion: 19}}}
	r := domain.ChatAssessmentReceipt{ID: scope.AssessmentID, CandidateID: f.candidate.ID, ThreadRef: f.candidate.ThreadRef, MessageID: f.candidate.MessageID, SourceSenderID: "user", AssessedBy: f.a, TurnID: f.scope.Claim.TurnID, PolicyRevision: f.policy.Revision, CandidateGeneration: f.candidate.Generation, Disposition: "request_work", Authority: "explicit_request", AuthorizationMessageID: f.candidate.MessageID, Action: domain.ChatActionWorkspaceChange, TaskID: scope.TaskID, MandateID: scope.MandateID, DispatchRequestID: dispatch.ID, NoticeMessageID: f.candidate.MessageID, Purpose: domain.ChatIntakePurpose, SourceID: f.candidate.SourceID, SourceKind: f.candidate.SourceKind, Outcome: "admitted", AuthorityEvidenceVerified: true, WorkspaceScope: scope}
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return r, tx
}

func TestChatWorkspaceSQLScopeBindsSourceAndAdmission(t *testing.T) {
	for _, scenario := range []string{"valid", "revision", "unproven-source", "wrong-root", "foreign-group", "grant-owner"} {
		t.Run(scenario, func(t *testing.T) {
			f := chatWorkspaceSetup(t, "user", false)
			chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
			if scenario == "unproven-source" {
				chatWorkspaceNextCandidate(t, &f, false)
			}
			r, tx := chatWorkspacePendingSQL(t, f)
			scope := *r.WorkspaceScope
			switch scenario {
			case "revision":
				scope.TaskRevision++
			case "wrong-root":
				scope.Grants[0].Root += "-wrong"
			case "foreign-group":
				scope.GroupID = f.otherID
			case "grant-owner":
				scope.Grants[0].MemberID = f.a
			}
			err := insertChatWorkspaceScopeInTx(ctx, tx, scope)
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatalf("SQL scope accepted %s", scenario)
			}
		})
	}
}

func TestChatWorkspaceSQLReceiptMatchesScopeAndSource(t *testing.T) {
	for _, scenario := range []string{"valid", "source", "action", "authority", "scope-root", "scope-members", "missing-scope"} {
		t.Run(scenario, func(t *testing.T) {
			f := chatWorkspaceSetup(t, "user", false)
			chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
			r, tx := chatWorkspacePendingSQL(t, f)
			if scenario != "missing-scope" {
				if err := insertChatWorkspaceScopeInTx(ctx, tx, *r.WorkspaceScope); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "source":
				r.SourceID = "forged-user-command"
			case "action":
				r.Action = domain.ChatActionTextSubmission
			case "authority":
				r.Authority = "written_preauthorization"
			case "scope-root":
				r.WorkspaceScope.Grants[0].Root += "-wrong"
			case "scope-members":
				r.WorkspaceScope.Grants = nil
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var decision string
			if err := tx.QueryRow("SELECT decision_id FROM mandates WHERE id=?", r.MandateID).Scan(&decision); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(`INSERT INTO chat_assessments VALUES(?,?,?,?,?,?,?)`, r.ID, r.CandidateID, decision, r.DispatchRequestID, r.NoticeMessageID, []byte("sql fixture"), string(data))
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatalf("SQL receipt accepted %s", scenario)
			}
		})
	}
}

func TestChatWorkspaceCannotAttachScopeToFinalizedText(t *testing.T) {
	f := chatWorkspaceSetup(t, "user", false)
	chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
	f.input.Action = domain.ChatActionTextSubmission
	r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "仅允许文本。", "final-text")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.db.Exec(`INSERT INTO chat_workspace_scopes VALUES(?,?,?,?,?,?,?,?,?,?,?)`, r.TaskID, f.groupID, f.scope.TopicID, 1, r.MandateID, f.candidate.ID, r.ID, r.DispatchRequestID, f.policy.Revision, "workspace:1:2", "local")
	if err == nil {
		t.Fatal("finalized text admission gained a workspace scope after its receipt")
	}
}

func TestChatWorkspaceBudgetSharedWithText(t *testing.T) {
	f := chatWorkspaceSetup(t, "user", false)
	chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
	for i := 0; i < 3; i++ {
		if i > 0 {
			chatWorkspaceNextCandidate(t, &f, true)
		}
		f.input.Action = domain.ChatActionWorkspaceChange
		if i == 1 {
			f.input.Action = domain.ChatActionTextSubmission
		}
		r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "共用本版准入额度。", fmt.Sprintf("shared-budget-%d", i))
		if i < 2 {
			if err != nil || r.TaskID == "" {
				t.Fatalf("item %d: %+v %v", i, r, err)
			}
		} else {
			chatAssertNotAdmitted(t, r, err, "budget_exhausted")
		}
	}
}
