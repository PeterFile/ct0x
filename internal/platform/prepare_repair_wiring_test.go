//go:build unix

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type repairPrepareFixture struct {
	*rolePrepareFixture
	parent        store.TurnClaim
	review        domain.ReviewDecision
	notice        domain.Message
	pendingReview store.PendingDelivery
	version       MemberWorkspaceArtifactResult
	directory     string
	source        domain.ChatRepairSource
}

// This fixture uses actual admission/IO/blob/Submission/Review APIs. Native
// events and normalized receipts are trusted in-process seams, not CLI proof.
func newRepairPreparation(t *testing.T, summaries ...string) *repairPrepareFixture {
	t.Helper()
	f, token, directory, claim := workspaceWorkerRegistration(t, true)
	ctx := context.Background()
	tools := f.opts.MemberTools.tools
	prepareRoleOK(t, f.s.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "repair-parent-native"}))
	write, err := tools.WorkspaceWrite(ctx, token, MemberWorkspaceWriteInput{Path: "report.txt", Create: true, Text: "rejected actual bytes"}, "parent-write")
	prepareRoleOK(t, err)
	version, err := tools.PublishWorkspaceArtifact(ctx, token, MemberWorkspaceArtifactInput{OperationID: write.OperationID, Name: "原成果"}, "parent-publish")
	prepareRoleOK(t, err)
	summary := "实际文件版本，等待用户审查。"
	if len(summaries) > 0 {
		summary = summaries[0]
	}
	submission, err := tools.SubmitAssignmentResult(ctx, token, store.AssignmentResultInput{Summary: summary, ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.VersionID}}}, "parent-submit")
	prepareRoleOK(t, err)
	task, err := f.s.GetTask(ctx, f.group.ID, submission.TaskID)
	prepareRoleOK(t, err)
	review, err := NewLocalUser(f.s).ReviewTask(ctx, f.group.ID, task.ID, TaskReviewInput{SubmissionID: submission.ID, ExpectedRevision: task.Revision, Outcome: domain.ReviewOutcomeRejected, Reason: "请在原授权目录修正report.txt，沿原Artifact和这个冻结版本继续；实际拒收理由到此结束。"}, "review-before-terminal")
	prepareRoleOK(t, err)
	var messageID string
	prepareRoleOK(t, f.db.QueryRow("SELECT message_id FROM chat_review_notices WHERE review_id=?", review.ID).Scan(&messageID))
	notice, err := f.s.GetMessage(ctx, f.group.ID, messageID)
	prepareRoleOK(t, err)
	if len(notice.Deliveries) != 1 || notice.SourceKind != "platform" || notice.SourceID != "review:"+review.ID {
		t.Fatalf("Review lost formal queued provenance: %+v", notice)
	}
	source, err := f.s.GetChatRepairSource(ctx, domain.ThreadRef{GroupID: f.group.ID, TopicID: notice.TopicID}, review.ID)
	prepareRoleOK(t, err)
	tools.Revoke(token)
	return &repairPrepareFixture{rolePrepareFixture: f, parent: claim, review: review, notice: notice, pendingReview: store.PendingDelivery{GroupID: f.group.ID, SenderID: notice.SenderID, Delivery: notice.Deliveries[0]}, version: version, directory: directory, source: source}
}

func (f *repairPrepareFixture) settleParent(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	prepareRoleOK(t, f.s.RecordTurnEvent(ctx, f.parent, provider.Event{Kind: provider.TerminalEvent, NativeSessionID: "repair-parent-native", Terminal: provider.OutcomeSucceeded}))
	_, err := f.s.ApplyTurnReceipt(ctx, f.parent, provider.ExecutionReceipt{Started: true, Accepted: true, Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, NativeSessionID: "repair-parent-native", DirectChildReaped: true, OutputComplete: true, ExitCode: 0})
	prepareRoleOK(t, err)
}

func (f *repairPrepareFixture) assertRepairUnchanged(t *testing.T, before map[string][][]any) {
	t.Helper()
	if !reflect.DeepEqual(f.snapshot(t), before) {
		t.Fatal("repair preparation changed durable state")
	}
	notice, err := f.s.GetMessage(context.Background(), f.group.ID, f.notice.ID)
	prepareRoleOK(t, err)
	if !reflect.DeepEqual(notice, f.notice) {
		t.Fatal("repair preparation changed the queued Review delivery")
	}
}

