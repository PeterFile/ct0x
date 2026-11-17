//go:build unix

package platform_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type workspaceRuntime interface {
	EnableWorkspace(platform.MemberWorkspaceOptions) error
	Close() error
}
type workspaceTools interface {
	WorkspaceWrite(context.Context, string, platform.MemberWorkspaceWriteInput, string) (platform.MemberWorkspaceWriteResult, error)
	WorkspaceRead(context.Context, string, platform.MemberWorkspacePathInput) (platform.MemberWorkspaceReadResult, error)
	WorkspaceList(context.Context, string, platform.MemberWorkspacePathInput) (platform.MemberWorkspaceListResult, error)
	PublishWorkspaceArtifact(context.Context, string, platform.MemberWorkspaceArtifactInput, string) (platform.MemberWorkspaceArtifactResult, error)
}

type workspaceRunFixture struct {
	*roleRuntimeFixture
	run       roleRun
	scope     domain.TaskWorkspaceScope
	directory string
	blobs     *artifactfs.Store
	tools     workspaceTools
}

func newWorkspaceRun(t *testing.T) *workspaceRunFixture {
	t.Helper()
	ctx := context.Background()
	f := newRoleRuntime(t)
	routes := append(httpapi.MemberRoleToolRoutes(f.runtime.Tools()), httpapi.MemberMCPRoutes(f.runtime.Tools())...)
	server, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
	roleOK(t, err)
	f.handler = server.Handler
	runtime, ok := any(f.runtime).(workspaceRuntime)
	if !ok {
		t.Fatal("runtime has no private workspace owner/close seam")
	}
	tools, ok := any(f.runtime.Tools()).(workspaceTools)
	if !ok {
		t.Fatal("worker tools have no real workspace IO/publication seam")
	}
	private := t.TempDir()
	roleOK(t, os.Chmod(private, 0700))
	owner, err := os.OpenRoot(private)
	roleOK(t, err)
	blobs, err := artifactfs.New(owner, 64<<20)
	roleOK(t, err)
	roleOK(t, owner.Close())
	t.Cleanup(func() { roleOK(t, blobs.Close()) })
	scratch := t.TempDir()
	roleOK(t, os.Chmod(scratch, 0700))
	roleOK(t, runtime.EnableWorkspace(platform.MemberWorkspaceOptions{Blobs: blobs, ScratchRoot: scratch, ProtectedRoots: []string{private, f.options.ConfigRoot, f.root}}))
	t.Cleanup(func() { roleOK(t, runtime.Close()) })
	directory := t.TempDir()
	keys, err := scheduler.WorkspaceResourceKeys([]string{directory})
	roleOK(t, err)
	grant, err := f.db.AuthorizeWorkspaceGrant(ctx, store.AuthorizeWorkspaceGrantInput{GroupID: f.group, MemberID: f.worker.ID, Root: directory, Scope: "read_write", IdempotencyKey: "directory"}, func([]store.WorkspaceGrantReceipt) (store.WorkspaceGrantDirectory, error) {
		return store.WorkspaceGrantDirectory{ResolvedRoot: directory, ResourceKey: keys[0]}, nil
	})
	roleOK(t, err)
	_, err = f.db.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.group, TopicID: "files"}, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, AllowWorkspaceChanges: true, MaxAdmissions: 2}, 0, "chat-policy")
	roleOK(t, err)
	source, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: "files", Content: "按讨论在已授权目录修改并提交真实文件版本", RecipientIDs: []string{f.lead.ID}}, "file-source")
	roleOK(t, err)
	intake, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: source.Deliveries[0].ID, MaxActive: 1})
	roleOK(t, err)
	member, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: f.group, TurnID: intake.ID, Token: intake.ClaimToken})
	roleOK(t, err)
	candidate, err := f.db.CaptureChatCandidate(ctx, member)
	roleOK(t, err)
	admitted, err := f.db.ApplyChatAssessment(ctx, member, candidate.ID, domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionWorkspaceChange, Title: "实际文件", Description: "仅授权目录", Acceptance: "实际版本", Reason: "实际用户要求落实"}, "登记任务，尚未执行。", "admission")
	roleOK(t, err)
	if admitted.WorkspaceScope == nil {
		t.Fatal("missing Task workspace scope")
	}
	_, err = f.db.ApplyTurnReceipt(ctx, member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	roleOK(t, err)
	materialized, err := f.db.MaterializeDispatchRequest(ctx, admitted.DispatchRequestID, 1, time.Now().UTC())
	roleOK(t, err)
	if materialized.Delivery == nil {
		t.Fatal("missing coordinator delivery")
	}
	lead := f.claim(t, materialized.Delivery.DeliveryID)
	leadMember, err := f.db.BindMemberTool(ctx, lead.claim)
	roleOK(t, err)
	fact := store.DispatchWorkerCapabilityFact{GroupID: f.group, MemberID: f.worker.ID, ProviderID: f.worker.ProviderID, Model: f.worker.Model, RequiredProfile: store.DispatchWorkspaceWorkerProfile, TaskID: admitted.TaskID, TaskRevision: admitted.WorkspaceScope.TaskRevision, ResourceKey: keys[0], NodeID: "local", GrantID: grant.ID}
	assigned, err := f.db.DecideDispatchWithCapabilities(ctx, store.CoordinationToolScope{Member: leadMember, Provenance: lead.provenance}, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "实际验证的文件工具profile"}, "assign", []store.DispatchWorkerCapabilityFact{fact})
	roleOK(t, err)
	_, err = f.db.ApplyTurnReceipt(ctx, lead.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	roleOK(t, err)
	workerDelivery, err := f.db.MaterializeAssignment(ctx, assigned.Assignment.ID, 1, time.Now().UTC())
	roleOK(t, err)
	if workerDelivery.Delivery == nil {
		t.Fatal("missing worker delivery")
	}
	state, err := f.db.ResolveCoordinationDelivery(ctx, f.group, workerDelivery.Delivery.DeliveryID)
	roleOK(t, err)
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: workerDelivery.Delivery.DeliveryID, MaxActive: 1, ResourceKeys: keys})
	roleOK(t, err)
	run := roleRun{binding: platform.MemberToolRunBinding{GroupID: f.group, MemberID: f.worker.ID, ConversationID: turn.ConversationID, DeliveryID: turn.DeliveryID, ProviderID: f.worker.ProviderID, ProviderName: provider.Pi, Model: f.worker.Model}, provenance: state.Provenance, claim: store.TurnClaim{GroupID: f.group, TurnID: turn.ID, Token: turn.ClaimToken}, request: provider.ExecuteRequest{RunID: turn.ID, RuntimeDir: f.root, CWD: directory, Model: f.worker.Model, Prompt: "trusted workspace task"}}
	return &workspaceRunFixture{roleRuntimeFixture: f, run: run, scope: *admitted.WorkspaceScope, directory: directory, blobs: blobs, tools: tools}
}

