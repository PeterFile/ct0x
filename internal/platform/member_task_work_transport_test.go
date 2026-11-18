package platform_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

type boundWorkReportFixture struct {
	*roleRuntimeFixture
	engine     *coordination.Engine
	policy     domain.ChatPolicy
	source     domain.Message
	original   domain.AssignmentAttemptBinding
	delegation domain.TaskWorkDelegation
}

func newBoundWorkReportFixture(t *testing.T) *boundWorkReportFixture {
	t.Helper()
	f := &boundWorkReportFixture{roleRuntimeFixture: newRoleRuntime(t)}
	f.engine = coordination.New(f.db)
	f.options.TaskWorkReports = f.engine
	var err error
	f.runtime, err = platform.NewMemberToolRuntime(f.db, f.options)
	roleOK(t, err)
	f.mount(t)
	return f
}

func (f *boundWorkReportFixture) mount(t *testing.T) {
	t.Helper()
	tools := f.runtime.Tools()
	routes := slices.Concat(httpapi.MemberToolRoutes(tools), httpapi.MemberRoleToolRoutes(tools),
		httpapi.MemberInvestigationToolRoutes(tools), httpapi.MemberMCPRoutes(tools))
	srv, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
	roleOK(t, err)
	f.handler = srv.Handler
}

func (f *boundWorkReportFixture) claimWork(t *testing.T, deliveryID string) store.MemberToolScope {
	t.Helper()
	turn, err := f.db.ClaimTurn(t.Context(), store.ClaimTurnInput{GroupID: f.group, DeliveryID: deliveryID, MaxActive: 2,
		ResourceKeys: []string{"report-fixture/" + deliveryID}})
	roleOK(t, err)
	scope, err := f.db.BindMemberTool(t.Context(), store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	roleOK(t, err)
	return scope
}

func (f *boundWorkReportFixture) settleWork(t *testing.T, scope store.MemberToolScope, ran bool) {
	t.Helper()
	receipt := roleNotStarted()
	if ran {
		receipt = provider.ExecutionReceipt{Started: true, Accepted: true, Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded,
			NativeSessionID: scope.Claim.TurnID, DirectChildReaped: true, OutputComplete: true}
	}
	_, err := f.db.ApplyTurnReceipt(t.Context(), scope.Claim, receipt)
	roleOK(t, err)
}

func (f *boundWorkReportFixture) acceptWork(t *testing.T, scope store.MemberToolScope) {
	t.Helper()
	roleOK(t, f.db.RecordTurnEvent(t.Context(), scope.Claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: scope.Claim.TurnID}))
}

