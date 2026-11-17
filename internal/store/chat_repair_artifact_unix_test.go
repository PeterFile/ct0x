//go:build unix

package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// Real immutable blob bytes plus the real Store role/operation chain. The F4
// grant fixture is metadata-only; physical grant identity/CLI acceptance remains
// Root's independent product-entry gate, not a claim made by this unit fixture.
func chatRepairFileOperation(t *testing.T, s *Store, role CoordinationToolScope, source *os.Root, blobs *artifactfs.Store, content []byte, replace bool, key string) (WorkspaceOperation, artifactfs.Blob) {
	t.Helper()
	input := WorkspaceOperationInput{RelativePath: "note.txt", Kind: "create", AfterHash: fmt.Sprintf("%x", sha256.Sum256(content)), AfterSize: int64(len(content))}
	preimage := WorkspaceOperationPreimage{}
	if replace {
		before, err := blobs.Publish(source, input.RelativePath)
		if err != nil {
			t.Fatal(err)
		}
		input.Kind, input.ExpectedHash = "replace", before.Hash
		preimage = WorkspaceOperationPreimage{Exists: true, Hash: before.Hash, BlobHash: before.Hash, Size: before.Size}
	}
	o, created, err := s.BeginWorkspaceOperation(ctx, role, input, preimage, key)
	if err != nil || !created {
		t.Fatalf("durable intent before fixture IO: %+v, %t, %v", o, created, err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if replace {
		flags = os.O_WRONLY | os.O_TRUNC
	}
	file, err := source.OpenFile(input.RelativePath, flags, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		t.Fatal(err)
	}
	directory, err := source.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		t.Fatal(err)
	}
	blob, err := blobs.Publish(source, input.RelativePath)
	if err != nil || blob.Hash != input.AfterHash || blob.Size != input.AfterSize {
		t.Fatalf("actual bytes differ from intended result: %+v, %v", blob, err)
	}
	o, err = s.FinishWorkspaceOperation(ctx, role, o.ID, WorkspaceOperationResult{Outcome: "done", ObservedHash: blob.Hash, ObservedSize: blob.Size})
	if err != nil {
		t.Fatal(err)
	}
	return o, blob
}

func chatRepairBoundWorkspaceWorker(t *testing.T, s *Store, receipt domain.ChatAssessmentReceipt, workerID string) CoordinationToolScope {
	t.Helper()
	view, err := s.GetDispatchRequest(ctx, receipt.GroupID, receipt.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := coordinationDecideBindRequest(t, s, view.Request)
	fact := workspaceOperationCapabilityFact(t, s, view.Request, workerID)
	decision, err := s.DecideDispatchWithCapabilities(ctx, coordinator, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: workerID, Reason: "原物理范围修复"}, "assign-repair-file", []DispatchWorkerCapabilityFact{fact})
	if err != nil || decision.Assignment == nil {
		t.Fatalf("repair workspace assignment: %+v, %v", decision, err)
	}
	materialized, err := s.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("repair workspace materialization: %+v, %v", materialized, err)
	}
	d := materialized.Delivery
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: receipt.GroupID, DeliveryID: d.DeliveryID, MaxActive: 8, ResourceKeys: []string{receipt.WorkspaceScope.ResourceKey}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.BindMemberTool(ctx, TurnClaim{GroupID: receipt.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	role := CoordinationToolScope{Member: member, Provenance: CoordinationRoleProvenance{Kind: CoordinationRoleWorker, GroupID: receipt.GroupID,
		DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID, MemberID: workerID, TopicID: receipt.TopicID,
		RequestID: view.Request.ID, TaskID: receipt.TaskID, TaskRevision: view.Request.TaskRevision, AssignmentID: decision.Assignment.ID}}
	binding, err := s.BindAssignmentAttempt(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	role.AttemptID = binding.AttemptID
	return role
}

func TestChatRepairWorkspaceVersionsKeepOriginalBytesAndExactBase(t *testing.T) {
	f, request, originalWorker := workspaceOperationFixture(t)
	chatRepairInstall(t, f.s)
	source, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	private, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	blobs, err := artifactfs.New(private, 16384)
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	beforeBytes := []byte("original version: missing the third citation\n")
	o, oldBlob := chatRepairFileOperation(t, f.s, originalWorker, source, blobs, beforeBytes, false, "original-file")
	v1, err := f.s.PublishWorkerArtifact(ctx, originalWorker, WorkerArtifactInput{OperationID: o.ID, Name: "原成果"}, WorkerArtifactBlob{Hash: oldBlob.Hash, Size: oldBlob.Size}, "original-version")
	if err != nil {
		t.Fatal(err)
	}
	submission, err := f.s.SubmitAssignmentResult(ctx, originalWorker, AssignmentResultInput{Summary: "原版本已冻结。", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: v1.ArtifactID, VersionID: v1.ID}}}, "original-file-submission")
	if err != nil {
		t.Fatalf("formal workspace Submission is required, never bypass its publication-proof gate: %v", err)
	}
	input := taskReviewInput(submission)
	input.Outcome, input.Reason = domain.ReviewOutcomeRejected, "第三条依据不全，请在原目录和动作范围补齐，保留原版本。"
	review, err := f.s.ReviewTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	notice := chatReviewNotice(t, f.s, review)
	if len(notice.Deliveries) != 1 {
		t.Fatalf("missing real Review intake: %+v", notice)
	}
	if err := f.s.CheckChatRepairIntakeReady(ctx, f.groupID, notice.Deliveries[0].ID); !errors.Is(err, ErrChatRepairPending) {
		t.Fatalf("Review bypassed original running native execution: %v", err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, originalWorker.Member.Claim, queueReceipt(provider.OutcomeSucceeded)); err != nil {
		t.Fatal(err)
	}
	var coordinator TurnClaim
	coordinator.GroupID = f.groupID
	if err := f.s.db.QueryRow("SELECT id,claim_token FROM turns WHERE group_id=? AND member_id=? AND state IN ('starting','running')", f.groupID, f.coord).Scan(&coordinator.TurnID, &coordinator.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, coordinator, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CheckChatRepairIntakeReady(ctx, f.groupID, notice.Deliveries[0].ID); err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: notice.Deliveries[0].ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.s.CaptureChatCandidate(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	assessment := chatRepairInput(t, domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionWorkspaceChange, Title: "修复提案", Acceptance: "原验收", Reason: "真实用户Review要求补齐原成果"}, review.ID, "review_request")
	receipt, err := f.s.ApplyChatAssessment(ctx, member, candidate.ID, assessment, "在原物理范围交付新版本。", "repair-file-intent")
	if err != nil || receipt.Outcome != "admitted" || receipt.WorkspaceScope == nil || receipt.RepairLink == nil {
		t.Fatalf("formal workspace repair not admitted: %+v, %v", receipt, err)
	}
	parentScope, err := f.s.GetTaskWorkspaceScope(ctx, f.groupID, request.TaskID)
	if err != nil || parentScope.ResourceKey != receipt.WorkspaceScope.ResourceKey || parentScope.NodeID != receipt.WorkspaceScope.NodeID || !reflect.DeepEqual(parentScope.Grants, receipt.WorkspaceScope.Grants) {
		t.Fatalf("repair expanded physical grant scope: parent=%+v repair=%+v, %v", parentScope, receipt.WorkspaceScope, err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	worker := chatRepairBoundWorkspaceWorker(t, f.s, receipt, f.worker)
	afterBytes := []byte("repaired version: all three citations now recorded\n")
	repaired, newBlob := chatRepairFileOperation(t, f.s, worker, source, blobs, afterBytes, true, "repair-file")
	blob := WorkerArtifactBlob{Hash: newBlob.Hash, Size: newBlob.Size}
	if _, err := f.s.PublishWorkerArtifact(ctx, worker, WorkerArtifactInput{OperationID: repaired.ID, Name: "无关联成果"}, blob, "detached"); !errors.Is(err, ErrConflict) {
		t.Fatalf("repair created a detached new Artifact: %v", err)
	}
	publish := WorkerArtifactInput{OperationID: repaired.ID, ArtifactID: v1.ArtifactID, ExpectedVersionID: v1.ID}
	v2, err := f.s.PublishWorkerArtifact(ctx, worker, publish, blob, "repair-version")
	if err != nil || !v2.Applied || v2.ArtifactID != v1.ArtifactID || v2.ExpectedVersionID != v1.ID || v2.SourceTaskID != receipt.TaskID {
		t.Fatalf("repair publication lost exact original base: %+v, %v", v2, err)
	}
	fixed, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "新版本已冻结，未代表用户接纳。", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: v2.ArtifactID, VersionID: v2.ID}}}, "repair-file-result")
	if err != nil || fixed.TaskID != receipt.TaskID {
		t.Fatalf("repair result did not use original worker chain: %+v, %v", fixed, err)
	}
	if replay, err := f.s.PublishWorkerArtifact(ctx, worker, publish, blob, "repair-version"); err != nil || replay != v2 {
		t.Fatalf("new eligibility displaced original publication receipt: %+v, %v", replay, err)
	}
	for _, version := range []struct {
		v     domain.ArtifactVersion
		bytes []byte
	}{{v1, beforeBytes}, {v2, afterBytes}} {
		actual, err := f.s.GetArtifactVersion(ctx, f.groupID, version.v.ArtifactID, version.v.ID)
		if err != nil || actual != version.v {
			t.Fatalf("immutable Artifact version changed: %+v, %v", actual, err)
		}
		reader, err := blobs.Open(actual.BlobHash)
		if err != nil {
			t.Fatal(err)
		}
		content, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || !reflect.DeepEqual(content, version.bytes) {
			t.Fatalf("original/new immutable bytes lost: %q, %v, %v", content, readErr, closeErr)
		}
	}
	if got, err := f.s.GetTaskSubmission(ctx, f.groupID, submission.TaskID, submission.ID); err != nil || !reflect.DeepEqual(got, submission) {
		t.Fatalf("original Submission overwritten: %+v, %v", got, err)
	}
	if got, err := f.s.GetTaskReview(ctx, f.groupID, submission.TaskID, review.ID); err != nil || !reflect.DeepEqual(got, review) {
		t.Fatalf("original Review overwritten: %+v, %v", got, err)
	}
	dispatchAssertFK(t, f.s)
}