func (f *repairPrepareFixture) admitRepair(t *testing.T) domain.ChatAssessmentReceipt {
	t.Helper()
	ctx := context.Background()
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pendingReview.Delivery.ID, MaxActive: 1})
	prepareRoleOK(t, err)
	claim := store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}
	token, err := f.opts.MemberTools.tools.IssueChatIntake(ctx, claim)
	prepareRoleOK(t, err)
	receipt, err := f.opts.MemberTools.tools.AssessChatIntent(ctx, token, repairPreparationAssessment(f.review.ID), "原执行已终态，按真实拒收请求登记同范围修复，尚未执行。", "repair-assessment")
	prepareRoleOK(t, err)
	f.opts.MemberTools.tools.Revoke(token)
	_, err = f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	prepareRoleOK(t, err)
	return receipt
}

func repairPreparationAssessment(reviewID string) domain.ChatAssessment {
	return domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionWorkspaceChange, Title: "原范围修复", Description: "按真实Review在同目录修正report.txt", Acceptance: "更新同一成果，保留原冻结版本", Reason: "实际本机用户请求修复", Repair: &domain.ChatRepairAssessment{ReviewID: reviewID, Authorization: domain.ChatRepairReviewRequest}}
}

type repairPreparationNative func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt

func (fn repairPreparationNative) Execute(ctx context.Context, request provider.ExecuteRequest, event func(provider.Event) error) provider.ExecutionReceipt {
	return fn(ctx, request, event)
}

func repairPacketResponsibility(packet string) (string, error) {
	var frame struct {
		Responsibility string `json:"responsibility"`
	}
	if err := json.Unmarshal([]byte(packet), &frame); err != nil {
		return "", err
	}
	return frame.Responsibility, nil
}