func (f *boundWorkReportFixture) delegate(t *testing.T, parentRefs, childRefs []domain.ArtifactVersionRef, topicGrant bool) {
	t.Helper()
	ctx := t.Context()
	child, err := f.local.CreateMember(ctx, f.group, platform.MemberInput{Name: "Child", Role: "Proofreader", ProviderID: f.worker.ProviderID, Model: f.worker.Model}, "")
	roleOK(t, err)
	_, err = f.db.SetDispatchPolicy(ctx, f.group, []string{f.worker.ID, child.ID}, 1, "report-workers")
	roleOK(t, err)
	thread := domain.ThreadRef{GroupID: f.group, TopicID: "report-fixture"}
	f.policy, err = f.engine.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
		Investigation: &domain.ChatInvestigationPolicy{Objective: "Check the notes", AllowTopicMessages: true, MaxDelegations: 1, ArtifactVersions: parentRefs}}, 0, "report-policy")
	roleOK(t, err)
	f.engine.EnableInvestigationExecution()
	f.engine.EnableTaskWorkExecution() // Fixture only: no C executable is installed.
	f.source, err = f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: thread.TopicID, Content: "请调查公开讨论。", RecipientIDs: []string{f.lead.ID}}, "source")
	roleOK(t, err)
	intake := f.claimWork(t, f.source.Deliveries[0].ID)
	candidate, err := f.engine.CaptureChatCandidate(ctx, intake)
	roleOK(t, err)
	admitted, err := f.engine.AssessChatIntent(ctx, intake, candidate.ID, domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionReadOnlyInvestigation,
		Reason: "按原规则调查"}, "记录授权范围。", "admit")
	roleOK(t, err)
	if admitted.Outcome != "admitted" || !admitted.AuthorityEvidenceVerified {
		t.Fatalf("fixture lacks verified v30 investigation admission: %s %s", admitted.Outcome, admitted.ReasonCode)
	}
	f.settleWork(t, intake, false)
	dispatch, err := f.db.MaterializeDispatchRequest(ctx, admitted.DispatchRequestID, 1, time.Now().UTC())
	roleOK(t, err)
	co := f.claim(t, dispatch.Delivery.DeliveryID)
	coMember, err := f.db.BindMemberTool(ctx, co.claim)
	roleOK(t, err)
	decision, err := f.db.DecideDispatchWithCapabilities(ctx, store.CoordinationToolScope{Member: coMember, Provenance: co.provenance},
		store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "调查"}, "assign", []store.DispatchWorkerCapabilityFact{{
			GroupID: f.group, MemberID: f.worker.ID, ProviderID: f.worker.ProviderID, Model: f.worker.Model, TaskID: co.provenance.TaskID,
			TaskRevision: co.provenance.TaskRevision, RequiredProfile: store.DispatchInvestigationWorkerProfile}})
	roleOK(t, err)
	f.settleWork(t, coMember, false)
	assignment, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	roleOK(t, err)
	owner := f.claim(t, assignment.Delivery.DeliveryID)
	ownerScope, err := f.db.BindMemberTool(ctx, owner.claim)
	roleOK(t, err)
	f.original, err = f.db.BindAssignmentAttempt(ctx, store.CoordinationToolScope{Member: ownerScope, Provenance: owner.provenance})
	roleOK(t, err)
	f.acceptWork(t, ownerScope)
	// The writer is not a member operation in this slice. B derives the original
	// scope, author Attempt and allowed subset from the live owner here.
	f.delegation, err = f.engine.DelegateWork(ctx, ownerScope, domain.DelegateWorkInput{MemberID: child.ID, Question: "核对第一条意见",
		AllowTopicMessages: topicGrant, ArtifactVersions: childRefs}, "delegate")
	roleOK(t, err)
	f.settleWork(t, ownerScope, true)
}

func (f *boundWorkReportFixture) call(t *testing.T, token, id, op, args string, failed bool) string {
	t.Helper()
	w := memberMCPRoleHTTP(t, f.roleRuntimeFixture, token, id, "tools/call", fmt.Sprintf(`{"name":%q,"arguments":%s}`, op, args))
	return memberMCPRoleText(t, w, failed)
}

func (f *boundWorkReportFixture) expectHTTP(t *testing.T, token, op, body, key string, code int) string {
	t.Helper()
	w := f.http(t, token, op, body, key)
	if w.Code != code || w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() > 65536 || strings.Contains(w.Body.String(), token) {
		t.Fatalf("%s: status %d, want %d, or unsafe response", op, w.Code, code)
	}
	return w.Body.String()
}

func (f *boundWorkReportFixture) stopScope(t *testing.T) {
	t.Helper()
	policy := f.policy.ChatPolicyRequest
	policy.Enabled = false
	_, err := f.engine.SetChatPolicy(t.Context(), f.policy.ThreadRef, policy, f.policy.Revision, "revoke-investigation")
	roleOK(t, err)
}

func (f *boundWorkReportFixture) expectStillQueued(t *testing.T, messageID string) {
	t.Helper()
	ctx := t.Context()
	msg, err := f.db.GetMessage(ctx, f.group, messageID)
	roleOK(t, err)
	if len(msg.Deliveries) != 1 || msg.Deliveries[0].TurnID != "" || msg.Deliveries[0].State != domain.DeliveryStateQueued {
		t.Fatal("expected exactly one unclaimed wake")
	}
	private := filepath.Join(t.TempDir(), "private")
	roleOK(t, os.Mkdir(private, 0700))
	extension, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	roleOK(t, err)
	// Even mounting the transport cannot make a backed wake startable.
	p, err := platform.NewPreparer(f.db, platform.PrepareOptions{MemberTools: f.runtime, PrivateWorkspaceRoot: private, PiMemberToolsExtension: extension})
	roleOK(t, err)
	wake := store.PendingDelivery{GroupID: f.group, Delivery: msg.Deliveries[0], SenderID: msg.SenderID}
	var blocked *platform.RolePreparationBlocked
	if got, err := p.Prepare(ctx, wake); !reflect.ValueOf(got).IsZero() || !errors.Is(err, scheduler.ErrNotReady) ||
		!errors.As(err, &blocked) || blocked.Code != "typed_profile_unavailable" {
		t.Fatalf("typed wake reached preparation: %T, %v", got.Executor, err)
	}
	entries, err := os.ReadDir(private)
	roleOK(t, err)
	after, err := f.db.GetMessage(ctx, f.group, messageID)
	roleOK(t, err)
	if len(entries) != 0 || !reflect.DeepEqual(after, msg) {
		t.Fatal("preparer changed a queued wake or allocated a directory")
	}
}

