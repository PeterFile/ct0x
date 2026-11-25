package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestMigrationsBatch1RegistrationOrder(t *testing.T) {
	reqIdx := slices.Index(migrations, decisionRequestSchema)
	ansIdx := slices.Index(migrations, decisionAnswerSchema)
	idIdx := slices.Index(migrations, projectIdentitySchema)
	dirIdx := slices.Index(migrations, projectDirectorySchema)
	threadIdx := slices.Index(migrations, decisionThreadAnswerSchema)
	expIdx := slices.Index(migrations, agentRunExplanationSchema)
	quoteIdx := slices.Index(migrations, messageQuoteSchema)
	projExpIdx := slices.Index(migrations, projectExperienceSchema)

	if reqIdx != 31 || ansIdx != 32 || idIdx != 33 || dirIdx != 34 {
		t.Fatalf("v32..v35 frozen prefix moved: req=v%d ans=v%d identity=v%d directory=v%d",
			reqIdx+1, ansIdx+1, idIdx+1, dirIdx+1)
	}
	if threadIdx != dirIdx+1 || expIdx != threadIdx+1 || quoteIdx != threadIdx+2 || projExpIdx != threadIdx+3 {
		t.Fatalf("batch 1 schemas missing or non-contiguous from v36: thread=v%d explanation=v%d quote=v%d project_experience=v%d",
			threadIdx+1, expIdx+1, quoteIdx+1, projExpIdx+1)
	}
}