func (f *workspaceRunFixture) execute(t *testing.T, use func(context.Context, string) provider.ExecutionReceipt) provider.ExecutionReceipt {
	t.Helper()
	executor, err := f.runtime.WrapCoordinationRoleWithCapabilities(f.run.binding, f.run.provenance, nil, func(path, id string, names []string) (provider.Executor, error) {
		if id != f.run.claim.TurnID || !slices.Equal(names, provider.WorkspaceWorkerToolNames()) {
			t.Fatalf("not exact file-worker profile: %v", names)
		}
		wire, err := os.ReadFile(path)
		roleOK(t, err)
		var config struct{ Token string }
		roleOK(t, json.Unmarshal(wire, &config))
		return roleNative(func(ctx context.Context, _ provider.ExecuteRequest, _ func(provider.Event) error) provider.ExecutionReceipt {
			return use(ctx, config.Token)
		}), nil
	})
	roleOK(t, err)
	return executor.Execute(context.Background(), f.run.request, nil)
}

func TestWorkspaceWiringIsolationAndWholeResultBudget(t *testing.T) {
	f := newWorkspaceRun(t)
	roleOK(t, os.WriteFile(filepath.Join(f.directory, "large.txt"), []byte(strings.Repeat("a", 65536)), 0600))
	foreign := t.TempDir()
	roleOK(t, os.WriteFile(filepath.Join(foreign, "private.txt"), []byte("foreign-private-bytes"), 0600))
	roleOK(t, os.Symlink(foreign, filepath.Join(f.directory, "foreign")))
	receipt := f.execute(t, func(ctx context.Context, token string) provider.ExecutionReceipt {
		list, err := f.tools.WorkspaceList(ctx, token, platform.MemberWorkspacePathInput{Path: "."})
		roleOK(t, err)
		if len(list.Entries) != 1 || list.Entries[0].Name != "large.txt" {
			t.Fatalf("list followed/disclosed a foreign symlink: %+v", list)
		}
		denied, err := f.tools.WorkspaceRead(ctx, token, platform.MemberWorkspacePathInput{Path: "foreign/private.txt"})
		if err == nil || !reflect.ValueOf(denied).IsZero() {
			t.Fatalf("foreign symlink disclosed bytes: %+v %v", denied, err)
		}
		large := f.http(t, token, "workspace_read", `{"path":"large.txt"}`, "")
		if large.Code != 413 || strings.Contains(large.Body.String(), strings.Repeat("a", 100)) {
			t.Fatalf("full oversized JSON was truncated or disclosed: %d", large.Code)
		}
		wire, err := json.Marshal(platform.MemberWorkspaceWriteInput{Path: "oversize.txt", Create: true, Text: strings.Repeat("中", 2731)})
		roleOK(t, err)
		oversized := f.http(t, token, "workspace_write", string(wire), "oversize-text")
		if oversized.Code != 400 {
			t.Fatalf("UTF-8 write byte limit bypassed: %d", oversized.Code)
		}
		wire, err = json.Marshal(platform.MemberWorkspaceWriteInput{Path: strings.Repeat("<", 4096), Create: true, Text: strings.Repeat("\x00", 8192)})
		roleOK(t, err)
		encoded := f.http(t, token, "workspace_write", string(wire), "oversize-json")
		if len(wire) <= 65536 || encoded.Code != 413 {
			t.Fatalf("complete escaped HTTP input budget bypassed: %d bytes=%d", encoded.Code, len(wire))
		}
		if _, err := os.Stat(filepath.Join(f.directory, "oversize.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("oversized text was partially written")
		}
		return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, ExitCode: 0}
	})
	if receipt.Outcome != provider.OutcomeSucceeded {
		t.Fatalf("isolation aggregate: %+v", receipt)
	}
}

