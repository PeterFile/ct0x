package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestChatInvestigationScopedReadsAndFormalReview(t *testing.T) {
	for _, name := range []string{"artifact-and-topic", "artifact-only"} {
		t.Run(name, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			input := artifactPublishInput(f.groupID)
			base := mustPublishArtifact(t, f.s, input)
			input.ArtifactID, input.ExpectedVersionID, input.Name = base.ArtifactID, base.ID, ""
			head := mustPublishArtifact(t, f.s, input)
			approved := mustPublishArtifact(t, f.s, input) // Stale base: observed head differs from expected base.
			if approved.Applied || approved.ObservedVersionID != head.ID || approved.ExpectedVersionID != base.ID {
				t.Fatalf("fixture did not produce conflict metadata: %+v", approved)
			}
			ref := domain.ArtifactVersionRef{ArtifactID: approved.ArtifactID, VersionID: approved.ID}
			config := domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: name == "artifact-and-topic", ArtifactVersions: []domain.ArtifactVersionRef{ref}, MaxDelegations: 2}
			allowed, excluded, wantVersion := base.ID, head.ID, approved
			wantVersion.ObservedVersionID = ""
			if config.AllowTopicMessages {
				allowed, excluded, wantVersion = head.ID, base.ID, approved
				wantVersion.ExpectedVersionID = ""
			}
			config.ArtifactVersions = append(config.ArtifactVersions, domain.ArtifactVersionRef{ArtifactID: ref.ArtifactID, VersionID: allowed})
			policy := investigationPolicy(t, f, config, 2)
			f.s.EnableInvestigationExecution()
			// Capture a real member suggestion, not a user-labelled message body.
			message, err := f.s.SendMessageForTool(ctx, f.scope, MemberToolMessageInput{Content: "建议仅按配置调查。", RecipientIDs: []string{f.b}}, "peer-investigation")
			if err != nil {
				t.Fatal(err)
			}
			investigationFinishTurn(t, f)
			investigationBindCandidate(t, &f, message.Deliveries[0])
			f.input = investigationProposal()
			role := investigationBindWorker(t, f)
			id := role.Provenance.TaskID
			receipt, err := f.s.GetChatAdmission(ctx, f.groupID, id)
			if err != nil || receipt.Action != domain.ChatActionReadOnlyInvestigation || receipt.SourceKind != "member" || receipt.SourceID != message.SourceID || receipt.SourceSenderID != f.a ||
				receipt.Authority != "written_preauthorization" || receipt.AuthorizationMessageID == message.ID || !receipt.AuthorityEvidenceVerified {
				t.Fatalf("wrong member intake or written authority: %+v, %v", receipt, err)
			}
			authorization, err := f.s.GetMessage(ctx, f.groupID, receipt.AuthorizationMessageID)
			if err != nil || authorization.SourceKind != "local_user_policy" || authorization.SourceID != policy.ConfigurationSourceID {
				t.Fatalf("config did not authorize this work: %+v, %v", authorization, err)
			}
			want := domain.TaskInvestigationScope{ThreadRef: policy.ThreadRef, TaskID: id, TaskRevision: role.Provenance.TaskRevision,
				MandateID: receipt.MandateID, PolicyRevision: policy.Revision, ConfigurationSourceID: policy.ConfigurationSourceID, ChatInvestigationPolicy: config}
			if scope, err := f.s.GetTaskInvestigationScope(ctx, f.groupID, id); err != nil || !reflect.DeepEqual(scope, want) {
				t.Fatalf("wrong scope: %+v, %v", scope, err)
			}
			view, err := f.s.GetTaskCoordination(ctx, f.groupID, id)
			if err != nil {
				t.Fatal(err)
			}
			mandate := view.Admission.Mandate
			if mandate.ID != want.MandateID || mandate.Scope.Objective != config.Objective || !strings.Contains(mandate.Scope.Description, approved.ID) ||
				!strings.Contains(mandate.Scope.Description, policy.ConfigurationSourceID) || !strings.Contains(mandate.Scope.Acceptance, "纯文本") {
				t.Fatalf("model replaced the user's objective, materials or acceptance: %+v", mandate)
			}
			if dispatch, err := f.s.GetDispatchRequest(ctx, f.groupID, receipt.DispatchRequestID); err != nil || dispatch.Request.Reason != "按本机用户的当前只读调查策略准备文本交付。" {
				t.Fatalf("dispatch lost the server's reason: %+v, %v", dispatch.Request, err)
			}
			if _, err := f.s.GetTaskInvestigationScope(ctx, f.otherID, id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign scope read: %v", err)
			}
			if err := f.s.CheckTaskInvestigationReady(ctx, f.groupID, id); err != nil {
				t.Fatalf("admission not ready: %v", err)
			}
			reopened := queueHandle(t, f.path, false)
			if saved, err := reopened.GetTaskInvestigationScope(ctx, f.groupID, id); err != nil || !reflect.DeepEqual(saved, want) {
				t.Fatalf("reopen lost scope: %+v, %v", saved, err)
			}
			if err := reopened.CheckTaskInvestigationReady(ctx, f.groupID, id); !errors.Is(err, ErrTaskInvestigationNotReady) {
				t.Fatalf("startup opt-in became data or was shared across stores: %v", err)
			}
			if state, err := f.s.ResolveCoordinationExecution(ctx, role.Member, role.Provenance); !errors.Is(err, ErrMemberToolDenied) || state != (CoordinationRoleState{}) {
				t.Fatalf("investigation fell back to generic worker: %+v, %v", state, err)
			}
			if state, err := f.s.ResolveInvestigationExecution(ctx, role.Member, role.Provenance); err != nil || state.AttemptID != role.AttemptID || state.Blocked != CoordinationReady {
				t.Fatalf("ready investigation role: %+v, %v", state, err)
			}
			if _, err := reopened.ResolveInvestigationExecution(ctx, role.Member, role.Provenance); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("unenabled Store resolved an investigation role: %v", err)
			}
			if a, v, err := f.s.ReadInvestigationArtifact(ctx, role, ref.ArtifactID, ref.VersionID); err != nil || v != wantVersion || a.ID != approved.ArtifactID || a.CurrentVersionID != "" {
				t.Fatalf("investigation must retain only allowlisted version links: %+v %+v, %v", a, v, err)
			}
			if v, err := f.s.GetArtifactVersion(ctx, f.groupID, ref.ArtifactID, ref.VersionID); err != nil || v != approved {
				t.Fatalf("product Artifact read lost conflict metadata: %+v, %v", v, err)
			}
			otherArtifact := mustPublishArtifact(t, f.s, artifactPublishInput(f.groupID))
			forged := role
			forged.Provenance.TaskID = "another-task"
			wrongClaim := role
			wrongClaim.Member.Claim.Token += "wrong"
			for _, tc := range []struct {
				store             *Store
				role              CoordinationToolScope
				artifact, version string
			}{
				{f.s, role, ref.ArtifactID, excluded}, {f.s, role, otherArtifact.ArtifactID, ref.VersionID},
				{f.s, forged, ref.ArtifactID, ref.VersionID}, {f.s, wrongClaim, ref.ArtifactID, ref.VersionID}, {reopened, role, ref.ArtifactID, ref.VersionID},
			} {
				if a, v, err := tc.store.ReadInvestigationArtifact(ctx, tc.role, tc.artifact, tc.version); !errors.Is(err, ErrMemberToolDenied) || a.ID != "" || v.ID != "" {
					t.Fatalf("unapproved material disclosed: %+v %+v, %v", a, v, err)
				}
			}
			for _, thread := range []domain.ThreadRef{{GroupID: f.groupID, TopicID: "another-topic"}, {GroupID: f.otherID, TopicID: role.Member.TopicID}} {
				chatLocalMessage(t, f.s, MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: "不可跨范围读取"}, nil)
			}
			page, err := f.s.ReadInvestigationChannel(ctx, role, ListOptions{})
			if !config.AllowTopicMessages {
				if !errors.Is(err, ErrMemberToolDenied) || len(page.Items) != 0 {
					t.Fatalf("artifact-only profile exposed topic messages: %+v, %v", page, err)
				}
				binding, err := f.s.BindAssignmentAttempt(ctx, role)
				if err != nil || binding.AttemptID != role.AttemptID {
					t.Fatalf("current binding replay: %+v, %v", binding, err)
				}
				investigationDisablePolicy(t, f.s, policy)
				if _, err := f.s.ResolveInvestigationExecution(ctx, role.Member, role.Provenance); !errors.Is(err, ErrMemberToolDenied) {
					t.Fatalf("revoked role resolved: %v", err)
				}
				if a, v, err := f.s.ReadInvestigationArtifact(ctx, role, ref.ArtifactID, ref.VersionID); !errors.Is(err, ErrMemberToolDenied) || a.ID != "" || v.ID != "" {
					t.Fatalf("stale policy allowed reads under an otherwise live claim: %+v %+v %v", a, v, err)
				}
				before := coordinationDecideSnapshot(t, f.s)
				if got, err := f.s.BindAssignmentAttempt(ctx, role); err != nil || got != binding {
					t.Fatalf("revocation displaced exact binding replay: %+v, %v", got, err)
				}
				coordinationDecidePreserved(t, f.s, before)
				return
			}
			if err != nil || len(page.Items) == 0 {
				t.Fatalf("approved channel material omitted: %+v, %v", page, err)
			}
			for _, item := range page.Items {
				if item.GroupID != want.GroupID || item.TopicID != want.TopicID {
					t.Fatalf("channel exposed foreign material: %+v", item)
				}
			}
			investigationSubmitAndReview(t, f, role, ref, policy)
			if err := f.s.CheckTaskInvestigationReady(ctx, f.groupID, id); !errors.Is(err, ErrTaskInvestigationNotReady) {
				t.Fatalf("reviewed task still ready for execution: %v", err)
			}
			if scope, err := f.s.GetTaskInvestigationScope(ctx, f.groupID, id); err != nil || !reflect.DeepEqual(scope, want) {
				t.Fatalf("review changed historical scope: %+v, %v", scope, err)
			}
			dispatchAssertFK(t, f.s)
		})
	}
}

