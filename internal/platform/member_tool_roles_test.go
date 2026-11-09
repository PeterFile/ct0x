package platform

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
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
	"modernc.org/sqlite"
)

// Role fixtures use the reviewed Store admission, receipt and binding primitives.
// Neither identity nor grants gain a public/model-selectable issuance path.
type memberRoleFixture struct {
	localMessageFixture
	tools            *MemberTools
	request          domain.DispatchRequest
	coordinator      domain.Member
	worker           domain.Member
	coordinatorScope store.CoordinationToolScope
}

func memberRoleCoordinatorFixture(t *testing.T) memberRoleFixture {
	t.Helper()
	local := setupLocalMessages(t)
	ctx := context.Background()
	create := func(name string) domain.Member {
		t.Helper()
		m, err := local.local.CreateMember(ctx, local.groupID, MemberInput{Name: name,
			Role: "Untrusted descriptive text", ProviderID: "pi", Model: "selected"}, "")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	coordinator, worker := create("lead"), create("worker")
	if _, err := local.db.SetCoordinator(ctx, local.groupID, coordinator.ID, 0, "binding"); err != nil {
		t.Fatal(err)
	}
	if _, err := local.db.SetDispatchPolicy(ctx, local.groupID, []string{worker.ID}, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	source, err := local.local.CreateMessage(ctx, local.groupID, MessageInput{TopicID: "role-topic",
		Content: "请研究并交付文本结果", RecipientIDs: []string{}}, "")
	if err != nil {
		t.Fatal(err)
	}
	admission, err := local.db.RequestWork(ctx, local.groupID, domain.RequestWorkRequest{
		TopicID: source.TopicID, MessageID: source.ID, Mode: domain.RoutingDecisionCreateNew,
		Reason: "本地用户明确授权", NewTask: &domain.NewTaskRequest{Title: "研究目标", Acceptance: "有依据的文本结论"}}, "admission")
	if err != nil {
		t.Fatal(err)
	}
	current, err := local.db.GetTaskCoordination(ctx, local.groupID, admission.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := local.db.RequestDispatch(ctx, local.groupID, admission.Task.ID, admission.Task.RevisionAtAdmission,
		current.Coordination.AuthorityGeneration, "安排研究", "dispatch")
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := local.db.MaterializeDispatchRequest(ctx, request.ID, 1, time.Now().UTC())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("real coordinator materialization: %v", err)
	}
	d := materialized.Delivery
	p := store.CoordinationRoleProvenance{Kind: store.CoordinationRoleCoordinator, GroupID: request.GroupID,
		DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID,
		MemberID: coordinator.ID, TopicID: request.TopicID, RequestID: request.ID,
		TaskID: request.TaskID, TaskRevision: request.TaskRevision}
	scope := memberRoleClaimDelivery(t, local, p)
	return memberRoleFixture{localMessageFixture: local, tools: NewMemberTools(local.db), request: request,
		coordinator: coordinator, worker: worker, coordinatorScope: scope}
}

func memberRoleClaimDelivery(t *testing.T, f localMessageFixture, p store.CoordinationRoleProvenance) store.CoordinationToolScope {
	t.Helper()
	ctx := context.Background()
	queued, err := f.db.ResolveCoordinationDelivery(ctx, p.GroupID, p.DeliveryID)
	want := store.CoordinationRoleState{Provenance: p, ProviderID: "pi", Model: "selected", Blocked: store.CoordinationReady}
	if err != nil || queued != want {
		t.Fatalf("queued receipt proof: %+v, %v", queued, err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: p.GroupID, DeliveryID: p.DeliveryID,
		MaxActive: 8, ResourceKeys: []string{"role-tools-" + p.DeliveryID}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: p.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	live, err := f.db.ResolveCoordinationExecution(ctx, member, p)
	if err != nil || live != want {
		t.Fatalf("postclaim receipt proof: %+v, %v", live, err)
	}
	return store.CoordinationToolScope{Member: member, Provenance: p}
}

func (f memberRoleFixture) claimWorker(t *testing.T, decision domain.DecisionReceipt, bind bool) store.CoordinationToolScope {
	t.Helper()
	if decision.Assignment == nil {
		t.Fatal("worker fixture requires a real assign decision")
	}
	ctx := context.Background()
	materialized, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("real worker materialization: %v", err)
	}
	d := materialized.Delivery
	r := f.request
	p := store.CoordinationRoleProvenance{Kind: store.CoordinationRoleWorker, GroupID: r.GroupID,
		DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID,
		MemberID: f.worker.ID, TopicID: r.TopicID, RequestID: r.ID, TaskID: r.TaskID,
		TaskRevision: r.TaskRevision, AssignmentID: decision.Assignment.ID}
	scope := memberRoleClaimDelivery(t, f.localMessageFixture, p)
	if bind {
		binding, err := f.db.BindAssignmentAttempt(ctx, scope)
		if err != nil || binding.AssignmentID != p.AssignmentID || binding.GroupID != p.GroupID ||
			binding.TaskID != p.TaskID || binding.TaskRevision != p.TaskRevision || binding.WorkerMemberID != p.MemberID ||
			binding.TurnID != scope.Member.Claim.TurnID || binding.AttemptID == "" {
			t.Fatalf("real worker Attempt binding: %v", err)
		}
		view, err := f.db.GetAssignment(ctx, p.GroupID, p.AssignmentID)
		if err != nil || view.Binding == nil || *view.Binding != binding {
			t.Fatalf("worker binding was not persisted: %v", err)
		}
		scope.AttemptID = binding.AttemptID
	}
	return scope
}

func memberRoleIssue(t *testing.T, tools *MemberTools, scope store.CoordinationToolScope) string {
	t.Helper()
	token, err := tools.issueCoordinationRole(context.Background(), scope)
	if err != nil || token == "" || token == scope.Member.Claim.Token {
		t.Fatalf("private role issuance did not return a distinct capability: %v", err)
	}
	t.Cleanup(func() { tools.Revoke(token) })
	return token
}

// A fixed, small set of relevant persisted tables, not an alternative Store.
func memberRoleSnapshot(t *testing.T, f localMessageFixture) map[string][][]any {
	t.Helper()
	result := make(map[string][][]any)
	for _, table := range []string{"members", "messages", "message_log", "sqlite_sequence", "deliveries", "conversations", "turns",
		"resource_claims", "tasks", "task_attempts", "task_submissions", "task_submission_artifacts", "task_review_decisions",
		"coordination_dispatch_requests", "coordination_dispatch_state", "coordination_decisions", "coordination_assignments",
		"coordination_assignment_dispatch", "coordination_assignment_attempts", "mutation_idempotency"} {
		rows, err := f.raw.Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for i, value := range values {
				if data, ok := value.([]byte); ok {
					values[i] = slices.Clone(data)
				}
			}
			result[table] = append(result[table], values)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func memberRolePreserved(t *testing.T, f localMessageFixture, before map[string][][]any) {
	t.Helper()
	if after := memberRoleSnapshot(t, f); !reflect.DeepEqual(before, after) {
		for table, rows := range before {
			if !reflect.DeepEqual(rows, after[table]) {
				t.Errorf("unexpected change in %s", table)
			}
		}
		t.Fatal("role operation left a partial write, changed history, or changed execution claims")
	}
}

func memberRoleProfile(t *testing.T, tools *MemberTools, token string, scope store.CoordinationToolScope, operations []string) {
	t.Helper()
	for range 2 {
		if err := tools.withAuthorized(context.Background(), token, "read_channel", func(member store.MemberToolScope, grants MemberToolGrants) error {
			if member != scope.Member || !slices.Equal(grants.Operations, operations) || grants.Skills != nil || grants.Artifacts != nil {
				t.Error("role registration changed its scope/profile or inherited resource grants")
			}
			if tools.capabilities[sha256.Sum256([]byte(token))].skills != nil {
				t.Error("role registration allocated an ordinary skill cache")
			}
			data, err := json.Marshal([]any{tools, scope, grants})
			if err != nil || string(data) != "[{},{},{}]" {
				t.Error("role authority escaped JSON")
			}
			grants.Operations[0], member.Claim.Token = "send_message", "forged"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for _, operation := range []string{"send_message", "create_member", "search_skills", "load_skill", "read_artifact", "unknown"} {
		requireCapabilityDenied(t, capabilityUse(ctx, tools, token, operation))
	}
	message, err := tools.SendMessage(ctx, token, MemberMessageInput{Content: "不得发送"}, "denied-send")
	requireCapabilityDenied(t, err)
	if !reflect.ValueOf(message).IsZero() {
		t.Fatal("role sent a general message")
	}
	created, err := tools.CreateMember(ctx, token, MemberCreateInput{Name: "denied", Role: "role", Reason: "denied"}, "denied-create")
	requireCapabilityDenied(t, err)
	if !reflect.ValueOf(created).IsZero() {
		t.Fatal("role created a member")
	}
	skills, err := tools.SearchSkills(ctx, token, MemberSkillSearchInput{})
	requireCapabilityDenied(t, err)
	if !reflect.ValueOf(skills).IsZero() {
		t.Fatal("role inherited Skills")
	}
	loaded, err := tools.LoadSkill(ctx, token, MemberSkillLoadInput{})
	requireCapabilityDenied(t, err)
	if !reflect.ValueOf(loaded).IsZero() {
		t.Fatal("role loaded a skill")
	}
	artifact, err := tools.ReadArtifact(ctx, token, MemberArtifactReadInput{})
	memberArtifactZeroError(t, artifact, err, store.ErrMemberToolDenied)
	if scope.Provenance.Kind == store.CoordinationRoleCoordinator {
		got, err := tools.SubmitAssignmentResult(ctx, token, store.AssignmentResultInput{Summary: "denied"}, "wrong-role")
		requireCapabilityDenied(t, err)
		if !reflect.ValueOf(got).IsZero() {
			t.Fatal("coordinator acquired submit_result")
		}
	} else {
		got, err := tools.DecideDispatch(ctx, token, store.CoordinatorDecisionInput{Kind: "decline", Reason: "denied"}, "wrong-role")
		requireCapabilityDenied(t, err)
		if !reflect.ValueOf(got).IsZero() {
			t.Fatal("worker acquired decide_dispatch")
		}
	}
}

func memberRoleReads(t *testing.T, f memberRoleFixture, token string, scope store.CoordinationToolScope, label string) {
	t.Helper()
	ctx := context.Background()
	needle := label + "-needle"
	visible, err := f.local.CreateMessage(ctx, f.groupID, MessageInput{TopicID: scope.Member.TopicID,
		Content: needle, RecipientIDs: []string{}}, "")
	if err != nil {
		t.Fatal(err)
	}
	var hidden []string
	for _, location := range [][2]string{{f.groupID, "other-topic"}, {f.otherID, scope.Member.TopicID}} {
		message, err := f.local.CreateMessage(ctx, location[0], MessageInput{TopicID: location[1],
			Content: needle + " PRIVATE", RecipientIDs: []string{}}, "")
		if err != nil {
			t.Fatal(err)
		}
		hidden = append(hidden, message.ID)
	}
	foreign := f.member(t, f.otherID)
	members, err := f.tools.ListMembers(ctx, token, store.ListOptions{})
	if scope.Provenance.Kind == store.CoordinationRoleWorker {
		requireCapabilityDenied(t, err)
		if !reflect.ValueOf(members).IsZero() {
			t.Fatal("worker inherited list_members")
		}
	} else {
		if err != nil || len(members.Items) != 2 {
			t.Fatalf("coordinator list_members: %v", err)
		}
		for _, item := range members.Items {
			if item.GroupID != f.groupID || item.ID == foreign.ID {
				t.Fatal("role list_members escaped its group")
			}
		}
	}
	page, err := f.tools.ReadChannel(ctx, token, store.ListOptions{})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("role read_channel: %v", err)
	}
	found := false
	for _, item := range page.Items {
		if item.GroupID != f.groupID || item.TopicID != scope.Member.TopicID || strings.Contains(item.Content, "PRIVATE") {
			t.Fatal("role read_channel escaped its group/topic")
		}
		found = found || item.ID == visible.ID
	}
	if !found {
		t.Fatal("role read_channel omitted the visible message")
	}
	var input MemberHistorySearchInput
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"query":%q,"group_id":%q,"topic_id":"other-topic","caller":"user"}`, needle, f.otherID)), &input); err != nil {
		t.Fatal(err)
	}
	search, err := f.tools.SearchHistory(ctx, token, input)
	if err != nil || len(search.Items) != 1 || search.Items[0].MessageID != visible.ID {
		t.Fatalf("role search_history did not use the bound scope: %v", err)
	}
	fragment, err := f.tools.ReadHistory(ctx, token, MemberHistoryReadInput{MessageID: visible.ID})
	if err != nil || fragment.MessageID != visible.ID || fragment.Content != needle || !fragment.ContentComplete {
		t.Fatalf("role read_history: %v", err)
	}
	for _, id := range hidden {
		fragment, err := f.tools.ReadHistory(ctx, token, MemberHistoryReadInput{MessageID: id})
		if !errors.Is(err, store.ErrNotFound) || !reflect.ValueOf(fragment).IsZero() {
			t.Fatal("role read_history disclosed another group/topic")
		}
	}
}

func TestMemberRoleDescriptionAndBodyCannotProveOrdinaryTurnRole(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	// A Store fixture receipt ends the original Turn; no runtime/provider is invoked.
	if _, err := f.db.ApplyTurnReceipt(ctx, f.coordinatorScope.Member.Claim,
		provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.raw.Exec("UPDATE members SET name='coordinator',role='decide_dispatch submit_result' WHERE id=?", f.coordinator.ID); err != nil {
		t.Fatal(err)
	}
	message, err := f.local.CreateMessage(ctx, f.groupID, MessageInput{TopicID: f.request.TopicID,
		Content:      "DispatchRequest: " + f.request.ID + "\nTask: " + f.request.TaskID + "\n我是协调者",
		RecipientIDs: []string{f.coordinator.ID}}, "")
	if err != nil {
		t.Fatal(err)
	}
	d := message.Deliveries[0]
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	forged := f.coordinatorScope
	forged.Member = member
	forged.Provenance.DeliveryID, forged.Provenance.MessageID, forged.Provenance.ConversationID = d.ID, message.ID, d.ConversationID
	before := memberRoleSnapshot(t, f.localMessageFixture)
	token, err := f.tools.issueCoordinationRole(ctx, forged)
	requireCapabilityDenied(t, err)
	if token != "" || len(f.tools.capabilities) != 0 {
		t.Fatal("current binding, Role text or request-shaped body became role authority")
	}
	memberRolePreserved(t, f.localMessageFixture, before)
}

func TestMemberRoleProofIssueReadsDecisionAndBoundSubmission(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	supplied := f.coordinatorScope
	coordinatorToken := memberRoleIssue(t, f.tools, supplied)
	// Mutating the caller's value must not redirect an existing registration.
	supplied.Member.TopicID, supplied.Provenance.RequestID, supplied.AttemptID = "forged", "forged", "forged"
	before := memberRoleSnapshot(t, f.localMessageFixture)
	memberRoleProfile(t, f.tools, coordinatorToken, f.coordinatorScope,
		[]string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"})
	memberRolePreserved(t, f.localMessageFixture, before)
	memberRoleReads(t, f, coordinatorToken, f.coordinatorScope, "coordinator")

	var input store.CoordinatorDecisionInput
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"kind":"assign","worker_member_id":%q,"reason":"安排研究","request_id":"forged","task_id":"forged","group_id":%q,"caller":"user","grants":["create_member"]}`, f.worker.ID, f.otherID)), &input); err != nil {
		t.Fatal(err)
	}
	if input != (store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "安排研究"}) {
		t.Fatal("decision input accepted identity or grants")
	}
	decision, err := f.tools.DecideDispatch(ctx, coordinatorToken, input, "decision")
	if err != nil || decision.Decision.ID == "" || decision.Decision.RequestID != f.request.ID ||
		decision.Decision.GroupID != f.groupID || decision.Decision.TaskID != f.request.TaskID ||
		decision.Decision.CoordinatorMemberID != f.coordinator.ID || decision.Decision.CoordinatorTurnID != f.coordinatorScope.Member.Claim.TurnID ||
		decision.Assignment == nil || decision.Assignment.WorkerMemberID != f.worker.ID {
		t.Fatalf("token-only DecideDispatch did not persist the server-derived decision: %v", err)
	}
	storedDecision, err := f.db.GetDispatchDecision(ctx, f.groupID, f.request.ID)
	if err != nil || !reflect.DeepEqual(storedDecision, decision) {
		t.Fatalf("decision receipt was not persisted: %v", err)
	}
	before = memberRoleSnapshot(t, f.localMessageFixture)
	if replay, err := f.tools.DecideDispatch(ctx, coordinatorToken, input, "decision"); err != nil || !reflect.DeepEqual(replay, decision) {
		t.Fatalf("assigned coordinator replay failed: %v", err)
	}
	changedDecision := input
	changedDecision.Reason += " changed"
	if got, err := f.tools.DecideDispatch(ctx, coordinatorToken, changedDecision, "decision"); !errors.Is(err, store.ErrConflict) || !reflect.ValueOf(got).IsZero() {
		t.Fatal("changed decision payload did not return zero/conflict")
	}
	memberRolePreserved(t, f.localMessageFixture, before)

	workerScope := f.claimWorker(t, decision, true)
	supplied = workerScope
	workerToken := memberRoleIssue(t, f.tools, supplied)
	supplied.Member.Claim.Token, supplied.Provenance.AssignmentID, supplied.AttemptID = "forged", "forged", "forged"
	before = memberRoleSnapshot(t, f.localMessageFixture)
	memberRoleProfile(t, f.tools, workerToken, workerScope, []string{"read_channel", "search_history", "read_history", "submit_result"})
	memberRolePreserved(t, f.localMessageFixture, before)
	memberRoleReads(t, f, workerToken, workerScope, "worker")
	// Trusted metadata evidence does not grant the worker artifact I/O or publishing.
	version := taskLifecyclePublishMetadata(t, f.db, store.PublishArtifactInput{GroupID: f.groupID, Name: "result evidence", SourceTaskID: f.request.TaskID})
	result := store.AssignmentResultInput{Summary: "有依据的研究结果", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}}
	refs := slices.Clone(result.ArtifactVersions)
	before = memberRoleSnapshot(t, f.localMessageFixture)
	submission, err := f.tools.SubmitAssignmentResult(ctx, workerToken, result, "result")
	if err != nil || submission.ID == "" || submission.GroupID != f.groupID || submission.TaskID != f.request.TaskID ||
		submission.TaskRevision != f.request.TaskRevision || submission.AttemptID != workerScope.AttemptID ||
		submission.SubmittedBy != f.worker.ID || submission.Summary != result.Summary ||
		!slices.Equal(submission.ArtifactVersions, refs) || !slices.Equal(result.ArtifactVersions, refs) {
		t.Fatalf("bound token-only SubmitAssignmentResult: %v", err)
	}
	task, err := f.db.GetTask(ctx, f.groupID, f.request.TaskID)
	if err != nil || task.Status != domain.TaskStatusSubmitted || task.CurrentSubmissionID != submission.ID || task.ActiveAttemptID != workerScope.AttemptID {
		t.Fatalf("submission was lost or confused with acceptance: %v", err)
	}
	storedSubmission, err := f.db.GetTaskSubmission(ctx, f.groupID, f.request.TaskID, submission.ID)
	if err != nil || !reflect.DeepEqual(storedSubmission, submission) {
		t.Fatalf("submission receipt was not persisted: %v", err)
	}
	legacy := store.SubmitTaskInput{GroupID: f.groupID, TaskID: f.request.TaskID, ExpectedRevision: f.request.TaskRevision,
		AttemptID: workerScope.AttemptID, ActorID: f.worker.ID, MemberScope: &workerScope.Member,
		Summary: result.Summary, ArtifactVersions: refs, IdempotencyKey: "result"}
	if replay, err := f.db.SubmitTask(ctx, legacy); err != nil || !reflect.DeepEqual(replay, submission) {
		t.Fatalf("role wrapper changed the existing submit_task key namespace/payload: %v", err)
	}
	if after := memberRoleSnapshot(t, f.localMessageFixture); !reflect.DeepEqual(before["resource_claims"], after["resource_claims"]) {
		t.Fatal("submit_result released physical execution claims")
	}
	before = memberRoleSnapshot(t, f.localMessageFixture)
	if replay, err := f.tools.SubmitAssignmentResult(ctx, workerToken, result, "result"); err != nil || !reflect.DeepEqual(replay, submission) {
		t.Fatalf("submitted worker replay failed: %v", err)
	}
	changedResult := result
	changedResult.Summary += " changed"
	if got, err := f.tools.SubmitAssignmentResult(ctx, workerToken, changedResult, "result"); !errors.Is(err, store.ErrConflict) || !reflect.ValueOf(got).IsZero() {
		t.Fatal("changed submission payload did not return zero/conflict")
	}
	memberRolePreserved(t, f.localMessageFixture, before)

	if _, err := f.local.ReviewTask(ctx, f.groupID, f.request.TaskID, TaskReviewInput{ExpectedRevision: f.request.TaskRevision,
		SubmissionID: submission.ID, Outcome: domain.ReviewOutcomeRejected, Reason: "本地用户要求修订"}, "review"); err != nil {
		t.Fatal(err)
	}
	blocked, err := f.db.ResolveCoordinationExecution(ctx, workerScope.Member, workerScope.Provenance)
	if err != nil || blocked.Provenance != workerScope.Provenance || blocked.AttemptID != workerScope.AttemptID || blocked.Blocked != store.CoordinationTaskStale {
		t.Fatalf("rejected Task lost its historical worker identity/binding: %+v, %v", blocked, err)
	}
	if binding, err := f.db.BindAssignmentAttempt(ctx, workerScope); !errors.Is(err, store.ErrConflict) || !reflect.ValueOf(binding).IsZero() {
		t.Fatal("fixture did not distinguish strict startup Bind from historical submission identity")
	}
	if _, err := f.db.SetDispatchPolicy(ctx, f.groupID, nil, 1, "later-policy"); err != nil {
		t.Fatal(err)
	}
	f.tools.Revoke(coordinatorToken)
	f.tools.Revoke(workerToken)
	before = memberRoleSnapshot(t, f.localMessageFixture)
	coordinatorToken = memberRoleIssue(t, f.tools, f.coordinatorScope)
	workerToken = memberRoleIssue(t, f.tools, workerScope)
	if replay, err := f.tools.DecideDispatch(ctx, coordinatorToken, input, "decision"); err != nil || !reflect.DeepEqual(replay, decision) {
		t.Fatalf("private Issue/wrapper put startup readiness before decision replay: %v", err)
	}
	if replay, err := f.tools.SubmitAssignmentResult(ctx, workerToken, result, "result"); err != nil || !reflect.DeepEqual(replay, submission) {
		t.Fatalf("private Issue/wrapper called Bind/readiness before historical result replay: %v", err)
	}
	if got, err := f.tools.SubmitAssignmentResult(ctx, workerToken, changedResult, "result"); !errors.Is(err, store.ErrConflict) || !reflect.ValueOf(got).IsZero() {
		t.Fatal("current readiness displaced the historical payload conflict")
	}
	memberRolePreserved(t, f.localMessageFixture, before)
}

func TestMemberRoleDeclinedCoordinatorCanReissueAndReplay(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	token := memberRoleIssue(t, f.tools, f.coordinatorScope)
	input := store.CoordinatorDecisionInput{Kind: "decline", Reason: "暂不分配"}
	original, err := f.tools.DecideDispatch(ctx, token, input, "decline")
	if err != nil || original.Decision.Kind != domain.CoordinatorDecisionKindDecline || original.Assignment != nil || original.Decision.AssignmentID != "" {
		t.Fatalf("real decline receipt: %v", err)
	}
	if _, err := f.db.SetCoordinator(ctx, f.groupID, f.coordinator.ID, 1, "later-binding"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.raw.Exec("UPDATE members SET model='current-selection' WHERE id=?", f.coordinator.ID); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.ResolveCoordinationExecution(ctx, f.coordinatorScope.Member, f.coordinatorScope.Provenance)
	if err != nil || state.Provenance != f.coordinatorScope.Provenance || state.AttemptID != "" || state.Blocked == store.CoordinationReady || state.Model != "current-selection" {
		t.Fatalf("current selection/readiness rewrote declined provenance: %+v, %v", state, err)
	}
	f.tools.Revoke(token)
	before := memberRoleSnapshot(t, f.localMessageFixture)
	token = memberRoleIssue(t, f.tools, f.coordinatorScope)
	if replay, err := f.tools.DecideDispatch(ctx, token, input, "decline"); err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatalf("declined coordinator replay rejected by current readiness: %v", err)
	}
	changed := input
	changed.Reason += " changed"
	for _, call := range []struct {
		input store.CoordinatorDecisionInput
		key   string
	}{{changed, "decline"}, {input, "new-key"}} {
		if got, err := f.tools.DecideDispatch(ctx, token, call.input, call.key); !errors.Is(err, store.ErrConflict) || !reflect.ValueOf(got).IsZero() {
			t.Fatal("declined request accepted a changed payload/new decision")
		}
	}
	memberRolePreserved(t, f.localMessageFixture, before)
}

func memberRoleDenyIssue(t *testing.T, f memberRoleFixture, scope store.CoordinationToolScope) {
	t.Helper()
	token, err := f.tools.issueCoordinationRole(context.Background(), scope)
	requireCapabilityDenied(t, err)
	if token != "" {
		t.Fatal("invalid role proof returned a token")
	}
}

func (f memberRoleFixture) assignWorker(t *testing.T, bind bool) store.CoordinationToolScope {
	t.Helper()
	decision, err := f.db.DecideDispatch(context.Background(), f.coordinatorScope,
		store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "合法夹具"}, "fixture-decision")
	if err != nil {
		t.Fatal(err)
	}
	return f.claimWorker(t, decision, bind)
}