func TestMigrationsBatch1UpgradeReplayAndEntrypoints(t *testing.T) {
	// Phase 1: create a real v33 database (missing D2/D3 and all four newly
	// registered schemas) and verify missing-table fail-closed behavior while
	// legacy entrypoints still work.
	prior := migrations
	t.Cleanup(func() { migrations = prior })
	migrations = prior[:33]
	f := chatSetup(t, "user", false)
	messageSequenceLedger(t, f.s, 33)

	// decisionThreadAnswerSchema absent: handoff probe false, legacy
	// clarification is recorded via v32 but not answerable as a thread relay.
	if installed, err := f.s.ThreadDecisionHandoffInstalled(ctx); err != nil || installed {
		t.Fatalf("v33 reported thread handoff installed: %t, %v", installed, err)
	}
	v33Ask := askThreadDecision(t, f, "v33-ask")
	f.s.EnableThreadDecisionExecution()
	if pending, err := f.s.ListPendingDecisionRequests(ctx, ListOptions{Limit: 10}); err != nil || len(pending.Items) != 1 || pending.Items[0].ID != v33Ask.ID {
		t.Fatalf("v33 legacy pending request read failed: %+v, %v", pending, err)
	}
	if answerable, err := f.s.ListAnswerableDecisions(ctx, ListOptions{Limit: 10}); err != nil || len(answerable.Items) != 0 {
		t.Fatalf("v33 advertised thread decision answer before registration: %+v, %v", answerable, err)
	}
	if _, err := f.s.AnswerDecisionRequest(ctx, "user", v33Ask.ThreadRef, v33Ask.ID,
		domain.DecisionAnswerInput{Version: v33Ask.Version, OptionIndex: 1}, "v33-answer"); !errors.Is(err, ErrDecisionRelayUnsupported) {
		t.Fatalf("v33 answered thread decision before registration: %v", err)
	}

	// agentRunExplanationSchema absent: explanation write unavailable, legacy
	// agent run log still readable.
	if supported, err := f.s.AgentRunExplanationSupported(ctx); err != nil || supported {
		t.Fatalf("v33 reported run explanation supported: %t, %v", supported, err)
	}
	if _, err := f.s.PublishAgentRunExplanation(ctx, f.scope, "旧库不可写公开说明", "v33-exp"); !errors.Is(err, ErrAgentRunExplanationUnavailable) {
		t.Fatalf("v33 accepted run explanation before registration: %v", err)
	}
	if runBefore, err := f.s.GetAgentRun(ctx, f.scope.Claim.GroupID, f.scope.Claim.TurnID, ListOptions{}); err != nil || len(runBefore.Timeline.Items) == 0 {
		t.Fatalf("v33 legacy agent run read failed: %+v, %v", runBefore, err)
	}

	// messageQuoteSchema absent: plain user message succeeds; quote fails closed.
	plainReq := MessageRequest{
		GroupID:        f.groupID,
		TopicID:        "legacy-topic",
		ActorID:        "user",
		Content:        "v33 普通旧入口消息：必须兼容旧版 CSV",
		RecipientIDs:   []string{},
		IdempotencyKey: "v33-plain-key",
	}
	v33Plain := postThreadMessage(t, f.s, plainReq)
	if v33Plain.ID == "" || v33Plain.TopicID != "legacy-topic" || v33Plain.ReplyTo != nil {
		t.Fatalf("v33 legacy plain message failed: %+v", v33Plain)
	}
	badQuoteReq := plainReq
	badQuoteReq.IdempotencyKey = "v33-quote-rejected"
	badQuoteReq.ReplyToMessageID = v33Plain.ID
	if _, err := f.s.PostLocalUserMessage(ctx, badQuoteReq); !errors.Is(err, ErrInvalid) {
		t.Fatalf("v33 accepted quote before registration: %v", err)
	}

	// projectExperienceSchema absent: search returns empty page, select returns
	// unavailable without consuming the idempotency key.
	expChoice := experienceStoreChoice(v33Plain, v33Plain.ID)
	if page, err := f.s.SearchProjectExperiences(ctx, f.groupID, ProjectExperienceQuery{Query: "CSV-218"}); err != nil || len(page.Items) != 0 {
		t.Fatalf("v33 project experience search was not empty: %+v, %v", page, err)
	}
	if _, err := f.s.SelectProjectExperience(ctx, f.groupID, expChoice, "batch1-e3-key"); !errors.Is(err, ErrProjectExperienceUnavailable) {
		t.Fatalf("v33 accepted project experience before registration: %v", err)
	}

	// Close the v33 handle while leaving the live v33 turn intact for post-upgrade
	// settlement via chatNextCandidate.
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 1b: upgrade v33 -> immediate pre-batch prefix (containing D2/D3 but
	// none of the four batch schemas) and verify legacy continuity + fail-closed.
	batchStart := slices.Index(prior, decisionThreadAnswerSchema)
	migrations = prior[:batchStart]
	sPrior := queueHandle(t, f.path, false)
	messageSequenceLedger(t, sPrior, batchStart)
	if installed, err := sPrior.ThreadDecisionHandoffInstalled(ctx); err != nil || installed {
		t.Fatalf("pre-batch v%d reported thread handoff installed: %t, %v", batchStart, installed, err)
	}
	if supported, err := sPrior.AgentRunExplanationSupported(ctx); err != nil || supported {
		t.Fatalf("pre-batch v%d reported run explanation supported: %t, %v", batchStart, supported, err)
	}
	if _, err := sPrior.PostLocalUserMessage(ctx, badQuoteReq); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pre-batch v%d accepted quote before registration: %v", batchStart, err)
	}
	if _, err := sPrior.SelectProjectExperience(ctx, f.groupID, expChoice, "batch1-e3-key"); !errors.Is(err, ErrProjectExperienceUnavailable) {
		t.Fatalf("pre-batch v%d accepted project experience before registration: %v", batchStart, err)
	}
	if err := sPrior.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 2: upgrade to current public schema via Store.Open and check integrity.
	migrations = prior
	s := queueHandle(t, f.path, false)
	f.s = s
	f.api = chatIntake(t, s)
	messageSequenceLedger(t, s, len(migrations))
	assertSQLiteQuickCheckOK(t, s)
	dispatchAssertFK(t, s)

	// Legacy plain message replays identically after upgrade without duplication.
	plainReplay := postThreadMessage(t, s, plainReq)
	if !reflect.DeepEqual(plainReplay, v33Plain) {
		t.Fatalf("v33 plain message replay drifted after upgrade: got %+v, want %+v", plainReplay, v33Plain)
	}
	var legacyMsgCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE id=?`, v33Plain.ID).Scan(&legacyMsgCount); err != nil || legacyMsgCount != 1 {
		t.Fatalf("legacy message duplicated: count=%d err=%v", legacyMsgCount, err)
	}

	// Phase 3a (decisionThreadAnswerSchema): real normal entrypoint + same-key replay.
	if installed, err := s.ThreadDecisionHandoffInstalled(ctx); err != nil || !installed {
		t.Fatalf("upgraded DB did not detect thread handoff: %t, %v", installed, err)
	}
	s.EnableThreadDecisionExecution()
	// Pre-install v33 clarification was stamped withdrawn on upgrade.
	if answerable, err := s.ListAnswerableDecisions(ctx, ListOptions{Limit: 10}); err != nil || len(answerable.Items) != 0 {
		t.Fatalf("pre-upgrade v33 clarification became retroactively answerable: %+v, %v", answerable, err)
	}
	chatNextCandidate(t, &f, true)
	v34Ask := askThreadDecision(t, f, "batch1-ask")
	if _, err := s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	answerable, err := s.ListAnswerableDecisions(ctx, ListOptions{Limit: 10})
	if err != nil || len(answerable.Items) != 1 || answerable.Items[0].ID != v34Ask.ID {
		t.Fatalf("registered thread clarification not answerable: %+v, %v", answerable, err)
	}
	ansInput := domain.DecisionAnswerInput{Version: v34Ask.Version, OptionIndex: 1}
	ansReceipt, err := s.AnswerDecisionRequest(ctx, "user", v34Ask.ThreadRef, v34Ask.ID, ansInput, "batch1-answer-key")
	if err != nil || ansReceipt.ID == "" || ansReceipt.WakeDeliveryID == "" || ansReceipt.ReplyMessageID == "" {
		t.Fatalf("registered thread answer failed: %+v, %v", ansReceipt, err)
	}
	ansReplay, err := s.AnswerDecisionRequest(ctx, "user", v34Ask.ThreadRef, v34Ask.ID, ansInput, "batch1-answer-key")
	if err != nil || ansReplay != ansReceipt {
		t.Fatalf("thread answer same-key replay mismatch: got %+v, want %+v, err=%v", ansReplay, ansReceipt, err)
	}
	wakes, err := s.ListPendingDeliveries(ctx, PendingOptions{Lane: PendingThreadDecisions, Limit: 5})
	if err != nil || len(wakes.Items) != 1 || wakes.Items[0].Delivery.ID != ansReceipt.WakeDeliveryID {
		t.Fatalf("thread answer wake not queued for original asker: %+v, %v", wakes, err)
	}

	// Phase 3b (agentRunExplanationSchema): real normal entrypoint + same-key replay.
	if supported, err := s.AgentRunExplanationSupported(ctx); err != nil || !supported {
		t.Fatalf("upgraded DB did not detect run explanation: %t, %v", supported, err)
	}
	expDelivery := queueDelivery(t, s, f.groupID, f.b, "explain-topic")
	expTurn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: expDelivery.ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	expScope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: expTurn.GroupID, TurnID: expTurn.ID, Token: expTurn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	const expText = "已完成旧库升级后的公开说明验证。"
	expEntry, err := s.PublishAgentRunExplanation(ctx, expScope, expText, "batch1-explain-key")
	if err != nil || expEntry.Kind != "public_explanation" || expEntry.PublicExplanation == nil || expEntry.PublicExplanation.Text != expText {
		t.Fatalf("publish explanation failed: %+v, %v", expEntry, err)
	}
	expReplay, err := s.PublishAgentRunExplanation(ctx, expScope, expText, "batch1-explain-key")
	if err != nil || !reflect.DeepEqual(expReplay, expEntry) {
		t.Fatalf("explanation same-key replay mismatch: got %+v, want %+v, err=%v", expReplay, expEntry, err)
	}
	if _, err := s.PublishAgentRunExplanation(ctx, expScope, "冲突正文", "batch1-explain-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("explanation allowed conflicting text on same key: %v", err)
	}
	runView, err := s.GetAgentRun(ctx, expTurn.GroupID, expTurn.ID, ListOptions{Limit: 5})
	if err != nil || len(runView.Timeline.Items) == 0 || !reflect.DeepEqual(runView.Timeline.Items[0], expEntry) {
		t.Fatalf("explanation missing from GetAgentRun timeline: %+v, %v", runView, err)
	}

	// Phase 3c (messageQuoteSchema): real normal entrypoint + same-key replay.
	memberMsg, err := s.CreateMessage(ctx, MessageRequest{
		GroupID: f.groupID,
		TopicID: "quote-topic",
		ActorID: f.b,
		Content: "这是待引用的成员原话。",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	quoteReq := MessageRequest{
		GroupID:        f.groupID,
		ActorID:        "user",
		Content:        "请针对这句话补充核对口径。",
		IdempotencyKey: "batch1-quote-key",
		ThreadLinks:    domain.ThreadLinks{ReplyToMessageID: memberMsg.ID},
	}
	quoted := postThreadMessage(t, s, quoteReq)
	if quoted.TopicID != "quote-topic" || quoted.ReplyToMessageID != memberMsg.ID || quoted.ReplyTo == nil ||
		quoted.ReplyTo.MessageID != memberMsg.ID || len(quoted.Deliveries) != 1 || quoted.Deliveries[0].RecipientID != f.b {
		t.Fatalf("quote post did not route to quoted author or project quote: %+v", quoted)
	}
	if dq, err := s.ReadDeliveryQuote(ctx, f.groupID, quoted.Deliveries[0].ID); err != nil || dq == nil || dq.MessageID != memberMsg.ID {
		t.Fatalf("delivery quote read failed: %+v, %v", dq, err)
	}
	compareReq := MessageRequest{
		GroupID:        f.groupID,
		ActorID:        "user",
		Content:        "引用仅作对照。",
		IdempotencyKey: "batch1-compare-key",
		ThreadLinks:    domain.ThreadLinks{ReplyToMessageID: memberMsg.ID},
	}
	compared := postThreadMessage(t, s, compareReq)
	if compared.ReplyTo == nil || compared.ReplyTo.MessageID != memberMsg.ID || len(compared.Deliveries) != 0 {
		t.Fatalf("comparison quote unexpectedly woke author: %+v", compared)
	}

	// Phase 3d (projectExperienceSchema): real normal entrypoint + same-key replay.
	selectedExp, err := s.SelectProjectExperience(ctx, f.groupID, expChoice, "batch1-e3-key")
	if err != nil || selectedExp.ID == "" || selectedExp.Origin.WorkspaceID == "" ||
		selectedExp.Origin.ProjectID == "" || selectedExp.Origin.ChannelID == "" ||
		selectedExp.Origin.GroupID != f.groupID || selectedExp.OriginalMessageID != v33Plain.ID {
		t.Fatalf("select project experience failed: %+v, %v", selectedExp, err)
	}
	selectedReplay, err := s.SelectProjectExperience(ctx, f.groupID, expChoice, "batch1-e3-key")
	if err != nil || !reflect.DeepEqual(selectedReplay, selectedExp) {
		t.Fatalf("project experience same-key replay mismatch: got %+v, want %+v, err=%v", selectedReplay, selectedExp, err)
	}
	conflictChoice := expChoice
	conflictChoice.ApplicableVersion = "v3.0"
	if _, err := s.SelectProjectExperience(ctx, f.groupID, conflictChoice, "batch1-e3-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("project experience allowed conflicting payload on same key: %v", err)
	}
	expPage, err := s.SearchProjectExperiences(ctx, f.groupID, ProjectExperienceQuery{Query: "CSV-218"})
	if err != nil || len(expPage.Items) != 1 || expPage.Items[0].ID != selectedExp.ID || expPage.Items[0].Status != domain.ProjectExperienceActive {
		t.Fatalf("project experience search after selection failed: %+v, %v", expPage, err)
	}
	// Verify D's real project identity binding and permission-first filtering
	// (same-project sibling channel and unauthorized group must return 0 hits even
	// on exact keyword match).
	ownership, err := s.GetGroupOwnership(ctx, f.groupID)
	if err != nil || selectedExp.Origin.WorkspaceID != ownership.WorkspaceID ||
		selectedExp.Origin.ProjectID != ownership.ProjectID ||
		selectedExp.Origin.ChannelID != ownership.ChannelID ||
		selectedExp.Origin.GroupID != ownership.GroupID {
		t.Fatalf("selected experience origin %+v != D ownership %+v (err=%v)", selectedExp.Origin, ownership, err)
	}
	siblingChannel, err := s.CreateChannel(ctx, CreateChannelInput{
		ProjectID:      ownership.ProjectID,
		Name:           "sibling-channel",
		IdempotencyKey: "batch1-sibling-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if siblingHits, err := s.SearchProjectExperiences(ctx, siblingChannel.GroupID, ProjectExperienceQuery{Query: "CSV-218"}); err != nil || len(siblingHits.Items) != 0 {
		t.Fatalf("sibling channel leaked project experience across permission boundary: %+v, %v", siblingHits, err)
	}
	expReader, err := s.CreateMember(ctx, CreateMemberInput{
		ActorID: "user", GroupID: f.groupID, Name: "exp-reader", Role: "分析",
		ProviderID: provider.Pi, Model: "openai-codex/gpt-6.1-sol",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherTopicDelivery := queueDelivery(t, s, f.groupID, expReader.ID, "other-topic")
	otherTopicTurn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: otherTopicDelivery.ID, MaxActive: 4})
	if err != nil {
		t.Fatal(err)
	}
	otherTopicScope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: otherTopicTurn.GroupID, TurnID: otherTopicTurn.ID, Token: otherTopicTurn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	if topicHits, err := s.SearchProjectExperiencesForTool(ctx, otherTopicScope, false, ProjectExperienceQuery{Query: "CSV-218"}, nil); err != nil || len(topicHits.Items) != 0 {
		t.Fatalf("tool search leaked project experience across topic boundary: %+v, %v", topicHits, err)
	}
	if _, err := s.ApplyTurnReceipt(ctx, otherTopicScope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	legacyTopicDelivery := queueDelivery(t, s, f.groupID, expReader.ID, "legacy-topic")
	legacyTopicTurn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: legacyTopicDelivery.ID, MaxActive: 4})
	if err != nil {
		t.Fatal(err)
	}
	legacyTopicScope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: legacyTopicTurn.GroupID, TurnID: legacyTopicTurn.ID, Token: legacyTopicTurn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	if allowedHits, err := s.SearchProjectExperiencesForTool(ctx, legacyTopicScope, false, ProjectExperienceQuery{Query: "CSV-218"}, nil); err != nil || len(allowedHits.Items) != 1 || allowedHits.Items[0].ID != selectedExp.ID {
		t.Fatalf("authorized same-topic tool search missed project experience: %+v, %v", allowedHits, err)
	}

	// Phase 4: close and reopen upgraded DB; verify same-key replays produce zero duplicates.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	reopened.EnableThreadDecisionExecution()

	ansAfterReopen, err := reopened.AnswerDecisionRequest(ctx, "user", v34Ask.ThreadRef, v34Ask.ID, ansInput, "batch1-answer-key")
	if err != nil || ansAfterReopen != ansReceipt {
		t.Fatalf("thread answer replay after reopen drifted: got %+v, want %+v, err=%v", ansAfterReopen, ansReceipt, err)
	}
	expAfterReopen, err := reopened.PublishAgentRunExplanation(ctx, expScope, expText, "batch1-explain-key")
	if err != nil || !reflect.DeepEqual(expAfterReopen, expEntry) {
		t.Fatalf("explanation replay after reopen drifted: got %+v, want %+v, err=%v", expAfterReopen, expEntry, err)
	}
	quoteAfterReopen := postThreadMessage(t, reopened, quoteReq)
	if !reflect.DeepEqual(quoteAfterReopen, quoted) {
		t.Fatalf("quote replay after reopen drifted: got %+v, want %+v", quoteAfterReopen, quoted)
	}
	selectedAfterReopen, err := reopened.SelectProjectExperience(ctx, f.groupID, expChoice, "batch1-e3-key")
	if err != nil || !reflect.DeepEqual(selectedAfterReopen, selectedExp) {
		t.Fatalf("project experience replay after reopen drifted: got %+v, want %+v, err=%v", selectedAfterReopen, selectedExp, err)
	}

	for table, want := range map[string]int{
		"decision_thread_answers": 1,
		"agent_run_explanations":  1,
		"message_quotes":          2,
		"project_experiences":     1,
	} {
		var count int
		if err := reopened.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d want %d (err=%v)", table, count, want, err)
		}
	}
	assertSQLiteQuickCheckOK(t, reopened)
	dispatchAssertFK(t, reopened)
}

func assertSQLiteQuickCheckOK(t *testing.T, s *Store) {
	t.Helper()
	var check string
	if err := s.db.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("PRAGMA quick_check = %q, err=%v", check, err)
	}
}