func investigationDisablePolicy(t *testing.T, s *Store, policy domain.ChatPolicy) {
	t.Helper()
	input := policy.ChatPolicyRequest
	input.Enabled, input.PreauthorizationMessageID = false, ""
	if _, err := s.SetChatPolicy(ctx, policy.ThreadRef, input, policy.Revision, "disable-live"); err != nil {
		t.Fatal(err)
	}
}

func investigationUserCandidate(t *testing.T) (chatFixture, domain.ChatPolicy) {
	t.Helper()
	f := chatSetup(t, "user", true)
	config := domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}
	policy := investigationPolicy(t, f, config, 2)
	f.s.EnableInvestigationExecution()
	chatNextCandidate(t, &f, true)
	f.input = investigationProposal()
	return f, policy
}

func investigationUserWorker(t *testing.T) (chatFixture, domain.ChatPolicy, CoordinationToolScope) {
	t.Helper()
	f, policy := investigationUserCandidate(t)
	return f, policy, investigationBindWorker(t, f)
}

func investigationTaskSubmitInput(role CoordinationToolScope, input AssignmentResultInput, key string) SubmitTaskInput {
	p := role.Provenance
	return SubmitTaskInput{GroupID: p.GroupID, TaskID: p.TaskID, AttemptID: role.AttemptID, ExpectedRevision: p.TaskRevision,
		ActorID: role.Member.MemberID, MemberScope: &role.Member, Summary: input.Summary, ArtifactVersions: input.ArtifactVersions, IdempotencyKey: key}
}

