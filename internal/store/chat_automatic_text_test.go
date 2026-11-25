package store

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func withChatAutomaticTextSchema(t *testing.T) {
	t.Helper()
	if slices.Contains(migrations, chatAutomaticTextSchema) {
		return
	}
	original := migrations
	migrations = append(slices.Clone(original), chatAutomaticTextSchema)
	t.Cleanup(func() { migrations = original })
}

const automaticTextRequest = "按刚才讨论的开始做。请在原话题交付两行纯文本说明，不改文件。"

var automaticTextProposal = domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionTextSubmission,
	Title: "季度统计口径", Description: "仅原群纯文本，不得使用目录。", Acceptance: "分别说明本季合计及同比对照。", Reason: "本机用户明确要求交付。"}

type autoTextFixture struct {
	s          *Store
	path       string
	thread     domain.ThreadRef
	co, worker domain.Member
	facts      ChatAutomaticTextFacts
}

func newAutoTextFixture(t *testing.T) autoTextFixture {
	t.Helper()
	withChatAutomaticTextSchema(t)
	s, path := openTestStore(t)
	f := autoTextFixture{s: s, path: path, thread: domain.ThreadRef{GroupID: group(t, s).ID}}
	f.co = f.member(t, f.thread.GroupID, "background", "intake")
	f.worker = f.member(t, f.thread.GroupID, "user", "approved-worker")
	f.facts = ChatAutomaticTextFacts{Coordinator: automaticTextFact(f.co), Workers: []DispatchWorkerCapabilityFact{automaticTextFact(f.worker)}}
	return f
}

