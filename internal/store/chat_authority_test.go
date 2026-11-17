package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// All trusted chat sources use the real local command seam, never a source flag.
func chatLocalMessage(t *testing.T, s *Store, r MessageRequest, recipients []string) domain.Message {
	t.Helper()
	api, ok := any(s).(interface {
		CreateLocalUserMessage(context.Context, MessageRequest, []string) (domain.Message, error)
	})
	if !ok {
		t.Fatal("missing local-user command receipt entry")
	}
	message, err := api.CreateLocalUserMessage(ctx, r, recipients)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func chatNextCandidate(t *testing.T, f *chatFixture, trusted bool, beforeCapture ...func()) {
	t.Helper()
	if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	r := MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: "按讨论约定形成新的文本摘要", RecipientIDs: []string{f.a}}
	var message domain.Message
	var err error
	if trusted {
		message = chatLocalMessage(t, f.s, r, []string{f.a})
	} else {
		message, err = f.s.CreateMessage(ctx, r, []string{f.a})
		if err != nil {
			t.Fatal(err)
		}
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.scope, err = f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeCapture) == 1 {
		beforeCapture[0]()
	}
	f.candidate, err = f.api.CaptureChatCandidate(ctx, f.scope)
	if err != nil {
		t.Fatal(err)
	}
}

func chatAssertNotAdmitted(t *testing.T, r domain.ChatAssessmentReceipt, err error, reason string) {
	t.Helper()
	data, _ := json.Marshal(r)
	var state struct{ Outcome, ReasonCode, Question string }
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	_ = json.Unmarshal(fields["outcome"], &state.Outcome)
	_ = json.Unmarshal(fields["reason_code"], &state.ReasonCode)
	_ = json.Unmarshal(fields["question"], &state.Question)
	if err != nil || r.TaskID != "" || r.MandateID != "" || r.DispatchRequestID != "" || r.NoticeMessageID == "" || state.Outcome != "not_admitted" || state.ReasonCode != reason || state.Question == "" {
		t.Fatalf("expected durable non-admission %s, got %s err=%v", reason, data, err)
	}
}

func TestChatAuthorityUnprovenUserCannotAuthorize(t *testing.T) {
	f := chatSetup(t, "user", false)
	chatNextCandidate(t, &f, false)
	receipt, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "这是提议，不是用户授权。", "unproven")
	chatAssertNotAdmitted(t, receipt, err, "untrusted_source")
	before := messageSequenceSnapshot(t, f.s)
	replay, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "这是提议，不是用户授权。", "unproven")
	if err != nil || !reflect.DeepEqual(receipt, replay) {
		t.Fatalf("non-admission replay diverged: %+v %v", replay, err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatAuthorityPurposeAndCommandEvidence(t *testing.T) {
	f := chatSetup(t, "user", false)
	r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "落实文本。", "evidence")
	if err != nil || r.TaskID == "" {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	var evidence map[string]any
	_ = json.Unmarshal(data, &evidence)
	if evidence["purpose"] != "chat_intake/v1" || evidence["source_kind"] != "local_user_chat" || evidence["source_id"] == "" || evidence["source_id"] == nil {
		t.Fatalf("no durable purpose/command evidence: %s", data)
	}
}

func TestChatAuthorityKnownRoutingFailureIsDurable(t *testing.T) {
	f := chatSetup(t, "user", false)
	coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.b)
	r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "开始落实。", "no-worker")
	chatAssertNotAdmitted(t, r, err, "routing_unavailable")
	var tasks int
	if err := f.s.db.QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("unroutable request created work: %d %v", tasks, err)
	}
}

func TestChatAuthorityPurposeEvidenceIsImmutable(t *testing.T) {
	f := chatSetup(t, "user", true)
	for _, table := range []string{"message_command_sources", "chat_policy_scopes", "chat_intake_executions"} {
		column := "id"
		if table == "chat_policy_scopes" {
			column = "group_id"
		}
		if table == "chat_intake_executions" {
			column = "candidate_id"
		}
		for _, statement := range []string{"UPDATE " + table + " SET " + column + "=" + column, "DELETE FROM " + table, "INSERT OR REPLACE INTO " + table + " SELECT * FROM " + table + " LIMIT 1"} {
			if _, err := f.s.db.Exec(statement); err == nil {
				t.Fatalf("authority proof mutated: %s", statement)
			}
		}
	}
}