func TestChatRepairPreparationWaitsUnclaimedAndSchedulerContinues(t *testing.T) {
	f := newRepairPreparation(t)
	p := f.preparer(t)
	ctx := context.Background()
	_, err := f.s.RequestTurnCancel(ctx, f.group.ID, f.parent.TurnID)
	prepareRoleOK(t, err)
	before := f.snapshot(t)
	prepared, err := p.Prepare(ctx, f.pendingReview)
	var blocked *RolePreparationBlocked
	if !errors.Is(err, scheduler.ErrNotReady) || !errors.As(err, &blocked) || blocked.Code != "repair_parent_pending" || prepared.Executor != nil {
		t.Fatalf("pending Review was consumed/prepared instead of typed preclaim wait: %+v %v", prepared, err)
	}
	f.assertRepairUnchanged(t, before)

	blockedObserved := make(chan struct{}, 1)
	type assessmentResult struct {
		receipt domain.ChatAssessmentReceipt
		err     error
	}
	assessed := make(chan assessmentResult, 1)
	loopCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	joined := false
	defer func() {
		stop()
		if !joined {
			result := <-done
			if result != nil && !errors.Is(result, context.Canceled) {
				t.Error(result)
			}
		}
	}()
	go func() {
		done <- scheduler.Run(loopCtx, f.s, scheduler.Options{Recovered: true, MaxActive: 2, PollInterval: 20 * time.Millisecond, RuntimeDir: f.runtimeOptions.RoleExecutionRoot}, func(ctx context.Context, pending store.PendingDelivery) (scheduler.PreparedExecution, error) {
			if pending.Delivery.ID != f.pendingReview.Delivery.ID {
				return scheduler.PreparedExecution{}, scheduler.ErrNotReady
			}
			prepared, err := p.Prepare(ctx, pending)
			if errors.Is(err, scheduler.ErrNotReady) {
				select {
				case blockedObserved <- struct{}{}:
				default:
				}
				return prepared, err
			}
			if err != nil {
				return prepared, err
			}
			source, err := json.Marshal(f.source)
			responsibility, decodeErr := repairPacketResponsibility(prepared.Request.Prompt)
			if err != nil || decodeErr != nil || !strings.Contains(responsibility, string(source)) {
				return scheduler.PreparedExecution{}, errors.New("complete trusted repair source missing from intake packet")
			}
			prepared.Executor, err = f.opts.MemberTools.WrapChatIntake(MemberToolRunBinding{GroupID: f.group.ID, MemberID: f.coordinator.ID, ConversationID: pending.Delivery.ConversationID, DeliveryID: pending.Delivery.ID, ProviderID: f.coordinator.ProviderID, Model: f.coordinator.Model}, func(path, id string, names []string) (provider.Executor, error) {
				if !slices.Equal(names, chatIntakeOperations()) {
					return nil, errors.New("noncanonical intake tools")
				}
				wire, err := os.ReadFile(path)
				if err != nil {
					return nil, err
				}
				var config struct{ Token string }
				if err = json.Unmarshal(wire, &config); err != nil {
					return nil, err
				}
				return repairPreparationNative(func(ctx context.Context, _ provider.ExecuteRequest, event func(provider.Event) error) provider.ExecutionReceipt {
					nativeID := "repair-intake-native"
					fail := func(err error) provider.ExecutionReceipt {
						assessed <- assessmentResult{err: err}
						return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1}
					}
					if err := event(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: nativeID}); err != nil {
						return fail(err)
					}
					candidate, err := f.opts.MemberTools.tools.ChatCandidate(ctx, config.Token)
					if err != nil {
						return fail(err)
					}
					if candidate == nil || candidate.RepairSource == nil || candidate.RepairSource.OriginalAuthorization.Content != f.source.OriginalAuthorization.Content || candidate.RepairSource.ReviewReason != f.review.Reason {
						return fail(errors.New("actual permission evidence replaced by notice/generated text"))
					}
					receipt, err := f.opts.MemberTools.tools.AssessChatIntent(ctx, config.Token, repairPreparationAssessment(f.review.ID), "按原请求登记修复，尚未执行或验收。", "same-review-after-terminal")
					if err != nil {
						return fail(err)
					}
					if err = event(provider.Event{Kind: provider.TerminalEvent, NativeSessionID: nativeID, Terminal: provider.OutcomeSucceeded}); err != nil {
						return fail(err)
					}
					assessed <- assessmentResult{receipt: receipt}
					return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, Started: true, Accepted: true, NativeSessionID: nativeID, DirectChildReaped: true, OutputComplete: true, ExitCode: 0}
				}), nil
			})
			return prepared, err
		})
	}()
	select {
	case <-blockedObserved:
	case err := <-done:
		joined = true
		t.Fatalf("scheduler did not retain pending Review: %v", err)
	}
	current, err := f.s.GetMessage(ctx, f.group.ID, f.notice.ID)
	prepareRoleOK(t, err)
	if !reflect.DeepEqual(current, f.notice) {
		t.Fatal("waiting claimed/replaced the same Review delivery")
	}
	var claims int
	prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", f.parent.TurnID).Scan(&claims))
	if claims != 1 {
		t.Fatal("cancel request/wait released parent resource")
	}
	f.settleParent(t)
	var result assessmentResult
	select {
	case result = <-assessed:
	case err := <-done:
		joined = true
		t.Fatalf("same delivery did not continue automatically: %v", err)
	}
	prepareRoleOK(t, result.err)
	receipt := result.receipt
	if receipt.Outcome != "admitted" || receipt.RepairLink == nil || receipt.MessageID != f.notice.ID || receipt.SourceKind != "platform" || receipt.SourceID != f.notice.SourceID || receipt.RepairLink.ParentReviewID != f.review.ID {
		t.Fatalf("repair required another user action or changed source: %+v", receipt)
	}
	// Use the Store's single connection so the observer cannot contend with the receipt commit.
	observeCtx, cancelObserve := context.WithTimeout(ctx, 30*time.Second)
	defer cancelObserve()
	for {
		turn, err := f.s.GetTurn(observeCtx, f.group.ID, receipt.TurnID)
		if err != nil {
			t.Fatalf("read automatic intake turn %s: %v", receipt.TurnID, err)
		}
		if turn.DeliveryID != f.pendingReview.Delivery.ID {
			t.Fatalf("automatic intake used delivery %s, want %s", turn.DeliveryID, f.pendingReview.Delivery.ID)
		}
		if turn.State == domain.TurnStateSucceeded {
			break
		}
		select {
		case err := <-done:
			joined = true
			t.Fatalf("automatic intake receipt failed: %v", err)
		case <-observeCtx.Done():
			t.Fatalf("automatic intake turn %s did not succeed: last state %s", turn.ID, turn.State)
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()
	err = <-done
	joined = true
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestChatRepairPreparationDefaultIntakeProfile(t *testing.T) {
	f := newPrepareFixture(t)
	selectPrepareMemberToolsPi(f)
	options := configurePrepareMemberTools(t, f)
	options.Operations = slices.Clone(historyProfileNames)
	options.RoleMaxActive, options.RoleExecutionRoot = 1, filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "runs")
	prepareRoleOK(t, os.Mkdir(options.RoleExecutionRoot, 0700))
	options.ChatIntake = coordination.New(f.s)
	var err error
	f.opts.MemberTools, err = NewMemberToolRuntime(f.s, options)
	prepareRoleOK(t, err)
	t.Cleanup(func() { f.opts.MemberTools.Close() })
	ctx := context.Background()
	conversation, err := f.s.GetConversation(ctx, f.group.ID, f.pending.Delivery.ConversationID)
	prepareRoleOK(t, err)
	// Packet data only: no admission, Assignment, file publication or claim.
	// No actual user text, host configuration or runtime database is required.
	id := func(letter string) string { return strings.Repeat(letter, 26) }
	thread := domain.ThreadRef{GroupID: f.group.ID, TopicID: f.trigger.TopicID}
	source := domain.ChatRepairSource{ThreadRef: thread,
		ParentTaskID: id("t"), ParentSubmissionID: id("s"), ParentReviewID: id("r"), ReviewedTaskRevision: 1,
		ParentMandate: domain.Mandate{ID: id("m"), DecisionID: id("d"), OriginTriggerID: id("o"),
			GroupID: f.group.ID, TopicID: thread.TopicID, TaskID: id("t"), TaskRevisionAtAdmission: 1,
			AuthorityKind: domain.MandateAuthorityLocalUserExplicit, AuthorizedBy: "user", Strength: domain.MandateStrengthExplicit,
			PolicyRevision: domain.LocalUserExplicitPolicyRevision, CreatedAt: f.trigger.CreatedAt,
			Scope: domain.TaskScope{Objective: "在原授权范围修订文档，提供精确冻结版本供本机用户评审", DependencyIDs: []string{}}},
		ParentAdmission: domain.ChatAssessmentReceipt{ID: id("a"), CandidateID: id("c"), ThreadRef: thread,
			MessageID: id("u"), SourceSenderID: "user", AssessedBy: f.member.ID, TurnID: id("i"),
			PolicyRevision: 1, CandidateGeneration: 8, Disposition: "request_work", Authority: "explicit_request",
			AuthorizationMessageID: id("u"), Action: domain.ChatActionWorkspaceChange, TaskID: id("t"), MandateID: id("m"),
			DispatchRequestID: id("q"), NoticeMessageID: id("n"), CreatedAt: f.trigger.CreatedAt,
			Purpose: domain.ChatIntakePurpose, SourceID: id("z"), SourceKind: "local_user_chat", Outcome: "admitted", AuthorityEvidenceVerified: true,
			WorkspaceScope: &domain.TaskWorkspaceScope{ThreadRef: thread, TaskID: id("t"), TaskRevision: 1, MandateID: id("m"),
				CandidateID: id("c"), AssessmentID: id("a"), DispatchRequestID: id("q"), PolicyRevision: 1,
				ResourceKey: "workspace:fixture", NodeID: "local", Grants: []domain.TaskWorkspaceGrant{{GrantID: id("g"), MemberID: id("w"),
					Root: f.opts.PrivateWorkspaceRoot, ResolvedRoot: f.opts.PrivateWorkspaceRoot, GrantSchemaVersion: 19}}}},
		ArtifactVersions:      []domain.ArtifactVersionRef{{ArtifactID: id("b"), VersionID: id("v")}},
		OriginalAuthorization: domain.ChatRepairAuthorization{SourceKind: "local_user_chat", SourceID: id("z"), MessageID: id("u"), PolicyRevision: 1}}
	source.OriginalAuthorization.Content = strings.Repeat("请仅修订原授权文件并提交冻结版本，由我评审。", 10)
	source.ParentMandate.Scope.Description = strings.Repeat("原授权目录内修改文档，保留既有内容，不执行外部动作。", 12)
	source.ParentMandate.Scope.Acceptance = strings.Repeat("提供真实文件、精确版本和校验依据；提交不代表接纳。", 8)
	source.ParentSubmissionSummary = strings.Repeat("已读取旧文件并在原范围修改，发布实际冻结版本，仍待本机用户评审。\n", 21) + "完整正文尾部<&>。"
	source.ReviewReason = strings.Repeat("请按原约定继续修订同一文件，保留原版本并发布新版本供我评审。", 8)
	trigger := f.trigger
	trigger.SourceKind, trigger.SourceID = "platform", "review:"+source.ParentReviewID
	trigger.Content = "平台记录：本机用户拒收原版本。\n任务：" + source.ParentTaskID + "\nSubmission：" + source.ParentSubmissionID +
		"\nReview：" + source.ParentReviewID + "\n" + strings.Repeat("拒收不自动授予修复权限，保留原始版本和原评审记录。", 6) + "\n" + source.ReviewReason
	history := make([]domain.Message, 17)
	for i := range history {
		history[i] = domain.Message{ID: strings.Repeat("h", 25) + string(rune('a'+i)), GroupID: f.group.ID,
			TopicID: trigger.TopicID, SenderID: f.member.ID, Content: "可省略正文，但来源引用不可丢弃。", CreatedAt: trigger.CreatedAt}
	}
	build := func(p *Preparer, history []domain.Message) ([]byte, error) {
		return p.buildContext(f.group, f.member, conversation, trigger, history, true, &source)
	}
	minimum := func(wire []byte) int {
		t.Helper()
		packet := decodePreparation(t, string(wire))
		_, err := contextpack.Build(contextpack.Input{Scope: packet.Scope, Responsibility: packet.Responsibility,
			Permissions: packet.Permissions, Pending: packet.Pending, History: packet.History, MaxBytes: 0})
		var budget *contextpack.BudgetError
		if !errors.As(err, &budget) {
			t.Fatalf("minimum packet measurement failed: %v", err)
		}
		return budget.Required
	}
	f.opts.MaxContextBytes = 1 << 20
	large := f.preparer(t)
	withoutRefs, err := build(large, nil)
	prepareRoleOK(t, err)
	full, err := build(large, history)
	prepareRoleOK(t, err)
	required := minimum(full)
	if minimum(withoutRefs) > 16<<10 || required <= 16<<10 {
		t.Fatalf("fixture lost actual failure shape: without_refs=%d with_17_refs=%d", minimum(withoutRefs), required)
	}
	t.Logf("portable_full_source: without_refs=%d with_17_refs=%d generic_default=%d", minimum(withoutRefs), required, 16<<10)
	f.opts.MaxContextBytes = 0
	wire, err := build(f.preparer(t), history)
	if err != nil {
		t.Fatalf("trusted intake default stranded complete repair source and mandatory refs: %v", err)
	}
	assertComplete := func(t *testing.T, wire []byte) {
		t.Helper()
		packet := decodePreparation(t, string(wire))
		var metadata struct {
			Source domain.ChatRepairSource `json:"repair_source"`
		}
		prepareRoleOK(t, json.Unmarshal([]byte(packet.Responsibility), &metadata))
		if !reflect.DeepEqual(metadata.Source, source) || len(packet.History)+len(packet.Overflow) != len(history) {
			t.Fatal("intake profile dropped full authority, Review, Summary, exact versions or history refs")
		}
	}
	assertComplete(t, wire)
	for _, tc := range []struct {
		name      string
		max, role int
		blocked   bool
	}{
		{"explicit ordinary limit", 16 << 10, 0, true},
		{"explicit exact limit overrides role limit", required, 1, false},
		{"configured intake fallback limit", 0, required - 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.opts.MaxContextBytes, f.opts.RoleMaxContextBytes = tc.max, tc.role
			wire, err := build(f.preparer(t), history)
			if tc.blocked {
				if err != scheduler.ErrNotReady || len(wire) != 0 {
					t.Fatalf("explicit context budget was bypassed: %v", err)
				}
				return
			}
			prepareRoleOK(t, err)
			if len(wire) != tc.max {
				t.Fatal("exact encoded byte limit changed")
			}
			assertComplete(t, wire)
		})
	}
	t.Run("generic ordinary remains 16 KiB", func(t *testing.T) {
		ordinary := newPrepareFixture(t)
		ordinary.opts.RoleMaxContextBytes = 1 << 20
		p := ordinary.preparer(t)
		conversation, err := ordinary.s.GetConversation(ctx, ordinary.group.ID, ordinary.pending.Delivery.ConversationID)
		prepareRoleOK(t, err)
		trigger := ordinary.trigger
		trigger.Content = strings.Repeat("ordinary", 2048)
		wire, err := p.buildContext(ordinary.group, ordinary.member, conversation, trigger, nil, false)
		if err != scheduler.ErrNotReady || len(wire) != 0 || p.maxContextBytes != 16<<10 {
			t.Fatalf("trusted intake profile changed generic ordinary budget: %v", err)
		}
	})
}