func TestMemberRolePrivateIssueRejectsProvenanceDrift(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	worker := f.assignWorker(t, true)
	before := memberRoleSnapshot(t, f.localMessageFixture)
	for _, scope := range []store.CoordinationToolScope{f.coordinatorScope, worker} {
		fields := reflect.TypeOf(scope.Provenance)
		for i := 0; i < fields.NumField(); i++ {
			t.Run(string(scope.Provenance.Kind)+"/"+fields.Field(i).Name, func(t *testing.T) {
				bad := scope
				field := reflect.ValueOf(&bad.Provenance).Elem().Field(i)
				if field.Kind() == reflect.String {
					field.SetString(field.String() + "-wrong")
				} else {
					field.SetInt(field.Int() + 1)
				}
				memberRoleDenyIssue(t, f, bad)
			})
		}
		f.tools.Revoke(memberRoleIssue(t, f.tools, scope))
	}
	for _, kind := range []store.CoordinationRoleKind{"", store.CoordinationRoleOrdinary} {
		bad := f.coordinatorScope
		bad.Provenance.Kind = kind
		memberRoleDenyIssue(t, f, bad)
	}
	memberRolePreserved(t, f.localMessageFixture, before)
}

func TestMemberRolePrivateIssueRequiresBoundAttempt(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	before := memberRoleSnapshot(t, f.localMessageFixture)
	bad := f.coordinatorScope
	bad.AttemptID = "coordinator-must-not-have-attempt"
	memberRoleDenyIssue(t, f, bad)
	f.tools.Revoke(memberRoleIssue(t, f.tools, f.coordinatorScope))
	memberRolePreserved(t, f.localMessageFixture, before)

	worker := f.assignWorker(t, false)
	before = memberRoleSnapshot(t, f.localMessageFixture)
	memberRoleDenyIssue(t, f, worker)
	bad = worker
	bad.AttemptID = "model-selected-attempt"
	memberRoleDenyIssue(t, f, bad)
	memberRolePreserved(t, f.localMessageFixture, before)
	view, err := f.db.GetAssignment(ctx, f.groupID, worker.Provenance.AssignmentID)
	if err != nil || view.Binding != nil {
		t.Fatalf("private Issue created a worker binding: %v", err)
	}
	binding, err := f.db.BindAssignmentAttempt(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	worker.AttemptID = binding.AttemptID
	before = memberRoleSnapshot(t, f.localMessageFixture)
	for _, attempt := range []string{"", "wrong-attempt"} {
		bad = worker
		bad.AttemptID = attempt
		memberRoleDenyIssue(t, f, bad)
	}
	f.tools.Revoke(memberRoleIssue(t, f.tools, worker))
	memberRolePreserved(t, f.localMessageFixture, before)
}

func TestMemberRolePrivateIssueRejectsLiveScopeMismatch(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	ctx := context.Background()
	worker := f.assignWorker(t, true)
	before := memberRoleSnapshot(t, f.localMessageFixture)
	for _, change := range []func(*store.MemberToolScope){
		func(m *store.MemberToolScope) { m.Claim.Token = "old-claim" },
		func(m *store.MemberToolScope) { m.Claim.GroupID = f.otherID },
		func(m *store.MemberToolScope) { m.MemberID = f.coordinator.ID },
		func(m *store.MemberToolScope) { m.ConversationID = "wrong" },
		func(m *store.MemberToolScope) { m.TopicID = "wrong" },
	} {
		bad := worker
		change(&bad.Member)
		memberRoleDenyIssue(t, f, bad)
	}
	f.tools.Revoke(memberRoleIssue(t, f.tools, worker))
	memberRolePreserved(t, f.localMessageFixture, before)

	if _, err := f.db.ApplyTurnReceipt(ctx, worker.Member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeFailed,
		Terminal: provider.OutcomeFailed, Started: true, Accepted: true, NativeSessionID: "fixture-native",
		DirectChildReaped: true, OutputComplete: true, ExitCode: 1}); err != nil {
		t.Fatal(err)
	}
	message, err := f.local.CreateMessage(ctx, f.groupID, MessageInput{Content: "ordinary second Turn",
		TopicID: worker.Member.TopicID, RecipientIDs: []string{f.worker.ID}}, "")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	bad := worker
	bad.Member = second
	before = memberRoleSnapshot(t, f.localMessageFixture)
	memberRoleDenyIssue(t, f, bad)
	memberRolePreserved(t, f.localMessageFixture, before)
}

