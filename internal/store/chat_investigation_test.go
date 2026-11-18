package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

const investigationObjective = "归纳讨论中尚未解决的交付问题"

func TestChatInvestigationRequiresExplicitScope(t *testing.T) {
	f := chatSetup(t, "agent", true) // topic_summary alone is not investigation authority.
	f.s.EnableInvestigationExecution()
	input := investigationProposal()
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, input, "建议调查。", "investigation")
	chatAssertNotAdmitted(t, got, err, "investigation_not_authorized")
}

func TestChatInvestigationPolicyBeforeRuntime(t *testing.T) {
	f := chatSetup(t, "user", false)
	var input domain.ChatPolicyRequest
	if err := json.Unmarshal([]byte(`{"enabled":true,"action":"text_submission","max_admissions":2,"investigation":{"objective":"`+investigationObjective+`","allow_topic_messages":true,"max_delegations":0}}`), &input); err != nil {
		t.Fatal(err)
	}
	policy, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, input, 1, "investigation-scope")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(policy)
	if err != nil || !strings.Contains(string(data), `"investigation":{"objective":"`+investigationObjective+`"`) {
		t.Fatalf("policy did not persist the opt-in scope: %s, %v", data, err)
	}
	chatNextCandidate(t, &f, true)
	proposal := investigationProposal()
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "建议调查。", "investigation")
	chatAssertNotAdmitted(t, got, err, "investigation_runtime_unavailable")
	replay, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "建议调查。", "investigation")
	if err != nil || !reflect.DeepEqual(got, replay) {
		t.Fatalf("denial replay changed: %+v, %v", replay, err)
	}
}

func TestChatInvestigationCandidateExecutionCapability(t *testing.T) {
	f := chatSetup(t, "user", true)
	policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
	arrive := func(topic string) (MemberToolScope, string) {
		t.Helper()
		msg := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: topic, ActorID: "user",
			Content: "请提出建议。", RecipientIDs: []string{f.b}}, []string{f.b})
		return discussionArrive(t, f.s, f.groupID, msg.Deliveries[0].ID)
	}
	check := func(scope MemberToolScope, id string, want domain.ChatPolicy) {
		t.Helper()
		if c, err := f.s.CaptureChatCandidate(ctx, scope); err != nil || c.ID != id || !reflect.DeepEqual(c.Policy, want) {
			t.Fatalf("candidate policy=%+v, %v; want %+v", c.Policy, err, want)
		}
	}
	b, cb := arrive(policy.TopicID)
	ordinary := f.policy
	for _, enabled := range []bool{false, true} {
		if enabled {
			f.s.EnableInvestigationExecution()
		}
		policy.InvestigationExecutionEnabled, ordinary.InvestigationExecutionEnabled = enabled, enabled
		if current, err := f.s.GetChatPolicy(ctx, policy.ThreadRef); err != nil || !reflect.DeepEqual(current, policy) {
			t.Fatalf("current policy=%+v, %v; want %+v", current, err, policy)
		}
		check(b, cb, policy)
		check(f.scope, f.candidate.ID, ordinary) // Still bound to the ordinary revision without an investigation scope.
	}
	discussionEnd(t, f.s, b)
	investigationDisablePolicy(t, f.s, policy)
	b, cb = arrive(policy.TopicID)
	current, err := f.s.GetChatPolicy(ctx, policy.ThreadRef)
	if err != nil || current.Enabled || current.Revision != policy.Revision+1 || !current.InvestigationExecutionEnabled {
		t.Fatalf("disabled policy and process readiness must stay independent: %+v, %v", current, err)
	}
	check(b, cb, current)
	discussionEnd(t, f.s, b)
	b, cb = arrive("unconfigured-topic")
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: b.TopicID}
	unset, err := f.s.GetChatPolicy(ctx, thread)
	if err != nil || unset.Enabled || unset.Revision != 0 || !unset.InvestigationExecutionEnabled {
		t.Fatalf("unexpected revision-zero policy: %+v, %v", unset, err)
	}
	if _, err := f.s.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1}, 0, "new-topic-policy"); err != nil {
		t.Fatal(err)
	}
	check(b, cb, unset) // Revision zero is a disabled snapshot, not a lookup of the current policy.
}