func (f *boundWorkReportFixture) workCounts(t *testing.T) [4]int {
	t.Helper()
	var counts [4]int
	roleOK(t, f.raw.QueryRow(`SELECT (SELECT count(*) FROM messages), (SELECT count(*) FROM deliveries),
		(SELECT count(*) FROM task_work_deliveries), (SELECT count(*) FROM task_work_reports)`).Scan(&counts[0], &counts[1], &counts[2], &counts[3]))
	return counts
}

func TestBoundWorkDelegationTransport(t *testing.T) {
	ctx := t.Context()
	f := newBoundWorkReportFixture(t)
	root, err := os.OpenRoot(t.TempDir())
	roleOK(t, err)
	defer root.Close()
	blobs, err := artifactfs.New(root, 1<<20)
	roleOK(t, err)
	defer blobs.Close()
	artifacts, err := platform.NewArtifacts(f.db, blobs)
	roleOK(t, err)
	publish := func(group string, input platform.PublishArtifactInput, text string) domain.ArtifactVersionRef {
		t.Helper()
		roleOK(t, root.WriteFile("source", []byte(text), 0600))
		v, err := artifacts.PublishArtifact(ctx, group, input, root, "source", "")
		roleOK(t, err)
		return domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ID}
	}
	const exact = "核对这一版本的资料"
	approved := publish(f.group, platform.PublishArtifactInput{Name: "notes"}, exact)
	head := publish(f.group, platform.PublishArtifactInput{ArtifactID: approved.ArtifactID, ExpectedVersionID: approved.VersionID}, "parent only")
	foreign := publish(f.other, platform.PublishArtifactInput{Name: "foreign"}, "foreign artifact")
	grant, err := platform.NewMemberArtifactGrant(artifacts)
	roleOK(t, err)
	f.delegate(t, []domain.ArtifactVersionRef{approved, head}, []domain.ArtifactVersionRef{approved}, true)
	d := f.delegation
	f.expectStillQueued(t, d.MessageID)
	child := f.claimWork(t, d.DeliveryID)
	live, err := f.engine.GetTaskWorkExecution(ctx, child)
	roleOK(t, err)
	saved, err := f.engine.GetTaskWorkDelegation(ctx, f.group, live.SourceID)
	roleOK(t, err)
	if live.Blocked != "" || live.SourceKind != domain.TaskWorkSourceDelegation || live.SenderMemberID != f.worker.ID || live.MemberID != child.MemberID ||
		!reflect.DeepEqual(saved, d) || saved.AuthorTurnID != f.original.TurnID || saved.AuthorAttemptID != f.original.AttemptID ||
		saved.OriginalDeliveryID != saved.AuthorDeliveryID || !slices.Equal(saved.ArtifactVersions, []domain.ArtifactVersionRef{approved}) {
		t.Fatal("B did not preserve the original owner and exact delegated subset")
	}
	wrong := child.Claim
	wrong.GroupID = f.other
	if token, err := f.runtime.IssueWorkDelegationTools(ctx, wrong, grant); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("cross-group claim acquired a delegation token")
	}
	if token, err := f.runtime.IssueWorkDelegationTools(ctx, child.Claim, nil); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("missing borrowed blob owner silently changed the profile")
	}
	token, err := f.runtime.IssueWorkDelegationTools(ctx, child.Claim, grant)
	roleOK(t, err)
	tools := f.runtime.Tools()
	defer tools.Revoke(token)
	if duplicate, err := f.runtime.IssueWorkDelegationTools(ctx, child.Claim, grant); duplicate != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("duplicate claim acquired a second capability")
	}
	const channel, artifact = platform.ReadInvestigationChannelOperation, platform.ReadInvestigationArtifactOperation
	memberMCPRoleProfile(t, f.roleRuntimeFixture, token, channel, artifact, "report_work")
	page := f.expectHTTP(t, token, channel, `{}`, "", http.StatusOK)
	var material struct{ Items []domain.Message }
	roleOK(t, json.Unmarshal([]byte(page), &material))
	foundSource := false
	for _, msg := range material.Items {
		if msg.GroupID != f.group || msg.TopicID != d.TopicID || len(msg.Deliveries) != 0 {
			t.Fatal("delegation channel exposed another scope or delivery receipt")
		}
		foundSource = foundSource || msg.ID == f.source.ID && msg.Content == f.source.Content
	}
	if !foundSource || f.call(t, token, `"topic"`, channel, `{}`, false) != page {
		t.Fatal("transport did not return the same scoped topic material")
	}
	f.expectHTTP(t, token, channel, `{"topic_id":"other-topic"}`, "", http.StatusBadRequest)
	args := func(ref domain.ArtifactVersionRef) string {
		return fmt.Sprintf(`{"artifact_id":%q,"version_id":%q}`, ref.ArtifactID, ref.VersionID)
	}
	fragment := f.expectHTTP(t, token, artifact, args(approved), "", http.StatusOK)
	if text := f.call(t, token, `"version"`, artifact, args(approved), false); text != fragment || !strings.Contains(text, exact) ||
		strings.Contains(text, head.VersionID) || strings.Contains(text, "current_version_id") {
		t.Fatal("transport did not return only the exact granted artifact version")
	}
	for i, ref := range []domain.ArtifactVersionRef{head, foreign} {
		f.expectHTTP(t, token, artifact, args(ref), "", http.StatusForbidden)
		if text := f.call(t, token, fmt.Sprintf(`"denied-%d"`, i), artifact, args(ref), true); !strings.Contains(text, `"code":"forbidden"`) {
			t.Fatal("artifact selector escaped the child grant")
		}
	}
	f.expectHTTP(t, token, "read_channel", `{}`, "", http.StatusForbidden)
	f.expectHTTP(t, token, "submit_result", `{"summary":"x"}`, "not-owner", http.StatusForbidden)
	if text := f.call(t, token, `"no-inbox"`, "read_work_inputs", `{}`, true); !strings.Contains(text, `"code":"forbidden"`) {
		t.Fatal("delegate could use the owner inbox")
	}
	f.stopScope(t)
	f.expectHTTP(t, token, channel, `{}`, "", http.StatusForbidden)
	if text := f.call(t, token, `"version"`, artifact, args(approved), true); !strings.Contains(text, `"code":"forbidden"`) || strings.Contains(text, exact) {
		t.Fatal("MCP returned an artifact after current B policy revocation")
	}
	tools.Revoke(token)
	f.expectHTTP(t, token, artifact, args(approved), "", http.StatusForbidden)
}