func TestMemberRoleInvalidClaimsDenyNewCommandsAndHistoricalReplay(t *testing.T) {
	for _, scenario := range []string{"coordinator-old-claim", "coordinator-cancel-replay", "worker-unknown", "worker-recovery-replay", "worker-left"} {
		t.Run(scenario, func(t *testing.T) {
			f := memberRoleCoordinatorFixture(t)
			ctx := context.Background()
			scope := f.coordinatorScope
			if strings.HasPrefix(scenario, "worker-") {
				decision, err := f.db.DecideDispatch(ctx, scope,
					store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "合法夹具"}, "fixture-decision")
				if err != nil {
					t.Fatal(err)
				}
				scope = f.claimWorker(t, decision, true)
			}
			token := memberRoleIssue(t, f.tools, scope)
			decisionInput := store.CoordinatorDecisionInput{Kind: "decline", Reason: "决定"}
			resultInput := store.AssignmentResultInput{Summary: "结果"}
			if scenario == "coordinator-cancel-replay" {
				if _, err := f.tools.DecideDispatch(ctx, token, decisionInput, "command"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "worker-recovery-replay" {
				if _, err := f.tools.SubmitAssignmentResult(ctx, token, resultInput, "command"); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "coordinator-old-claim":
				if _, err := f.raw.Exec("UPDATE turns SET claim_token='rotated' WHERE id=?", scope.Member.Claim.TurnID); err != nil {
					t.Fatal(err)
				}
			case "coordinator-cancel-replay":
				if _, err := f.db.RequestTurnCancel(ctx, f.groupID, scope.Member.Claim.TurnID); err != nil {
					t.Fatal(err)
				}
			case "worker-unknown":
				if _, err := f.db.ApplyTurnReceipt(ctx, scope.Member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
					t.Fatal(err)
				}
			case "worker-left":
				if _, err := f.raw.Exec("UPDATE members SET status='left' WHERE id=?", scope.Member.MemberID); err != nil {
					t.Fatal(err)
				}
			case "worker-recovery-replay":
				if _, err := f.db.RecoverInFlight(ctx); err != nil {
					t.Fatal(err)
				}
			}
			before := memberRoleSnapshot(t, f.localMessageFixture)
			for _, presented := range []string{token, scope.Member.Claim.Token, "unknown-token", ""} {
				if scope.Provenance.Kind == store.CoordinationRoleCoordinator {
					got, err := f.tools.DecideDispatch(ctx, presented, decisionInput, "command")
					if !errors.Is(err, store.ErrMemberToolDenied) || !reflect.ValueOf(got).IsZero() || strings.Contains(err.Error(), scope.Member.Claim.Token) {
						t.Fatalf("invalid claim lost denial/zero DTO/sanitization: %v", err)
					}
				} else {
					got, err := f.tools.SubmitAssignmentResult(ctx, presented, resultInput, "command")
					if !errors.Is(err, store.ErrMemberToolDenied) || !reflect.ValueOf(got).IsZero() || strings.Contains(err.Error(), scope.Member.Claim.Token) {
						t.Fatalf("invalid claim lost denial/zero DTO/sanitization: %v", err)
					}
				}
			}
			page, err := f.tools.ReadChannel(ctx, token, store.ListOptions{})
			requireCapabilityDenied(t, err)
			if !reflect.ValueOf(page).IsZero() {
				t.Fatal("invalid role claim returned channel history")
			}
			fresh, err := NewMemberTools(f.db).issueCoordinationRole(ctx, scope)
			requireCapabilityDenied(t, err)
			if fresh != "" {
				t.Fatal("invalid role claim issued a new token")
			}
			memberRolePreserved(t, f.localMessageFixture, before)
		})
	}
}

func TestMemberRoleContextAndOperationBoundary(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	worker := f.assignWorker(t, true)
	coordinatorToken := memberRoleIssue(t, f.tools, f.coordinatorScope)
	workerToken := memberRoleIssue(t, f.tools, worker)
	before := memberRoleSnapshot(t, f.localMessageFixture)
	for _, deadline := range []bool{false, true} {
		requestCtx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			requestCtx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}
		cancel()
		for _, presented := range []struct {
			token string
			want  error
		}{{coordinatorToken, requestCtx.Err()}, {workerToken, store.ErrMemberToolDenied}, {"unknown", store.ErrMemberToolDenied}} {
			got, err := f.tools.DecideDispatch(requestCtx, presented.token, store.CoordinatorDecisionInput{Kind: "decline", Reason: "上下文"}, "context")
			if !errors.Is(err, presented.want) || !reflect.ValueOf(got).IsZero() {
				t.Fatalf("decision context/operation classification: %v", err)
			}
		}
		for _, presented := range []struct {
			token string
			want  error
		}{{workerToken, requestCtx.Err()}, {coordinatorToken, store.ErrMemberToolDenied}, {"unknown", store.ErrMemberToolDenied}} {
			got, err := f.tools.SubmitAssignmentResult(requestCtx, presented.token, store.AssignmentResultInput{Summary: "上下文"}, "context")
			if !errors.Is(err, presented.want) || !reflect.ValueOf(got).IsZero() {
				t.Fatalf("submission context/operation classification: %v", err)
			}
		}
		token, err := NewMemberTools(f.db).issueCoordinationRole(requestCtx, worker)
		if token != "" || !errors.Is(err, requestCtx.Err()) {
			t.Fatalf("private Issue lost request context: %v", err)
		}
	}
	memberRolePreserved(t, f.localMessageFixture, before)
}

