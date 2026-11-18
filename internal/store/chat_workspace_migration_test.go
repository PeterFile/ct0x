package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func chatWorkspaceRead(t *testing.T, s *Store, groupID, taskID string) (domain.TaskWorkspaceScope, error) {
	t.Helper()
	api, ok := any(s).(interface {
		GetTaskWorkspaceScope(context.Context, string, string) (domain.TaskWorkspaceScope, error)
	})
	if !ok {
		t.Fatal("missing trusted GetTaskWorkspaceScope historical read")
	}
	return api.GetTaskWorkspaceScope(ctx, groupID, taskID)
}

func TestChatWorkspaceReadScopeIsolationAndReopen(t *testing.T) {
	f, r := chatWorkspaceAdmitted(t)
	for _, s := range []*Store{f.s, queueHandle(t, f.path, false)} {
		got, err := chatWorkspaceRead(t, s, f.groupID, r.TaskID)
		if err != nil || !reflect.DeepEqual(got, *r.WorkspaceScope) {
			t.Fatalf("scope read/reopen: %+v %v", got, err)
		}
		if _, err := chatWorkspaceRead(t, s, f.otherID, r.TaskID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign scope disclosed: %v", err)
		}
	}
	text := chatSetup(t, "user", false)
	r, err := text.api.ApplyChatAssessment(ctx, text.scope, text.candidate.ID, text.input, "仍为纯文本。", "text")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chatWorkspaceRead(t, text.s, text.groupID, r.TaskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("text task inferred a directory: %v", err)
	}
}

func TestChatWorkspaceGrantGuardUsesSnapshotAndActiveMember(t *testing.T) {
	for _, scenario := range []string{"worker", "assessor", "foreign-group", "inactive-worker"} {
		t.Run(scenario, func(t *testing.T) {
			f, r := chatWorkspaceAdmitted(t)
			groupID, memberID := f.groupID, f.b
			switch scenario {
			case "assessor":
				memberID = f.a
			case "foreign-group":
				groupID = f.otherID
			case "inactive-worker":
				coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.b)
			}
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			// The caller binds execution identity before consulting scope facts.
			if err := memberToolScopeInTx(ctx, tx, f.scope); err != nil {
				t.Fatal(err)
			}
			grant, err := taskWorkspaceGrantInTx(ctx, tx, groupID, r.TaskID, memberID)
			if scenario == "worker" {
				if err != nil || grant != r.WorkspaceScope.Grants[0] {
					t.Fatalf("valid scope member: %+v %v", grant, err)
				}
			} else if !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("scope guard %s: %+v %v", scenario, grant, err)
			}
		})
	}
}

func TestChatWorkspaceSupportMetadataIsNotPermission(t *testing.T) {
	for _, version := range []int{16, 19, 20} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			original := migrations
			defer func() { migrations = original }()
			migrations = original[:version]
			s, _ := openTestStore(t)
			g := group(t, s)
			thread := domain.ThreadRef{GroupID: g.ID, TopicID: "metadata"}
			check := func() {
				t.Helper()
				policy, err := s.GetChatPolicy(ctx, thread)
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(policy)
				if err != nil {
					t.Fatal(err)
				}
				want := fmt.Sprintf(`"workspace_changes_supported":%t`, version >= 20)
				if !strings.Contains(string(data), want) || policy.AllowWorkspaceChanges {
					t.Fatalf("support metadata grants permission or is missing: %s", data)
				}
			}
			check()
			if version == 16 {
				coordinationDecideExec(t, s, `INSERT INTO chat_policy_revisions VALUES(?, 'metadata',1,1,0,NULL,'text_submission',2,'user'); INSERT INTO chat_policies VALUES(?,'metadata',1)`, g.ID, g.ID)
			} else {
				if _, err := s.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2}, 0, "plain-policy"); err != nil {
					t.Fatal(err)
				}
			}
			check()
			if _, err := chatWorkspaceRead(t, s, g.ID, "missing-task"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("legacy scope read: %v", err)
			}
			if _, ok := reflect.TypeOf(domain.ChatPolicyRequest{}).FieldByName("WorkspaceChangesSupported"); ok {
				t.Fatal("read-only support became a PolicyRequest field")
			}
		})
	}
}