func TestWorkspaceWiringNativeHTTPAndMCPStrictSuccessOrDeadlineUnknown(t *testing.T) {
	f := newWorkspaceRun(t)
	deadlineObserved := false
	receipt := f.execute(t, func(ctx context.Context, token string) provider.ExecutionReceipt {
		for i, body := range []string{`{"path":"native.txt","create":true}`, `{"path":"native.txt","create":null,"text":"x"}`, `{"path":"native.txt","create":true,"text":"x","expected_sha256":""}`, `{"path":"native.txt","create":true,"text":"x","root":"/outside"}`, `{"path":"native.txt","create":true,"text":"x","caller":"user"}`, `{"path":"../outside","create":true,"text":"x"}`, `{"path":"colon:name","create":true,"text":"x"}`, `{"path":"native.txt","create":true,"text":"x","path":"other"}`} {
			response := f.http(t, token, "workspace_write", body, fmt.Sprintf("invalid-%d", i))
			if response.Code != 400 {
				t.Fatalf("unsafe native body %d: %d %s", i, response.Code, response.Body.String())
			}
		}
		response := f.http(t, token, "workspace_write", `{"path":"native.txt","create":true,"text":"actual native bytes"}`, "native-write")
		if response.Code == http.StatusGatewayTimeout {
			var failure struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			roleOK(t, json.Unmarshal(response.Body.Bytes(), &failure))
			if failure.Error.Code != "deadline_exceeded" {
				t.Fatalf("unclassified write failure: %s", response.Body.String())
			}
			// Race instrumentation can exhaust the REAL production HTTP deadline
			// in SQLite's existing planner. Do not widen it or retry a mutation.
			deadlineObserved = true
			t.Log("bounded native HTTP deadline observed; validating unknown/claim retention, not HTTP mutation success")
			return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, ExitCode: -1, Reason: "原生HTTP写入超时，原key须先对账"}
		}
		var write platform.MemberWorkspaceWriteResult
		if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &write) != nil || write.Outcome != "done" {
			t.Fatalf("native write: %d %s", response.Code, response.Body.String())
		}
		publish := f.http(t, token, "publish_workspace_artifact", `{"operation_id":"`+write.OperationID+`","name":"原生文件"}`, "native-publish")
		var version platform.MemberWorkspaceArtifactResult
		if publish.Code != 201 || json.Unmarshal(publish.Body.Bytes(), &version) != nil || version.VersionID == "" {
			t.Fatalf("native publish: %d %s", publish.Code, publish.Body.String())
		}
		stored, err := f.db.GetArtifactVersion(ctx, f.group, version.ArtifactID, version.VersionID)
		roleOK(t, err)
		if stored.AuthorID != f.worker.ID || stored.SourceTaskID != f.scope.TaskID {
			t.Fatal("HTTP selected a caller or Task")
		}
		mcp := func(body string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/mcp", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", "2025-06-18")
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, req)
			return w
		}
		list := mcp(`{"jsonrpc":"2.0","id":"list","method":"tools/list","params":{}}`)
		var discovery struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		roleOK(t, json.Unmarshal(list.Body.Bytes(), &discovery))
		names := []string{}
		for _, tool := range discovery.Result.Tools {
			names = append(names, tool.Name)
		}
		if list.Code != 200 || !slices.Equal(names, provider.WorkspaceWorkerToolNames()) {
			t.Fatalf("MCP leaked another profile: %d %v", list.Code, names)
		}
		read := mcp(`{"jsonrpc":"2.0","id":"read","method":"tools/call","params":{"name":"workspace_read","arguments":{"path":"native.txt"}}}`)
		if read.Code != 200 || !strings.Contains(read.Body.String(), "actual native bytes") || strings.Contains(read.Body.String(), token) {
			t.Fatalf("MCP real read failed or leaked identity: %d %s", read.Code, read.Body.String())
		}
		return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, ExitCode: 0}
	})
	if deadlineObserved {
		if receipt.Outcome != provider.OutcomeUnknown {
			t.Fatalf("HTTP deadline became success: %+v", receipt)
		}
		_, err := f.db.ApplyTurnReceipt(context.Background(), f.run.claim, receipt)
		roleOK(t, err)
		f.claims(t, f.run, domain.TurnStateUnknown, 1)
		return
	}
	if receipt.Outcome != provider.OutcomeSucceeded {
		t.Fatalf("native API aggregate: %+v", receipt)
	}
}