const testReportBody = `{"kind":"progress","summary":"Checked the notes"}`

func TestBoundWorkReportReadTransport(t *testing.T) {
	ctx := t.Context()
	f := newBoundWorkReportFixture(t)
	f.delegate(t, nil, nil, false)
	d := f.delegation
	child := f.claimWork(t, d.DeliveryID)
	tools := f.runtime.Tools()
	const read, write = platform.ReadWorkReportOperation, "report_work"
	if issued, err := f.runtime.IssueWorkReportRead(ctx, child.Claim); issued != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("child could acquire an owner read capability")
	}
	token, err := f.runtime.IssueWorkDelegationTools(ctx, child.Claim, nil)
	roleOK(t, err)
	defer tools.Revoke(token)
	f.expectHTTP(t, token, read, `{}`, "", http.StatusForbidden)
	for _, body := range []string{`{"kind":"progress","summary":"x","answer_to":"foreign"}`, `{"kind":"progress","summary":"\ud800"}`} {
		f.expectHTTP(t, token, write, body, "invalid", http.StatusBadRequest)
	}
	before := f.workCounts(t)
	progress := f.expectHTTP(t, token, write, testReportBody, "progress", http.StatusCreated)
	before[3]++
	if f.workCounts(t) != before || !strings.Contains(progress, `"wake_queued":false`) {
		t.Fatal("progress did not persist exactly one report without a wake")
	}
	var progressReceipt struct {
		ReportID string `json:"report_id"`
	}
	roleOK(t, json.Unmarshal([]byte(progress), &progressReceipt))
	// Exercise the complete report projection under maximal JSON escaping.
	summary := strings.Repeat("<", domain.MaxWorkReportBytes)
	body := fmt.Sprintf(`{"kind":"delivered","summary":%q}`, summary)
	text := f.call(t, token, `"delivered"`, write, body, false)
	var receipt struct {
		ReportID   string `json:"report_id"`
		WakeQueued bool   `json:"wake_queued"`
	}
	roleOK(t, json.Unmarshal([]byte(text), &receipt))
	report, err := f.engine.GetTaskWorkReport(ctx, f.group, receipt.ReportID)
	roleOK(t, err)
	wake, err := f.engine.ResolveTaskWorkDelivery(ctx, f.group, report.DeliveryID)
	roleOK(t, err)
	for i := range before {
		before[i]++
	}
	if f.workCounts(t) != before || !receipt.WakeQueued || report.TaskWorkParent != d.TaskWorkParent || report.DelegationID != d.ID ||
		report.SourceTurnID != child.Claim.TurnID || report.SourceDeliveryID != d.DeliveryID || report.SourceMessageID != d.MessageID ||
		report.ActorID != child.MemberID || wake.SourceKind != domain.TaskWorkSourceReport || wake.SourceID != report.ID ||
		wake.MemberID != f.worker.ID || wake.SenderMemberID != d.MemberID || wake.Blocked != "prior_turn_unsettled" {
		t.Fatal("waking report lost durable source, recipient, or settlement evidence")
	}
	if _, err := f.engine.BindTaskWorkContinuation(ctx, child); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("child acquired an owner Attempt: %v", err)
	}
	task, err := f.db.GetTask(ctx, f.group, d.TaskID)
	roleOK(t, err)
	if task.ActiveAttemptID != f.original.AttemptID || task.CurrentSubmissionID != "" {
		t.Fatal("the report changed the original owner Attempt or submitted a result")
	}
	f.claims(t, roleRun{claim: child.Claim}, domain.TurnStateStarting, 1)
	f.acceptWork(t, child)
	f.settleWork(t, child, true)
	f.expectStillQueued(t, report.MessageID)

	owner := f.claimWork(t, report.DeliveryID)
	if issued, err := f.runtime.IssueWorkReportRead(ctx, owner.Claim); issued != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("unbound owner acquired report authority, or denial lost: %v", err)
	}
	attempt, err := f.engine.BindTaskWorkContinuation(ctx, owner)
	roleOK(t, err)
	ownerToken, err := f.runtime.IssueWorkReportRead(ctx, owner.Claim)
	roleOK(t, err)
	defer tools.Revoke(ownerToken)
	memberMCPRoleProfile(t, f.roleRuntimeFixture, ownerToken, read)
	data := f.expectHTTP(t, ownerToken, read, `{}`, "", http.StatusOK)
	maxRPCID, err := json.Marshal(strings.Repeat("\x00", 128))
	roleOK(t, err)
	if f.call(t, ownerToken, string(maxRPCID), read, `{}`, false) != data {
		t.Fatal("MCP did not return the same complete bounded report as HTTP")
	}
	var fields map[string]any
	roleOK(t, json.Unmarshal([]byte(data), &fields))
	if len(fields) != 4 || fields["report_id"] != report.ID || fields["summary"] != report.Summary || fields["kind"] != string(report.Kind) || fields["created_at"] == nil {
		t.Fatal("owner read disclosed more or less than the exact formal report")
	}
	f.expectHTTP(t, ownerToken, read, `null`, "", http.StatusBadRequest)
	for _, id := range []string{progressReceipt.ReportID, report.ID} {
		args := fmt.Sprintf(`{"report_id":%q}`, id)
		f.expectHTTP(t, ownerToken, read, args, "", http.StatusBadRequest)
		if text := f.call(t, ownerToken, fmt.Sprintf(`"select-%s"`, id), read, args, true); !strings.Contains(text, `"code":"invalid_json"`) {
			t.Fatal("MCP let the owner select a report source")
		}
	}
	f.expectHTTP(t, ownerToken, write, testReportBody, "not-delegate", http.StatusForbidden)
	f.expectHTTP(t, ownerToken, "submit_result", `{"summary":"x"}`, "read-only", http.StatusForbidden)

	// Same group and topic, different trusted claim and ordinary capability.
	msg, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: d.TopicID, Content: "unrelated", RecipientIDs: []string{f.lead.ID}}, "third-caller")
	roleOK(t, err)
	third := f.claimWork(t, msg.Deliveries[0].ID)
	if issued, err := f.runtime.IssueWorkReportRead(ctx, third.Claim); issued != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("ordinary claim became a report owner")
	}
	if issued, err := tools.Issue(ctx, third.Claim, platform.MemberToolGrants{Operations: []string{read, write}}); issued != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("generic Issue accepted a typed report operation")
	}
	thirdToken, err := tools.Issue(ctx, third.Claim, platform.MemberToolGrants{Operations: []string{"list_members"}})
	roleOK(t, err)
	defer tools.Revoke(thirdToken)
	memberMCPRoleProfile(t, f.roleRuntimeFixture, thirdToken, "list_members")
	f.expectHTTP(t, thirdToken, read, `{}`, "", http.StatusForbidden)
	if text := f.call(t, thirdToken, `"third"`, read, `{}`, true); !strings.Contains(text, `"code":"forbidden"`) {
		t.Fatal("ordinary third caller received the report")
	}
	f.call(t, thirdToken, `"third"`, "list_members", `{}`, false) // The withheld read didn't reserve this ID.
	f.expectHTTP(t, thirdToken, write, testReportBody, "third-write", http.StatusForbidden)
	tools.Revoke(ownerToken)
	if text := f.call(t, ownerToken, `"revoked"`, read, `{}`, true); !strings.Contains(text, `"code":"forbidden"`) {
		t.Fatal("revoked capability still read a report on the live claim")
	}
	currentToken, err := f.runtime.IssueWorkReportRead(ctx, owner.Claim)
	roleOK(t, err)
	defer tools.Revoke(currentToken)
	f.stopScope(t)
	f.expectHTTP(t, currentToken, read, `{}`, "", http.StatusForbidden)
	task, err = f.db.GetTask(ctx, f.group, d.TaskID)
	roleOK(t, err)
	if task.ActiveAttemptID != attempt.ID || task.CurrentSubmissionID != "" {
		t.Fatal("report read changed the explicitly bound Attempt or submission")
	}
	f.claims(t, roleRun{claim: owner.Claim}, domain.TurnStateStarting, 1)
}