func TestChatInvestigationMissingMigrationMarkerFailsClosed(t *testing.T) {
	// Materialize the exact v27 prefix before testing the missing marker.
	f, role := func() (chatFixture, CoordinationToolScope) {
		prior := migrations
		defer func() { migrations = prior }()
		migrations = prior[:27]
		f, _, role := investigationUserWorker(t)
		return f, role
	}()
	messageSequenceLedger(t, f.s, 27)
	id := role.Provenance.TaskID
	if scope, err := f.s.GetTaskInvestigationScope(ctx, f.groupID, id); err != nil || scope.TaskID != id {
		t.Fatalf("fixture does not have an investigation scope: %+v, %v", scope, err)
	}
	if ready, err := f.s.ResolveInvestigationExecution(ctx, role.Member, role.Provenance); err != nil || ready.Blocked != CoordinationReady || ready.AttemptID != role.AttemptID {
		t.Fatalf("fixture does not have a live investigation worker: %+v, %v", ready, err)
	}
	coordinationDecideExec(t, f.s, "DELETE FROM schema_migrations WHERE version=27")
	if state, err := f.s.ResolveCoordinationExecution(ctx, role.Member, role.Provenance); !errors.Is(err, ErrMemberToolDenied) || state != (CoordinationRoleState{}) {
		t.Fatalf("a missing marker exposed a generic worker: %+v, %v", state, err)
	}
	if scope, err := f.s.GetTaskInvestigationScope(ctx, f.groupID, id); !errors.Is(err, ErrTaskInvestigationNotReady) || scope.TaskID != "" {
		t.Fatalf("a missing marker was treated as legacy absence: %+v, %v", scope, err)
	}
}