func TestMemberRoleCommandsRevalidateFrozenProvenanceBeforeReplay(t *testing.T) {
	for _, role := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(role), func(t *testing.T) {
			f := memberRoleCoordinatorFixture(t)
			ctx := context.Background()
			scope := f.coordinatorScope
			if role == store.CoordinationRoleWorker {
				scope = f.assignWorker(t, true)
			}
			token := memberRoleIssue(t, f.tools, scope)
			call := func() (any, error) {
				if role == store.CoordinationRoleCoordinator {
					return f.tools.DecideDispatch(ctx, token, store.CoordinatorDecisionInput{Kind: "decline", Reason: "保存决定"}, "original")
				}
				return f.tools.SubmitAssignmentResult(ctx, token, store.AssignmentResultInput{Summary: "保存结果"}, "original")
			}
			if _, err := call(); err != nil {
				t.Fatal(err)
			}
			// Counterfactual negative damage only, after a real authorized command.
			if _, err := f.raw.Exec("DROP TRIGGER coordination_dispatch_requests_no_update"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.raw.Exec("UPDATE coordination_dispatch_requests SET topic_id='damaged-topic' WHERE id=?", f.request.ID); err != nil {
				t.Fatal(err)
			}
			before := memberRoleSnapshot(t, f.localMessageFixture)
			got, err := call()
			if !errors.Is(err, store.ErrMemberToolDenied) || !reflect.ValueOf(got).IsZero() || strings.Contains(err.Error(), scope.Member.Claim.Token) {
				t.Fatalf("damaged provenance lost denial/zero DTO/sanitization: %v", err)
			}
			memberRolePreserved(t, f.localMessageFixture, before)
		})
	}
}

