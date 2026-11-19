package platform_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const (
	delegateWorkOp = platform.DelegateWorkOperation
	handbackWorkOp = platform.HandbackWorkOperation
)

func TestInvestigationOwnerManagementHTTP(t *testing.T) {
	t.Parallel()
	testInvestigationOwnerManagementHTTP(t, domain.TaskWorkSourceReport)
}

func TestInvestigationOwnerManagementHTTPOnFollowup(t *testing.T) {
	t.Parallel()
	testInvestigationOwnerManagementHTTP(t, domain.TaskWorkSourceFollowup)
}

func testInvestigationOwnerManagementHTTP(t *testing.T, kind domain.TaskWorkSourceKind) {
	ctx := t.Context()
	f := newBoundWorkReportFixture(t)
	// Both continuations start with a real delegation. Exercise the common
	// initial-owner rejection and scope checks only in the report case.
	isReport := kind == domain.TaskWorkSourceReport
	if isReport {
		var version int
		roleOK(t, f.raw.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version))
		if version != 30 {
			t.Fatalf("fixture did not open v30: %d", version)
		}
	}
	child := ownerFixtureMember(t, f, f.group, "child")
	peer := ownerFixtureMember(t, f, f.group, "second child")
	grant, publishMaterial := newOwnerFixtureMaterialPublisher(t, f)
	approved := publishMaterial(f.group)
	workerIDs := []string{child.ID, peer.ID}
	var unsupportedID, outsideID, foreignID string
	var forbidden domain.ArtifactVersionRef
	if isReport {
		unsupportedID = ownerFixtureMember(t, f, f.group, "partial transport support").ID
		outsideID = ownerFixtureMember(t, f, f.group, "outside policy").ID
		foreignID = ownerFixtureMember(t, f, f.other, "foreign").ID
		workerIDs = append(workerIDs, unsupportedID)
		forbidden = publishMaterial(f.other)
	}
	role := f.prepareInvestigationOwner(t, []domain.ArtifactVersionRef{approved}, 2, workerIDs...)
	const question = "  核对第一项。\n保留原文。🧪 "
	const firstKey = "Owner/Exact:Delegation1"
	input := domain.DelegateWorkInput{MemberID: child.ID, Question: question, AllowTopicMessages: true, ArtifactVersions: []domain.ArtifactVersionRef{approved}}
	body := ownerWorkJSON(t, input)
	const handback = `{"summary":"请协调者核对后续范围。"}`
	if isReport {
		// Reports-only opt-in does not mount either new tool or issue its purpose.
		for _, op := range []string{delegateWorkOp, handbackWorkOp} {
			if w := f.http(t, role.Member.Claim.Token, op, body, firstKey); w.Code != http.StatusNotFound {
				t.Fatalf("default owner write mounted: %s %d", op, w.Code)
			}
		}
	}
	legacy := f.runtime
	sampled := map[string][]string{}
	sample := func(m domain.Member, p provider.PiFreshInvestigationProfile) bool {
		if m.GroupID != f.group || m.ID == f.lead.ID || m.ID == f.worker.ID || m.ID == outsideID || m.ID == foreignID ||
			p.SourceKind != domain.TaskWorkSourceDelegation || p.Action != domain.ChatActionReadOnlyInvestigation ||
			len(p.Operations) == 0 || p.Operations[len(p.Operations)-1] != platform.ReportWorkOperation {
			t.Fatal("sampling escaped the bounded PolicyRevision/child profile")
		}
		if isReport {
			sampled[m.ID] = append(sampled[m.ID], strings.Join(p.Operations, "/"))
		}
		return m.ID != unsupportedID || len(p.Operations) < 2
	}
	var err error
	f.runtime, err = platform.NewMemberToolRuntimeWithInvestigationOwnerManagement(f.db, f.options, f.engine, sample)
	roleOK(t, err)
	f.mount(t)
	tools := f.runtime.Tools()
	denyInitial := func(r *platform.MemberToolRuntime, scope store.CoordinationToolScope, assets *platform.MemberArtifactGrant) {
		t.Helper()
		if token, _, err := r.IssueInitialInvestigationOwnerTools(ctx, scope, assets); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatalf("unexpected initial owner registration: %v", err)
		}
	}
	if isReport {
		denyInitial(f.runtime, role, grant) // Unbound Attempt.
	}
	role = f.bindInvestigationOwner(t, role)
	if isReport {
		denyInitial(legacy, role, grant)
		denyInitial(f.runtime, role, nil) // Cannot silently widen or shrink #200 materials.
		stale := role
		stale.Member.Claim.Token = "old-claim"
		denyInitial(f.runtime, stale, grant)
	}
	token, profile, err := f.runtime.IssueInitialInvestigationOwnerTools(ctx, role, grant)
	roleOK(t, err)
	defer tools.Revoke(token)
	candidates := []string{child.ID, peer.ID}
	slices.Sort(candidates)
	const channel, artifact = platform.ReadInvestigationChannelOperation, platform.ReadInvestigationArtifactOperation
	if isReport {
		denyInitial(f.runtime, role, grant)
		want := []string{channel, artifact, "submit_result", delegateWorkOp}
		if !slices.Equal(profile.Operations, want) || !slices.Equal(profile.DelegateCandidateIDs, candidates) ||
			!slices.Equal(sampled[child.ID], []string{"report_work", channel + "/report_work", artifact + "/report_work", channel + "/" + artifact + "/report_work"}) {
			t.Fatalf("initial token lacks exact owner/child transport profiles: %v", profile.Operations)
		}
		profile.DelegateCandidateIDs[0], profile.Operations[0] = foreignID, handbackWorkOp
		if names, err := tools.TransportOperations(ctx, token); err != nil || !slices.Equal(names, want) {
			t.Fatalf("issuer result changed registration or discovery: %v", err)
		}
	}
	f.acceptWork(t, role.Member)
	if isReport {
		if got := f.expectHTTP(t, token, channel, `{}`, "", http.StatusOK); !strings.Contains(got, f.source.ID) {
			t.Fatal("lost the initial scoped material read")
		}
		f.expectHTTP(t, token, artifact, ownerWorkJSON(t, approved), "", http.StatusOK)
		f.expectHTTP(t, token, "read_channel", `{}`, "", http.StatusForbidden)
		f.expectHTTP(t, token, handbackWorkOp, handback, "initial-handback", http.StatusForbidden)
		memberMCPRoleProfile(t, f.roleRuntimeFixture, token, "submit_result", channel, artifact) // Catalog order; no new MCP writes.
		ownerWorkRejectOrdinary(t, f, role, grant, body, handback)

		before := f.ownerWorkCounts(t)
		for _, extra := range []string{`"caller_id":"fake"`, `"group_id":"foreign"`, `"task_id":"foreign"`, `"grant":{}`, `"claim":"fake"`, `"turn_id":"fake"`, `"question":"duplicate"`} {
			f.expectHTTP(t, token, delegateWorkOp, strings.TrimSuffix(body, "}")+","+extra+"}", "bad-wire", http.StatusBadRequest)
		}
		for _, raw := range []string{`"allow_topic_messages":null`, `"artifact_versions":[null]`, `"artifact_versions":[{"artifact_id":"a","version_id":"v","grant":true}]`,
			`"artifact_versions":[{"artifact_id":"a","Version_ID":"v"}]`, `"artifact_versions":[{"artifact_id":"a","version_id":"v","version_id":"v"}]`} {
			args := fmt.Sprintf(`{"member_id":%q,"question":"x",%s}`, child.ID, raw)
			f.expectHTTP(t, token, delegateWorkOp, args, "bad-material-wire", http.StatusBadRequest)
		}
		f.expectHTTP(t, token, delegateWorkOp, strings.Replace(body, `"question"`, `"Question"`, 1), firstKey, http.StatusBadRequest)
		f.expectHTTP(t, token, delegateWorkOp, fmt.Sprintf(`{"member_id":%q,"question":"\ud800"}`, child.ID), firstKey, http.StatusBadRequest)
		for _, key := range []string{"", " " + firstKey, firstKey + ",other"} {
			f.expectHTTP(t, token, delegateWorkOp, body, key, http.StatusBadRequest)
		}
		for _, id := range []string{foreignID, outsideID, unsupportedID, f.worker.ID, f.lead.ID} {
			f.expectHTTP(t, token, delegateWorkOp, strings.Replace(body, child.ID, id, 1), firstKey, http.StatusForbidden)
		}
		in := input
		in.ArtifactVersions = []domain.ArtifactVersionRef{forbidden}
		f.expectHTTP(t, token, delegateWorkOp, ownerWorkJSON(t, in), firstKey, http.StatusForbidden) // Actual B subset fence.
		in.ArtifactVersions = []domain.ArtifactVersionRef{approved, approved}
		f.expectHTTP(t, token, delegateWorkOp, ownerWorkJSON(t, in), firstKey, http.StatusBadRequest) // B's input bound.
		if f.ownerWorkCounts(t) != before {
			t.Fatal("malformed/unauthorized write left a receipt or effect")
		}
	}

	delegated, id := f.proveOwnerWorkReceipt(t, token, delegateWorkOp, body, firstKey, isReport)
	d, err := f.engine.GetTaskWorkDelegation(ctx, f.group, id)
	roleOK(t, err)
	f.delegation = d
	var receipt map[string]any
	roleOK(t, json.Unmarshal([]byte(delegated), &receipt))
	if len(receipt) != 3 || receipt["delegation_id"] != d.ID || receipt["created_at"] == nil || receipt["wake_queued"] != true ||
		d.AuthorTurnID != role.Member.Claim.TurnID || d.AuthorAttemptID != role.AttemptID || d.MemberID != child.ID || d.Question != question ||
		d.AssignmentID != role.Provenance.AssignmentID || !d.AllowTopicMessages || !slices.Equal(d.ArtifactVersions, []domain.ArtifactVersionRef{approved}) {
		t.Fatal("delegation receipt leaked or lost the unique B effect/provenance")
	}
	if isReport {
		wake, err := f.engine.ResolveTaskWorkDelivery(ctx, f.group, d.DeliveryID)
		roleOK(t, err)
		if wake.SourceID != d.ID || wake.SourceKind != domain.TaskWorkSourceDelegation || wake.MemberID != child.ID || wake.MessageID != d.MessageID {
			t.Fatal("delegation has no matching formal wake")
		}
		f.expectHTTP(t, token, delegateWorkOp, strings.Replace(body, "核对第一项", "改变请求", 1), firstKey, http.StatusConflict)
	}
	f.settleWork(t, role.Member, true)
	if isReport {
		f.expectHTTP(t, token, delegateWorkOp, body, firstKey, http.StatusForbidden) // Still registered, but stale in B.
	}

	worker := f.claimWork(t, d.DeliveryID)
	if isReport {
		wrong := role
		wrong.Member = worker
		denyInitial(f.runtime, wrong, grant)
	}
	workerToken, err := f.runtime.IssueWorkDelegationTools(ctx, worker.Claim, grant)
	roleOK(t, err)
	defer tools.Revoke(workerToken)
	if isReport {
		if names, err := tools.TransportOperations(ctx, workerToken); err != nil || !slices.Equal(names, []string{channel, artifact, platform.ReportWorkOperation}) {
			t.Fatal("managed runtime changed the delegated child profile")
		}
		f.expectHTTP(t, workerToken, delegateWorkOp, body, firstKey, http.StatusForbidden)
		f.expectHTTP(t, workerToken, handbackWorkOp, handback, "not-owner", http.StatusForbidden)
	}
	f.acceptWork(t, worker)
	var next string
	if isReport {
		var r struct {
			ReportID string `json:"report_id"`
		}
		roleOK(t, json.Unmarshal([]byte(f.expectHTTP(t, workerToken, platform.ReportWorkOperation, testDeliveredBody, "report-http", http.StatusCreated)), &r))
		saved, err := f.engine.GetTaskWorkReport(ctx, f.group, r.ReportID)
		roleOK(t, err)
		next = saved.DeliveryID
	}
	f.settleWork(t, worker, true)
	if !isReport {
		queued, _ := queueInvestigationWorkInput(t, f, "核对原材料。", false)
		next = queued.WorkDeliveryID
	}
	ownerWorkContinuation(t, f, kind, next, grant, candidates, body, firstKey, peer.ID, handback)
}