func TestChatAuthorityStorageFailureIsNotKnownDenial(t *testing.T) {
	f := chatSetup(t, "user", false)
	for i := 0; i < 2; i++ {
		if _, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "落实。", fmt.Sprintf("admit-%d", i)); err != nil {
			t.Fatal(err)
		}
		chatNextCandidate(t, &f, true)
	}
	coordinationDecideExec(t, f.s, `CREATE TRIGGER fail_assessment_storage BEFORE INSERT ON chat_assessments BEGIN SELECT RAISE(ABORT,'injected storage failure'); END`)
	before := messageSequenceSnapshot(t, f.s)
	r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "预算外请求。", "failed-write")
	if err == nil || !reflect.ValueOf(r).IsZero() {
		t.Fatalf("SQL failure became a committed known outcome: %+v %v", r, err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatAuthorityFreeTextPreauthorizationIsNotExecutable(t *testing.T) {
	f := chatSetup(t, "user", false)
	rule, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: "仅允许A，不允许B"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := f.policy.ChatPolicyRequest
	input.AllowPreparation = true
	input.PreauthorizationMessageID = rule.ID
	before := messageSequenceSnapshot(t, f.s)
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, input, 1, "free-rule"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arbitrary sender=user text became executable scope: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
}

func TestChatAuthorityPreparationCannotChooseAnotherGoal(t *testing.T) {
	f := chatSetup(t, "user", true)
	f.input.Disposition = "prepare_work"
	f.input.Title = "与摘要范围无关的B目标"
	r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "建议B。", "different-goal")
	chatAssertNotAdmitted(t, r, err, "preparation_scope_mismatch")
}

func TestChatAuthorityPlatformDispatchDoesNotSupersedeUser(t *testing.T) {
	for _, phase := range []string{"before-capture", "after-capture"} {
		t.Run(phase, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			first, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "第一项摘要。", "first")
			if err != nil {
				t.Fatal(err)
			}
			materialize := func() {
				materialized, err := f.s.MaterializeDispatchRequest(ctx, first.DispatchRequestID, 1, time.Now())
				if err != nil || materialized.Delivery == nil {
					t.Fatalf("actual platform dispatch: %+v %v", materialized, err)
				}
				message, err := f.s.GetMessage(ctx, f.groupID, materialized.Delivery.MessageID)
				if err != nil || message.SenderID != "user" || message.SourceKind != "platform" {
					t.Fatalf("fixture did not exercise proven platform user sender: %+v %v", message, err)
				}
			}
			if phase == "before-capture" {
				chatNextCandidate(t, &f, true, materialize)
			} else {
				chatNextCandidate(t, &f, true)
				materialize()
			}
			r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "最新用户请求仍有效。", "after-platform")
			if err != nil || r.TaskID == "" {
				t.Fatalf("platform sender=user falsely superseded actual user command: outcome=%s reason=%s err=%v", r.Outcome, r.ReasonCode, err)
			}
		})
	}
}

func TestChatAuthorityBudgetExhaustionIsDurable(t *testing.T) {
	f := chatSetup(t, "user", false)
	for i := 0; i < 3; i++ {
		if i > 0 {
			chatNextCandidate(t, &f, true)
		}
		key := fmt.Sprintf("budget-%d", i)
		r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "文本回复。", key)
		if i < 2 {
			if err != nil || r.TaskID == "" {
				t.Fatalf("item %d: %+v %v", i, r, err)
			}
			continue
		}
		chatAssertNotAdmitted(t, r, err, "budget_exhausted")
		notice, err := f.s.GetMessage(ctx, f.groupID, r.NoticeMessageID)
		if err != nil || !strings.Contains(notice.Content, "2/2") || len(notice.Deliveries) != 0 {
			t.Fatalf("budget fact not returned to topic: %+v %v", notice, err)
		}
		before := messageSequenceSnapshot(t, f.s)
		replay, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "文本回复。", key)
		if err != nil || !reflect.DeepEqual(r, replay) {
			t.Fatalf("budget replay: %+v %v", replay, err)
		}
		messageSequencePreserved(t, f.s, before)
	}
}