func TestMemberRoleCommandCommitFailurePreservesError(t *testing.T) {
	for _, role := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(role), func(t *testing.T) {
			f := memberRoleCoordinatorFixture(t)
			scope, operation, table := f.coordinatorScope, "decide_dispatch", "coordination_decisions"
			if role == store.CoordinationRoleWorker {
				scope, operation, table = f.assignWorker(t, true), "submit_result", "task_submissions"
			}
			token := memberRoleIssue(t, f.tools, scope)
			if _, err := f.raw.Exec("CREATE TABLE role_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.raw.Exec("CREATE TRIGGER role_commit_fault AFTER INSERT ON " + table + " BEGIN INSERT INTO role_commit_guard VALUES('PRIVATE-ROLE-MISSING-GROUP'); END"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var result any
			var storeErr error
			err := f.tools.withCoordinationRole(ctx, token, operation, role, func(frozen store.CoordinationToolScope) error {
				if role == store.CoordinationRoleCoordinator {
					result, storeErr = f.db.DecideDispatch(ctx, frozen, store.CoordinatorDecisionInput{Kind: "decline", Reason: "暂不分配"}, "commit-failure")
				} else {
					result, storeErr = f.db.SubmitAssignmentResult(ctx, frozen, store.AssignmentResultInput{Summary: "结果"}, "commit-failure")
				}
				// Cancel only after the real Store COMMIT failure, before wrapper handling.
				cancel()
				return storeErr
			})
			var storeCause *sqlite.Error
			if storeErr == nil || !reflect.ValueOf(result).IsZero() || !errors.As(storeErr, &storeCause) || storeCause.Code() != 787 ||
				errors.Is(storeErr, context.Canceled) || ctx.Err() != context.Canceled {
				t.Fatalf("expected zero DTO and an actual deferred foreign-key COMMIT failure before cancellation: %v", storeErr)
			}
			if role == store.CoordinationRoleWorker {
				var outcome interface {
					CommitOutcomeUnknown() bool
					RollbackConfirmed() bool
				}
				if !errors.As(storeErr, &outcome) || !outcome.CommitOutcomeUnknown() || outcome.RollbackConfirmed() {
					t.Fatalf("Store COMMIT failure lost its unknown outcome: %v", storeErr)
				}
				var returnedOutcome interface {
					CommitOutcomeUnknown() bool
					RollbackConfirmed() bool
				}
				if !errors.As(err, &returnedOutcome) || !returnedOutcome.CommitOutcomeUnknown() || returnedOutcome.RollbackConfirmed() {
					t.Errorf("command wrapper erased the actual unknown outcome: %v", err)
				}
			}
			// The coordinator's unknown marker is private; preserve the entire original object.
			if err != storeErr || !errors.Is(err, storeErr) || !errors.Is(err, storeCause) {
				t.Errorf("command wrapper replaced the original Store COMMIT error: got %v, want original %v", err, storeErr)
			}
			var returnedCause *sqlite.Error
			if !errors.As(err, &returnedCause) || returnedCause != storeCause || returnedCause.Code() != 787 {
				t.Errorf("command wrapper lost the original SQLite 787 cause: %v", err)
			}
			t.Logf("real Store COMMIT failure: sqlite_code=%d; canceled after Store returned", storeCause.Code())
		})
	}
}