func ownerWorkContinuation(t *testing.T, f *boundWorkReportFixture, kind domain.TaskWorkSourceKind, deliveryID string, grant *platform.MemberArtifactGrant,
	candidates []string, initialBody, initialKey, nextChildID, handback string) {
	t.Helper()
	ctx := t.Context()
	state, err := f.db.ResolveCoordinationDelivery(ctx, f.group, deliveryID)
	roleOK(t, err)
	wake, err := f.engine.ResolveTaskWorkDelivery(ctx, f.group, deliveryID)
	roleOK(t, err)
	owner := f.claimWork(t, deliveryID)
	role := store.CoordinationToolScope{Member: owner, Provenance: state.Provenance, AttemptID: f.original.AttemptID}
	deny := func(w domain.TaskWorkDelivery) {
		t.Helper()
		if token, _, err := f.runtime.IssueFreshInvestigationOwnerManagementTools(ctx, role, w, grant); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatalf("mismatched continuation acquired owner write authority: %v", err)
		}
	}
	deny(wake)
	attempt, err := f.engine.BindTaskWorkContinuation(ctx, owner)
	roleOK(t, err)
	role.AttemptID = attempt.ID
	wrong := wake
	wrong.MandateID = "different-mandate"
	deny(wrong)
	tools := f.runtime.Tools()
	if kind == domain.TaskWorkSourceReport {
		readOnly, err := f.runtime.IssueWorkReportRead(ctx, owner.Claim)
		roleOK(t, err)
		f.expectHTTP(t, readOnly, delegateWorkOp, initialBody, initialKey, http.StatusForbidden)
		f.expectHTTP(t, readOnly, handbackWorkOp, handback, "read-only-owner", http.StatusForbidden)
		tools.Revoke(readOnly)
	}
	legacy, base, err := f.runtime.IssueFreshInvestigationOwnerTools(ctx, role, wake, grant)
	roleOK(t, err)
	f.expectHTTP(t, legacy, delegateWorkOp, initialBody, initialKey, http.StatusForbidden)
	f.expectHTTP(t, legacy, handbackWorkOp, handback, "legacy-owner", http.StatusForbidden)
	tools.Revoke(legacy)

	token, profile, err := f.runtime.IssueFreshInvestigationOwnerManagementTools(ctx, role, wake, grant)
	roleOK(t, err)
	defer tools.Revoke(token)
	if !slices.Equal(profile.DelegateCandidateIDs, candidates) || !slices.Equal(profile.Operations, append(base.Operations, delegateWorkOp, handbackWorkOp)) {
		t.Fatal("management widened the typed material/report profile or lost a command")
	}
	commit := store.InvestigationTaskWorkLaunch{Scope: owner, Delivery: wake, RequestID: state.Provenance.RequestID, AttemptID: attempt.ID,
		ProviderID: state.ProviderID, Model: state.Model, ResourceKey: "report-fixture/" + deliveryID}
	admitted := false
	roleOK(t, f.db.CommitInvestigationTaskWorkLaunch(ctx, commit, func() bool { admitted = true; return true }))
	if !admitted {
		t.Fatal("fixture did not reach the separate frozen B #203 startup gate")
	}
	f.acceptWork(t, owner)
	if kind == domain.TaskWorkSourceReport {
		f.expectHTTP(t, token, platform.ReadWorkReportOperation, `{}`, "", http.StatusOK)
		f.expectHTTP(t, token, platform.ReadInvestigationChannelOperation, `{}`, "", http.StatusForbidden)
	} else {
		f.expectHTTP(t, token, platform.ReadInvestigationChannelOperation, `{}`, "", http.StatusOK)
		f.expectHTTP(t, token, platform.ReadWorkReportOperation, `{}`, "", http.StatusForbidden)
	}
	f.expectHTTP(t, token, platform.ReportWorkOperation, testReportBody, "not-the-child", http.StatusForbidden)
	f.expectHTTP(t, token, delegateWorkOp, initialBody, initialKey, http.StatusConflict) // Same owner/key, wrong Turn/Attempt.
	nextBody := ownerWorkJSON(t, domain.DelegateWorkInput{MemberID: nextChildID, Question: "核对余项。"})
	const nextKey = "Owner/Exact:Delegation2"
	second, secondID := f.proveOwnerWorkReceipt(t, token, delegateWorkOp, nextBody, nextKey, false)
	d, err := f.engine.GetTaskWorkDelegation(ctx, f.group, secondID)
	roleOK(t, err)
	if d.Ordinal != 2 || d.MemberID != nextChildID || d.AuthorTurnID != owner.Claim.TurnID || d.AuthorAttemptID != attempt.ID || d.MandateID != wake.MandateID ||
		d.AllowTopicMessages || len(d.ArtifactVersions) != 0 || d.TaskWorkParent != f.delegation.TaskWorkParent {
		t.Fatal("continuation delegation changed owner/grant or lost its B binding")
	}
	for _, raw := range []string{`{"input_id":"not-a-task","reason":"x"}`, `{"summary":"x","coordinator_member_id":"foreign"}`, `{"summary":null}`, `{"summary":"\udfff"}`} {
		f.expectHTTP(t, token, handbackWorkOp, raw, "bad-handback", http.StatusBadRequest)
	}
	const backKey = "Owner/Exact:Handback"
	before, err := f.db.GetTask(ctx, f.group, wake.TaskID)
	roleOK(t, err)
	back, id := f.proveOwnerWorkReceipt(t, token, handbackWorkOp, handback, backKey, kind == domain.TaskWorkSourceReport)
	var receipt map[string]any
	roleOK(t, json.Unmarshal([]byte(back), &receipt))
	var noticeID, summary, taskID, assignmentID, mandateID, coordinatorID, authorTurn, authorAttempt string
	roleOK(t, f.raw.QueryRow(`SELECT notice_message_id,summary,task_id,assignment_id,mandate_id,coordinator_member_id,author_turn_id,author_attempt_id
	 FROM task_work_handbacks WHERE id=?`, id).Scan(&noticeID, &summary, &taskID, &assignmentID, &mandateID, &coordinatorID, &authorTurn, &authorAttempt))
	message, err := f.db.GetMessage(ctx, f.group, noticeID)
	roleOK(t, err)
	after, err := f.db.GetTask(ctx, f.group, wake.TaskID)
	roleOK(t, err)
	if len(receipt) != 3 || receipt["handback_id"] != id || receipt["notice_message_id"] != noticeID || receipt["created_at"] == nil || len(message.Deliveries) != 0 ||
		message.SourceID != "work:handback:"+id || ownerWorkJSON(t, domain.HandbackWorkInput{Summary: summary}) != handback || !reflect.DeepEqual(before, after) ||
		taskID != wake.TaskID || assignmentID != wake.AssignmentID || mandateID != wake.MandateID || coordinatorID != f.lead.ID || authorTurn != owner.Claim.TurnID || authorAttempt != attempt.ID {
		t.Fatal("handback was not the exact whole-task, non-waking notice to the bound coordinator")
	}
	counts := f.ownerWorkCounts(t)
	f.expectHTTP(t, token, handbackWorkOp, handback, "new-handback-key", http.StatusConflict)
	f.expectHTTP(t, token, delegateWorkOp, nextBody, "over-b-budget", http.StatusConflict)
	policy, err := f.db.GetDispatchPolicy(ctx, f.group)
	roleOK(t, err)
	_, err = f.db.SetDispatchPolicy(ctx, f.group, []string{f.worker.ID}, policy.Revision, "revoke-eligible-collaborators")
	roleOK(t, err)
	if f.expectHTTP(t, token, delegateWorkOp, nextBody, nextKey, http.StatusCreated) != second ||
		f.expectHTTP(t, token, handbackWorkOp, handback, backKey, http.StatusCreated) != back || f.ownerWorkCounts(t) != counts {
		t.Fatal("current readiness preempted B same-claim replay, or replay grew effects")
	}
	_, err = f.db.RequestTurnCancel(ctx, f.group, owner.Claim.TurnID)
	roleOK(t, err)
	f.expectHTTP(t, token, delegateWorkOp, nextBody, nextKey, http.StatusForbidden)
	f.expectHTTP(t, token, handbackWorkOp, handback, backKey, http.StatusForbidden)
}