func TestChatInvestigationPolicyOnV22(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	migrations = original[:22]
	// Explicit work supplies a live worker without settling a chat Turn, which
	// requires the v23 native binding schema in the current Store.
	f, _, worker := assignmentBoundWorker(t)
	migrations = original
	messageSequenceLedger(t, f.s, 22)
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: "investigation"}
	f.s.EnableInvestigationExecution()
	if policy, err := f.s.GetChatPolicy(ctx, thread); err != nil || policy.InvestigationSupported || policy.InvestigationExecutionEnabled || policy.Revision != 0 {
		t.Fatalf("v22 claimed investigation support or readiness: %+v, %v", policy, err)
	}
	if got, err := f.s.ResolveCoordinationExecution(ctx, worker.Member, worker.Provenance); err != nil ||
		got.Blocked != CoordinationReady || got.AttemptID != worker.AttemptID {
		t.Fatalf("ordinary v22 worker requires investigation schema: %+v, %v", got, err)
	}
	if got, err := f.s.ResolveInvestigationExecution(ctx, worker.Member, worker.Provenance); !errors.Is(err, ErrMemberToolDenied) || got != (CoordinationRoleState{}) {
		t.Fatalf("ordinary v22 worker resolved as an investigation: %+v, %v", got, err)
	}
}