func (f autoTextFixture) member(t *testing.T, groupID, creator, key string) domain.Member {
	t.Helper()
	m, err := f.s.CreateMember(ctx, CreateMemberInput{GroupID: groupID, Name: key, Role: "user", ActorID: creator,
		CreatorID: creator, OwnerID: "user", ProviderID: "fixture-provider", Model: t.Name(), IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func automaticTextFact(m domain.Member) DispatchWorkerCapabilityFact {
	return DispatchWorkerCapabilityFact{GroupID: m.GroupID, MemberID: m.ID, ProviderID: m.ProviderID, Model: m.Model}
}

func (f autoTextFixture) policy(t *testing.T, topic string, rev int64, input domain.ChatPolicyRequest) domain.ChatPolicy {
	t.Helper()
	p, err := f.s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.thread.GroupID, TopicID: topic}, input, rev, "rule-"+t.Name()+topic+input.PreparationScopeKind)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func automaticTextPolicy(budget int) domain.ChatPolicyRequest {
	return domain.ChatPolicyRequest{Enabled: true, AutomaticTextDispatch: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: budget}
}

func (f autoTextFixture) post(t *testing.T, topic, body string, recipients ...string) domain.Message {
	t.Helper()
	return chatLocalMessage(t, f.s, MessageRequest{GroupID: f.thread.GroupID, TopicID: topic, ActorID: "user", Content: body, RecipientIDs: recipients}, recipients)
}

func (f autoTextFixture) claim(t *testing.T, m domain.Message, actor string) (MemberToolScope, string) {
	t.Helper()
	for _, d := range m.Deliveries {
		if d.RecipientID == actor {
			return discussionArrive(t, f.s, m.GroupID, d.ID)
		}
	}
	t.Fatal("missing exact ordinary delivery")
	return MemberToolScope{}, ""
}

func (f autoTextFixture) assess(scope MemberToolScope, c string, input domain.ChatAssessment, key string) (domain.ChatAssessmentReceipt, error) {
	return f.s.ApplyChatAssessmentWithAutomaticText(ctx, scope, c, input, "处理纯文本请求。", key, f.facts)
}

func automaticWorkSnapshot(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	out := map[string][][]any{}
	for _, table := range []string{"group_coordinator_binding_revisions", "group_coordinator_bindings", "group_dispatch_policy_revisions",
		"group_dispatch_policies", "chat_automatic_text_routes", "tasks", "mandates", "routing_decisions", "coordination_dispatch_requests",
		"coordination_dispatch_state", "coordination_assignments", "task_attempts", "task_submissions", "resource_claims"} {
		out[table] = messageSequenceTableRows(t, s, table)
	}
	return out
}

func assertAutomaticTextQuestion(t *testing.T, s *Store, r domain.ChatAssessmentReceipt, err error, code string) {
	t.Helper()
	chatAssertNotAdmitted(t, r, err, code)
	notice, err := s.GetMessage(ctx, r.GroupID, r.NoticeMessageID)
	if err != nil || notice.TopicID != r.TopicID || notice.SenderID != r.AssessedBy || len(notice.Deliveries) != 0 ||
		strings.Count(notice.Content, "？") != 1 || !strings.HasSuffix(notice.Content, r.Question) {
		t.Fatalf("expected one public question without a wake: %+v, %v", notice, err)
	}
}

func TestChatAutomaticTextAdmissionProvenanceAndSourceReplay(t *testing.T) {
	f := newAutoTextFixture(t)
	p := f.policy(t, "", 0, automaticTextPolicy(1))
	origin := f.post(t, "", automaticTextRequest, f.co.ID, f.worker.ID)
	co, c := f.claim(t, origin, f.co.ID)
	peer, cPeer := f.claim(t, origin, f.worker.ID)
	r, err := f.assess(co, c, automaticTextProposal, "admit")
	if err != nil || r.TaskID == "" || r.SourceID != origin.SourceID || r.MessageID != origin.ID || r.ThreadRef != f.thread ||
		r.PolicyRevision != p.Revision || r.AssessedBy != f.co.ID || r.TurnID != co.Claim.TurnID ||
		!r.AuthorityEvidenceVerified || r.Authority != "explicit_request" || r.AuthorizationMessageID != origin.ID {
		t.Fatalf("missing claim-bound admission: %+v %v", r, err)
	}
	view, err := f.s.GetDispatchRequest(ctx, f.thread.GroupID, r.DispatchRequestID)
	if err != nil || view.Request.TopicID != "" || view.Request.MandateID != r.MandateID || view.Request.CoordinatorMemberID != f.co.ID ||
		view.Request.BindingRevision != 1 || view.Request.PolicyRevision != 1 || view.State.Status != domain.DispatchStatusPending {
		t.Fatalf("no formal pending dispatch: %+v %v", view, err)
	}
	task, err := f.s.GetTask(ctx, r.GroupID, r.TaskID)
	if err != nil || task.AssigneeID != f.worker.ID || task.Status != domain.TaskStatusOpen || task.ActiveAttemptID != "" || task.Acceptance != automaticTextProposal.Acceptance {
		t.Fatalf("wrong scope, participant or stage: %+v %v", task, err)
	}
	rows := messageSequenceRows(t, f.s.db, `SELECT a.source_id,c.message_id,c.generation,a.group_id,a.topic_id,a.chat_policy_revision,s.command_source_id,
	 rule.kind, a.coordinator_id,a.coordinator_provider_id,a.coordinator_model,a.worker_id,a.worker_provider_id,a.worker_model,a.worker_creation_key,a.initializes_routing,
	 json_extract(saved.receipt_json,'$.automatic_text_routing_checked')
	 FROM chat_automatic_text_routes a JOIN chat_assessments saved ON saved.decision_id=a.decision_id
	 JOIN chat_candidates c ON c.id=a.candidate_id JOIN chat_policy_scopes s
	 ON (s.group_id,s.topic_id,s.revision)=(a.group_id,a.topic_id,a.chat_policy_revision) JOIN message_command_sources rule ON rule.id=s.command_source_id`)
	want := [][]any{{origin.SourceID, origin.ID, r.CandidateGeneration, r.GroupID, "", p.Revision, p.ConfigurationSourceID, "local_user_policy",
		f.co.ID, f.co.ProviderID, f.co.Model, f.worker.ID, f.worker.ProviderID, f.worker.Model, "approved-worker", int64(1), int64(1)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("incorrect selection/rule provenance: %v; want %v", rows, want)
	}
	beforeAliases := messageSequenceSnapshot(t, f.s)
	for _, proposal := range []domain.ChatAssessment{
		automaticTextProposal,
		{Disposition: domain.ChatDispositionContinueWork, Reason: "误评为后续输入。"},
		{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "误评为准备。"},
		{Disposition: "discuss", Reason: "误评为讨论。"},
		{Disposition: "clarify", Question: "需要澄清吗？", Reason: "误评为澄清。"},
	} {
		key := "peer-" + proposal.Disposition
		for range 2 {
			if got, err := f.assess(peer, cPeer, proposal, key); err != nil || !reflect.DeepEqual(got, r) {
				t.Fatalf("another candidate with %s did not replay the one source: %+v %v", proposal.Disposition, got, err)
			}
		}
	}
	delete(beforeAliases, "mutation_idempotency") // Only transport aliases may be added.
	messageSequencePreserved(t, f.s, beforeAliases)
	disabled := p.ChatPolicyRequest
	disabled.Enabled = false
	if _, err := f.s.SetChatPolicy(ctx, f.thread, disabled, p.Revision, "disable"); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	f.s = queueHandle(t, f.path, false)
	for _, pair := range []struct {
		scope   MemberToolScope
		id, key string
	}{{co, c, "admit"}, {peer, cPeer, "peer-request_work"}} {
		if got, err := f.s.ApplyChatAssessment(ctx, pair.scope, pair.id, automaticTextProposal, "处理纯文本请求。", pair.key); err != nil || !reflect.DeepEqual(got, r) {
			t.Fatalf("durable replay incorrectly needed live capability or current opt-in: %+v %v", got, err)
		}
	}
	wrong := peer
	wrong.Claim.Token += "bad"
	if _, err := f.assess(wrong, cPeer, automaticTextProposal, "peer-request_work"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("invalid claim read same-source receipt: %v", err)
	}
	changed := automaticTextProposal
	changed.Acceptance += "不同载荷"
	if _, err := f.assess(co, c, changed, "admit"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same-key changed payload: %v", err)
	}
	if _, err := f.assess(peer, cPeer, automaticTextProposal, "peer-continue_work"); !errors.Is(err, ErrConflict) {
		t.Fatalf("aliased key did not bind the later proposal's payload: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
	for _, table := range []string{"chat_automatic_text_routes", "chat_automatic_text_policy_revisions"} {
		for _, q := range []string{"UPDATE " + table + " SET group_id=group_id", "DELETE FROM " + table, "INSERT OR REPLACE INTO " + table + " SELECT * FROM " + table} {
			if _, err := f.s.db.Exec(q); err == nil {
				t.Fatalf("mutable automatic routing evidence: %s", q)
			}
		}
	}
	dispatchAssertFK(t, f.s)
}

func TestChatAutomaticTextDenialsHaveNoWorkOrRouting(t *testing.T) {
	for mode, reason := range map[string]string{
		"missing-policy": "policy_disabled", "disabled": "policy_disabled", "omitted-opt-in": "routing_unavailable",
		"no-runtime": "automatic_text_coordinator_unavailable", "wrong-coordinator": "automatic_text_coordinator_unavailable",
		"worker-foreign": "automatic_text_worker_unavailable", "same-member": "automatic_text_worker_unavailable",
		"departed": "automatic_text_worker_unavailable", "model-changed": "automatic_text_worker_unavailable",
		"worker-workspace": "automatic_text_worker_unavailable", "coordinator-workspace": "automatic_text_coordinator_unavailable",
		"member-created-owner-user": "automatic_text_worker_unavailable", "unkeyed-member": "automatic_text_worker_unavailable",
		"only-discussion": "discussion_only", "past-and-current-discussion": "discussion_only", "bare-time-marker": "discussion_only",
		"past-and-current-no-start": "discussion_only", "past-and-current-no-work": "discussion_only", "past-and-current-no-task": "discussion_only",
		"stale-source": "stale_source", "stale-policy": "stale_policy", "sender-label": "untrusted_source",
	} {
		t.Run(mode, func(t *testing.T) {
			f := newAutoTextFixture(t)
			input := automaticTextPolicy(1)
			input.Enabled = mode != "disabled"
			input.AutomaticTextDispatch = mode != "omitted-opt-in"
			if mode != "missing-policy" {
				f.policy(t, "", 0, input)
			}
			var m domain.Message
			switch mode {
			case "only-discussion":
				m = f.post(t, "", "这次只讨论，不开工。去年同期列计入本季合计吗？", f.co.ID)
			case "past-and-current-discussion":
				m = f.post(t, "", "上一条只讨论；现在这次只讨论，不开工。", f.co.ID)
			case "past-and-current-no-start":
				m = f.post(t, "", "上一条只讨论；现在不要启动任务", f.co.ID)
			case "past-and-current-no-work":
				m = f.post(t, "", "上一条只讨论；本次不要开始工作。", f.co.ID)
			case "past-and-current-no-task":
				m = f.post(t, "", "上一条只讨论；这次不启动任务。", f.co.ID)
			case "bare-time-marker":
				m = f.post(t, "", "上一条只讨论；现在。", f.co.ID)
			case "sender-label":
				var err error
				m, err = f.s.CreateMessage(ctx, MessageRequest{GroupID: f.thread.GroupID, ActorID: "user", Content: automaticTextRequest}, []string{f.co.ID})
				if err != nil {
					t.Fatal(err)
				}
			default:
				m = f.post(t, "", automaticTextRequest, f.co.ID)
			}
			scope, id := f.claim(t, m, f.co.ID)
			switch mode {
			case "no-runtime":
				f.facts = ChatAutomaticTextFacts{}
			case "wrong-coordinator":
				f.facts.Coordinator = automaticTextFact(f.worker)
			case "same-member":
				f.facts.Workers = []DispatchWorkerCapabilityFact{automaticTextFact(f.co)}
			case "worker-foreign":
				f.facts.Workers = []DispatchWorkerCapabilityFact{automaticTextFact(f.member(t, group(t, f.s).ID, "user", "foreign"))}
			case "departed":
				coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.worker.ID)
			case "model-changed":
				coordinationDecideExec(t, f.s, "UPDATE members SET model=model||'-changed' WHERE id=?", f.worker.ID)
			case "worker-workspace":
				coordinationDecideExec(t, f.s, "UPDATE members SET workspace=? WHERE id=?", t.TempDir(), f.worker.ID)
			case "coordinator-workspace":
				coordinationDecideExec(t, f.s, "UPDATE members SET workspace=? WHERE id=?", t.TempDir(), f.co.ID)
			case "member-created-owner-user":
				m, err := f.s.CreateMemberForTool(ctx, scope, MemberToolCreateInput{Name: "讨论伙伴", Role: "user", Reason: "只协助讨论"},
					MemberToolCreatePolicy{ProviderID: f.worker.ProviderID, Model: f.worker.Model + "-discussion", MaxMembers: 4}, "ordinary-member-tool")
				if err != nil || m.Reused || m.Member.CreatorID != f.co.ID || m.Member.OwnerID != "user" {
					t.Fatalf("fixture requires real member-created participant: %+v %v", m, err)
				}
				f.facts.Workers = []DispatchWorkerCapabilityFact{automaticTextFact(m.Member)}
			case "unkeyed-member":
				m, err := f.s.CreateMember(ctx, CreateMemberInput{GroupID: f.thread.GroupID, ActorID: "user", CreatorID: "user",
					Name: "无凭据成员", Role: "user", OwnerID: "user", ProviderID: f.worker.ProviderID, Model: f.worker.Model})
				if err != nil {
					t.Fatal(err)
				}
				f.facts.Workers = []DispatchWorkerCapabilityFact{automaticTextFact(m)}
			case "stale-source":
				f.post(t, "", "我先补充另一个要求。")
			case "stale-policy":
				if _, err := f.s.SetChatPolicy(ctx, f.thread, input, 1, "changed"); err != nil {
					t.Fatal(err)
				}
			}
			before := automaticWorkSnapshot(t, f.s)
			r, err := f.assess(scope, id, automaticTextProposal, "assess")
			assertAutomaticTextQuestion(t, f.s, r, err, reason)
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestChatAutomaticTextClosedDiscussionAndDeniedSourceDoNotSpendQuota(t *testing.T) {
	f := newAutoTextFixture(t)
	f.policy(t, "", 0, automaticTextPolicy(1))
	root := f.post(t, "", "本次只讨论，不启动任务。", f.co.ID, f.worker.ID)
	co, c := f.claim(t, root, f.co.ID)
	peer, cPeer := f.claim(t, root, f.worker.ID)
	if _, err := f.assess(co, c, domain.ChatAssessment{Disposition: "discuss", Reason: "仅讨论。", DiscussionOnlyExcerpt: new("本次只讨论")}, "discussion"); err != nil {
		t.Fatal(err)
	}
	discussionApply(t, f.s, co, c, "resolve", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionResolve, Content: "本季合计不含去年同期。"})
	before := automaticWorkSnapshot(t, f.s)
	r, err := f.assess(peer, cPeer, automaticTextProposal, "denied-old-root")
	assertAutomaticTextQuestion(t, f.s, r, err, "discussion_only")
	messageSequencePreserved(t, f.s, before)
	discussionEnd(t, f.s, co)
	discussionEnd(t, f.s, peer)

	origin := f.post(t, "", automaticTextRequest, f.co.ID, f.worker.ID)
	co, c = f.claim(t, origin, f.co.ID)
	peer, cPeer = f.claim(t, origin, f.worker.ID)
	r, err = f.s.ApplyChatAssessment(ctx, co, c, automaticTextProposal, "没有采样。", "no-sampler")
	assertAutomaticTextQuestion(t, f.s, r, err, "automatic_text_coordinator_unavailable")
	denied := messageSequenceSnapshot(t, f.s)
	for _, proposal := range []domain.ChatAssessment{
		automaticTextProposal,
		{Disposition: domain.ChatDispositionContinueWork, Reason: "误评为后续输入。"},
		{Disposition: "discuss", Reason: "误评为讨论。"},
	} {
		for range 2 {
			if replay, err := f.assess(peer, cPeer, proposal, "denied-"+proposal.Disposition); err != nil || !reflect.DeepEqual(replay, r) {
				t.Fatalf("different %s proposal repeated a denial or changed its outcome: %+v %v", proposal.Disposition, replay, err)
			}
		}
	}
	delete(denied, "mutation_idempotency")
	messageSequencePreserved(t, f.s, denied)
	messageSequencePreserved(t, f.s, before)
	discussionEnd(t, f.s, co)
	discussionEnd(t, f.s, peer)

	origin = f.post(t, "", "上一条只讨论；现在开始做这项纯文本交付，不要改文件。", f.co.ID)
	co, c = f.claim(t, origin, f.co.ID)
	r, err = f.assess(co, c, automaticTextProposal, "new-independent-source")
	if err != nil || r.TaskID == "" || r.MessageID != origin.ID || r.MessageID == root.ID {
		t.Fatalf("denials or a closed discussion blocked the independently authorized request: %+v %v", r, err)
	}
}

func TestChatAutomaticTextLateDiscussionLimitFencesDescendant(t *testing.T) {
	for _, outcome := range []string{"not_admitted", "admitted"} {
		t.Run(outcome, func(t *testing.T) {
			f := newAutoTextFixture(t)
			policy := automaticTextPolicy(2)
			policy.AllowPreparation, policy.PreparationScopeKind = true, domain.ChatPreparationTopicSummary
			f.policy(t, "", 0, policy)
			quote := "本次只讨论"
			var routingRevision int64
			if outcome == "admitted" {
				// A misassessment can miss wording outside the narrow veto.
				quote, routingRevision = "本次仅交换意见，暂缓落实", 1
			}
			root := f.post(t, "", quote+"。", f.co.ID, f.worker.ID)
			co, c := f.claim(t, root, f.co.ID)
			peer, cPeer := f.claim(t, root, f.worker.ID)
			work, err := f.assess(co, c, automaticTextProposal, "work-first")
			if outcome == "not_admitted" {
				assertAutomaticTextQuestion(t, f.s, work, err, "discussion_only")
			} else if err != nil || work.Outcome != outcome || work.TaskID == "" {
				t.Fatalf("fixture needs a saved admission: %+v, %v", work, err)
			}
			plain := domain.ChatAssessment{Disposition: "discuss", Reason: "先交换意见。"}
			if got, err := f.assess(peer, cPeer, plain, "work-alias"); err != nil || !reflect.DeepEqual(got, work) {
				t.Fatalf("plain discuss did not alias work: %+v, %v", got, err)
			}
			limit := plain
			limit.DiscussionOnlyExcerpt = new("原文没有的限制")
			before := messageSequenceSnapshot(t, f.s)
			if got, err := f.assess(peer, cPeer, limit, "forged"); !errors.Is(err, ErrInvalid) || got.ID != "" {
				t.Errorf("forged excerpt was not rejected: %+v, %v", got, err)
			} else {
				messageSequencePreserved(t, f.s, before)
			}
			limit.DiscussionOnlyExcerpt = &quote
			if got, err := f.assess(peer, cPeer, limit, "work-alias"); !errors.Is(err, ErrConflict) || got.ID != "" {
				t.Fatalf("changing a saved alias's payload did not conflict: %+v, %v", got, err)
			}
			beforeWork := automaticWorkSnapshot(t, f.s)
			proof, err := f.assess(peer, cPeer, limit, "verified-limit")
			if err != nil || proof.Outcome != "discuss" || !proof.DiscussionOnly || proof.DiscussionOnlyExcerpt != quote ||
				proof.SourceID != root.SourceID || proof.CandidateID != cPeer || proof.TaskID != "" || proof.MandateID != "" || proof.DispatchRequestID != "" {
				t.Errorf("verified source limitation was not saved separately: %+v, %v", proof, err)
			}
			before = messageSequenceSnapshot(t, f.s)
			if got, err := f.assess(peer, cPeer, limit, "verified-limit"); err != nil || !reflect.DeepEqual(got, proof) {
				t.Fatalf("restriction key did not replay exactly: %+v, %v", got, err)
			}
			if got, err := f.assess(peer, cPeer, plain, "work-alias"); err != nil || !reflect.DeepEqual(got, work) {
				t.Fatalf("restriction rewrote a saved work alias: %+v, %v", got, err)
			}
			messageSequencePreserved(t, f.s, before)
			messageSequencePreserved(t, f.s, beforeWork)

			// The child has a manual pair and a valid written preparation scope.
			if _, err := f.s.SetCoordinator(ctx, root.GroupID, f.co.ID, routingRevision, "manual-coordinator"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.SetDispatchPolicy(ctx, root.GroupID, []string{f.worker.ID}, routingRevision, "manual-workers"); err != nil {
				t.Fatal(err)
			}
			relay, err := f.s.SendMessageForChatIntake(ctx, co, c, MemberToolMessageInput{Content: "请补充意见。", RecipientIDs: []string{f.worker.ID}}, "relay")
			if err != nil || len(relay.Deliveries) != 1 {
				t.Fatalf("fixture relay: %+v, %v", relay, err)
			}
			discussionEnd(t, f.s, peer)
			child, cChild := f.claim(t, relay, f.worker.ID)
			beforeWork = automaticWorkSnapshot(t, f.s)
			denied, err := f.assess(child, cChild, domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议准备话题摘要。"}, "descendant")
			assertAutomaticTextQuestion(t, f.s, denied, err, "discussion_only")
			messageSequencePreserved(t, f.s, beforeWork)
		})
	}
}

func TestChatAutomaticTextLateDiscussionLimitReplaysManualFollowup(t *testing.T) {
	f := chatSetup(t, "user", false)
	worker := chatAuthorityWorker(t, f)
	const quote = "对统计口径只讨论"
	recipients := []string{member(t, f.s, f.groupID).ID, member(t, f.s, f.groupID).ID}
	message := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user",
		Content: "请继续任务 " + worker.Provenance.TaskID + "，" + quote + "。", RecipientIDs: recipients}, recipients)
	first, cFirst := workInputCandidate(t, f, message, 0)
	peer, cPeer := workInputCandidate(t, f, message, 1)
	input := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "补充原任务。",
		Followup: &domain.ChatFollowupAssessment{TaskID: worker.Provenance.TaskID}}
	continued, err := f.s.ApplyChatAssessmentDormant(ctx, first, cFirst.ID, input, "记录补充。", "followup")
	if err != nil || continued.Outcome != "continued" || continued.WorkInputID == "" || continued.SourceID != message.SourceID {
		t.Fatalf("fixture needs a saved manual follow-up: %+v, %v", continued, err)
	}

	before := messageSequenceSnapshot(t, f.s)
	limit := domain.ChatAssessment{Disposition: "discuss", Reason: "先交换意见。", DiscussionOnlyExcerpt: new(quote)}
	for range 2 {
		got, err := f.s.ApplyChatAssessment(ctx, peer, cPeer.ID, limit, "记录限制。", "discussion-alias")
		if err != nil || !reflect.DeepEqual(got, continued) {
			t.Fatalf("late excerpt did not replay the manual follow-up: %+v, %v", got, err)
		}
	}
	delete(before, "mutation_idempotency") // Only a transport alias may be added.
	messageSequencePreserved(t, f.s, before)
}

func TestChatAutomaticTextOnlyOriginalTopicAndPerRevisionBudget(t *testing.T) {
	f := newAutoTextFixture(t)
	input := automaticTextPolicy(2)
	input.AllowWorkspaceChanges, input.AllowPreparation = true, true
	input.PreparationScopeKind = domain.ChatPreparationTopicSummary
	input.Investigation = &domain.ChatInvestigationPolicy{Objective: "核对话题记录", AllowTopicMessages: true}
	p := f.policy(t, "", 0, input)
	f.policy(t, "other", 0, input)
	assess := func(topic, key string, proposal domain.ChatAssessment) (domain.ChatAssessmentReceipt, error) {
		m := f.post(t, topic, automaticTextRequest, f.co.ID)
		scope, c := f.claim(t, m, f.co.ID)
		defer discussionEnd(t, f.s, scope)
		return f.assess(scope, c, proposal, key)
	}
	first, err := assess("", "first", automaticTextProposal)
	if err != nil || first.TaskID == "" {
		t.Fatalf("founding admission: %+v %v", first, err)
	}
	before := automaticWorkSnapshot(t, f.s)
	for key, action := range map[string]string{"prepare": domain.ChatActionTextSubmission, "workspace": domain.ChatActionWorkspaceChange,
		"investigation": domain.ChatActionReadOnlyInvestigation, "repair": domain.ChatActionTextSubmission, "other": domain.ChatActionTextSubmission} {
		proposal, topic := automaticTextProposal, ""
		proposal.Action = action
		switch key {
		case "prepare", "investigation":
			proposal = domain.ChatAssessment{Disposition: "prepare_work", Action: action, Reason: "自行准备"}
		case "repair":
			proposal.Repair = &domain.ChatRepairAssessment{ReviewID: "review", Authorization: domain.ChatRepairChatRequest}
		case "other":
			topic = "other"
		}
		r, err := assess(topic, key, proposal)
		assertAutomaticTextQuestion(t, f.s, r, err, "automatic_text_routing_scope")
		messageSequencePreserved(t, f.s, before)
	}
	second, err := assess("", "second", automaticTextProposal)
	if err != nil || second.TaskID == "" || second.TaskID == first.TaskID || second.PolicyRevision != p.Revision {
		t.Fatalf("same topic could not reuse the exact pair within budget: %+v %v", second, err)
	}
	for _, table := range []string{"group_coordinator_binding_revisions", "group_coordinator_bindings", "group_dispatch_policy_revisions", "group_dispatch_policies"} {
		if len(messageSequenceTableRows(t, f.s, table)) != 1 {
			t.Fatalf("second request initialized %s again", table)
		}
	}
	before = automaticWorkSnapshot(t, f.s)
	r, err := assess("", "over-budget", automaticTextProposal)
	assertAutomaticTextQuestion(t, f.s, r, err, "budget_exhausted")
	messageSequencePreserved(t, f.s, before)
	input.AutomaticTextDispatch = false
	if _, err := f.s.SetChatPolicy(ctx, f.thread, input, p.Revision, "uncheck"); err != nil {
		t.Fatal(err)
	}
	r, err = assess("", "no-longer-opted-in", automaticTextProposal)
	assertAutomaticTextQuestion(t, f.s, r, err, "automatic_text_dispatch_disabled")
	messageSequencePreserved(t, f.s, before)
	input.AutomaticTextDispatch = true
	if _, err := f.s.SetChatPolicy(ctx, f.thread, input, p.Revision+1, "new-budget"); err != nil {
		t.Fatal(err)
	}
	r, err = assess("", "new-revision", automaticTextProposal)
	if err != nil || r.TaskID == "" || r.PolicyRevision != p.Revision+2 {
		t.Fatalf("real new opt-in revision did not supply its own budget: %+v %v", r, err)
	}
	dispatchAssertFK(t, f.s)
}

func (f autoTextFixture) startCoordinator(t *testing.T) CoordinationToolScope {
	t.Helper()
	f.policy(t, "", 0, automaticTextPolicy(2))
	intake, id := f.claim(t, f.post(t, "", automaticTextRequest, f.co.ID), f.co.ID)
	r, err := f.assess(intake, id, automaticTextProposal, "admit")
	if err != nil || r.DispatchRequestID == "" {
		t.Fatalf("fixture admission: %+v, %v", r, err)
	}
	discussionEnd(t, f.s, intake)
	d, err := f.s.MaterializeDispatchRequest(ctx, r.DispatchRequestID, 1, time.Now())
	if err != nil || d.Delivery == nil {
		t.Fatalf("coordinator delivery: %+v, %v", d, err)
	}
	return f.claimRole(t, d.Delivery.DeliveryID)
}

func (f autoTextFixture) claimRole(t *testing.T, deliveryID string) CoordinationToolScope {
	t.Helper()
	groupID := f.thread.GroupID
	queued, err := f.s.ResolveCoordinationDelivery(ctx, groupID, deliveryID)
	if err != nil || queued.Blocked != CoordinationReady || queued.AutomaticTextWorkerID != f.worker.ID {
		t.Fatalf("prepare automatic role: %+v, %v", queued, err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: groupID, DeliveryID: deliveryID, MaxActive: 6, ResourceKeys: []string{"fixture/" + deliveryID}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	role, err := f.s.ResolveCoordinationExecution(ctx, member, queued.Provenance)
	if err != nil || role != queued {
		t.Fatalf("postclaim role proof: %+v, %v; want %+v", role, err, queued)
	}
	return CoordinationToolScope{Member: member, Provenance: role.Provenance}
}

func (f autoTextFixture) assignmentInput() CoordinatorDecisionInput {
	return CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "安排获准的纯文本交付。"}
}

func (f autoTextFixture) enqueueWorker(t *testing.T, co CoordinationToolScope) (domain.DecisionReceipt, string) {
	t.Helper()
	r, err := f.s.DecideDispatchWithCapabilities(ctx, co, f.assignmentInput(), "assignment", f.facts.Workers)
	if err != nil || r.Assignment == nil {
		t.Fatalf("fixture assignment: %+v, %v", r, err)
	}
	d, err := f.s.MaterializeAssignment(ctx, r.Assignment.ID, 1, time.Now())
	if err != nil || d.Delivery == nil {
		t.Fatalf("worker delivery: %+v, %v", d, err)
	}
	return r, d.Delivery.DeliveryID
}

func (f autoTextFixture) grantWorkspace(t *testing.T, member domain.Member) {
	t.Helper()
	root := t.TempDir()
	_, err := f.s.AuthorizeWorkspaceGrant(ctx, AuthorizeWorkspaceGrantInput{GroupID: member.GroupID, MemberID: member.ID,
		Root: root, Scope: "read_only", IdempotencyKey: "workspace-" + member.ID}, func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
		return WorkspaceGrantDirectory{ResolvedRoot: root, ResourceKey: "workspace:fixture-" + member.ID}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestChatAutomaticTextSavedSourceAliasesReplayExactReceipts(t *testing.T) {
	f := newAutoTextFixture(t)
	co := f.startCoordinator(t)
	assignment, _ := f.enqueueWorker(t, co)
	discussionEnd(t, f.s, co.Member)
	reader := f.member(t, f.thread.GroupID, "user", "alias-reader")
	id := assignment.Assignment.TaskID
	m := f.post(t, "", "继续任务 "+id+"，请注明统计口径。", f.co.ID, f.worker.ID, reader.ID)
	first, cFirst := f.claim(t, m, f.co.ID)
	second, cSecond := f.claim(t, m, f.worker.ID)
	peer, cPeer := f.claim(t, m, reader.ID)
	prepare := domain.ChatAssessment{Disposition: "prepare_work", Action: domain.ChatActionTextSubmission, Reason: "建议准备。"}
	auto, err := f.assess(first, cFirst, prepare, "automatic-denial")
	assertAutomaticTextQuestion(t, f.s, auto, err, "automatic_text_routing_scope")
	if got, err := f.assess(peer, cPeer, automaticTextProposal, "automatic-alias"); err != nil || !reflect.DeepEqual(got, auto) {
		t.Fatalf("fixture automatic alias: %+v, %v", got, err)
	}

	// Synthesize coexisting records and saved aliases on this schema. Hide only
	// the marker during the second write to model a writer without this fence.
	coordinationDecideExec(t, f.s, "DROP TRIGGER chat_assessments_no_update")
	coordinationDecideExec(t, f.s, `UPDATE chat_assessments SET receipt_json=json_remove(receipt_json,'$.automatic_text_routing_checked') WHERE id=?`, auto.ID)
	input := domain.ChatAssessment{Disposition: domain.ChatDispositionContinueWork, Reason: "补充原任务", Followup: &domain.ChatFollowupAssessment{TaskID: id}}
	followup, err := f.s.ApplyChatAssessmentDormant(ctx, second, cSecond, input, "记录补充。", "followup")
	if err != nil || followup.Outcome != "continued" || followup.TaskID != id || followup.WorkInputID == "" || followup.SourceID != m.SourceID {
		t.Fatalf("fixture follow-up: %+v, %v", followup, err)
	}
	if got, err := f.assess(peer, cPeer, automaticTextProposal, "followup-alias"); err != nil || !reflect.DeepEqual(got, followup) {
		t.Fatalf("new work proposal did not alias the existing follow-up: %+v, %v", got, err)
	}
	coordinationDecideExec(t, f.s, `UPDATE chat_assessments SET receipt_json=json_set(receipt_json,'$.automatic_text_routing_checked',json('true')) WHERE id=?`, auto.ID)
	if got, err := f.assess(peer, cPeer, automaticTextProposal, "new-alias"); err != nil || !reflect.DeepEqual(got, auto) {
		t.Fatalf("new alias did not select the canonical automatic outcome: %+v, %v", got, err)
	}
	before := messageSequenceSnapshot(t, f.s)
	aliases := map[string]domain.ChatAssessmentReceipt{"automatic-alias": auto, "followup-alias": followup}
	for key, want := range aliases {
		if got, err := f.assess(peer, cPeer, automaticTextProposal, key); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("saved %s did not replay its exact receipt %s: %+v, %v", key, want.ID, got, err)
		}
	}
	messageSequencePreserved(t, f.s, before)

	// Probe the alias resolver with the saved IDs after the transport's key and
	// payload fence. Change one captured scope coordinate at a time.
	candidate, err := f.s.CaptureChatCandidate(ctx, peer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.beginMemberToolTx(ctx, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for name, change := range map[string]func(*domain.ChatCandidate){
		"group":      func(c *domain.ChatCandidate) { c.GroupID = "other-group" },
		"topic":      func(c *domain.ChatCandidate) { c.TopicID = "other-topic" },
		"generation": func(c *domain.ChatCandidate) { c.Generation++ },
	} {
		wrong := candidate
		change(&wrong)
		for key, want := range aliases {
			if got, err := chatAssessmentReplayInTx(ctx, tx, peer, wrong, want.ID); err == nil || got.ID != "" {
				t.Errorf("%s escaped the %s fence: %+v, %v", key, name, got, err)
			}
		}
	}
}

func TestChatAutomaticTextRoleWritesRecheckFrozenActors(t *testing.T) {
	for _, drift := range []string{"coordinator-model", "coordinator-model-without-auto-ledger", "worker-provider", "worker-workspace"} {
		t.Run("decision/"+drift, func(t *testing.T) {
			f := newAutoTextFixture(t)
			co := f.startCoordinator(t)
			switch drift {
			case "coordinator-model", "coordinator-model-without-auto-ledger":
				if drift == "coordinator-model-without-auto-ledger" {
					version := slices.Index(migrations, chatAutomaticTextSchema) + 1
					coordinationDecideExec(t, f.s, "DELETE FROM schema_migrations WHERE version=?", version)
				}
				coordinationDecideExec(t, f.s, "UPDATE members SET model=model||'-changed' WHERE id=?", f.co.ID)
			case "worker-provider":
				f.facts.Workers[0].ProviderID = "replacement-provider" // Even a fresh sample isn't the frozen provider.
				coordinationDecideExec(t, f.s, "UPDATE members SET provider_id=? WHERE id=?", f.facts.Workers[0].ProviderID, f.worker.ID)
			case "worker-workspace":
				f.grantWorkspace(t, f.worker)
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err := f.s.DecideDispatchWithCapabilities(ctx, co, f.assignmentInput(), "new-decision", f.facts.Workers)
			requestDispatchZero(t, got, err, ErrConflict)
			got, err = f.s.DecideDispatch(ctx, co, f.assignmentInput(), "new-decision")
			requestDispatchZero(t, got, err, ErrConflict)
			messageSequencePreserved(t, f.s, before)
		})
	}
	t.Run("bind-after-postclaim-read", func(t *testing.T) {
		f := newAutoTextFixture(t)
		co := f.startCoordinator(t)
		decision, deliveryID := f.enqueueWorker(t, co)
		worker := f.claimRole(t, deliveryID)
		f.grantWorkspace(t, f.worker)
		before := messageSequenceSnapshot(t, f.s)
		if got, err := f.s.BindAssignmentAttempt(ctx, worker); !errors.Is(err, ErrConflict) || got != (domain.AssignmentAttemptBinding{}) {
			t.Fatalf("a new bind wrote through role drift: %+v, %v", got, err)
		}
		if got, err := f.s.DecideDispatchWithCapabilities(ctx, co, f.assignmentInput(), "assignment", nil); err != nil || !reflect.DeepEqual(got, decision) {
			t.Fatalf("historical decision did not replay after role drift: %+v, %v", got, err)
		}
		messageSequencePreserved(t, f.s, before)
		cancelled, err := f.s.RequestTurnCancel(ctx, worker.Member.Claim.GroupID, worker.Member.Claim.TurnID)
		if err != nil || cancelled.State != domain.TurnStateCancelRequested {
			t.Fatalf("cancellation request is not terminal: %+v, %v", cancelled, err)
		}
		if got, err := f.s.BindAssignmentAttempt(ctx, worker); !errors.Is(err, ErrMemberToolDenied) || got != (domain.AssignmentAttemptBinding{}) {
			t.Fatalf("cancelled scope bound an attempt: %+v, %v", got, err)
		}
		delete(before, "turns") // Only the cancellation record may change; claims remain held.
		messageSequencePreserved(t, f.s, before)
	})
	t.Run("bound-replay", func(t *testing.T) {
		f := newAutoTextFixture(t)
		co := f.startCoordinator(t)
		unrelated := f.member(t, f.thread.GroupID, "user", "unrelated")
		f.grantWorkspace(t, unrelated) // Not part of the frozen pair.
		_, deliveryID := f.enqueueWorker(t, co)
		worker := f.claimRole(t, deliveryID)
		binding, err := f.s.BindAssignmentAttempt(ctx, worker)
		if err != nil || binding.AttemptID == "" {
			t.Fatalf("unrelated member affected new binding: %+v, %v", binding, err)
		}
		f.grantWorkspace(t, f.worker)
		before := messageSequenceSnapshot(t, f.s)
		if got, err := f.s.BindAssignmentAttempt(ctx, worker); err != nil || got != binding {
			t.Fatalf("historical binding did not replay after role drift: %+v, %v", got, err)
		}
		messageSequencePreserved(t, f.s, before)
	})
}

func TestChatAutomaticTextDeclineRechecksCoordinatorOnly(t *testing.T) {
	input := CoordinatorDecisionInput{Kind: "decline", Reason: "没有可用的执行成员。"}
	t.Run("drifted-coordinator", func(t *testing.T) {
		f := newAutoTextFixture(t)
		co := f.startCoordinator(t)
		f.grantWorkspace(t, f.co)
		before := messageSequenceSnapshot(t, f.s)
		got, err := f.s.DecideDispatchWithCapabilities(ctx, co, input, "decline", nil)
		requestDispatchZero(t, got, err, ErrConflict)
		messageSequencePreserved(t, f.s, before)
	})
	t.Run("unavailable-worker-and-replay", func(t *testing.T) {
		f := newAutoTextFixture(t)
		co := f.startCoordinator(t)
		coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.worker.ID)
		before := messageSequenceSnapshot(t, f.s)
		saved, err := f.s.DecideDispatchWithCapabilities(ctx, co, input, "decline", nil)
		if err != nil || saved.Decision.Kind != domain.CoordinatorDecisionKindDecline || saved.Assignment != nil {
			t.Fatalf("intact coordinator could not decline without a worker: %+v, %v", saved, err)
		}
		r, err := f.s.GetDispatchRequest(ctx, co.Provenance.GroupID, co.Provenance.RequestID)
		if err != nil || r.State.Status != domain.DispatchStatusDeclined {
			t.Fatalf("decline did not update dispatch state: %+v, %v", r, err)
		}
		for _, table := range []string{"coordination_decisions", "coordination_dispatch_state", "mutation_idempotency"} {
			delete(before, table)
		}
		messageSequencePreserved(t, f.s, before)
		f.grantWorkspace(t, f.co)
		before = messageSequenceSnapshot(t, f.s)
		if replay, err := f.s.DecideDispatchWithCapabilities(ctx, co, input, "decline", nil); err != nil || !reflect.DeepEqual(replay, saved) {
			t.Fatalf("saved decline did not replay after coordinator drift: %+v, %v", replay, err)
		}
		messageSequencePreserved(t, f.s, before)
	})
}

func TestChatAutomaticTextLateRollbackDoesNotSpendQuota(t *testing.T) {
	f := newAutoTextFixture(t)
	f.policy(t, "", 0, automaticTextPolicy(1))
	m := f.post(t, "", automaticTextRequest, f.co.ID)
	scope, c := f.claim(t, m, f.co.ID)
	before := messageSequenceSnapshot(t, f.s)
	coordinationDecideExec(t, f.s, `CREATE TRIGGER fail_new_auto_text BEFORE INSERT ON chat_assessments BEGIN SELECT RAISE(ABORT,'fixture late failure'); END`)
	r, err := f.assess(scope, c, automaticTextProposal, "admit")
	if err == nil || !reflect.ValueOf(r).IsZero() {
		t.Fatalf("late failure was reported as committed: %+v %v", r, err)
	}
	messageSequencePreserved(t, f.s, before)
	coordinationDecideExec(t, f.s, "DROP TRIGGER fail_new_auto_text")
	r, err = f.assess(scope, c, automaticTextProposal, "admit")
	if err != nil || r.TaskID == "" {
		t.Fatalf("rollback consumed quota, routing, or key: %+v %v", r, err)
	}
}

func TestChatAutomaticTextPreservesManualAndEmptyRouting(t *testing.T) {
	for _, mode := range []string{"manual-pair", "explicit-empty", "coordinator-only", "dispatch-only"} {
		t.Run(mode, func(t *testing.T) {
			f := newAutoTextFixture(t)
			f.policy(t, "", 0, automaticTextPolicy(1))
			groupID := f.thread.GroupID
			if mode != "dispatch-only" {
				if _, err := f.s.SetCoordinator(ctx, groupID, f.worker.ID, 0, "manual-coordinator"); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "coordinator-only" {
				var ids []string
				if mode != "explicit-empty" {
					ids = []string{f.co.ID}
				}
				if _, err := f.s.SetDispatchPolicy(ctx, groupID, ids, 0, "manual-workers"); err != nil {
					t.Fatal(err)
				}
			}
			scope, c := f.claim(t, f.post(t, "", automaticTextRequest, f.co.ID), f.co.ID)
			before := automaticWorkSnapshot(t, f.s)
			if mode == "manual-pair" {
				r, err := f.s.ApplyChatAssessment(ctx, scope, c, automaticTextProposal, "沿用旧协调。", "manual")
				if err != nil || r.TaskID == "" {
					t.Fatalf("manual route incorrectly required automatic facts: %+v %v", r, err)
				}
				v, err := f.s.GetDispatchRequest(ctx, groupID, r.DispatchRequestID)
				if err != nil || v.Request.CoordinatorMemberID != f.worker.ID || v.Request.BindingRevision != 1 || v.Request.PolicyRevision != 1 {
					t.Fatalf("intake replaced manual coordinator: %+v %v", v, err)
				}
				if len(messageSequenceTableRows(t, f.s, "chat_automatic_text_routes")) != 0 {
					t.Fatal("manual route was tagged as generated")
				}
			} else {
				r, err := f.assess(scope, c, automaticTextProposal, "manual-blocks-auto")
				assertAutomaticTextQuestion(t, f.s, r, err, "routing_unavailable")
				messageSequencePreserved(t, f.s, before)
			}
		})
	}
}

func TestChatAutomaticTextBindingsCannotBeBorrowed(t *testing.T) {
	f := newAutoTextFixture(t)
	p := f.policy(t, "", 0, automaticTextPolicy(1))
	scope, c := f.claim(t, f.post(t, "", automaticTextRequest, f.co.ID), f.co.ID)
	r, err := f.assess(scope, c, automaticTextProposal, "admit")
	if err != nil || r.TaskID == "" {
		t.Fatalf("fixture admission: %+v %v", r, err)
	}
	discussionEnd(t, f.s, scope)
	delivery, err := f.s.MaterializeDispatchRequest(ctx, r.DispatchRequestID, 1, time.Now())
	if err != nil || delivery.Delivery == nil {
		t.Fatalf("did not materialize the scoped request: %+v %v", delivery, err)
	}
	// An edit to ChatPolicy affects future admission, not the approved request.
	disabled := p.ChatPolicyRequest
	disabled.Enabled = false
	if _, err := f.s.SetChatPolicy(ctx, f.thread, disabled, p.Revision, "disable"); err != nil {
		t.Fatal(err)
	}
	groupID, deliveryID := f.thread.GroupID, delivery.Delivery.DeliveryID
	state, err := f.s.ResolveCoordinationDelivery(ctx, groupID, deliveryID)
	if err != nil || state.Blocked != CoordinationReady || state.AutomaticTextWorkerID != f.worker.ID {
		t.Fatalf("policy edit changed an existing request: %+v %v", state, err)
	}
	coordinationDecideExec(t, f.s, "UPDATE members SET workspace=? WHERE id=?", t.TempDir(), f.worker.ID)
	state, err = f.s.ResolveCoordinationDelivery(ctx, groupID, deliveryID)
	if err != nil || state.Blocked != CoordinationAutomaticTextChanged {
		t.Fatalf("workspace drift was not fenced: %+v %v", state, err)
	}
	coordinationDecideExec(t, f.s, "UPDATE members SET workspace='' WHERE id=?", f.worker.ID)

	// Even an explicitly created manual task must not borrow a generated pair.
	m := f.post(t, "other", "这是独立的手动任务。")
	a, err := f.s.RequestWork(ctx, groupID, domain.RequestWorkRequest{TopicID: m.TopicID, MessageID: m.ID,
		Mode: domain.RoutingDecisionCreateNew, Reason: "人工命令", NewTask: &domain.NewTaskRequest{Title: "手动工作", Acceptance: "仅文本"}}, "manual-task")
	if err != nil {
		t.Fatal(err)
	}
	request := func() (domain.DispatchRequest, error) {
		return f.s.RequestDispatch(ctx, groupID, a.Task.ID, a.Task.RevisionAtAdmission, 1, "手动分派", "manual-dispatch")
	}
	assertBlocked := func() {
		t.Helper()
		before := messageSequenceSnapshot(t, f.s)
		if got, err := request(); err == nil || got.ID != "" {
			t.Fatalf("manual dispatch used automatically generated authority: %+v %v", got, err)
		}
		messageSequencePreserved(t, f.s, before)
	}
	assertBlocked()
	if _, err := f.s.SetCoordinator(ctx, groupID, f.co.ID, 1, "manual-coordinator"); err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	if _, err := f.s.SetDispatchPolicy(ctx, groupID, []string{f.worker.ID}, 1, "manual-workers"); err != nil {
		t.Fatal(err)
	}
	if got, err := request(); err != nil || got.ID == "" || got.TopicID != m.TopicID || got.BindingRevision != 2 || got.PolicyRevision != 2 {
		t.Fatalf("explicitly replaced pair did not restore manual dispatch: %+v %v", got, err)
	}
}

func TestChatAutomaticTextMigrationDoesNotPromoteHistory(t *testing.T) {
	withChatAutomaticTextSchema(t)
	prefix := slices.Index(migrations, chatAutomaticTextSchema)
	if prefix < 30 {
		t.Fatal("automatic text migration must be appended after the existing schema")
	}
	f := taskWorkChatAtPrefix(t, prefix)
	first, err := f.s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "旧版手动路由。", "old-assess")
	if err != nil || first.TaskID == "" {
		t.Fatalf("fixture must have existing work: %+v %v", first, err)
	}
	before := messageSequenceSnapshot(t, f.s)
	if _, err := f.s.SetChatPolicy(ctx, f.policy.ThreadRef, automaticTextPolicy(1), f.policy.Revision, "unsupported"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old schema silently accepted an unsupported configuration: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
	reopened := queueHandle(t, f.path, false)
	messageSequencePreserved(t, reopened, before)
	for _, table := range []string{"chat_automatic_text_policy_revisions", "chat_automatic_text_routes", "chat_automatic_text_group_imports"} {
		if got := messageSequenceTableRows(t, reopened, table); len(got) != 0 {
			t.Fatalf("migration wrote authorization: %v", got)
		}
	}
	p, err := reopened.SetChatPolicy(ctx, f.policy.ThreadRef, f.policy.ChatPolicyRequest, 0, "policy")
	if err != nil || !reflect.DeepEqual(p, f.policy) || p.AutomaticTextDispatch {
		t.Fatalf("policy replay upgraded historical permissions: %+v %v", p, err)
	}
	replay, err := reopened.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "旧版手动路由。", "old-assess")
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatalf("old assessment changed after upgrade: %+v %v", replay, err)
	}
	messageSequencePreserved(t, reopened, before)
	dispatchAssertFK(t, reopened)
}

func TestChatAutomaticTextUnregisteredFailsClosed(t *testing.T) {
	original := migrations
	if i := slices.Index(original, chatAutomaticTextSchema); i >= 0 {
		migrations = original[:i]
		t.Cleanup(func() { migrations = original })
	}
	s, _ := openTestStore(t)
	g, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "未登记旧群", ActorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: g.ID}, automaticTextPolicy(1), 0, "unregistered-policy"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unregistered schema allowed automatic text policy: %v", err)
	}
	if _, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "导入失败", ActorID: "user", ImportAuthorizationFromGroupID: g.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unregistered schema allowed group auth import: %v", err)
	}
}

func TestChatAutomaticTextGroupImportOneLineStartAndA1Worker(t *testing.T) {
	withChatAutomaticTextSchema(t)
	s, _ := openTestStore(t)
	if err := s.EnableGroupBackgroundDispatch(ctx); err != nil {
		t.Fatal(err)
	}
	srcGroup, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "源授权群", ActorID: "user", IdempotencyKey: "src-group"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "未开策略导入", ActorID: "user", ImportAuthorizationFromGroupID: srcGroup.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("import succeeded without enabled source automatic-text policy: %v", err)
	}
	if _, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "缺少源群", ActorID: "user", ImportAuthorizationFromGroupID: "missing-group"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing source group error = %v; want ErrNotFound", err)
	}
	if _, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "只有话题无源群", ActorID: "user", ImportAuthorizationFromGroupID: "", ImportAuthorizationFromTopicID: "topic-a"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("topic without group error = %v; want ErrInvalid", err)
	}
	if _, err := s.CreateGroup(ctx, CreateGroupInput{Name: "非用户导入", ActorID: "member", ImportAuthorizationFromGroupID: srcGroup.ID}); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("non-user import error = %v; want ErrMemberToolDenied", err)
	}
	disabledPolicy := automaticTextPolicy(2)
	disabledPolicy.Enabled = false
	srcPolicy, err := s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: srcGroup.ID}, disabledPolicy, 0, "src-disabled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "源未启用导入", ActorID: "user", ImportAuthorizationFromGroupID: srcGroup.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("import succeeded from disabled source policy: %v", err)
	}
	srcPolicy, err = s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: srcGroup.ID}, automaticTextPolicy(2), srcPolicy.Revision, "src-enabled")
	if err != nil {
		t.Fatal(err)
	}

	importInput := CreateGroupInput{Name: "一句话新群", Description: "导入已有授权", ActorID: "user", IdempotencyKey: "import-new-group", ImportAuthorizationFromGroupID: srcGroup.ID}
	importedGroup, err := s.CreateLocalUserGroup(ctx, importInput)
	if err != nil {
		t.Fatal(err)
	}
	replayedGroup, err := s.CreateLocalUserGroup(ctx, importInput)
	if err != nil || replayedGroup.ID != importedGroup.ID {
		t.Fatalf("idempotent group import replay failed: %+v %v", replayedGroup, err)
	}
	mismatchInput := importInput
	mismatchInput.ImportAuthorizationFromTopicID = "other-topic"
	if _, err := s.CreateLocalUserGroup(ctx, mismatchInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched import replay error = %v; want ErrConflict", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE chat_automatic_text_group_imports SET max_admissions=99 WHERE group_id=?`, importedGroup.ID); err == nil {
		t.Fatal("chat_automatic_text_group_imports permitted UPDATE")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chat_automatic_text_group_imports WHERE group_id=?`, importedGroup.ID); err == nil {
		t.Fatal("chat_automatic_text_group_imports permitted DELETE")
	}

	// Disabling source group policy after import must not invalidate existing imports,
	// while future group creations importing from it must be rejected.
	if _, err := s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: srcGroup.ID}, disabledPolicy, srcPolicy.Revision, "src-later-disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "停用后新建导入", ActorID: "user", ImportAuthorizationFromGroupID: srcGroup.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("import succeeded after source policy was disabled: %v", err)
	}

	// Verify brand-new topic in imported group needs NO PutChatPolicy and NO coordinator setup,
	// and accepts an A1 background-joined peer (creator_id='system', owner_id='user') as worker.
	lead, err := s.CreateMember(ctx, CreateMemberInput{
		GroupID: importedGroup.ID, Name: "接收成员", Role: "user",
		ActorID: "user", CreatorID: "user", OwnerID: "user",
		ProviderID: "fixture-provider", Model: t.Name(), IdempotencyKey: "lead-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Create an A1 background-joined peer in importedGroup without any manual coordinator setup.
	profile := BackgroundDispatchProfile{ProviderID: "fixture-provider", Model: t.Name() + "-a1-peer"}
	if err := s.RunGroupBackgroundDispatch(ctx, BackgroundDispatchOptions{
		MaxActiveMembers: 5,
		ReadyProfiles:    []BackgroundDispatchProfile{profile},
		Create:           &BackgroundDispatchTemplate{ID: "peer-auto", Name: "A1同伴", Role: "纯文本执行", Profile: profile},
	}); err != nil {
		t.Fatal(err)
	}
	a1Seed, err := s.PostLocalUserMessage(ctx, MessageRequest{GroupID: importedGroup.ID, TopicID: "bg-seed", ActorID: "user", Content: "请补一位后台讨论同伴。"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunGroupBackgroundDispatch(ctx, BackgroundDispatchOptions{
		MaxActiveMembers: 5,
		ReadyProfiles:    []BackgroundDispatchProfile{profile},
		Create:           &BackgroundDispatchTemplate{ID: "peer-auto", Name: "A1同伴", Role: "纯文本执行", Profile: profile},
	}); err != nil {
		t.Fatal(err)
	}
	var a1PeerID string
	if err := s.db.QueryRowContext(ctx, `SELECT member_id FROM group_background_joins WHERE group_id=? AND origin_message_id=?`, importedGroup.ID, a1Seed.ID).Scan(&a1PeerID); err != nil {
		t.Fatalf("expected A1 joined peer: %v", err)
	}
	a1Peer, err := s.GetMember(ctx, importedGroup.ID, a1PeerID)
	if err != nil || a1Peer.CreatorID != "system" || a1Peer.OwnerID != "user" {
		t.Fatalf("unexpected A1 joined peer: %+v %v", a1Peer, err)
	}

	const topic = "sprint-1"
	if p, err := s.GetChatPolicy(ctx, domain.ThreadRef{GroupID: importedGroup.ID, TopicID: topic}); err != nil || p.Revision != 0 {
		t.Fatalf("topic policy materialized prematurely before user message: %+v %v", p, err)
	}
	workMsg := chatLocalMessage(t, s, MessageRequest{GroupID: importedGroup.ID, TopicID: topic, ActorID: "user", Content: automaticTextRequest, RecipientIDs: []string{lead.ID}}, []string{lead.ID})
	if len(workMsg.Deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %+v", workMsg.Deliveries)
	}
	scope, candidateID := discussionArrive(t, s, importedGroup.ID, workMsg.Deliveries[0].ID)
	facts := ChatAutomaticTextFacts{
		Coordinator: automaticTextFact(lead),
		Workers:     []DispatchWorkerCapabilityFact{automaticTextFact(a1Peer)},
	}
	receipt, err := s.ApplyChatAssessmentWithAutomaticText(ctx, scope, candidateID, automaticTextProposal, "一句直达。", "one-line-admit", facts)
	if err != nil || receipt.Outcome != "admitted" || receipt.PolicyRevision != 1 || receipt.TaskID == "" || receipt.DispatchRequestID == "" {
		t.Fatalf("one-line start on imported group failed: receipt=%+v err=%v", receipt, err)
	}
	matPolicy, err := s.GetChatPolicy(ctx, domain.ThreadRef{GroupID: importedGroup.ID, TopicID: topic})
	if err != nil || matPolicy.Revision != 1 || !matPolicy.Enabled || !matPolicy.AutomaticTextDispatch || matPolicy.MaxAdmissions != 2 || matPolicy.ConfigurationSourceID == "" {
		t.Fatalf("materialized topic policy mismatch: %+v %v", matPolicy, err)
	}
	task, err := s.GetTask(ctx, importedGroup.ID, receipt.TaskID)
	if err != nil || task.AssigneeID != a1Peer.ID {
		t.Fatalf("task assignee = %+v (want %s), err = %v", task, a1Peer.ID, err)
	}
	// Coordinator decides & dispatches to the A1-joined peer cleanly without manual pre-config.
	discussionEnd(t, s, scope)
	d, err := s.MaterializeDispatchRequest(ctx, receipt.DispatchRequestID, 1, time.Now())
	if err != nil || d.Delivery == nil {
		t.Fatalf("coordinator delivery: %+v, %v", d, err)
	}
	fImported := autoTextFixture{s: s, thread: domain.ThreadRef{GroupID: importedGroup.ID}, co: lead, worker: a1Peer, facts: facts}
	co := fImported.claimRole(t, d.Delivery.DeliveryID)
	decision, workerDelivery := fImported.enqueueWorker(t, co)
	if decision.Assignment == nil || decision.Assignment.WorkerMemberID != a1Peer.ID || workerDelivery == "" {
		t.Fatalf("coordinator assign to A1 peer failed: %+v workerDelivery=%q", decision, workerDelivery)
	}
	discussionEnd(t, s, co.Member)

	// Explicit user update to rev 2 (disabled) takes precedence and blocks next admission.
	dis2 := automaticTextPolicy(2)
	dis2.Enabled = false
	if p2, err := s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: importedGroup.ID, TopicID: topic}, dis2, matPolicy.Revision, "explicit-disable"); err != nil || p2.Revision != 2 || p2.Enabled {
		t.Fatalf("explicit rev 2 override failed: %+v %v", p2, err)
	}
	nextMsg := chatLocalMessage(t, s, MessageRequest{GroupID: importedGroup.ID, TopicID: topic, ActorID: "user", Content: automaticTextRequest, RecipientIDs: []string{lead.ID}}, []string{lead.ID})
	nextScope, nextCand := discussionArrive(t, s, importedGroup.ID, nextMsg.Deliveries[0].ID)
	r2, err := s.ApplyChatAssessmentWithAutomaticText(ctx, nextScope, nextCand, automaticTextProposal, "已禁用。", "after-disable", facts)
	assertAutomaticTextQuestion(t, s, r2, err, "policy_disabled")
	dispatchAssertFK(t, s)
}