func ownerFixtureMember(t *testing.T, f *boundWorkReportFixture, group, name string) domain.Member {
	t.Helper()
	member, err := f.local.CreateMember(t.Context(), group, platform.MemberInput{Name: name, Role: "fixture", ProviderID: f.worker.ProviderID, Model: f.worker.Model}, "")
	roleOK(t, err)
	return member
}

func ownerFixtureMaterials(t *testing.T, f *boundWorkReportFixture) (*platform.MemberArtifactGrant, domain.ArtifactVersionRef, domain.ArtifactVersionRef) {
	t.Helper()
	grant, publish := newOwnerFixtureMaterialPublisher(t, f)
	return grant, publish(f.group), publish(f.other)
}

func newOwnerFixtureMaterialPublisher(t *testing.T, f *boundWorkReportFixture) (*platform.MemberArtifactGrant, func(group string) domain.ArtifactVersionRef) {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	roleOK(t, err)
	t.Cleanup(func() { root.Close() })
	blobs, err := artifactfs.New(root, 1<<20)
	roleOK(t, err)
	t.Cleanup(func() { blobs.Close() })
	artifacts, err := platform.NewArtifacts(f.db, blobs)
	roleOK(t, err)
	roleOK(t, root.WriteFile("material", []byte("scoped material"), 0600))
	publish := func(group string) domain.ArtifactVersionRef {
		t.Helper()
		v, err := artifacts.PublishArtifact(t.Context(), group, platform.PublishArtifactInput{Name: "material"}, root, "material", "")
		roleOK(t, err)
		return domain.ArtifactVersionRef{ArtifactID: v.ArtifactID, VersionID: v.ID}
	}
	grant, err := platform.NewMemberArtifactGrant(artifacts)
	roleOK(t, err)
	return grant, publish
}