func TestWorkspaceWiringPublishedVersionsPreserveActualHistory(t *testing.T) {
	f := newWorkspaceRun(t)
	receipt := f.execute(t, func(ctx context.Context, token string) provider.ExecutionReceipt {
		first, err := f.tools.WorkspaceWrite(ctx, token, platform.MemberWorkspaceWriteInput{Path: "versions.txt", Create: true, Text: "first actual bytes"}, "write-first")
		roleOK(t, err)
		v1, err := f.tools.PublishWorkspaceArtifact(ctx, token, platform.MemberWorkspaceArtifactInput{OperationID: first.OperationID, Name: "版本"}, "publish-first")
		roleOK(t, err)
		second, err := f.tools.WorkspaceWrite(ctx, token, platform.MemberWorkspaceWriteInput{Path: "versions.txt", Text: "second actual bytes", ExpectedSHA256: &first.AfterSHA256}, "write-second")
		roleOK(t, err)
		v2, err := f.tools.PublishWorkspaceArtifact(ctx, token, platform.MemberWorkspaceArtifactInput{OperationID: second.OperationID, ArtifactID: v1.ArtifactID, ExpectedVersionID: v1.VersionID}, "publish-second")
		roleOK(t, err)
		if v2.ArtifactID != v1.ArtifactID || v2.VersionID == v1.VersionID || !v2.Applied {
			t.Fatalf("version CAS/history broken: %+v %+v", v1, v2)
		}
		for _, version := range []struct {
			result platform.MemberWorkspaceArtifactResult
			text   string
		}{{v1, "first actual bytes"}, {v2, "second actual bytes"}} {
			old, err := f.db.GetArtifactVersion(ctx, f.group, version.result.ArtifactID, version.result.VersionID)
			roleOK(t, err)
			if old.BlobHash != version.result.SHA256 {
				t.Fatal("old immutable metadata overwritten")
			}
			blob, err := f.blobs.Open(old.BlobHash)
			roleOK(t, err)
			actual, err := io.ReadAll(blob)
			roleOK(t, err)
			roleOK(t, blob.Close())
			if string(actual) != version.text {
				t.Fatal("immutable actual history overwritten")
			}
		}
		return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, ExitCode: 0}
	})
	if receipt.Outcome != provider.OutcomeSucceeded {
		t.Fatalf("version aggregate: %+v", receipt)
	}
}