func TestChatRepairPreparationFullSourceCannotBeBudgetTruncated(t *testing.T) {
	f := newRepairPreparation(t)
	f.settleParent(t)
	p := f.preparer(t)
	ctx := context.Background()
	prepared, err := p.Prepare(ctx, f.pendingReview)
	prepareRoleOK(t, err)
	source, err := json.Marshal(f.source)
	prepareRoleOK(t, err)
	responsibility, err := repairPacketResponsibility(prepared.Request.Prompt)
	prepareRoleOK(t, err)
	if !strings.Contains(responsibility, string(source)) || f.source.OriginalAuthorization.Content == f.source.ParentMandate.Scope.Acceptance {
		t.Fatal("complete actual source omitted or replaced with generated acceptance")
	}
	member, err := f.s.GetMember(ctx, f.group.ID, f.pendingReview.Delivery.RecipientID)
	prepareRoleOK(t, err)
	conversation, err := f.s.GetConversation(ctx, f.group.ID, f.pendingReview.Delivery.ConversationID)
	prepareRoleOK(t, err)
	baseline, err := p.buildContext(f.group, member, conversation, f.notice, nil, true)
	prepareRoleOK(t, err)
	f.opts.MaxContextBytes = len(baseline)
	p = f.preparer(t)
	before := f.snapshot(t)
	blocked, err := p.Prepare(ctx, f.pendingReview)
	if !errors.Is(err, scheduler.ErrNotReady) || blocked.Executor != nil || blocked.Request.Prompt != "" {
		t.Fatalf("permission-critical source was dropped/truncated to fit budget: %+v %v", blocked, err)
	}
	f.assertRepairUnchanged(t, before)
}

