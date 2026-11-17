//go:build unix

package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
	"agentworkspace/internal/workspacefs"
)

func workspaceWorkerRegistration(t *testing.T, chatIntake ...bool) (*rolePrepareFixture, string, string, store.TurnClaim) {
	t.Helper()
	ctx := context.Background()
	f, scope, directory := newWorkspacePreparation(t)
	if len(chatIntake) > 0 && chatIntake[0] {
		prepareRoleOK(t, f.opts.MemberTools.tools.EnableChatIntake(coordination.New(f.s)))
	}
	facts := f.contextPlan(t, f.preparer(t)).dispatchCapabilities
	lead, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, MaxActive: 1})
	prepareRoleOK(t, err)
	leadClaim := store.TurnClaim{GroupID: f.group.ID, TurnID: lead.ID, Token: lead.ClaimToken}
	member, err := f.s.BindMemberTool(ctx, leadClaim)
	prepareRoleOK(t, err)
	assigned, err := f.s.DecideDispatchWithCapabilities(ctx, store.CoordinationToolScope{Member: member, Provenance: f.state.Provenance}, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "实际八工具采样"}, "assign", facts)
	prepareRoleOK(t, err)
	_, err = f.s.ApplyTurnReceipt(ctx, leadClaim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	prepareRoleOK(t, err)
	delivery, err := f.s.MaterializeAssignment(ctx, assigned.Assignment.ID, 1, time.Now().UTC())
	prepareRoleOK(t, err)
	if delivery.Delivery == nil {
		t.Fatal("worker delivery missing")
	}
	f.selectDelivery(t, delivery.Delivery.MessageID)
	// A model/member preference must not choose the file Task's authority/CWD.
	f.exec(t, "UPDATE members SET workspace='/not-authorized' WHERE id=?", f.worker.ID)
	p := f.preparer(t)
	inspection, err := p.InspectRoleReadiness(ctx, f.pending)
	prepareRoleOK(t, err)
	if inspection.Blocked != nil {
		t.Fatalf("Task root incorrectly replaced by member CWD preference: %+v", inspection.Blocked)
	}
	prepared, err := p.Prepare(ctx, f.pending)
	prepareRoleOK(t, err)
	if prepared.Request.CWD != directory || !reflect.DeepEqual(prepared.ResourceKeys, []string{scope.ResourceKey}) {
		t.Fatalf("file claim not task-bound: cwd=%s keys=%v", prepared.Request.CWD, prepared.ResourceKeys)
	}
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
	prepareRoleOK(t, err)
	claim := store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}
	member, err = f.s.BindMemberTool(ctx, claim)
	prepareRoleOK(t, err)
	role := store.CoordinationToolScope{Member: member, Provenance: f.state.Provenance}
	attempt, err := f.s.BindAssignmentAttempt(ctx, role)
	prepareRoleOK(t, err)
	role.AttemptID = attempt.AttemptID
	token, err := f.opts.MemberTools.tools.issueCoordinationRole(ctx, role)
	prepareRoleOK(t, err)
	t.Cleanup(func() { f.opts.MemberTools.tools.Revoke(token) })
	return f, token, directory, claim
}

type workspaceObservedRoot struct {
	memberWorkspaceRoot
	afterRead    func()
	afterReplace func()
}

func (r workspaceObservedRoot) Read(ctx context.Context, path string) (workspacefs.Snapshot, error) {
	snapshot, err := r.memberWorkspaceRoot.Read(ctx, path)
	if err == nil && r.afterRead != nil {
		r.afterRead()
	}
	return snapshot, err
}
func (r workspaceObservedRoot) Replace(ctx context.Context, path, expected, text string) (workspacefs.WriteReceipt, error) {
	receipt, err := r.memberWorkspaceRoot.Replace(ctx, path, expected, text)
	if r.afterReplace != nil {
		r.afterReplace()
	}
	return receipt, err
}

type workspaceConcurrentChangeRoot struct {
	memberWorkspaceRoot
	directory string
}