func TestWorkspaceWiringRepairSubmissionSummaryToolBudgetBlocksWithoutTruncation(t *testing.T) {
	f := newWorkspaceRun(t)
	ctx := context.Background()
	summary := strings.Repeat("<", 11000) + "\n正式Submission正文尾部<&>"
	var submission domain.Submission
	receipt := f.execute(t, func(ctx context.Context, token string) provider.ExecutionReceipt {
		roleOK(t, f.db.RecordTurnEvent(ctx, f.run.claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "summary-parent"}))
		write, err := f.tools.WorkspaceWrite(ctx, token, platform.MemberWorkspaceWriteInput{Path: "summary.txt", Create: true, Text: "actual rejected artifact"}, "summary-write")
		roleOK(t, err)
		version, err := f.tools.PublishWorkspaceArtifact(ctx, token, platform.MemberWorkspaceArtifactInput{OperationID: write.OperationID, Name: "原成果"}, "summary-publish")
		roleOK(t, err)
		submission, err = f.runtime.Tools().SubmitAssignmentResult(ctx, token, store.AssignmentResultInput{Summary: summary, ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.VersionID}}}, "summary-submit")
		roleOK(t, err)
		roleOK(t, f.db.RecordTurnEvent(ctx, f.run.claim, provider.Event{Kind: provider.TerminalEvent, NativeSessionID: "summary-parent", Terminal: provider.OutcomeSucceeded}))
		return provider.ExecutionReceipt{Started: true, Accepted: true, Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, NativeSessionID: "summary-parent", DirectChildReaped: true, OutputComplete: true, ExitCode: 0}
	})
	if receipt.Outcome != provider.OutcomeSucceeded {
		t.Fatalf("summary parent execution unresolved: %+v", receipt)
	}
	_, err := f.db.ApplyTurnReceipt(ctx, f.run.claim, receipt)
	roleOK(t, err)
	task, err := f.db.GetTask(ctx, f.group, submission.TaskID)
	roleOK(t, err)
	review, err := f.local.ReviewTask(ctx, f.group, task.ID, platform.TaskReviewInput{SubmissionID: submission.ID, ExpectedRevision: task.Revision, Outcome: domain.ReviewOutcomeRejected, Reason: "请在原许可范围修复这个具体版本。"}, "summary-review")
	roleOK(t, err)
	var messageID string
	roleOK(t, f.raw.QueryRow("SELECT message_id FROM chat_review_notices WHERE review_id=?", review.ID).Scan(&messageID))
	notice, err := f.db.GetMessage(ctx, f.group, messageID)
	roleOK(t, err)
	if len(notice.Deliveries) != 1 {
		t.Fatal("formal Review intake delivery missing")
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: notice.Deliveries[0].ID, MaxActive: 1})
	roleOK(t, err)
	roleOK(t, f.runtime.Tools().EnableChatIntake(coordination.New(f.db)))
	token, err := f.runtime.Tools().IssueChatIntake(ctx, store.TurnClaim{GroupID: f.group, TurnID: turn.ID, Token: turn.ClaimToken})
	roleOK(t, err)
	t.Cleanup(func() { f.runtime.Tools().Revoke(token) })
	candidate, err := f.runtime.Tools().ChatCandidate(ctx, token)
	roleOK(t, err)
	if candidate == nil || candidate.RepairSource == nil || candidate.RepairSource.ParentSubmissionSummary != summary || candidate.RepairSource.ParentSubmissionID != submission.ID {
		t.Fatal("captured actual Submission text was omitted or summarized")
	}
	source, err := f.db.GetChatRepairSource(ctx, candidate.ThreadRef, review.ID)
	roleOK(t, err)
	if !reflect.DeepEqual(*candidate.RepairSource, source) {
		t.Fatal("tool candidate replaced other trusted repair source fields")
	}
	encoded, err := json.Marshal(candidate)
	roleOK(t, err)
	if len(summary) >= 65536 || len(encoded) <= 65536 {
		t.Fatal("tool budget fixture must overflow only after JSON escaping")
	}
	routes := append(httpapi.MemberToolRoutes(f.runtime.Tools()), httpapi.MemberHistoryToolRoutes(f.runtime.Tools())...)
	routes = append(routes, httpapi.MemberMCPRoutes(f.runtime.Tools())...)
	server, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
	roleOK(t, err)
	f.handler = server.Handler
	before := f.decisionSnapshot(t)
	response := f.http(t, token, "read_channel", `{"limit":1}`, "")
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "tool_result_too_large") || strings.Contains(response.Body.String(), "chat_candidate") {
		t.Fatalf("complete source overflow was silently truncated/dropped: %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"summary-read","method":"tools/call","params":{"name":"read_channel","arguments":{"limit":1}}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2025-06-18")
	mcp := httptest.NewRecorder()
	f.handler.ServeHTTP(mcp, request)
	if mcp.Code != http.StatusOK || !strings.Contains(mcp.Body.String(), `"isError":true`) || !strings.Contains(mcp.Body.String(), "tool_result_too_large") || strings.Contains(mcp.Body.String(), "chat_candidate") || !reflect.DeepEqual(before, f.decisionSnapshot(t)) {
		t.Fatalf("MCP hid overflow or changed durable work: %d %s", mcp.Code, mcp.Body.String())
	}
	t.Logf("formal_submission=%s summary_utf8_bytes=%d encoded_candidate_bytes=%d; HTTP413/MCP isError, no truncated candidate", submission.ID, len(summary), len(encoded))
}