func TestChatRepairPreparationCompleteSubmissionSummaryAndEncodedBudget(t *testing.T) {
	summary := strings.Repeat("正式成果正文<&>，不是生成的任务摘要。\n", 120) + "必须保留正文尾部。"
	f := newRepairPreparation(t, summary)
	f.settleParent(t)
	f.opts.MaxContextBytes = 1 << 20
	p := f.preparer(t)
	ctx := context.Background()
	submission, err := f.s.GetTaskSubmission(ctx, f.group.ID, f.source.ParentTaskID, f.source.ParentSubmissionID)
	prepareRoleOK(t, err)
	if submission.Summary != summary || f.source.ParentSubmissionSummary != submission.Summary || summary == f.source.ParentMandate.Scope.Description {
		t.Fatal("repair source did not read distinct complete formal Submission text")
	}
	prepared, err := p.Prepare(ctx, f.pendingReview)
	prepareRoleOK(t, err)
	responsibility, err := repairPacketResponsibility(prepared.Request.Prompt)
	prepareRoleOK(t, err)
	var metadata struct {
		RepairSource *domain.ChatRepairSource `json:"repair_source"`
	}
	prepareRoleOK(t, json.Unmarshal([]byte(responsibility), &metadata))
	if metadata.RepairSource == nil || !reflect.DeepEqual(*metadata.RepairSource, f.source) || metadata.RepairSource.ParentSubmissionSummary != summary {
		t.Fatal("actual evaluator packet lost Submission text or other complete source fields")
	}
	member, err := f.s.GetMember(ctx, f.group.ID, f.pendingReview.Delivery.RecipientID)
	prepareRoleOK(t, err)
	conversation, err := f.s.GetConversation(ctx, f.group.ID, f.pendingReview.Delivery.ConversationID)
	prepareRoleOK(t, err)
	full, err := p.buildContext(f.group, member, conversation, f.notice, nil, true, &f.source)
	prepareRoleOK(t, err)
	withoutSummary := f.source
	withoutSummary.ParentSubmissionSummary = ""
	baseline, err := p.buildContext(f.group, member, conversation, f.notice, nil, true, &withoutSummary)
	prepareRoleOK(t, err)
	if len(full)-len(baseline) <= len(summary) {
		t.Fatal("context budget ignored nested JSON escaping of formal Submission text")
	}
	p.maxContextBytes = len(full)
	exact, err := p.buildContext(f.group, member, conversation, f.notice, nil, true, &f.source)
	prepareRoleOK(t, err)
	if string(exact) != string(full) {
		t.Fatal("exact context byte limit changed complete source")
	}
	p.maxContextBytes--
	if packet, err := p.buildContext(f.group, member, conversation, f.notice, nil, true, &f.source); !errors.Is(err, scheduler.ErrNotReady) || len(packet) != 0 {
		t.Fatal("one-byte context overflow silently truncated complete source")
	}
	p.maxContextBytes = len(baseline)
	before := f.snapshot(t)
	blocked, err := p.Prepare(ctx, f.pendingReview)
	if !errors.Is(err, scheduler.ErrNotReady) || blocked.Executor != nil || blocked.Request.Prompt != "" {
		t.Fatal("preclaim preparation dropped Submission text to fit the budget")
	}
	f.assertRepairUnchanged(t, before)
	t.Logf("formal_submission=%s summary_utf8_bytes=%d escaped_context_delta=%d full_context_bytes=%d", submission.ID, len(summary), len(full)-len(baseline), len(full))
}