func (r workspaceConcurrentChangeRoot) Replace(context.Context, string, string, string) (workspacefs.WriteReceipt, error) {
	err := os.WriteFile(filepath.Join(r.directory, "report.txt"), []byte("foreign after"), 0600)
	if err != nil {
		return workspacefs.WriteReceipt{}, err
	}
	return workspacefs.WriteReceipt{Stage: workspacefs.StagePrepare, Effect: workspacefs.EffectUnchanged}, errors.New("injected concurrent target change")
}

func TestWorkspaceDoneReadbackDeniedRemainsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode0000 read denial requires an unprivileged process")
	}
	f, token, directory, claim := workspaceWorkerRegistration(t)
	path := filepath.Join(directory, "report.txt")
	before := "actual before"
	prepareRoleOK(t, os.WriteFile(path, []byte(before), 0600))
	t.Cleanup(func() { os.Chmod(path, 0600) })
	tools := f.opts.MemberTools.tools
	grant := tools.workspace.grants[sha256.Sum256([]byte(token))]
	grant.root = workspaceObservedRoot{memberWorkspaceRoot: grant.root, afterReplace: func() { prepareRoleOK(t, os.Chmod(path, 0)) }}
	hash := memberWorkspaceHash(before)
	_, err := tools.WorkspaceWrite(context.Background(), token, MemberWorkspaceWriteInput{Path: "report.txt", ExpectedSHA256: &hash, Text: "actual applied bytes"}, "readback-denied")
	if !errors.Is(err, ErrWorkspaceOperationUnresolved) {
		t.Fatalf("done-stage failed readback became resolved: %v", err)
	}
	operations, err := f.s.ListWorkspaceOperationsForTurn(context.Background(), f.group.ID, claim.TurnID, store.ListOptions{Limit: 10})
	prepareRoleOK(t, err)
	if len(operations.Items) != 1 || operations.Items[0].Result == nil || operations.Items[0].Result.Outcome != "unknown" || operations.Items[0].Result.ObservedHash != "" {
		t.Fatalf("failed readback invented observation: %+v", operations.Items)
	}
	unresolved, err := f.s.HasUnresolvedWorkspaceOperations(context.Background(), f.group.ID, claim.TurnID)
	prepareRoleOK(t, err)
	if !unresolved {
		t.Fatal("failed readback released unresolved state")
	}
	prepareRoleOK(t, os.Chmod(path, 0600))
	actual, err := os.ReadFile(path)
	prepareRoleOK(t, err)
	if string(actual) != "actual applied bytes" {
		t.Fatal("real applied effect was not retained")
	}
}

func TestWorkspaceUnchangedReceiptDoesNotInventObservedPreimage(t *testing.T) {
	f, token, directory, claim := workspaceWorkerRegistration(t)
	before := "actual before"
	prepareRoleOK(t, os.WriteFile(filepath.Join(directory, "report.txt"), []byte(before), 0600))
	tools := f.opts.MemberTools.tools
	grant := tools.workspace.grants[sha256.Sum256([]byte(token))]
	grant.root = workspaceConcurrentChangeRoot{memberWorkspaceRoot: grant.root, directory: directory}
	hash := memberWorkspaceHash(before)
	_, err := tools.WorkspaceWrite(context.Background(), token, MemberWorkspaceWriteInput{Path: "report.txt", ExpectedSHA256: &hash, Text: "intended after"}, "foreign-change")
	if !errors.Is(err, ErrWorkspaceOperationUnresolved) {
		t.Fatalf("native unchanged was mistaken for observed original bytes: %v", err)
	}
	operations, err := f.s.ListWorkspaceOperationsForTurn(context.Background(), f.group.ID, claim.TurnID, store.ListOptions{Limit: 10})
	prepareRoleOK(t, err)
	if len(operations.Items) != 1 || operations.Items[0].Result == nil || operations.Items[0].Result.Outcome != "unknown" || operations.Items[0].Result.ObservedHash != memberWorkspaceHash("foreign after") {
		t.Fatalf("after state was fabricated: %+v", operations.Items)
	}
}