const boundWorkOrdinaryMCPLimit = 256

func TestBoundWorkReportOnlyMCPReserve(t *testing.T) {
	ctx := t.Context()
	f := newBoundWorkReportFixture(t)
	f.delegate(t, nil, nil, false)
	child := f.claimWork(t, f.delegation.DeliveryID)
	token, err := f.runtime.IssueWorkDelegationTools(ctx, child.Claim, nil)
	roleOK(t, err)
	tools := f.runtime.Tools()
	defer tools.Revoke(token)
	const op = "report_work"
	memberMCPRoleProfile(t, f.roleRuntimeFixture, token, op)

	// Match d966: seed the ordinary registry, then exercise the reserve via MCP.
	fingerprint := sha256.Sum256([]byte("ordinary request"))
	for i := 0; i < boundWorkOrdinaryMCPLimit; i++ {
		roleOK(t, tools.BindMCPRequest(token, sha256.Sum256([]byte(fmt.Sprintf("ordinary-%d", i))), fingerprint))
	}
	if text := f.call(t, token, `"not-report"`, "list_members", `{}`, true); !strings.Contains(text, `"code":"mcp_request_capacity"`) {
		t.Fatal("non-report operation reached the report reserve")
	}
	var first string
	for i := 0; i < domain.MaxWorkReportsPerDelegation-1; i++ {
		text := f.call(t, token, fmt.Sprintf(`"progress-%d"`, i), op, testReportBody, false)
		if i == 0 {
			first = text
		}
	}
	const id, delivered = `"delivery"`, `{"kind":"delivered","summary":"Evidence verified"}`
	// Real COMMIT failure after ReportWork has built the report and its wake.
	_, err = f.raw.Exec(`CREATE TABLE report_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
		CREATE TRIGGER report_commit_failure AFTER INSERT ON task_work_reports BEGIN INSERT INTO report_commit_guard VALUES('missing-group'); END`)
	roleOK(t, err)
	before := f.workCounts(t)
	if text := f.call(t, token, id, op, delivered, true); !strings.Contains(text, `"code":"internal_error"`) || !strings.Contains(text, "原幂等键") ||
		strings.Contains(text, `"wake_queued"`) || strings.Contains(text, "report_commit_guard") || f.workCounts(t) != before {
		t.Fatal("B commit uncertainty was reclassified, leaked, or treated as success")
	}
	if text := f.call(t, token, `"new-id"`, op, delivered, true); !strings.Contains(text, `"code":"mcp_request_capacity"`) {
		t.Fatal("unknown result freed its reserved MCP ID")
	}
	if text := f.call(t, token, id, op, testReportBody, true); !strings.Contains(text, "同一 JSON-RPC id") {
		t.Fatal("changed body reused the uncertain request binding")
	}
	_, err = f.raw.Exec(`DROP TRIGGER report_commit_failure`)
	roleOK(t, err)
	// This fixture checked rollback. A manual replay uses the original ID/body.
	text := f.call(t, token, id, op, delivered, false)
	var receipt struct {
		ReportID string `json:"report_id"`
	}
	roleOK(t, json.Unmarshal([]byte(text), &receipt))
	var key string
	roleOK(t, f.raw.QueryRow(`SELECT idempotency_key FROM mutation_idempotency WHERE operation='report_work' AND record_id=?`, receipt.ReportID).Scan(&key))
	for i := range before {
		before[i]++
	}
	if again := f.expectHTTP(t, token, op, delivered, key, http.StatusCreated); again != text || f.workCounts(t) != before {
		t.Fatal("cross-transport replay did not return the same single durable B result")
	}
	f.expectHTTP(t, token, op, testReportBody, key, http.StatusConflict)
	f.stopScope(t)
	before = f.workCounts(t)
	f.expectHTTP(t, token, op, testReportBody, "new-key", http.StatusConflict)
	if again := f.call(t, token, `"progress-0"`, op, testReportBody, false); again != first || f.workCounts(t) != before {
		t.Fatal("readiness loss or capacity hid a B replay on the live claim")
	}
	_, err = f.db.RequestTurnCancel(ctx, f.group, child.Claim.TurnID)
	roleOK(t, err)
	if text := f.call(t, token, id, op, delivered, true); !strings.Contains(text, `"code":"forbidden"`) {
		t.Fatal("a canceled claim replayed a report")
	}
}

func TestBoundWorkReportTransportDefaultClosed(t *testing.T) {
	f := newBoundWorkReportFixture(t)
	f.delegate(t, nil, nil, false)
	child := f.claimWork(t, f.delegation.DeliveryID)
	f.options.TaskWorkReports = nil
	var err error
	f.runtime, err = platform.NewMemberToolRuntime(f.db, f.options)
	roleOK(t, err)
	f.mount(t)
	if token, err := f.runtime.IssueWorkDelegationTools(t.Context(), child.Claim, nil); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("a backed live claim opened the default transport")
	}
	if w := f.http(t, child.Claim.Token, "report_work", testReportBody, "default-closed"); w.Code != http.StatusNotFound {
		t.Fatalf("default transport mounted report_work: %d", w.Code)
	}
	f.options.TaskWorkReports = coordination.New(nil)
	if runtime, err := platform.NewMemberToolRuntime(f.db, f.options); runtime != nil || err == nil {
		t.Fatal("report transport accepted an Engine without the registry Store")
	}
}