func ownerWorkRejectOrdinary(t *testing.T, f *boundWorkReportFixture, role store.CoordinationToolScope, grant *platform.MemberArtifactGrant, body, handback string) {
	t.Helper()
	ctx := t.Context()
	msg, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: role.Member.TopicID, Content: "ordinary turn of another member", RecipientIDs: []string{f.lead.ID}}, "ordinary-nonowner")
	roleOK(t, err)
	member := f.claimWork(t, msg.Deliveries[0].ID)
	role.Member = member
	if token, _, err := f.runtime.IssueInitialInvestigationOwnerTools(ctx, role, grant); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("nonowner substituted for the exact original Turn")
	}
	tools := f.runtime.Tools()
	for _, op := range []string{delegateWorkOp, handbackWorkOp} {
		if token, err := tools.Issue(ctx, member.Claim, platform.MemberToolGrants{Operations: []string{op}}); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatal("generic Issue accepted an owner operation")
		}
	}
	token, err := tools.Issue(ctx, member.Claim, platform.MemberToolGrants{Operations: []string{"list_members"}})
	roleOK(t, err)
	defer tools.Revoke(token)
	f.expectHTTP(t, token, delegateWorkOp, body, "ordinary", http.StatusForbidden)
	f.expectHTTP(t, token, handbackWorkOp, handback, "ordinary", http.StatusForbidden)
	f.settleWork(t, member, false)
}