func TestWorkspacePostReadScopeLossDisclosesNoBytes(t *testing.T) {
	f, token, directory, _ := workspaceWorkerRegistration(t)
	prepareRoleOK(t, os.WriteFile(filepath.Join(directory, "secret.txt"), []byte("actual task bytes"), 0600))
	tools := f.opts.MemberTools.tools
	key := sha256.Sum256([]byte(token))
	grant := tools.workspace.grants[key]
	grant.root = workspaceObservedRoot{memberWorkspaceRoot: grant.root, afterRead: func() { f.exec(t, "UPDATE members SET status='left' WHERE id=?", f.worker.ID) }}
	got, err := tools.WorkspaceRead(context.Background(), token, MemberWorkspacePathInput{Path: "secret.txt"})
	if !errors.Is(err, store.ErrMemberToolDenied) || !reflect.ValueOf(got).IsZero() {
		t.Fatalf("post-read authorization loss disclosed bytes: %+v %v", got, err)
	}
}

func TestWorkspacePostWriteScopeLossRetainsPendingActualEffect(t *testing.T) {
	f, token, directory, claim := workspaceWorkerRegistration(t)
	before := "actual before"
	prepareRoleOK(t, os.WriteFile(filepath.Join(directory, "report.txt"), []byte(before), 0600))
	tools := f.opts.MemberTools.tools
	grant := tools.workspace.grants[sha256.Sum256([]byte(token))]
	grant.root = workspaceObservedRoot{memberWorkspaceRoot: grant.root, afterReplace: func() { f.exec(t, "UPDATE members SET status='left' WHERE id=?", f.worker.ID) }}
	hash := memberWorkspaceHash(before)
	got, err := tools.WorkspaceWrite(context.Background(), token, MemberWorkspaceWriteInput{Path: "report.txt", ExpectedSHA256: &hash, Text: "actual applied"}, "scope-loss")
	if !errors.Is(err, ErrWorkspaceOperationUnresolved) || !reflect.ValueOf(got).IsZero() {
		t.Fatalf("post-write scope loss was not unresolved: %+v %v", got, err)
	}
	actual, err := os.ReadFile(filepath.Join(directory, "report.txt"))
	prepareRoleOK(t, err)
	if string(actual) != "actual applied" {
		t.Fatal("fixture did not apply actual IO")
	}
	unresolved, err := f.s.HasUnresolvedWorkspaceOperations(context.Background(), f.group.ID, claim.TurnID)
	prepareRoleOK(t, err)
	if !unresolved {
		t.Fatal("scope loss discarded pending intent")
	}
	var count int
	prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", claim.TurnID).Scan(&count))
	if count != 1 {
		t.Fatal("scope loss released original resource claim")
	}
}

func TestWorkspaceRevokeDrainsRealReadBeforeClosingPins(t *testing.T) {
	f, token, directory, _ := workspaceWorkerRegistration(t)
	prepareRoleOK(t, os.WriteFile(filepath.Join(directory, "report.txt"), []byte("actual"), 0600))
	tools := f.opts.MemberTools.tools
	grant := tools.workspace.grants[sha256.Sum256([]byte(token))]
	entered, release, readDone, revokeDone := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan struct{})
	grant.root = workspaceObservedRoot{memberWorkspaceRoot: grant.root, afterRead: func() { close(entered); <-release }}
	go func() {
		_, err := tools.WorkspaceRead(context.Background(), token, MemberWorkspacePathInput{Path: "report.txt"})
		readDone <- err
	}()
	<-entered
	go func() { tools.Revoke(token); close(revokeDone) }()
	select {
	case <-revokeDone:
		t.Fatal("revocation closed a pin during authorized IO")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	prepareRoleOK(t, <-readDone)
	<-revokeDone
	if _, err := tools.WorkspaceRead(context.Background(), token, MemberWorkspacePathInput{Path: "report.txt"}); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("revoked file tool survived: %v", err)
	}
	if _, err := grant.root.Read(context.Background(), "report.txt"); !errors.Is(err, workspacefs.ErrClosed) {
		t.Fatalf("pin leaked after drain: %v", err)
	}
}
