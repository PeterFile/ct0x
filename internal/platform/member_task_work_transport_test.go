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
	"sync"
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
	var err error
	f.runtime, err = platform.NewMemberToolRuntimeWithTaskWorkReports(f.db, f.options, f.engine)
	roleOK(t, err)
	f.mount(t)
	return f
}

func (f *boundWorkReportFixture) memberToolRoutes() []httpapi.Route {
	tools := f.runtime.Tools()
	return slices.Concat(httpapi.MemberToolRoutes(tools), httpapi.MemberRoleToolRoutes(tools),
		httpapi.MemberInvestigationToolRoutes(tools), httpapi.MemberMCPRoutes(tools))
}

func (f *boundWorkReportFixture) mount(t *testing.T) {
	t.Helper()
	srv, err := httpapi.NewServer(httpapi.DefaultAddress, f.memberToolRoutes()...)
	roleOK(t, err)
	f.handler = srv.Handler
}

// mountMemberToolHandlers exercises the real route, bearer, JSON and B checks
// in-process. The transport/security fixtures use mount for the outer
// Host/origin guard and network deadline.
func (f *boundWorkReportFixture) mountMemberToolHandlers(ctx context.Context) {
	mux := http.NewServeMux()
	for _, route := range f.memberToolRoutes() {
		mux.HandleFunc(route.Method+" "+route.Pattern, route.Handler)
	}
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The synthetic f.http requests have no independent context. Keep
		// their SQLite work within the fixture lifetime, not a network deadline.
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
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
	names, err := tools.TransportOperations(ctx, token)
	roleOK(t, err)
	if !slices.Equal(names, []string{channel, artifact, "report_work"}) {
		t.Fatal("delegate's B subset did not produce the canonical typed order")
	}
	_, _, err = provider.PiFreshInvestigationConfig(f.options.Endpoint, token, child.Claim.TurnID, provider.PiFreshInvestigationProfile{
		Action: domain.ChatActionReadOnlyInvestigation, SourceKind: domain.TaskWorkSourceDelegation, Operations: names})
	roleOK(t, err)
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
	if text := f.call(t, thirdToken, `"third-write"`, write, testReportBody, true); !strings.Contains(text, `"code":"forbidden"`) {
		t.Fatal("ordinary token acquired report authority")
	}
	if text := f.call(t, thirdToken, `"third-write"`, "list_members", `{}`, true); !strings.Contains(text, "同一 JSON-RPC id") {
		t.Fatal("unscoped report rejection changed ordinary binding semantics")
	}
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

func TestBoundWorkReportMCPReserve(t *testing.T) {
	for _, reportFirst := range []bool{false, true} {
		name := "reads-first"
		if reportFirst {
			name = "report-first"
		}
		t.Run(name, func(t *testing.T) {
			f := newBoundWorkReportFixture(t)
			f.delegate(t, nil, nil, true)
			child := f.claimWork(t, f.delegation.DeliveryID)
			token, err := f.runtime.IssueWorkDelegationTools(t.Context(), child.Claim, nil)
			roleOK(t, err)
			tools := f.runtime.Tools()
			defer tools.Revoke(token)
			const op, channel = platform.ReportWorkOperation, platform.ReadInvestigationChannelOperation
			const firstReadID, lastReadID = `"first-read"`, `"last-read"`
			before := f.workCounts(t)
			progress := func(i int) {
				t.Helper()
				text := f.call(t, token, fmt.Sprintf(`"progress-%d"`, i), op, testReportBody, false)
				if !strings.Contains(text, `"wake_queued":false`) {
					t.Fatal("progress queued a wake")
				}
			}
			if reportFirst {
				progress(0)
			}
			page := f.expectHTTP(t, token, channel, `{}`, "", http.StatusOK)
			if got := f.call(t, token, firstReadID, channel, `{}`, false); got != page {
				t.Fatal("first scoped MCP read differs from HTTP")
			}
			// Fill only the interior ordinary slots on this token without repeating
			// the SQLite read. The first and last slots still go through MCP.
			fingerprint := sha256.Sum256([]byte("synthetic ordinary read"))
			for i := 2; i < boundWorkOrdinaryMCPLimit; i++ {
				requestID := sha256.Sum256([]byte(fmt.Sprintf("reserve-test-fill-%d", i)))
				if err := tools.BindMCPRequest(token, requestID, fingerprint); err != nil {
					t.Fatalf("bind ordinary MCP slot %d: %v", i, err)
				}
			}
			if got := f.call(t, token, lastReadID, channel, `{}`, false); got != page {
				t.Fatal("last in-budget MCP read differs from HTTP")
			}
			if text := f.call(t, token, `"extra-read"`, channel, `{}`, true); !strings.Contains(text, `"code":"mcp_request_capacity"`) {
				t.Fatalf("ordinary tool reached the report reserve: %s", text)
			}
			if got := f.call(t, token, firstReadID, channel, `{}`, false); got != page {
				t.Fatal("capacity blocked an ordinary replay")
			}
			if !reportFirst {
				progress(0)
			}
			for i := 1; i < domain.MaxWorkReportsPerDelegation-1; i++ {
				progress(i)
			}
			before[3] += domain.MaxWorkReportsPerDelegation - 1
			if text := f.call(t, token, `"excess-progress"`, op, testReportBody, true); !strings.Contains(text, `"code":"conflict"`) {
				t.Fatalf("fourth progress did not reach B's reserved-wake rule: %s", text)
			}
			f.expectHTTP(t, token, op, testReportBody, "http-excess-progress", http.StatusConflict)
			if f.workCounts(t) != before || f.reportReceipts(t) != domain.MaxWorkReportsPerDelegation-1 {
				t.Fatal("rejected progress left a report, receipt, or wake")
			}
			text := f.call(t, token, `"fresh-delivered"`, op, testDeliveredBody, false)
			key := f.reportKey(t, text)
			for i := range before {
				before[i]++
			}
			if !strings.Contains(text, `"wake_queued":true`) || f.expectHTTP(t, token, op, testDeliveredBody, key, http.StatusCreated) != text ||
				f.workCounts(t) != before || f.reportReceipts(t) != domain.MaxWorkReportsPerDelegation {
				t.Fatal("MCP did not create exactly one waking report replayable through HTTP")
			}
		})
	}
}

const testDeliveredBody = `{"kind":"delivered","summary":"Evidence verified"}`

func (f *boundWorkReportFixture) reportReceipts(t *testing.T) int {
	t.Helper()
	var count int
	roleOK(t, f.raw.QueryRow(`SELECT count(*) FROM mutation_idempotency WHERE operation='report_work'`).Scan(&count))
	return count
}

func (f *boundWorkReportFixture) reportKey(t *testing.T, text string) string {
	t.Helper()
	var receipt struct {
		ReportID string `json:"report_id"`
	}
	roleOK(t, json.Unmarshal([]byte(text), &receipt))
	var key string
	roleOK(t, f.raw.QueryRow(`SELECT idempotency_key FROM mutation_idempotency WHERE operation='report_work' AND record_id=?`, receipt.ReportID).Scan(&key))
	return key
}

func TestBoundWorkReportMCPUnknownReplay(t *testing.T) {
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

	// Leave the ordinary bucket empty: new report IDs cannot spill into it.
	var first string
	for i := 0; i < domain.MaxWorkReportsPerDelegation-1; i++ {
		text := f.call(t, token, fmt.Sprintf(`"progress-%d"`, i), op, testReportBody, false)
		if i == 0 {
			first = text
		}
	}
	if text := f.call(t, token, `"invalid-report"`, op, `{"kind":"invalid","summary":"x"}`, true); !strings.Contains(text, `"code":"invalid_input"`) {
		t.Fatalf("invalid report did not reach the handler's 400: %s", text)
	}
	const id = `"delivery"`
	restore := f.failReportCommits(t)
	before := f.workCounts(t)
	if text := f.call(t, token, id, op, testDeliveredBody, true); !strings.Contains(text, `"code":"internal_error"`) || !strings.Contains(text, "原幂等键") ||
		strings.Contains(text, `"wake_queued"`) || strings.Contains(text, "report_commit_guard") || f.workCounts(t) != before {
		t.Fatal("B commit uncertainty was reclassified, leaked, or treated as success")
	}
	if text := f.call(t, token, `"new-id"`, op, testDeliveredBody, true); !strings.Contains(text, `"code":"mcp_request_capacity"`) {
		t.Fatalf("unknown result freed its reserved MCP ID or spilled into the ordinary budget: %s", text)
	}
	if text := f.call(t, token, id, op, testReportBody, true); !strings.Contains(text, "同一 JSON-RPC id") {
		t.Fatal("changed body reused the uncertain request binding")
	}
	restore()
	// This fixture checked rollback. A manual replay uses the original ID/body.
	text := f.call(t, token, id, op, testDeliveredBody, false)
	key := f.reportKey(t, text)
	for i := range before {
		before[i]++
	}
	if again := f.expectHTTP(t, token, op, testDeliveredBody, key, http.StatusCreated); again != text || f.workCounts(t) != before {
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
	if text := f.call(t, token, id, op, testDeliveredBody, true); !strings.Contains(text, `"code":"forbidden"`) {
		t.Fatal("a canceled claim replayed a report")
	}
}

func (f *boundWorkReportFixture) failReportCommits(t *testing.T) func() {
	t.Helper()
	// Real COMMIT failure after ReportWork has prepared the report and wake.
	_, err := f.raw.Exec(`CREATE TABLE report_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
		CREATE TRIGGER report_commit_failure AFTER INSERT ON task_work_reports BEGIN INSERT INTO report_commit_guard VALUES('missing-group'); END`)
	roleOK(t, err)
	return func() {
		t.Helper()
		_, err := f.raw.Exec(`DROP TRIGGER report_commit_failure`)
		roleOK(t, err)
	}
}

func boundWorkReportMCPRequest(ctx context.Context, token, id, body string) *http.Request {
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"report_work","arguments":%s}}`, id, body)
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/mcp", strings.NewReader(payload))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	return r
}

type boundWorkMCPReply struct {
	*httptest.ResponseRecorder
	write func([]byte) (int, error)
}

func (w boundWorkMCPReply) Write(body []byte) (int, error) { return w.write(body) }

func TestBoundWorkReportMCPInFlightPolicyLoss(t *testing.T) {
	f := newBoundWorkReportFixture(t)
	f.delegate(t, nil, nil, true)
	child := f.claimWork(t, f.delegation.DeliveryID)
	token, err := f.runtime.IssueWorkDelegationTools(t.Context(), child.Claim, nil)
	roleOK(t, err)
	tools := f.runtime.Tools()
	defer tools.Revoke(token)
	const report, channel = platform.ReportWorkOperation, platform.ReadInvestigationChannelOperation
	const activeID, readID, confirmedID = `"active-delivery"`, `"confirmed-read"`, `"confirmed-progress"`
	f.call(t, token, readID, channel, `{}`, false)
	confirmed := f.call(t, token, confirmedID, report, testReportBody, false)
	key := f.reportKey(t, confirmed)
	before := f.workCounts(t)

	restore := f.failReportCommits(t)
	recorder := httptest.NewRecorder()
	entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	w := boundWorkMCPReply{recorder, func(body []byte) (int, error) {
		// The real Store transaction ended, but the first MCP response hasn't.
		close(entered)
		<-resume
		return recorder.Write(body)
	}}
	r := boundWorkReportMCPRequest(t.Context(), token, activeID, testDeliveredBody)
	go func() {
		defer close(done)
		f.handler.ServeHTTP(w, r)
	}()
	finish := sync.OnceFunc(func() { close(resume); <-done })
	defer finish()
	select {
	case <-entered:
	case <-done:
		t.Fatal("initial MCP call did not reach the response barrier")
	}
	restore()
	if f.workCounts(t) != before || f.reportReceipts(t) != 1 {
		t.Fatal("the first COMMIT fault persisted an effect")
	}
	f.stopScope(t)
	afterPolicy := f.workCounts(t)
	if text := f.call(t, token, activeID, report, testDeliveredBody, true); !strings.Contains(text, `"code":"mcp_request_in_flight"`) {
		t.Fatalf("duplicate ran before the original MCP response (and could report a later B rejection): %s", text)
	}
	mismatch := func(id, op, body string) {
		t.Helper()
		if text := f.call(t, token, id, op, body, true); !strings.Contains(text, `"code":"conflict"`) || !strings.Contains(text, "同一 JSON-RPC id") {
			t.Fatalf("same ID crossed the binding namespace: %s", text)
		}
	}
	mismatch(activeID, channel, `{}`)
	mismatch(readID, report, testDeliveredBody)
	finish()
	if text := memberMCPRoleText(t, recorder, true); !strings.Contains(text, `"code":"internal_error"`) || !strings.Contains(text, "原幂等键") {
		t.Fatalf("first response lost real commit uncertainty: %s", text)
	}
	// B now definitively rejects this *attempt*. That cannot erase the prior
	// unknown binding, or turn a different body/tool into its reconciliation.
	if text := f.call(t, token, activeID, report, testDeliveredBody, true); !strings.Contains(text, `"code":"conflict"`) || strings.Contains(text, "同一 JSON-RPC id") {
		t.Fatalf("original replay did not reach B's policy rejection: %s", text)
	}
	mismatch(activeID, report, testReportBody)
	mismatch(activeID, channel, `{}`)
	mismatch(confirmedID, channel, `{}`)
	for i := 0; i <= domain.MaxWorkReportsPerDelegation; i++ {
		if text := f.call(t, token, fmt.Sprintf(`"denied-%d"`, i), report, testDeliveredBody, true); !strings.Contains(text, `"code":"conflict"`) {
			t.Fatalf("definitive policy rejections consumed report capacity: %s", text)
		}
	}
	if f.call(t, token, confirmedID, report, testReportBody, false) != confirmed ||
		f.expectHTTP(t, token, report, testReportBody, key, http.StatusCreated) != confirmed ||
		f.workCounts(t) != afterPolicy || f.reportReceipts(t) != 1 {
		t.Fatal("a call after policy loss changed the original B receipt or effects")
	}
	tools.Revoke(token)
	if text := f.call(t, token, activeID, report, testDeliveredBody, true); !strings.Contains(text, `"code":"forbidden"`) {
		t.Fatal("revoked token reached B through an old binding")
	}
}

func TestBoundWorkReportMCPUncertainRejection(t *testing.T) {
	for _, fault := range []string{"write-error", "cancel-at-reply", "canceled-handler"} {
		t.Run(fault, func(t *testing.T) {
			f := newBoundWorkReportFixture(t)
			f.delegate(t, nil, nil, false)
			child := f.claimWork(t, f.delegation.DeliveryID)
			token, err := f.runtime.IssueWorkDelegationTools(t.Context(), child.Claim, nil)
			roleOK(t, err)
			defer f.runtime.Tools().Revoke(token)
			const report, id = platform.ReportWorkOperation, `"fourth"`
			for i := 0; i < domain.MaxWorkReportsPerDelegation-1; i++ {
				f.call(t, token, fmt.Sprintf(`"progress-%d"`, i), report, testReportBody, false)
			}
			before := f.workCounts(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			recorder := httptest.NewRecorder()
			w := boundWorkMCPReply{recorder, func(body []byte) (int, error) {
				n, err := recorder.Write(body) // Capture the attempted response for assertions.
				switch fault {
				case "write-error":
					return 0, io.ErrClosedPipe
				case "cancel-at-reply":
					cancel()
				}
				return n, err
			}}
			want := `"code":"conflict"`
			if fault == "canceled-handler" {
				cancel()
				want = `"code":"request_canceled"`
			}
			f.handler.ServeHTTP(w, boundWorkReportMCPRequest(ctx, token, id, testReportBody))
			if text := memberMCPRoleText(t, recorder, true); !strings.Contains(text, want) {
				t.Fatalf("fixture did not exercise %s: %s", fault, text)
			}
			if text := f.call(t, token, id, report, testDeliveredBody, true); !strings.Contains(text, "同一 JSON-RPC id") {
				t.Fatalf("response fault or cancellation freed the request fingerprint: %s", text)
			}
			if text := f.call(t, token, `"new-delivered"`, report, testDeliveredBody, true); !strings.Contains(text, `"code":"mcp_request_capacity"`) {
				t.Fatalf("uncertain rejection freed a report slot: %s", text)
			}
			if f.workCounts(t) != before || f.reportReceipts(t) != domain.MaxWorkReportsPerDelegation-1 {
				t.Fatal("fault or rejected replay persisted an effect")
			}
		})
	}
}

func TestBoundWorkReportTransportDefaultClosed(t *testing.T) {
	f := newBoundWorkReportFixture(t)
	f.delegate(t, nil, nil, false)
	child := f.claimWork(t, f.delegation.DeliveryID)
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
	if runtime, err := platform.NewMemberToolRuntimeWithTaskWorkReports(f.db, f.options, coordination.New(nil)); runtime != nil || err == nil {
		t.Fatal("report transport accepted an Engine without the registry Store")
	}
	withoutFence := f.options
	withoutFence.RoleMaxActive, withoutFence.RoleExecutionRoot = 0, ""
	if runtime, err := platform.NewMemberToolRuntimeWithTaskWorkReports(f.db, withoutFence, f.engine); runtime != nil || err == nil {
		t.Fatal("report transport accepted a runtime without the role fence")
	}
}