func TestChatWorkspaceCurrentSQLFailuresDoNotDowngrade(t *testing.T) {
	for _, table := range []string{"chat_workspace_policy_revisions", "chat_workspace_scopes", "workspace_write_grants"} {
		t.Run(table, func(t *testing.T) {
			f := chatWorkspaceSetup(t, "user", false)
			chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
			coordinationDecideExec(t, f.s, "ALTER TABLE "+table+" RENAME TO damaged_"+table)
			before := messageSequenceSnapshot(t, f.s)
			var err error
			if table == "chat_workspace_policy_revisions" {
				_, err = f.s.GetChatPolicy(ctx, f.candidate.ThreadRef)
			} else if table == "chat_workspace_scopes" {
				_, err = chatWorkspaceRead(t, f.s, f.groupID, "missing-task")
			} else {
				_, err = f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "不能把SQL错误当无授权。", "damaged")
			}
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("damaged current schema silently downgraded: %v", err)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestChatWorkspaceV19ToV20AtomicMigration(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	if len(original) < 20 || original[19] != chatWorkspaceSchemaV20 {
		t.Fatal("missing append-only v20")
	}
	for i, ddl := range original[:19] {
		t.Logf("frozen v%d SHA256=%x", i+1, sha256.Sum256([]byte(ddl)))
	}
	migrations = original[:19]
	f, turn, claim := queueClaimFixture(t)
	seedPreV23TurnReceipt(t, f.s, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1})
	input := workspaceGrantInput(t, f.s)
	input.Scope = "read_write"
	grant, err := f.s.AuthorizeWorkspaceGrant(ctx, input, workspaceGrantValidation(input))
	if err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	objects := messageSequenceRows(t, f.s.db, assignmentSchemaQuery)
	migrations = slices.Clone(original[:20])
	migrations[19] += "\nINSERT INTO missing_workspace_v20_fixture VALUES(1);"
	if err := migrate(f.s.db); err == nil || !strings.Contains(err.Error(), "迁移版本 20") {
		t.Fatalf("v20 partial migration committed: %v", err)
	}
	messageSequenceLedger(t, f.s, 19)
	messageSequencePreserved(t, f.s, before)
	if got := messageSequenceRows(t, f.s.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
		t.Fatal("failed v20 leaked schema objects")
	}
	migrations = original[:20]
	if err := migrate(f.s.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, f.s, 20)
	messageSequencePreserved(t, f.s, before)
	currentObjects := messageSequenceRows(t, f.s.db, assignmentSchemaQuery)
	for _, object := range objects {
		if !slices.ContainsFunc(currentObjects, func(row []any) bool { return reflect.DeepEqual(row, object) }) {
			t.Fatalf("v20 changed historical schema: %v", object)
		}
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	messageSequenceLedger(t, reopened, 20)
	messageSequencePreserved(t, reopened, before)
	got, err := reopened.GetWorkspaceGrant(ctx, input.GroupID, input.MemberID)
	if err != nil || got != grant {
		t.Fatalf("v20 changed write grant: %+v %v", got, err)
	}
	state, err := reopened.GetTurn(ctx, f.groupID, turn.ID)
	if err != nil || state.State != domain.TurnStateUnknown {
		t.Fatalf("v20 erased unknown: %+v %v", state, err)
	}
	var count int
	if err := reopened.db.QueryRow("SELECT count(*) FROM chat_workspace_scopes").Scan(&count); err != nil || count != 0 {
		t.Fatalf("v20 inferred task authority: %d %v", count, err)
	}
	dispatchAssertFK(t, reopened)
}