func TestWorkspaceWiringReplayDoesNotReadOrWriteAgain(t *testing.T) {
	f := newWorkspaceRun(t)
	receipt := f.execute(t, func(ctx context.Context, token string) provider.ExecutionReceipt {
		input := platform.MemberWorkspaceWriteInput{Path: "new.txt", Create: true, Text: "frozen after"}
		original, err := f.tools.WorkspaceWrite(ctx, token, input, "one-write")
		roleOK(t, err)
		published, err := f.tools.PublishWorkspaceArtifact(ctx, token, platform.MemberWorkspaceArtifactInput{OperationID: original.OperationID, Name: "冻结"}, "one-publish")
		roleOK(t, err)
		// Remove the target. A replay that re-reads/re-copies or re-applies bytes
		// cannot return this original immutable receipt.
		roleOK(t, os.Remove(filepath.Join(f.directory, input.Path)))
		replay, err := f.tools.WorkspaceWrite(ctx, token, input, "one-write")
		roleOK(t, err)
		if replay != original {
			t.Fatal("write replay changed original receipt")
		}
		again, err := f.tools.PublishWorkspaceArtifact(ctx, token, platform.MemberWorkspaceArtifactInput{OperationID: original.OperationID, Name: "冻结"}, "one-publish")
		roleOK(t, err)
		if again != published {
			t.Fatal("publication replay changed frozen version")
		}
		if _, err := os.Stat(filepath.Join(f.directory, input.Path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("replay recreated a missing target")
		}
		input.Text = "altered payload"
		if _, err := f.tools.WorkspaceWrite(ctx, token, input, "one-write"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("changed payload did not conflict: %v", err)
		}
		return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, ExitCode: 0}
	})
	if receipt.Outcome != provider.OutcomeSucceeded {
		t.Fatalf("replay aggregate: %+v", receipt)
	}
}