func ownerWorkJSON(t *testing.T, input any) string {
	t.Helper()
	body, err := json.Marshal(input)
	roleOK(t, err)
	return string(body)
}

func (f *boundWorkReportFixture) ownerWorkCounts(t *testing.T) [6]int {
	t.Helper()
	var n [6]int
	roleOK(t, f.raw.QueryRow(`SELECT (SELECT count(*) FROM task_work_delegations), (SELECT count(*) FROM task_work_handbacks),
	 (SELECT count(*) FROM mutation_idempotency WHERE operation IN ('delegate_work','handback_work')),
	 (SELECT count(*) FROM messages), (SELECT count(*) FROM deliveries), (SELECT count(*) FROM task_submissions)`).Scan(&n[0], &n[1], &n[2], &n[3], &n[4], &n[5]))
	return n
}

func (f *boundWorkReportFixture) proveOwnerWorkReceipt(t *testing.T, token, op, body, key string, fault bool) (string, string) {
	t.Helper()
	n := f.ownerWorkCounts(t)
	if fault {
		table := "task_work_delegations"
		if op == handbackWorkOp {
			table = "task_work_handbacks"
		}
		// Insert succeeds; actual COMMIT fails in the Store. No production
		// fault hook or replacement Store is needed in this C fixture.
		_, err := f.raw.Exec(`CREATE TABLE fixture_owner_commit(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
		 CREATE TRIGGER fixture_owner_fault AFTER INSERT ON ` + table + ` BEGIN INSERT INTO fixture_owner_commit VALUES('no-group'); END`)
		roleOK(t, err)
		text := f.expectHTTP(t, token, op, body, key, http.StatusInternalServerError)
		if !strings.Contains(text, `"code":"internal_error"`) || !strings.Contains(text, "原幂等键") || strings.Contains(text, "fixture_owner") || f.ownerWorkCounts(t) != n {
			t.Fatal("real COMMIT failure leaked, committed, or was classified as a rejection")
		}
		_, err = f.raw.Exec(`DROP TRIGGER fixture_owner_fault; DROP TABLE fixture_owner_commit`)
		roleOK(t, err)
		// Conversely the next exact-key call commits, but loses its HTTP
		// response. A separate connection must see it before reconciliation.
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/"+op, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		w := &ownerWorkLostResponse{ResponseRecorder: httptest.NewRecorder()}
		f.handler.ServeHTTP(w, r)
		if w.Code != http.StatusCreated || w.writes != 1 || w.Body.Len() != 0 {
			t.Fatal("did not exercise a lost reply after real success")
		}
	}
	var first string
	if !fault {
		first = f.expectHTTP(t, token, op, body, key, http.StatusCreated)
	}
	var id string
	roleOK(t, f.raw.QueryRow(`SELECT record_id FROM mutation_idempotency WHERE group_id=? AND actor_id=? AND operation=? AND idempotency_key=?`,
		f.group, f.worker.ID, op, key).Scan(&id))
	n[2]++
	n[3]++
	if op == delegateWorkOp {
		n[0]++
		n[4]++
	} else {
		n[1]++
	}
	replayed := f.expectHTTP(t, token, op, body, key, http.StatusCreated)
	if (first != "" && first != replayed) || f.ownerWorkCounts(t) != n {
		t.Fatal("original key did not return the unique, durable B effect")
	}
	if fault {
		t.Logf("%s: real SQLite COMMIT uncertainty and lost success; original exact key reconciled one effect", op)
	}
	return replayed, id
}

type ownerWorkLostResponse struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *ownerWorkLostResponse) Write([]byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func TestInvestigationOwnerManagementRequiresRoles(t *testing.T) {
	f := newRoleRuntime(t)
	sample := func(domain.Member, provider.PiFreshInvestigationProfile) bool { return true }
	noRoles := f.options
	noRoles.RoleMaxActive, noRoles.RoleExecutionRoot = 0, ""
	engine := coordination.New(f.db)
	for _, tc := range []struct {
		options platform.MemberToolRuntimeOptions
		engine  *coordination.Engine
		sample  platform.InvestigationDelegateSupport
	}{{noRoles, engine, sample}, {f.options, coordination.New(nil), sample}, {f.options, engine, nil}} {
		if r, err := platform.NewMemberToolRuntimeWithInvestigationOwnerManagement(f.db, tc.options, tc.engine, tc.sample); r != nil || err == nil {
			t.Fatal("owner management accepted missing trusted Roles, Store or sampler")
		}
	}
}