func chatAuthorityWorker(t *testing.T, f chatFixture) CoordinationToolScope {
	t.Helper()
	admitted, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "文本任务。", "admit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := f.s.GetDispatchRequest(ctx, f.groupID, admitted.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := coordinationDecideBindRequest(t, f.s, dispatch.Request)
	input := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "文本成员"}
	var decision domain.DecisionReceipt
	if admitted.WorkspaceScope != nil {
		fact := workspaceOperationCapabilityFact(t, f.s, dispatch.Request, f.b)
		decision, err = f.s.DecideDispatchWithCapabilities(ctx, coordinator, input, "assign", []DispatchWorkerCapabilityFact{fact})
	} else {
		decision, err = f.s.DecideDispatch(ctx, coordinator, input, "assign")
	}
	if err != nil || decision.Assignment == nil {
		t.Fatalf("chat worker assignment: %+v, %v", decision, err)
	}
	worker := coordinationDecideWorker(t, requestDispatchFixture{s: f.s, worker: f.b}, dispatch.Request, decision, false)
	binding, err := f.s.BindAssignmentAttempt(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	worker.AttemptID = binding.AttemptID
	return worker
}

func TestChatAuthorityPreparedSummaryHasFixedBoundary(t *testing.T) {
	f := chatSetup(t, "user", true)
	f.input.Disposition = "prepare_work"
	f.input.Title, f.input.Description, f.input.Acceptance = "", "", ""
	worker := chatAuthorityWorker(t, f)
	input := AssignmentResultInput{Summary: strings.Repeat("字", 2730) + "abc"}
	before := messageSequenceSnapshot(t, f.s)
	if r, err := f.s.SubmitAssignmentResult(ctx, worker, input, "bounded-summary"); !errors.Is(err, ErrInvalid) || r.ID != "" {
		t.Fatalf("topic_summary exceeded 8192-byte boundary: %+v %v", r, err)
	}
	messageSequencePreserved(t, f.s, before)
	input.Summary = strings.Repeat("字", 2730) + "ab"
	if r, err := f.s.SubmitAssignmentResult(ctx, worker, input, "bounded-summary"); err != nil || r.Summary != input.Summary {
		t.Fatalf("valid boundary summary rejected: %+v %v", r, err)
	}
}

func TestChatAuthorityTextSubmitRejectsArtifacts(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	version := mustPublishArtifact(t, f.s, artifactPublishInput(f.groupID))
	input := AssignmentResultInput{Summary: "文本结果", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}}
	before := messageSequenceSnapshot(t, f.s)
	got, err := f.s.SubmitAssignmentResult(ctx, worker, input, "no-artifacts")
	if !errors.Is(err, ErrInvalid) || got.ID != "" {
		t.Fatalf("text-only chat admitted Artifact references: %+v %v", got, err)
	}
	messageSequencePreserved(t, f.s, before)
	direct := SubmitTaskInput{GroupID: f.groupID, TaskID: worker.Provenance.TaskID, AttemptID: worker.AttemptID, ExpectedRevision: worker.Provenance.TaskRevision, ActorID: worker.Member.MemberID, MemberScope: &worker.Member, Summary: input.Summary, ArtifactVersions: input.ArtifactVersions, IdempotencyKey: "direct-bypass"}
	if r, err := f.s.SubmitTask(ctx, direct); !errors.Is(err, ErrInvalid) || r.ID != "" {
		t.Fatalf("shared submit helper bypassed chat text scope: id=%s err=%v", r.ID, err)
	}
	messageSequencePreserved(t, f.s, before)
	input.ArtifactVersions = nil
	text, err := f.s.SubmitAssignmentResult(ctx, worker, input, "no-artifacts")
	if err != nil || text.ID == "" {
		t.Fatalf("rejected key was consumed or text submit broke: %+v %v", text, err)
	}
	replay, err := f.s.SubmitAssignmentResult(ctx, worker, input, "no-artifacts")
	if err != nil || !reflect.DeepEqual(text, replay) {
		t.Fatalf("text replay: %+v %v", replay, err)
	}
}

func BenchmarkChatMemberSend(b *testing.B) {
	s, r, recipients, _ := benchmarkMessages(b, 10)
	m, err := s.CreateMessage(ctx, r, recipients[:1])
	if err != nil {
		b.Fatal(err)
	}
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: m.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		b.Fatal(err)
	}
	scope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.SendMessageForTool(ctx, scope, MemberToolMessageInput{Content: "bounded member discussion"}, fmt.Sprintf("send-%d", i)); err != nil {
			b.Fatal(err)
		}
	}
}