func TestWorkspaceWiringFinalizeFailureRetainsClaimsAndNativeRaw(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "native-success", true: "cancel-request"}[cancel], func(t *testing.T) {
			f := newWorkspaceRun(t)
			_, err := f.raw.Exec(`CREATE TRIGGER fail_workspace_finalize BEFORE INSERT ON workspace_operation_receipts BEGIN SELECT RAISE(ABORT,'finalize unavailable'); END`)
			roleOK(t, err)
			raw := provider.ExecutionReceipt{Started: true, Accepted: true, Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, NativeSessionID: "native-actual", DirectChildReaped: true, OutputComplete: true, ExitCode: 0, StdoutPath: "native.stdout", StderrPath: "native.stderr", Reason: "模型完成不证明文件IO完成"}
			if cancel {
				raw.Outcome = provider.OutcomeAborted
				raw.Terminal = provider.OutcomeAborted
			}
			aggregate := f.execute(t, func(ctx context.Context, token string) provider.ExecutionReceipt {
				input := platform.MemberWorkspaceWriteInput{Path: "pending.txt", Create: true, Text: "actually applied"}
				if _, err := f.tools.WorkspaceWrite(ctx, token, input, "pending"); !errors.Is(err, platform.ErrWorkspaceOperationUnresolved) {
					t.Fatalf("finalize failure did not remain unknown: %v", err)
				}
				disk, err := os.ReadFile(filepath.Join(f.directory, input.Path))
				roleOK(t, err)
				if string(disk) != "actually applied" {
					t.Fatal("failure fixture did not exercise real applied IO")
				}
				if _, err := f.tools.WorkspaceWrite(ctx, token, input, "pending"); !errors.Is(err, platform.ErrWorkspaceOperationUnresolved) {
					t.Fatalf("pending replay not blocked: %v", err)
				}
				if cancel {
					_, err := f.db.RequestTurnCancel(ctx, f.group, f.run.claim.TurnID)
					roleOK(t, err)
				}
				return raw
			})
			if aggregate.Outcome != provider.OutcomeUnknown {
				t.Fatalf("native terminal released unresolved IO: %+v", aggregate)
			}
			diagnostics, err := f.runtime.InspectCoordinationRoleReceipt(context.Background(), f.group, f.run.claim.TurnID)
			roleOK(t, err)
			if diagnostics.Capture == nil || !reflect.DeepEqual(diagnostics.Capture.Raw, raw) {
				t.Fatalf("native Raw changed: %+v", diagnostics.Capture)
			}
			_, err = f.db.ApplyTurnReceipt(context.Background(), f.run.claim, aggregate)
			roleOK(t, err)
			f.claims(t, f.run, domain.TurnStateUnknown, 1)
		})
	}
}

