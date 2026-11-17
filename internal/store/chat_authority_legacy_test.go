package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// This is an actual v16 database, not trusted-source flags inserted into v17.
// Historical rows below deliberately have no new command/purpose proof.
func TestChatAuthorityV16PolicyReplayAndLegacyReview(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	migrations = original[:16]
	f := setupMessages(t)
	migrations = original
	topic := "legacy-topic"
	written, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: topic, ActorID: "user", Content: "只准备旧规则A，不代表任意目标授权"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldInput := domain.ChatPolicyRequest{Enabled: true, AllowPreparation: true, PreauthorizationMessageID: written.ID, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2}
	encoded, err := json.Marshal(oldInput)
	if err != nil {
		t.Fatal(err)
	}
	oldPayload := mutationPayload(f.groupID, topic, "0", string(encoded))
	coordinationDecideExec(t, f.s, `INSERT INTO chat_policy_revisions(group_id,topic_id,revision,enabled,allow_preparation,preauthorization_message_id,action,max_admissions,authorized_by) VALUES(?,?,1,1,1,?,'text_submission',2,'user')`, f.groupID, topic, written.ID)
	coordinationDecideExec(t, f.s, "INSERT INTO chat_policies VALUES(?,?,1)", f.groupID, topic)
	coordinationDecideExec(t, f.s, "INSERT INTO mutation_idempotency VALUES('set_chat_policy','user',?,'legacy-policy',?,'1')", f.groupID, oldPayload)
	if _, err := f.s.SetCoordinator(ctx, f.groupID, f.a, 0, "coord"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 0, "workers"); err != nil {
		t.Fatal(err)
	}
	oldMessageInput := MessageRequest{GroupID: f.groupID, TopicID: topic, ActorID: "user", Content: "旧应用记录的文本请求", RecipientIDs: []string{f.a}, IdempotencyKey: "old-chat-message"}
	source, err := f.s.CreateMessage(ctx, oldMessageInput, []string{f.a})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: source.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := f.s.db.QueryRow("SELECT sequence FROM message_log WHERE message_id=?", source.ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	admission, err := f.s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{TopicID: topic, MessageID: source.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "历史准入", NewTask: &domain.NewTaskRequest{Title: "历史目标", Acceptance: "历史文本验收"}}, "legacy-work")
	if err != nil {
		t.Fatal(err)
	}
	request, err := f.s.RequestDispatch(ctx, f.groupID, admission.Task.ID, 1, 1, "历史分派", "legacy-dispatch")
	if err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, f.s, `INSERT INTO chat_candidates(id,group_id,topic_id,message_id,member_id,turn_id,generation,policy_revision,binding_revision,dispatch_policy_revision) VALUES('legacy-candidate',?,?,?,?,?,?,1,1,1)`, f.groupID, topic, source.ID, f.a, turn.ID, generation)
	notice, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: topic, ActorID: f.a, Content: "历史准入说明"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	coordinator := coordinationDecideBindRequest(t, f.s, request)
	decision, err := f.s.DecideDispatch(ctx, coordinator, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "历史成员"}, "legacy-assign")
	if err != nil {
		t.Fatal(err)
	}
	worker := coordinationDecideWorker(t, requestDispatchFixture{s: f.s, worker: f.b}, request, decision, true)
	version := mustPublishArtifact(t, f.s, artifactPublishInput(f.groupID))
	resultInput := AssignmentResultInput{Summary: "旧版本已经保存的成果", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}}
	// Save the old artifact-bearing result before attaching the historical chat
	// annotation: the new gate must not be used to manufacture a legacy receipt.
	submission, err := f.s.SubmitAssignmentResult(ctx, worker, resultInput, "legacy-result")
	if err != nil {
		t.Fatal(err)
	}
	legacyReceipt := map[string]any{"id": "legacy-assessment", "candidate_id": "legacy-candidate", "group_id": f.groupID, "topic_id": topic, "message_id": source.ID, "source_sender_id": "user", "assessed_by": f.a, "turn_id": turn.ID, "policy_revision": 1, "candidate_generation": generation, "disposition": "prepare_work", "authority": "written_preauthorization", "authorization_message_id": written.ID, "action": "text_submission", "task_id": admission.Task.ID, "mandate_id": admission.Mandate.ID, "dispatch_request_id": request.ID, "notice_message_id": notice.ID, "created_at": time.Now().UTC()}
	receiptJSON, err := json.Marshal(legacyReceipt)
	if err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, f.s, `INSERT INTO chat_assessments(id,candidate_id,decision_id,dispatch_request_id,notice_message_id,payload,receipt_json) VALUES('legacy-assessment','legacy-candidate',?,?,?,?,?)`, admission.Decision.ID, request.ID, notice.ID, []byte("historical payload"), string(receiptJSON))
	if err := migrate(f.s.db); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	oldMessage, err := f.s.CreateLocalUserMessage(ctx, oldMessageInput, []string{f.a})
	if err != nil || oldMessage.ID != source.ID || oldMessage.SourceKind != "unknown" || oldMessage.SourceID != "" {
		t.Fatalf("legacy message replay fabricated new user command: %+v %v", oldMessage, err)
	}
	policy, err := f.s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.groupID, TopicID: topic}, oldInput, 0, "legacy-policy")
	if err != nil || !policy.AllowPreparation || policy.PreparationSupported || policy.PreparationScopeKind != "" || policy.ConfigurationSourceID != "" || policy.PreauthorizationMessageID != written.ID {
		t.Fatalf("original v16 PUT replay blocked/upgraded: %+v %v", policy, err)
	}
	if _, err := f.s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.groupID, TopicID: topic}, oldInput, 1, "new-legacy-shape"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old free rule enabled as new permission: %v", err)
	}
	evidence, err := f.s.GetChatAdmission(ctx, f.groupID, admission.Task.ID)
	if err != nil || evidence.SourceKind != "unknown" || evidence.SourceID != "" || evidence.Purpose != "" || evidence.AuthorityEvidenceVerified || evidence.Authority != "written_preauthorization" {
		t.Fatalf("legacy evidence mislabeled/blocked: %+v %v", evidence, err)
	}
	replay, err := f.s.SubmitAssignmentResult(ctx, worker, resultInput, "legacy-result")
	if err != nil || !reflect.DeepEqual(replay, submission) {
		t.Fatalf("old saved artifact receipt failed replay: %+v %v", replay, err)
	}
	messageSequencePreserved(t, f.s, before)
	review := taskReviewInput(submission)
	review.Outcome = domain.ReviewOutcomeRejected
	review.Reason = "不满足原任务约定，来源未验证不妨碍拒收具体成果"
	if _, err := f.s.ReviewTask(ctx, review); err != nil {
		t.Fatalf("legacy source display blocked local-user rejection: %v", err)
	}
	replay, err = f.s.SubmitAssignmentResult(ctx, worker, resultInput, "legacy-result")
	if err != nil || !reflect.DeepEqual(replay, submission) {
		t.Fatalf("historical replay changed after rejection: %+v %v", replay, err)
	}
	var fabricated int
	if err := f.s.db.QueryRow("SELECT count(*) FROM message_command_sources").Scan(&fabricated); err != nil || fabricated != 0 {
		t.Fatalf("migration fabricated legacy authorization sources: %d %v", fabricated, err)
	}
	dispatchAssertFK(t, f.s)
}