func TestChatInvestigationSubmitRejectsSupersededPolicy(t *testing.T) {
	f, policy, role := investigationUserWorker(t)
	// Even an enabled replacement with identical scope cannot authorize the old Task.
	replacement := policy.ChatPolicyRequest
	replacement.PreauthorizationMessageID = ""
	if _, err := f.s.SetChatPolicy(ctx, policy.ThreadRef, replacement, policy.Revision, "replace-policy"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CheckTaskInvestigationReady(ctx, f.groupID, role.Provenance.TaskID); !errors.Is(err, ErrTaskInvestigationNotReady) {
		t.Fatalf("fixture still has current policy: %v", err)
	}
	input := AssignmentResultInput{Summary: "按原策略完成的结论。"}
	before := messageSequenceSnapshot(t, f.s)
	if result, err := f.s.SubmitAssignmentResult(ctx, role, input, "stale-submit"); !errors.Is(err, ErrMemberToolDenied) || result.ID != "" {
		t.Fatalf("stale investigation submission: id=%s err=%v", result.ID, err)
	}
	if result, err := f.s.SubmitTask(ctx, investigationTaskSubmitInput(role, input, "stale-direct")); !errors.Is(err, ErrMemberToolDenied) || result.ID != "" {
		t.Fatalf("direct SubmitTask bypassed investigation authority: id=%s err=%v", result.ID, err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatInvestigationSubmitRechecksAfterReservation(t *testing.T) {
	f, _, role := investigationUserWorker(t)
	id := role.Provenance.TaskID
	input := AssignmentResultInput{Summary: "在有效授权下提交。"}
	before := messageSequenceSnapshot(t, f.s)
	reopened := queueHandle(t, f.path, false)
	if result, err := reopened.SubmitAssignmentResult(ctx, role, input, "submit"); !errors.Is(err, ErrMemberToolDenied) || result.ID != "" {
		t.Fatalf("reopen did not restore default-off execution: id=%s err=%v", result.ID, err)
	}
	// Drift the generation as a side effect of the new reservation. A read before
	// reserveMutation would have seen the old authorization and missed this change.
	coordinationDecideExec(t, f.s, `CREATE TRIGGER investigation_submit_drift AFTER INSERT ON mutation_idempotency
	 WHEN NEW.operation='submit_task' AND NEW.idempotency_key='submit'
	 BEGIN UPDATE task_coordination SET authority_generation=authority_generation+1; END`)
	if result, err := f.s.SubmitAssignmentResult(ctx, role, input, "submit"); !errors.Is(err, ErrMemberToolDenied) || result.ID != "" {
		t.Fatalf("authority drift after reserve was not denied: id=%s err=%v", result.ID, err)
	}
	messageSequencePreserved(t, f.s, before)
	if err := f.s.CheckTaskInvestigationReady(ctx, f.groupID, id); err != nil {
		t.Fatalf("failed submission did not roll back authority/task changes: %v", err)
	}
	coordinationDecideExec(t, f.s, `DROP TRIGGER investigation_submit_drift`)
	coordinationDecideExec(t, f.s, `CREATE TRIGGER investigation_notice_drift AFTER INSERT ON chat_submission_notices
	 BEGIN UPDATE chat_policies SET revision=revision-1
	 WHERE (group_id,topic_id)=(SELECT group_id,topic_id FROM messages WHERE id=NEW.message_id); END`)
	if result, err := f.s.SubmitAssignmentResult(ctx, role, input, "submit"); !errors.Is(err, ErrMemberToolDenied) || result.ID != "" {
		t.Fatalf("policy drift during the notice write was not denied: id=%s err=%v", result.ID, err)
	}
	messageSequencePreserved(t, f.s, before)
	coordinationDecideExec(t, f.s, `DROP TRIGGER investigation_notice_drift`)
	if result, err := f.s.SubmitAssignmentResult(ctx, role, input, "submit"); err != nil || result.ID == "" {
		t.Fatalf("retry after rollback: %+v %v", result, err)
	}
}

func investigationSubmitAndReview(t *testing.T, f chatFixture, role CoordinationToolScope, ref domain.ArtifactVersionRef, policy domain.ChatPolicy) {
	t.Helper()
	publication := artifactPublishInput(f.groupID)
	publication.SourceTaskID = role.Provenance.TaskID
	if v, err := f.s.PublishArtifact(ctx, publication); err == nil || v.ID != "" {
		t.Fatalf("investigation allowed artifact publication: %+v, %v", v, err)
	}
	for _, bad := range []AssignmentResultInput{
		{Summary: "文本", ArtifactVersions: []domain.ArtifactVersionRef{ref}},
		{Summary: "　 "}, {Summary: "包含\xff"}, {Summary: strings.Repeat("字", 2731)},
	} {
		if sub, err := f.s.SubmitAssignmentResult(ctx, role, bad, "submit"); !errors.Is(err, ErrInvalid) || sub.ID != "" {
			t.Fatalf("nonconforming result accepted: %+v, %v", sub, err)
		}
	}
	text := AssignmentResultInput{Summary: strings.Repeat("字", 2730) + "ok"}
	wrong := role
	wrong.AttemptID = "other-attempt"
	if _, err := f.s.SubmitAssignmentResult(ctx, wrong, text, "submit"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("wrong attempt submitted result: %v", err)
	}
	sub, err := f.s.SubmitAssignmentResult(ctx, role, text, "submit")
	if err != nil || sub.Summary != text.Summary || sub.AttemptID != role.AttemptID || len(sub.ArtifactVersions) != 0 {
		t.Fatalf("pure text result was not submitted: %+v, %v", sub, err)
	}
	if task, err := f.s.GetTask(ctx, f.groupID, sub.TaskID); err != nil || task.Status != domain.TaskStatusSubmitted {
		t.Fatalf("submission bypassed formal review: %+v, %v", task, err)
	}
	if replay, err := f.s.SubmitAssignmentResult(ctx, role, text, "submit"); err != nil || !reflect.DeepEqual(replay, sub) {
		t.Fatalf("current policy submission replay: %+v, %v", replay, err)
	}
	review, err := f.s.ReviewTask(ctx, taskReviewInput(sub))
	if err != nil || review.SubmissionID != sub.ID || review.ReviewedBy != "user" {
		t.Fatalf("formal local-user review failed: %+v, %v", review, err)
	}
	if notice := chatReviewNotice(t, f.s, review); notice.TopicID != role.Member.TopicID || notice.SourceID != "review:"+review.ID {
		t.Fatalf("review not returned to the authoritative thread: %+v", notice)
	}
	investigationDisablePolicy(t, f.s, policy)
	before := messageSequenceSnapshot(t, f.s)
	if replay, err := f.s.SubmitAssignmentResult(ctx, role, text, "submit"); err != nil || !reflect.DeepEqual(replay, sub) {
		t.Fatalf("policy revocation displaced historical assignment replay: %+v, %v", replay, err)
	}
	if replay, err := f.s.SubmitTask(ctx, investigationTaskSubmitInput(role, text, "submit")); err != nil || !reflect.DeepEqual(replay, sub) {
		t.Fatalf("policy revocation displaced shared submit_task replay: %+v, %v", replay, err)
	}
	messageSequencePreserved(t, f.s, before)
}