func TestMemberRoleCommandPostWriteDenialPreservesRollback(t *testing.T) {
	for _, cancelAfterStore := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel_after_store=%t", cancelAfterStore), func(t *testing.T) {
			f := memberRoleCoordinatorFixture(t)
			worker := f.assignWorker(t, true)
			token := memberRoleIssue(t, f.tools, worker)
			query := fmt.Sprintf("CREATE TRIGGER role_rotate_claim AFTER INSERT ON task_submissions BEGIN UPDATE turns SET claim_token='PRIVATE-ROLE-ROTATED' WHERE id='%s'; END", worker.Member.Claim.TurnID)
			if _, err := f.raw.Exec(query); err != nil {
				t.Fatal(err)
			}
			before := memberRoleSnapshot(t, f.localMessageFixture)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var result domain.Submission
			var storeErr error
			err := f.tools.withCoordinationRole(ctx, token, "submit_result", store.CoordinationRoleWorker, func(frozen store.CoordinationToolScope) error {
				result, storeErr = f.db.SubmitAssignmentResult(ctx, frozen, store.AssignmentResultInput{Summary: "结果"}, "post-write-denial")
				if cancelAfterStore {
					cancel()
				}
				return storeErr
			})
			var proof interface {
				RollbackConfirmed() bool
				CommitOutcomeUnknown() bool
			}
			if !reflect.ValueOf(result).IsZero() || !errors.Is(storeErr, store.ErrMemberToolDenied) ||
				!errors.As(storeErr, &proof) || !proof.RollbackConfirmed() || proof.CommitOutcomeUnknown() {
				t.Fatalf("expected an actual post-write denial with confirmed rollback and zero DTO: %v", storeErr)
			}
			memberRolePreserved(t, f.localMessageFixture, before)
			if err != storeErr || !errors.Is(err, storeErr) || !errors.Is(err, store.ErrMemberToolDenied) || errors.Is(err, context.Canceled) {
				t.Errorf("command wrapper replaced the original post-write denial: got %v, want original %v", err, storeErr)
			}
			var returnedProof interface {
				RollbackConfirmed() bool
				CommitOutcomeUnknown() bool
			}
			if !errors.As(err, &returnedProof) || !returnedProof.RollbackConfirmed() || returnedProof.CommitOutcomeUnknown() {
				t.Errorf("command wrapper erased confirmed rollback evidence: %v", err)
			}
			t.Logf("real post-write denial: rollback_confirmed=%t; no residual writes", proof.RollbackConfirmed())
		})
	}
}