func TestWorkspaceWiringRealBytesVersionAndWorkerSubmission(t *testing.T) {
	f := newWorkspaceRun(t)
	roleOK(t, os.WriteFile(filepath.Join(f.directory, "report.txt"), []byte("actual before\n"), 0600))
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("actual before\n")))
	var publication platform.MemberWorkspaceArtifactResult
	receipt := f.execute(t, func(ctx context.Context, token string) provider.ExecutionReceipt {
		httpRead := f.http(t, token, "workspace_read", `{"path":"report.txt"}`, "")
		if httpRead.Code != 200 {
			t.Fatalf("native read route: %d %s", httpRead.Code, httpRead.Body.String())
		}
		read, err := f.tools.WorkspaceRead(ctx, token, platform.MemberWorkspacePathInput{Path: "report.txt"})
		roleOK(t, err)
		if read.Text != "actual before\n" || read.SHA256 != hash {
			t.Fatal("not actual preimage")
		}
		write, err := f.tools.WorkspaceWrite(ctx, token, platform.MemberWorkspaceWriteInput{Path: "report.txt", Text: "actual after\n", ExpectedSHA256: &hash}, "write")
		roleOK(t, err)
		if write.Outcome != "done" || write.OperationID == "" || write.BeforeSHA256 != hash {
			t.Fatalf("not durable done: %+v", write)
		}
		disk, err := os.ReadFile(filepath.Join(f.directory, "report.txt"))
		roleOK(t, err)
		if string(disk) != "actual after\n" || write.AfterSHA256 != fmt.Sprintf("%x", sha256.Sum256(disk)) {
			t.Fatal("operation does not match real after bytes")
		}
		old, err := f.blobs.Open(hash)
		roleOK(t, err)
		preimage, err := io.ReadAll(old)
		roleOK(t, err)
		roleOK(t, old.Close())
		if string(preimage) != "actual before\n" {
			t.Fatal("actual preimage was not immutable")
		}
		publication, err = f.tools.PublishWorkspaceArtifact(ctx, token, platform.MemberWorkspaceArtifactInput{OperationID: write.OperationID, Name: "实际报告"}, "publish")
		roleOK(t, err)
		version, err := f.db.GetArtifactVersion(ctx, f.group, publication.ArtifactID, publication.VersionID)
		roleOK(t, err)
		if version.AuthorID != f.worker.ID || version.SourceTaskID != f.scope.TaskID || version.GroupID != f.group || version.BlobHash != write.AfterSHA256 {
			t.Fatalf("publication impersonated user or Task: %+v", version)
		}
		submission, err := f.runtime.Tools().SubmitAssignmentResult(ctx, token, store.AssignmentResultInput{Summary: "实际文件已冻结，等待本机用户评审。", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: publication.ArtifactID, VersionID: publication.VersionID}}}, "submit")
		roleOK(t, err)
		if submission.SubmittedBy != f.worker.ID || len(submission.ArtifactVersions) != 1 {
			t.Fatalf("submission not exact worker version: %+v", submission)
		}
		return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, ExitCode: 0, Reason: "完成"}
	})
	if receipt.Outcome != provider.OutcomeSucceeded {
		t.Fatalf("native success not preserved: %+v", receipt)
	}
	blob, err := f.blobs.Open(publication.SHA256)
	roleOK(t, err)
	bytes, err := io.ReadAll(blob)
	roleOK(t, err)
	roleOK(t, blob.Close())
	if string(bytes) != "actual after\n" {
		t.Fatal("published bytes differ")
	}
}