func TestChatRepairRoleContextAndRealFilesKeepExactRejectedBase(t *testing.T) {
	f := newRepairPreparation(t)
	f.settleParent(t)
	receipt := f.admitRepair(t)
	if receipt.RepairLink == nil || receipt.WorkspaceScope == nil {
		t.Fatal("real repair links/scope missing")
	}
	materialized, err := f.s.MaterializeDispatchRequest(context.Background(), receipt.DispatchRequestID, 1, time.Now().UTC())
	prepareRoleOK(t, err)
	if materialized.Delivery == nil {
		t.Fatal("new repair dispatch missing")
	}
	f.selectDelivery(t, materialized.Delivery.MessageID)
	p := f.preparer(t)
	coordinator, err := p.Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	coordinatorResponsibility, err := repairPacketResponsibility(coordinator.Request.Prompt)
	prepareRoleOK(t, err)
	for _, text := range []string{`"repair_link":`, `"parent_review_id":"` + f.review.ID + `"`, `"version_id":"` + f.version.VersionID + `"`} {
		if !strings.Contains(coordinatorResponsibility, text) {
			t.Fatalf("coordinator lost rejected frozen base: %s", text)
		}
	}
	claimRun := func(prepared scheduler.PreparedExecution, pending store.PendingDelivery) store.TurnClaim {
		t.Helper()
		turn, err := f.s.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: pending.Delivery.ID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
		prepareRoleOK(t, err)
		return store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken}
	}
	execute := func(prepared scheduler.PreparedExecution, claim store.TurnClaim, names []string, use func(string)) {
		t.Helper()
		run, ok := prepared.Executor.(*memberToolRuntimeRun)
		if !ok {
			t.Fatal("not actual role wrapper")
		}
		run.factory = func(path, id string, actual []string) (provider.Executor, error) {
			if id != claim.TurnID || !slices.Equal(actual, names) {
				return nil, errors.New("wrong native role profile")
			}
			wire, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			var config struct{ Token string }
			if err = json.Unmarshal(wire, &config); err != nil {
				return nil, err
			}
			return repairPreparationNative(func(ctx context.Context, _ provider.ExecuteRequest, event func(provider.Event) error) provider.ExecutionReceipt {
				nativeID := "repair-role-" + id
				prepareRoleOK(t, event(provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: nativeID}))
				use(config.Token)
				prepareRoleOK(t, event(provider.Event{Kind: provider.ResponseEvent, NativeSessionID: nativeID, Text: "角色工具已完成，成果尚待本机用户接纳。"}))
				prepareRoleOK(t, event(provider.Event{Kind: provider.TerminalEvent, NativeSessionID: nativeID, Terminal: provider.OutcomeSucceeded}))
				return provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, Started: true, Accepted: true, NativeSessionID: nativeID, DirectChildReaped: true, OutputComplete: true, ExitCode: 0}
			}), nil
		}
		request := prepared.Request
		request.RunID = claim.TurnID
		request.RuntimeDir = f.runtimeOptions.RoleExecutionRoot
		result := run.Execute(context.Background(), request, func(event provider.Event) error { return f.s.RecordTurnEvent(context.Background(), claim, event) })
		if result.Outcome != provider.OutcomeSucceeded {
			t.Fatalf("actual role wrapper rejected repair: %+v", result)
		}
		_, err := f.s.ApplyTurnReceipt(context.Background(), claim, result)
		prepareRoleOK(t, err)
	}
	var assignmentID string
	coorClaim := claimRun(coordinator, f.pending)
	execute(coordinator, coorClaim, memberToolRoleOperations(store.CoordinationRoleCoordinator), func(token string) {
		decision, err := f.opts.MemberTools.tools.DecideDispatch(context.Background(), token, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "同范围文件修复"}, "repair-assign")
		prepareRoleOK(t, err)
		if decision.Assignment == nil {
			t.Fatal("repair assignment missing")
		}
		assignmentID = decision.Assignment.ID
	})
	workerDelivery, err := f.s.MaterializeAssignment(context.Background(), assignmentID, 1, time.Now().UTC())
	prepareRoleOK(t, err)
	if workerDelivery.Delivery == nil {
		t.Fatal("repair worker missing")
	}
	f.selectDelivery(t, workerDelivery.Delivery.MessageID)
	worker, err := p.Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	workerResponsibility, err := repairPacketResponsibility(worker.Request.Prompt)
	prepareRoleOK(t, err)
	if worker.Request.CWD != f.directory || !slices.Equal(worker.ResourceKeys, []string{receipt.WorkspaceScope.ResourceKey}) || !strings.Contains(workerResponsibility, `"repair_link":`) || !strings.Contains(workerResponsibility, f.version.VersionID) {
		t.Fatal("worker selected CWD/latest head instead of rejected physical/base scope")
	}
	workerClaim := claimRun(worker, f.pending)
	var next MemberWorkspaceArtifactResult
	execute(worker, workerClaim, provider.WorkspaceWorkerToolNames(), func(token string) {
		hash := f.version.SHA256
		write, err := f.opts.MemberTools.tools.WorkspaceWrite(context.Background(), token, MemberWorkspaceWriteInput{Path: "report.txt", ExpectedSHA256: &hash, Text: "repaired actual bytes"}, "repair-write")
		prepareRoleOK(t, err)
		next, err = f.opts.MemberTools.tools.PublishWorkspaceArtifact(context.Background(), token, MemberWorkspaceArtifactInput{OperationID: write.OperationID, ArtifactID: f.version.ArtifactID, ExpectedVersionID: f.version.VersionID}, "repair-publish")
		prepareRoleOK(t, err)
		_, err = f.opts.MemberTools.tools.SubmitAssignmentResult(context.Background(), token, store.AssignmentResultInput{Summary: "同一成果的新冻结版本，尚待用户审查。", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: next.ArtifactID, VersionID: next.VersionID}}}, "repair-submit")
		prepareRoleOK(t, err)
	})
	if next.ArtifactID != f.version.ArtifactID || next.VersionID == f.version.VersionID {
		t.Fatal("repair replaced logical identity/history")
	}
	for _, expected := range []struct{ id, text string }{{f.version.VersionID, "rejected actual bytes"}, {next.VersionID, "repaired actual bytes"}} {
		version, err := f.s.GetArtifactVersion(context.Background(), f.group.ID, next.ArtifactID, expected.id)
		prepareRoleOK(t, err)
		blob, err := f.opts.MemberTools.tools.workspace.blobs.Open(version.BlobHash)
		prepareRoleOK(t, err)
		data, err := io.ReadAll(blob)
		prepareRoleOK(t, err)
		prepareRoleOK(t, blob.Close())
		if string(data) != expected.text {
			t.Fatal("frozen actual version bytes changed")
		}
	}
	data, err := os.ReadFile(filepath.Join(f.directory, "report.txt"))
	prepareRoleOK(t, err)
	if string(data) != "repaired actual bytes" {
		t.Fatal("repair was only metadata")
	}
	review, err := f.s.GetTaskReview(context.Background(), f.group.ID, f.review.TaskID, f.review.ID)
	prepareRoleOK(t, err)
	if !reflect.DeepEqual(review, f.review) {
		t.Fatal("new repair rewrote old Review")
	}
}