func TestMemberRoleCommandSQLFailuresReturnZeroAndRollback(t *testing.T) {
	for _, role := range []store.CoordinationRoleKind{store.CoordinationRoleCoordinator, store.CoordinationRoleWorker} {
		t.Run(string(role), func(t *testing.T) {
			f := memberRoleCoordinatorFixture(t)
			ctx := context.Background()
			scope := f.coordinatorScope
			table := "coordination_assignments"
			if role == store.CoordinationRoleWorker {
				decision, err := f.db.DecideDispatch(ctx, scope,
					store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "合法夹具"}, "fixture-decision")
				if err != nil {
					t.Fatal(err)
				}
				scope = f.claimWorker(t, decision, true)
				table = "task_submissions"
			}
			token := memberRoleIssue(t, f.tools, scope)
			// Counterfactual failure after a real INSERT, never a positive authorization fixture.
			if _, err := f.raw.Exec("CREATE TRIGGER role_command_fault AFTER INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'PRIVATE-ROLE-FAULT'); END"); err != nil {
				t.Fatal(err)
			}
			before := memberRoleSnapshot(t, f.localMessageFixture)
			call := func() (any, error) {
				if role == store.CoordinationRoleCoordinator {
					return f.tools.DecideDispatch(ctx, token, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "决定"}, "retry-same-key")
				}
				return f.tools.SubmitAssignmentResult(ctx, token, store.AssignmentResultInput{Summary: "结果"}, "retry-same-key")
			}
			got, err := call()
			var cause *sqlite.Error
			if err == nil || !reflect.ValueOf(got).IsZero() || !errors.As(err, &cause) ||
				strings.Contains(err.Error(), "PRIVATE-ROLE-FAULT") || strings.Contains(err.Error(), scope.Member.Claim.Token) {
				t.Fatalf("role wrapper lost zero-result/SQL-cause/sanitization semantics: %v", err)
			}
			memberRolePreserved(t, f.localMessageFixture, before)
			if _, err := f.raw.Exec("DROP TRIGGER role_command_fault"); err != nil {
				t.Fatal(err)
			}
			if _, err := call(); err != nil {
				t.Fatalf("failed role command burned its key: %v", err)
			}
		})
	}
}