func TestChatInvestigationHistoricalSchemaAndOrdinaryWorker(t *testing.T) {
	registered := migrations
	for _, version := range []int{23, 24, 25, 26} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			defer func() { migrations = registered }()
			migrations = registered[:version]
			f := chatSetup(t, "user", true)
			messageSequenceLedger(t, f.s, version)
			var present bool
			if err := f.s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='chat_investigation_policy_revisions')`).Scan(&present); err != nil || present {
				t.Fatalf("historical fixture has investigation storage: present=%v, %v", present, err)
			}
			migrations = registered // Current readers against the actual historical database.
			f.s.EnableInvestigationExecution()
			if candidate, err := f.s.CaptureChatCandidate(ctx, f.scope); err != nil || !reflect.DeepEqual(candidate, f.candidate) {
				t.Fatalf("historical candidate policy changed: %+v, %v", candidate, err)
			}
			thread := f.policy.ThreadRef
			if got, err := f.s.GetChatPolicy(ctx, thread); err != nil || !reflect.DeepEqual(got, f.policy) ||
				!got.PreparationSupported || got.InvestigationSupported || got.InvestigationExecutionEnabled {
				t.Fatalf("historical policy or schema capability changed: %+v, %v", got, err)
			}
			input := f.policy.ChatPolicyRequest
			input.PreauthorizationMessageID = ""
			input.Investigation = &domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}
			if got, err := f.s.SetChatPolicy(ctx, thread, input, f.policy.Revision, "no-investigation-schema"); !errors.Is(err, ErrInvalid) || got.Revision != 0 {
				t.Fatalf("historical schema accepted investigation configuration: %+v, %v", got, err)
			}

			f.input = domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议准备话题摘要。"}
			worker := chatAuthorityWorker(t, f) // Ordinary admission, assignment and attempt binding.
			scope, err := f.s.GetTaskInvestigationScope(ctx, f.groupID, worker.Provenance.TaskID)
			requestDispatchZero(t, scope, err, ErrNotFound)
			if got, err := f.s.ResolveCoordinationExecution(ctx, worker.Member, worker.Provenance); err != nil ||
				got.Blocked != CoordinationReady || got.AttemptID != worker.AttemptID {
				t.Fatalf("ordinary worker requires investigation schema: %+v, %v", got, err)
			}
			if got, err := f.s.ResolveInvestigationExecution(ctx, worker.Member, worker.Provenance); !errors.Is(err, ErrMemberToolDenied) || got != (CoordinationRoleState{}) {
				t.Fatalf("ordinary worker resolved as an investigation: %+v, %v", got, err)
			}
			result := AssignmentResultInput{Summary: "准备后的普通文本摘要。"}
			submission, err := f.s.SubmitAssignmentResult(ctx, worker, result, "ordinary-result")
			if err != nil || submission.ID == "" {
				t.Fatalf("ordinary submission requires investigation schema: %+v, %v", submission, err)
			}

			upgraded := queueHandle(t, f.path, false)
			messageSequenceLedger(t, upgraded, len(registered))
			want := f.policy
			want.InvestigationSupported = true
			if got, err := upgraded.GetChatPolicy(ctx, thread); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("upgrade changed legacy policy or enabled investigation: %+v, %v", got, err)
			}
			if got, err := upgraded.SubmitAssignmentResult(ctx, worker, result, "ordinary-result"); err != nil || !reflect.DeepEqual(got, submission) {
				t.Fatalf("upgrade changed ordinary submission replay: %+v, %v", got, err)
			}
		})
	}
}

func investigationProposal() domain.ChatAssessment {
	return domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionReadOnlyInvestigation,
		Title: "不是用户批准的目标", Description: "不是用户批准的资料", Acceptance: "不是用户批准的验收", Reason: "建议调查未决问题"}
}

func investigationPolicy(t *testing.T, f chatFixture, config domain.ChatInvestigationPolicy, budget int) domain.ChatPolicy {
	t.Helper()
	input := f.policy.ChatPolicyRequest
	input.PreauthorizationMessageID = "" // Server evidence, not an input selector.
	input.Investigation, input.MaxAdmissions = &config, budget
	policy, err := f.s.SetChatPolicy(ctx, f.candidate.ThreadRef, input, f.policy.Revision, "configure-investigation")
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestChatInvestigationPolicyValidationAndHistoricalReplay(t *testing.T) {
	f := chatSetup(t, "user", true)
	oldJSON, _ := json.Marshal(f.policy)
	if strings.Contains(string(oldJSON), `"investigation"`) {
		t.Fatal("legacy policy JSON gained an opt-in field")
	}
	own := mustPublishArtifact(t, f.s, artifactPublishInput(f.groupID))
	other := mustPublishArtifact(t, f.s, artifactPublishInput(f.otherID))
	ref := domain.ArtifactVersionRef{ArtifactID: own.ArtifactID, VersionID: own.ID}
	foreign := domain.ArtifactVersionRef{ArtifactID: other.ArtifactID, VersionID: other.ID}
	legacyInput := f.policy.ChatPolicyRequest
	legacyInput.PreauthorizationMessageID = ""
	input := legacyInput
	input.Investigation = &domain.ChatInvestigationPolicy{Objective: investigationObjective, ArtifactVersions: []domain.ArtifactVersionRef{ref}, MaxDelegations: 4}
	before := messageSequenceSnapshot(t, f.s)
	for name, mutate := range map[string]func(*domain.ChatInvestigationPolicy){
		"missing-material": func(p *domain.ChatInvestigationPolicy) { p.ArtifactVersions = nil },
		"wrong-group":      func(p *domain.ChatInvestigationPolicy) { p.ArtifactVersions = []domain.ArtifactVersionRef{foreign} },
		"wrong-artifact-version-pair": func(p *domain.ChatInvestigationPolicy) {
			p.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: own.ArtifactID, VersionID: foreign.VersionID}}
		},
		"duplicate":       func(p *domain.ChatInvestigationPolicy) { p.ArtifactVersions = []domain.ArtifactVersionRef{ref, ref} },
		"too-many":        func(p *domain.ChatInvestigationPolicy) { p.ArtifactVersions = make([]domain.ArtifactVersionRef, 17) },
		"out-of-range":    func(p *domain.ChatInvestigationPolicy) { p.MaxDelegations = 5 },
		"blank-objective": func(p *domain.ChatInvestigationPolicy) { p.Objective = " \t　" },
		"long-objective":  func(p *domain.ChatInvestigationPolicy) { p.Objective = strings.Repeat("字", 86) },
	} {
		t.Run(name, func(t *testing.T) {
			bad, config := input, *input.Investigation
			mutate(&config)
			bad.Investigation = &config
			if _, err := f.s.SetChatPolicy(ctx, f.candidate.ThreadRef, bad, 1, "configure-investigation"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid policy accepted: %v", err)
			}
		})
	}
	messageSequencePreserved(t, f.s, before)
	policy := investigationPolicy(t, f, *input.Investigation, input.MaxAdmissions)
	if policy.Revision != 2 || !policy.PreparationSupported || !reflect.DeepEqual(policy.Investigation, input.Investigation) {
		t.Fatalf("wrong policy: %+v", policy)
	}
	stored, err := f.s.GetChatPolicy(ctx, policy.ThreadRef)
	if err != nil || !reflect.DeepEqual(stored, policy) {
		t.Fatalf("GET differs from configuration: %+v, %v", stored, err)
	}
	if _, err := f.s.SetChatPolicy(ctx, policy.ThreadRef, input, 1, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	changed := *input.Investigation
	changed.Objective += "？"
	reused := input
	reused.Investigation = &changed
	if _, err := f.s.SetChatPolicy(ctx, policy.ThreadRef, reused, 1, "configure-investigation"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload replayed: %v", err)
	}
	if _, err := f.s.SetChatPolicy(ctx, policy.ThreadRef, legacyInput, 2, "remove-investigation"); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	replayed, err := reopened.SetChatPolicy(ctx, policy.ThreadRef, input, 1, "configure-investigation")
	if err != nil || !reflect.DeepEqual(replayed, policy) {
		t.Fatalf("historical policy/source replay changed: %+v, %v", replayed, err)
	}
	if current, err := reopened.GetChatPolicy(ctx, policy.ThreadRef); err != nil || current.Revision != 3 || current.Investigation != nil {
		t.Fatalf("replay rewound the current policy: %+v, %v", current, err)
	}
	for _, query := range []string{
		"UPDATE chat_investigation_policy_revisions SET scope_json=scope_json",
		"DELETE FROM chat_investigation_policy_revisions",
		"INSERT OR REPLACE INTO chat_investigation_policy_revisions SELECT * FROM chat_investigation_policy_revisions",
		"INSERT INTO chat_investigation_policy_revisions SELECT group_id,topic_id,3,scope_json FROM chat_investigation_policy_revisions",
	} {
		if _, err := reopened.db.Exec(query); err == nil {
			t.Fatalf("mutable/retroactive authorization: %s", query)
		}
	}
}

func investigationBindCandidate(t *testing.T, f *chatFixture, delivery domain.Delivery) {
	t.Helper()
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.scope, err = f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	f.candidate, err = f.s.CaptureChatCandidate(ctx, f.scope)
	if err != nil {
		t.Fatal(err)
	}
}

func investigationFinishTurn(t *testing.T, f chatFixture) {
	t.Helper()
	if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
}

func TestChatInvestigationReplayAndSharedBudget(t *testing.T) {
	f := chatSetup(t, "user", true)
	investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
	f.s.EnableInvestigationExecution()
	investigationFinishTurn(t, f)
	message := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user",
		Content: "请提出调研建议。", RecipientIDs: []string{f.a, f.b}}, []string{f.a, f.b})
	investigationBindCandidate(t, &f, message.Deliveries[0])
	proposal := investigationProposal()
	first, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "建议调查。", "admit")
	if err != nil || first.Outcome != "admitted" || first.SourceKind != "local_user_chat" || first.SourceID != message.SourceID {
		t.Fatalf("admission: %+v, %v", first, err)
	}
	before := messageSequenceSnapshot(t, f.s)
	replayed, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "建议调查。", "admit")
	if err != nil || !reflect.DeepEqual(replayed, first) {
		t.Fatalf("admission replay: %+v, %v", replayed, err)
	}
	messageSequencePreserved(t, f.s, before)
	investigationFinishTurn(t, f)
	investigationBindCandidate(t, &f, message.Deliveries[1])
	if f.candidate.ID == first.CandidateID || f.candidate.SourceID != first.SourceID {
		t.Fatal("fixture did not capture separate candidates for the same source")
	}
	second, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "建议调查。", "admit-other-candidate")
	chatAssertNotAdmitted(t, second, err, "source_already_routed")
	chatNextCandidate(t, &f, true)
	summary := domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议总结本话题"}
	third, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, summary, "建议摘要。", "summary")
	chatAssertNotAdmitted(t, third, err, "budget_exhausted")
	for _, table := range []string{"tasks", "mandates", "coordination_dispatch_requests"} {
		var count int
		if err := f.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d, %v", table, count, err)
		}
	}
}

func TestChatInvestigationAdmissionDenials(t *testing.T) {
	for name, reason := range map[string]string{
		"untrusted": "untrusted_source", "stale-policy": "stale_policy", "stale-source": "stale_source",
		"no-eligible-worker": "routing_unavailable", "disabled": "policy_disabled", "request-work": "investigation_prepare_required",
	} {
		t.Run(name, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
			f.s.EnableInvestigationExecution()
			chatNextCandidate(t, &f, name != "untrusted")
			proposal := investigationProposal()
			switch name {
			case "stale-policy", "disabled":
				input := policy.ChatPolicyRequest
				input.Enabled, input.PreauthorizationMessageID = name != "disabled", ""
				if _, err := f.s.SetChatPolicy(ctx, policy.ThreadRef, input, policy.Revision, "replace"); err != nil {
					t.Fatal(err)
				}
			case "stale-source":
				chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: "先不要调查。"}, nil)
			case "no-eligible-worker":
				coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.b)
			case "request-work":
				proposal.Disposition = "request_work"
			}
			got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, proposal, "建议调查。", "denial")
			chatAssertNotAdmitted(t, got, err, reason)
			notice, err := f.s.GetMessage(ctx, f.groupID, got.NoticeMessageID)
			if err != nil || notice.TopicID != f.scope.TopicID || strings.Count(notice.Content, "？") != 1 || !strings.HasSuffix(notice.Content, got.Question) {
				t.Fatalf("expected just one concrete question in the origin: %+v, %v", notice, err)
			}
		})
	}
}
