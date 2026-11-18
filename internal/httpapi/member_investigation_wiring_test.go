//go:build unix

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func TestInvestigationDisabledKeepsLegacyIntake(t *testing.T) {
	ctx := t.Context()
	f := newReadinessHTTPFixture(t)
	entry, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	readinessHTTPOK(t, err)
	f.options.PiMemberToolsExtension = entry
	db, tools := f.a.db, f.runtime.Tools()
	readinessHTTPOK(t, tools.EnableChatIntake(coordination.New(db)))
	thread := domain.ThreadRef{GroupID: f.group.ID, TopicID: "intake"}
	_, err = db.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
		Investigation: &domain.ChatInvestigationPolicy{Objective: "核对已有材料", AllowTopicMessages: true}}, 0, "investigation-policy")
	readinessHTTPOK(t, err)
	source, err := platform.NewLocalUser(db).CreateMessage(ctx, thread.GroupID, platform.MessageInput{TopicID: thread.TopicID, Content: "原文本处理", RecipientIDs: []string{f.lead.ID}}, "intake-source")
	readinessHTTPOK(t, err)
	f.selectMessage(t, source.ID)
	legacy, err := f.preparer(t).Prepare(ctx, f.pending)
	readinessHTTPOK(t, err)
	db.EnableInvestigationExecution() // The Store gate alone must not enable C intake.
	preview, err := f.preparer(t).Prepare(ctx, f.pending)
	readinessHTTPOK(t, err)
	var packet struct{ Permissions []string }
	readinessHTTPOK(t, json.Unmarshal([]byte(legacy.Request.Prompt), &packet))
	// The local user's stored policy message can still appear as history, not as a tool instruction.
	if preview.Request.Prompt != legacy.Request.Prompt || strings.Contains(strings.Join(packet.Permissions, "\n"), domain.ChatActionReadOnlyInvestigation) ||
		!slices.Contains(packet.Permissions, "prepare_work仅在policy.preparation_supported=true且preparation_scope_kind=topic_summary时使用；只填action=text_submission和reason，不填title/description/acceptance。服务端固定本话题摘要目标/验收，不能另选工作或将旧任意文本规则当宽授权。否则只讨论/澄清。") {
		t.Fatal("the Store gate alone changed the legacy intake prompt")
	}
	turn, err := db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: thread.GroupID, DeliveryID: f.pending.Delivery.ID, MaxActive: 1})
	readinessHTTPOK(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	binding := platform.MemberToolRunBinding{GroupID: turn.GroupID, MemberID: turn.MemberID, ConversationID: turn.ConversationID,
		DeliveryID: turn.DeliveryID, ProviderID: f.options.Providers[0].ID, Model: preview.Request.Model}
	calls := 0
	stub := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1}
	exec, err := f.runtime.WrapChatIntake(binding, func(path, _ string, names []string) (provider.Executor, error) {
		wire, err := os.ReadFile(path)
		readinessHTTPOK(t, err)
		var config map[string]string
		readinessHTTPOK(t, json.Unmarshal(wire, &config))
		if len(config) != 2 || config["endpoint"] == "" || config["token"] == "" || !slices.Equal(names, []string{"list_members", "read_channel", "send_message", "search_history", "read_history"}) {
			t.Fatal("issued Pi config and tools must keep the legacy intake shape")
		}
		return memberMCPMetadataExecutor(func() provider.ExecutionReceipt {
			calls++
			member, err := db.BindMemberTool(ctx, claim)
			readinessHTTPOK(t, err)
			candidate, err := db.CaptureChatCandidate(ctx, member)
			readinessHTTPOK(t, err)
			if candidate.Policy.Investigation == nil {
				t.Fatal("fixture intake must carry a real configured policy")
			}
			routes := slices.Concat(MemberToolRoutes(tools), MemberInvestigationToolRoutes(tools), MemberMCPRoutes(tools))
			h := &memberToolHTTPFixture{db: db, raw: f.raw, claim: claim, token: config["token"], path: f.a.path, handler: mustServer(t, DefaultAddress, routes...).Handler}
			const list = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"intake_assessment_action":"read_only_investigation"}}}`
			w := memberMCPHTTP(t, h, list)
			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "investigation") ||
				!strings.Contains(w.Body.String(), `"action":{"enum":["text_submission","workspace_change"]}`) || strings.Contains(w.Body.String(), `"continue_work"`) {
				t.Fatal("MCP exposed an unissued action or changed legacy/continue discovery")
			}
			w = h.request(t, "read_channel", `{}`)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"chat_candidate"`) || strings.Contains(w.Body.String(), `"investigation":`) {
				t.Fatal("intake metadata did not retain its disabled model-facing shape")
			}
			before := h.counts(t)
			const proposal = `{"content":"建议调查。","intent_assessment":{"disposition":"prepare_work","action":"read_only_investigation","reason":"尝试使用未签发的动作"}}`
			w = h.request(t, "send_message", proposal, "unsupported-native")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusForbidden, "forbidden")
			if text := memberMCPText(t, memberMCPHTTP(t, h, memberMCPCall(`"unsupported-rpc"`, "send_message", proposal)), true); !strings.Contains(text, `"code":"forbidden"`) || h.counts(t) != before {
				t.Fatal("an unadvertised investigation proposal consumed the intake or persisted a notice")
			}
			const discuss = `{"content":"先讨论。","intent_assessment":{"disposition":"discuss","reason":"本轮只讨论"}}`
			call := memberMCPCall(`"legacy-rpc"`, "send_message", discuss)
			first := memberMCPText(t, memberMCPHTTP(t, h, call), false)
			if !strings.Contains(first, `"outcome":"discuss"`) || first != memberMCPText(t, memberMCPHTTP(t, h, call), false) {
				t.Fatal("legacy assessment or same-key replay changed")
			}
			return stub
		}), nil
	})
	readinessHTTPOK(t, err)
	preview.Request.RunID, preview.Request.RuntimeDir = turn.ID, f.runRoot
	if got := exec.Execute(ctx, preview.Request, nil); calls != 1 || got != stub {
		t.Fatal("fixture did not exercise the scoped issued intake config")
	}
}

func TestInvestigationRejectsSharedRootAndRetainedAdmission(t *testing.T) {
	ctx := t.Context()
	f := newReadinessHTTPFixture(t)
	entry, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	readinessHTTPOK(t, err)
	f.options.PiMemberToolsExtension = entry
	db, tools := f.a.db, f.runtime.Tools()
	readinessHTTPOK(t, tools.EnableChatIntake(coordination.New(db)))
	f.a.handler = mustServer(t, DefaultAddress, slices.Concat(MemberToolRoutes(tools), MemberMCPRoutes(tools))...).Handler
	thread := domain.ThreadRef{GroupID: f.group.ID, TopicID: "shared-root"}
	_, err = db.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
		Investigation: &domain.ChatInvestigationPolicy{Objective: "核对已有材料", AllowTopicMessages: true}}, 0, "investigation-policy")
	readinessHTTPOK(t, err)
	db.EnableInvestigationExecution()
	readinessHTTPOK(t, os.Mkdir(filepath.Join(f.options.PrivateWorkspaceRoot, platform.InvestigationCWDName), 0700))
	f.options.Investigation = &platform.InvestigationPreparationOptions{PiPath: f.options.Providers[0].Path}
	p := f.preparer(t)
	const excerpt = "本次只讨论"
	root, err := platform.NewLocalUser(db).CreateMessage(ctx, thread.GroupID, platform.MessageInput{
		TopicID: thread.TopicID, Content: excerpt + "，先列出待核对材料。", RecipientIDs: []string{f.lead.ID, f.worker.ID},
	}, "shared-root")
	readinessHTTPOK(t, err)
	if root.SourceKind != "local_user_chat" || len(root.Deliveries) != 2 {
		t.Fatal("fixture needs a real shared user root")
	}
	first, later := root.Deliveries[0], root.Deliveries[1]
	f.pending = store.PendingDelivery{GroupID: root.GroupID, SenderID: root.SenderID, Delivery: first}
	var retained domain.ChatAssessmentReceipt
	runInvestigationHTTPPi(t, f, p, []string{"list_members", "read_channel", "send_message", "search_history", "read_history"}, "", true, func(h *memberToolHTTPFixture) {
		list := memberMCPHTTP(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if list.Code != 200 || strings.Contains(list.Body.String(), domain.ChatActionReadOnlyInvestigation) {
			t.Fatal("shared root was offered investigation")
		}
		const proposal = `{"content":"建议调查。","intent_assessment":{"disposition":"prepare_work","action":"read_only_investigation","reason":"检查材料"}}`
		w := h.request(t, "send_message", proposal, "must-not-admit")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusForbidden, "forbidden")

		// A previously deployed B-only composition could have admitted this root.
		// Construct that durable receipt using B, not by inserting rows.
		member, err := db.BindMemberTool(ctx, h.claim)
		readinessHTTPOK(t, err)
		candidate, err := db.CaptureChatCandidate(ctx, member)
		readinessHTTPOK(t, err)
		retained, err = db.ApplyChatAssessment(ctx, member, candidate.ID, domain.ChatAssessment{
			Disposition: "prepare_work", Action: domain.ChatActionReadOnlyInvestigation, Reason: "旧准入",
		}, "旧准入。", "stored-b-admission")
		readinessHTTPOK(t, err)
		if retained.Outcome != "admitted" {
			t.Fatal("fixture failed to reproduce the retained B admission")
		}
	})
	turn, err := db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: root.GroupID, DeliveryID: later.ID, MaxActive: 1})
	readinessHTTPOK(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	member, err := db.BindMemberTool(ctx, claim)
	readinessHTTPOK(t, err)
	candidate, err := db.CaptureChatCandidate(ctx, member)
	readinessHTTPOK(t, err)
	limit, err := db.ApplyChatAssessment(ctx, member, candidate.ID, domain.ChatAssessment{
		Disposition: "discuss", Reason: "引用原文", DiscussionOnlyExcerpt: new(excerpt),
	}, "先讨论。", "late-discussion-limit")
	readinessHTTPOK(t, err)
	if !limit.DiscussionOnly || limit.SourceID != retained.SourceID {
		t.Fatal("fixture did not record the late restriction for the admitted source")
	}
	_, err = db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	readinessHTTPOK(t, err)
	dispatch, err := db.MaterializeDispatchRequest(ctx, retained.DispatchRequestID, 1, time.Now().UTC())
	readinessHTTPOK(t, err)
	if dispatch.Delivery == nil {
		t.Fatal("fixture expected the v27 B dispatcher to materialize the retained admission")
	}
	f.selectMessage(t, dispatch.Delivery.MessageID)
	readiness, err := p.InspectRoleReadiness(ctx, f.pending)
	readinessHTTPOK(t, err)
	prepared, err := p.Prepare(ctx, f.pending)
	if readiness.Blocked == nil || readiness.Blocked.Code != "investigation_first_root_required" ||
		!errors.Is(err, scheduler.ErrNotReady) || prepared.Executor != nil || len(prepared.ResourceKeys) != 0 {
		t.Fatalf("retained shared-root admission escaped C's first-root fence: %+v, %v", readiness, err)
	}
}

func TestInvestigationNativeRoleWrites(t *testing.T) {
	ctx := t.Context()
	f := newReadinessHTTPFixture(t)
	entry, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	readinessHTTPOK(t, err)
	f.options.PiMemberToolsExtension = entry
	db, tools := f.a.db, f.runtime.Tools()
	readinessHTTPOK(t, tools.EnableChatIntake(coordination.New(db)))
	local := platform.NewLocalUser(db)
	routes := slices.Concat(MessageRoutes(local), MemberToolRoutes(tools), MemberRoleToolRoutes(tools), MemberInvestigationToolRoutes(tools), MemberMCPRoutes(tools))
	f.a.handler = mustServer(t, DefaultAddress, routes...).Handler
	root, err := os.OpenRoot(t.TempDir())
	readinessHTTPOK(t, err)
	t.Cleanup(func() { readinessHTTPOK(t, root.Close()) })
	blobs, err := artifactfs.New(root, 1<<20)
	readinessHTTPOK(t, err)
	t.Cleanup(func() { readinessHTTPOK(t, blobs.Close()) })
	service, err := platform.NewArtifacts(db, blobs)
	readinessHTTPOK(t, err)
	publish := func(input platform.PublishArtifactInput, text string) domain.ArtifactVersion {
		t.Helper()
		readinessHTTPOK(t, root.WriteFile("source", []byte(text), 0600))
		version, err := service.PublishArtifact(ctx, f.group.ID, input, root, "source", "")
		readinessHTTPOK(t, err)
		return version
	}
	const privateBody, tail = "PRIVATE_SCOPED_ARTIFACT", "approved exact bytes"
	content := privateBody + strings.Repeat("<", 13000) + tail
	approved := publish(platform.PublishArtifactInput{Name: "evidence"}, content)
	head := publish(platform.PublishArtifactInput{ArtifactID: approved.ArtifactID, ExpectedVersionID: approved.ID}, "unapproved head")
	grant, err := platform.NewMemberArtifactGrant(service)
	readinessHTTPOK(t, err)
	thread := domain.ThreadRef{GroupID: f.group.ID, TopicID: "investigation"}
	policy, err := db.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
		Investigation: &domain.ChatInvestigationPolicy{Objective: "检查已授权材料", AllowTopicMessages: true,
			ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: approved.ArtifactID, VersionID: approved.ID}}}}, 0, "investigation-policy")
	readinessHTTPOK(t, err)
	db.EnableInvestigationExecution() // This isolated Store; cmd has its own startup gate test.
	path := "/api/v1/groups/" + f.group.ID + "/messages"
	const material = "approved topic body, fetched by tool only"
	source := decoded[domain.Message](t, f.a.request("POST", path, fmt.Sprintf(`{"topic_id":%q,"content":%q,"recipient_ids":[%q]}`,
		thread.TopicID, material, f.lead.ID), "investigation-source"), http.StatusCreated)
	if source.SourceKind != "local_user_chat" || source.SourceID == "" || len(source.Deliveries) != 1 {
		t.Fatal("public local-user route did not persist a verifiable source and delivery")
	}
	decoded[domain.Message](t, f.a.request("POST", path, `{"topic_id":"other-topic","content":"unapproved topic","recipient_ids":[]}`, "other-topic"), http.StatusCreated)
	namespace := filepath.Join(f.options.PrivateWorkspaceRoot, platform.InvestigationCWDName)
	readinessHTTPOK(t, os.Mkdir(namespace, 0700))
	investigation := &platform.InvestigationPreparationOptions{PiPath: f.options.Providers[0].Path, Artifacts: grant}
	f.options.Investigation = investigation
	f.selectMessage(t, source.ID)
	var result chatIntentToolReceipt
	_, intake, _ := runInvestigationHTTPPi(t, f, f.preparer(t), []string{"list_members", "read_channel", "send_message", "search_history", "read_history"}, domain.ChatActionReadOnlyInvestigation, true, func(h *memberToolHTTPFixture) {
		w := memberMCPHTTP(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"action":{"enum":["text_submission","workspace_change","read_only_investigation"]}`) || strings.Contains(w.Body.String(), `"continue_work"`) {
			t.Fatal("the issued intake did not advertise the exact investigation action")
		}
		w = h.request(t, "read_channel", `{}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"investigation":`) {
			t.Fatal("the issued intake did not expose its captured investigation policy")
		}
		result = decoded[struct {
			Assessment chatIntentToolReceipt `json:"intent_assessment"`
		}](t, h.request(t, "send_message", `{"content":"将按策略调查。","intent_assessment":{"disposition":"prepare_work","action":"read_only_investigation","reason":"检查材料"}}`, "investigation-admit"), http.StatusCreated).Assessment
	})
	if !strings.Contains(intake.Request.Prompt, domain.ChatActionReadOnlyInvestigation) {
		t.Fatal("the opted-in Pi intake prompt did not explain the investigation action")
	}
	if result.Outcome != "admitted" {
		t.Fatalf("public-source investigation was not admitted: %+v", result)
	}
	admitted, err := db.GetChatAdmission(ctx, f.group.ID, result.TaskID)
	readinessHTTPOK(t, err)
	if admitted.SourceID != source.SourceID || admitted.SourceKind != source.SourceKind || admitted.DispatchRequestID != result.DispatchRequestID {
		t.Fatal("admission lost the actual public command source")
	}
	dispatch, err := db.MaterializeDispatchRequest(ctx, admitted.DispatchRequestID, 1, time.Now().UTC())
	readinessHTTPOK(t, err)
	if dispatch.Delivery == nil {
		t.Fatal("investigation coordinator delivery not materialized")
	}
	f.selectMessage(t, dispatch.Delivery.MessageID)
	assertQueued := func(p *platform.Preparer, code string) {
		t.Helper()
		inspected, err := p.InspectRoleReadiness(ctx, f.pending)
		readinessHTTPOK(t, err)
		prepared, err := p.Prepare(ctx, f.pending)
		if inspected.Blocked == nil || inspected.Blocked.Code != code ||
			!errors.Is(err, scheduler.ErrNotReady) || prepared.Executor != nil || len(prepared.ResourceKeys) != 0 {
			t.Fatalf("expected a queued investigation with %s, got %+v, %v", code, inspected, err)
		}
		message, err := db.GetMessage(ctx, f.group.ID, f.pending.Delivery.MessageID)
		readinessHTTPOK(t, err)
		if d := message.Deliveries[0]; d.State != domain.DeliveryStateQueued || d.TurnID != "" {
			t.Fatal("readiness consumed the delivery")
		}
	}
	f.options.Investigation = nil
	assertQueued(f.preparer(t), "investigation_runtime_unavailable")
	f.options.Investigation = investigation
	p := f.preparer(t)
	worker, err := db.GetMember(ctx, f.group.ID, f.worker.ID)
	readinessHTTPOK(t, err)
	_, err = f.raw.Exec("UPDATE members SET model='' WHERE id=?", worker.ID)
	readinessHTTPOK(t, err)
	assertQueued(p, "investigation_no_supported_worker")
	_, err = f.raw.Exec("UPDATE members SET model=? WHERE id=?", worker.Model, worker.ID)
	readinessHTTPOK(t, err)
	entries, err := os.ReadDir(namespace)
	readinessHTTPOK(t, err)
	if len(entries) != 0 {
		t.Fatal("blocked preparation created a CWD")
	}

	const channel, artifact = platform.ReadInvestigationChannelOperation, platform.ReadInvestigationArtifactOperation
	deny := func(h *memberToolHTTPFixture, operation, body string, keys ...string) {
		t.Helper()
		w := h.request(t, operation, body, keys...)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusForbidden, "forbidden")
	}
	var assignmentID string
	lead, coordinator, _ := runInvestigationHTTPPi(t, f, p, []string{channel, artifact, "decide_dispatch"}, "", true, func(h *memberToolHTTPFixture) {
		deny(h, "submit_result", `{"summary":"wrong role"}`, "not-mine")
		body := fmt.Sprintf(`{"kind":"assign","worker_member_id":%q,"reason":"eligible"}`, worker.ID)
		first := h.request(t, "decide_dispatch", body, "investigation-assign")
		assignmentID = decoded[struct {
			AssignmentID string `json:"assignment_id"`
		}](t, first, http.StatusCreated).AssignmentID
		replay := h.request(t, "decide_dispatch", body, "investigation-assign")
		if assignmentID == "" || replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() {
			t.Fatal("the sampled exact worker fact did not assign, or original-key replay failed after assignment")
		}
		deny(h, channel, `{}`) // Assignment closed the read scope, not same-key replay.
	})
	if lead.ReplyMessageID == "" {
		t.Fatal("expected a synthetic terminal reply")
	}
	reply, err := db.GetMessage(ctx, f.group.ID, lead.ReplyMessageID)
	readinessHTTPOK(t, err)
	if reply.SourceKind != "member" || len(reply.Deliveries) != 0 {
		t.Fatal("a public final reply must not become a new intake wakeup")
	}
	assignment, err := db.MaterializeAssignment(ctx, assignmentID, 1, time.Now().UTC())
	readinessHTTPOK(t, err)
	if assignment.Delivery == nil {
		t.Fatal("investigation worker delivery not materialized")
	}
	f.selectMessage(t, assignment.Delivery.MessageID)
	f.options.Investigation = &platform.InvestigationPreparationOptions{PiPath: f.options.Providers[0].Path}
	assertQueued(f.preparer(t), "investigation_artifacts_unavailable")

	workerTurn, prepared, capability := runInvestigationHTTPPi(t, f, p, []string{channel, artifact, "submit_result"}, "", false, func(h *memberToolHTTPFixture) {
		view, err := db.GetAssignment(ctx, f.group.ID, assignmentID)
		readinessHTTPOK(t, err)
		b := view.Binding
		if b == nil || b.TurnID != h.claim.TurnID || b.WorkerMemberID != worker.ID || b.TaskID != admitted.TaskID || b.AttemptID == "" || b.TaskRevision != 1 {
			t.Fatalf("native worker did not use a persisted exact Attempt: %+v", b)
		}
		deny(h, "decide_dispatch", `{"kind":"decline","reason":"wrong role"}`, "not-mine")
		deny(h, "read_channel", `{}`)
		page := decoded[listResult[domain.Message]](t, h.request(t, channel, `{}`), http.StatusOK)
		found := false
		for _, m := range page.Items {
			if m.GroupID != thread.GroupID || m.TopicID != thread.TopicID || len(m.Deliveries) != 0 {
				t.Fatal("topic read exposed material or receipts outside its scope")
			}
			found = found || m.ID == source.ID && m.Content == material
		}
		if !found {
			t.Fatal("allowed topic read lost real material")
		}
		tuple := func(id string) string {
			return fmt.Sprintf(`{"artifact_id":%q,"version_id":%q}`, approved.ArtifactID, id)
		}
		deny(h, artifact, tuple(head.ID))
		// The native JSON fits, but quoting it as MCP text overflows the envelope.
		// Neither overflow response may emit a partial fragment.
		chunk := strings.TrimSuffix(tuple(approved.ID), "}")
		args := chunk + `,"max_bytes":10000}`
		native := decoded[platform.InvestigationArtifactFragment](t, h.request(t, artifact, args), http.StatusOK)
		if native.Content != content[:10000] || native.NextOffset != 10000 || native.ContentComplete || native.VersionID != approved.ID || native.SHA256 != approved.BlobHash {
			t.Fatal("native read lost the exact version or rejected a fragment that fits")
		}
		mcp := memberMCPCall(`"oversized"`, artifact, args)
		if text := memberMCPText(t, memberMCPHTTP(t, h, mcp), true); text != `{"error":{"code":"tool_result_too_large","message":"工具结果过大，请减小 max_bytes 后重试"}}` {
			t.Fatal("MCP did not replace the oversized result with fixed max_bytes advice")
		}
		tooLarge := h.request(t, artifact, fmt.Sprintf(`%s,"max_bytes":%d}`, chunk, len(content)))
		assertError(t, tooLarge.Code, tooLarge.Header(), tooLarge.Body.Bytes(), http.StatusRequestEntityTooLarge, "tool_result_too_large")
		if text := tooLarge.Body.String(); !strings.Contains(text, "max_bytes") || strings.Contains(text, privateBody) || strings.Contains(text, approved.ID) {
			t.Fatal("native budget error omitted max_bytes guidance or projected private data")
		}
		var fragment platform.InvestigationArtifactFragment
		mcp = memberMCPCall(`"exact"`, artifact, fmt.Sprintf(`%s,"offset":%d,"max_bytes":%d}`, chunk, len(content)-len(tail), len(tail)))
		readinessHTTPOK(t, json.Unmarshal([]byte(memberMCPText(t, memberMCPHTTP(t, h, mcp), false)), &fragment))
		if fragment.VersionID != approved.ID || fragment.SHA256 != approved.BlobHash || fragment.Content != tail || !fragment.ContentComplete {
			t.Fatal("MCP did not deliver the approved immutable blob fragment")
		}
		bad := h.request(t, "submit_result", fmt.Sprintf(`{"summary":"not a file task","artifact_versions":[%s]}`, tuple(approved.ID)), "wrong-result")
		assertError(t, bad.Code, bad.Header(), bad.Body.Bytes(), http.StatusBadRequest, "invalid_input")

		const summary = `{"summary":"仅提交文字，尚待审查。"}`
		first := h.request(t, "submit_result", summary, "investigation-submit")
		sub := decoded[struct {
			SubmissionID string `json:"submission_id"`
		}](t, first, http.StatusCreated)
		request := policy.ChatPolicyRequest
		request.Enabled = false
		_, err = db.SetChatPolicy(ctx, thread, request, policy.Revision, "revoke-investigation")
		readinessHTTPOK(t, err)
		deny(h, artifact, tuple(approved.ID))
		for _, call := range []string{memberMCPCall(`"revoked-artifact"`, artifact, tuple(approved.ID)), memberMCPCall(`"revoked-channel"`, channel, `{}`)} {
			text := memberMCPText(t, memberMCPHTTP(t, h, call), true)
			if !strings.Contains(text, `"code":"forbidden"`) || strings.Contains(text, privateBody) || strings.Contains(text, material) {
				t.Fatal("MCP did not recheck the live investigation authority")
			}
		}
		listed := decoded[struct {
			Result struct{ Tools []memberMCPTool } `json:"result"`
		}](t, memberMCPHTTP(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), http.StatusOK)
		if len(listed.Result.Tools) != 1 || listed.Result.Tools[0].Name != "submit_result" {
			t.Fatal("revoked materials remained discoverable, or discovery hid the original command")
		}
		replay := h.request(t, "submit_result", summary, "investigation-submit")
		task, err := db.GetTask(ctx, f.group.ID, admitted.TaskID)
		readinessHTTPOK(t, err)
		if replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() || task.Status != domain.TaskStatusSubmitted ||
			sub.SubmissionID == "" || task.CurrentSubmissionID != sub.SubmissionID || task.ActiveAttemptID != b.AttemptID {
			t.Fatal("loss of read readiness broke original-key replay or submission bypassed formal review")
		}
	})
	for _, excluded := range []string{material, "unapproved", privateBody, tail, head.ID, worker.Model} {
		if strings.Contains(prepared.Request.Prompt, excluded) {
			t.Fatalf("strict preparation projected ambient metadata or material bytes: %q", excluded)
		}
	}
	if !strings.Contains(prepared.Request.Prompt, approved.ID) || filepath.Dir(prepared.Request.CWD) != namespace ||
		prepared.Request.CWD == coordinator.Request.CWD || slices.Equal(prepared.ResourceKeys, coordinator.ResourceKeys) {
		t.Fatal("worker lacks its exact material reference or separate private CWD")
	}
	keys, err := scheduler.WorkspaceResourceKeys([]string{prepared.Request.CWD})
	readinessHTTPOK(t, err)
	held, err := db.HasResourceClaim(ctx, keys[0])
	readinessHTTPOK(t, err)
	diagnostic, err := f.runtime.InspectCoordinationRoleReceipt(ctx, f.group.ID, workerTurn.ID)
	readinessHTTPOK(t, err)
	if !slices.Equal(prepared.ResourceKeys, keys) || !held || diagnostic.Capture == nil ||
		!diagnostic.Capture.Raw.DirectChildReaped || diagnostic.Capture.Raw.Terminal != "" || diagnostic.Capture.Aggregate.Outcome != provider.OutcomeUnknown {
		t.Fatal("unknown native result did not retain the exact CWD claim and original receipt")
	}
	if _, err := tools.TransportOperations(ctx, capability); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("worker token survived native drain")
	}
}

func runInvestigationHTTPPi(t *testing.T, f *readinessHTTPFixture, p *platform.Preparer, operations []string, wantAction string, terminal bool,
	use func(*memberToolHTTPFixture)) (domain.Turn, scheduler.PreparedExecution, string) {
	t.Helper()
	pending, db := f.pending, f.a.db
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	prepared, err := p.Prepare(ctx, pending)
	readinessHTTPOK(t, err)
	scratch := t.TempDir()
	marker, release, digestFile := filepath.Join(scratch, "argv"), filepath.Join(scratch, "release"), filepath.Join(scratch, "digest")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
/bin/cat >/dev/null
printf '%%s' "${AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256-}" > %q
printf '%%s\n' "$@" > %q
/bin/mv %q %q
while [ ! -f %q ]; do /bin/sleep 0.02; done
`, digestFile, marker+".tmp", marker+".tmp", marker, release)
	if terminal {
		script += `while [ "$1" != '--session-id' ]; do shift; done
printf '{"type":"session","id":"%s","cwd":"/fixture","version":3}\n' "$2"
printf '%s\n' '{"type":"agent_start"}' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"synthetic reply"}]}}' '{"type":"agent_settled","aborted":false}'
`
	}
	readinessHTTPOK(t, os.WriteFile(f.options.Providers[0].Path, []byte(script), 0700))
	stopped := make(chan struct{})
	var scheduleErr error
	go func() {
		defer close(stopped)
		scheduleErr = scheduler.Run(ctx, db, scheduler.Options{MaxActive: 1, RuntimeDir: f.runRoot, Recovered: true, PollInterval: 20 * time.Millisecond}, func(ctx context.Context, next store.PendingDelivery) (scheduler.PreparedExecution, error) {
			if next.Delivery.ID != pending.Delivery.ID {
				return scheduler.PreparedExecution{}, scheduler.ErrNotReady
			}
			return p.Prepare(ctx, next)
		})
	}()
	defer func() {
		cancel()
		<-stopped
		if !errors.Is(scheduleErr, context.Canceled) {
			t.Errorf("scheduler did not stop cleanly: %v", scheduleErr)
		}
	}()
	await := func(label string, ready func() bool) {
		t.Helper()
		for !ready() {
			select {
			case <-stopped:
				t.Fatalf("scheduler stopped while waiting for %s: %v", label, scheduleErr)
			case <-ctx.Done():
				t.Fatalf("timed out waiting for %s", label)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	var argv []byte
	await("synthetic Pi argv", func() bool {
		var err error
		argv, err = os.ReadFile(marker)
		if !os.IsNotExist(err) {
			readinessHTTPOK(t, err)
		}
		return err == nil
	})
	message, err := db.GetMessage(ctx, f.group.ID, pending.Delivery.MessageID)
	readinessHTTPOK(t, err)
	turn, err := db.GetTurn(ctx, f.group.ID, message.Deliveries[0].TurnID)
	readinessHTTPOK(t, err)
	for _, flag := range []string{"--extension\n" + f.options.PiMemberToolsExtension, "--no-builtin-tools", "--no-extensions", "--no-mcp", "--no-skills", "--no-context-files",
		"--tools\n" + strings.Join(operations, ","), "--model\n" + prepared.Request.Model, "--session-id\n" + turn.ID} {
		if !strings.Contains("\n"+string(argv), "\n"+flag+"\n") {
			t.Fatalf("Pi argv missing exact restriction %q", flag)
		}
	}
	files, err := os.ReadDir(f.configRoot)
	readinessHTTPOK(t, err)
	if len(files) != 1 {
		t.Fatal("expected a unique live capability config")
	}
	info, err := files[0].Info()
	readinessHTTPOK(t, err)
	wire, err := os.ReadFile(filepath.Join(f.configRoot, files[0].Name()))
	readinessHTTPOK(t, err)
	var config struct {
		Token                  string
		InvestigationTools     []string `json:"investigation_tools"`
		IntakeAssessmentAction string   `json:"intake_assessment_action"`
	}
	readinessHTTPOK(t, json.Unmarshal(wire, &config))
	if info.Mode() != 0600 || config.Token == "" || config.Token == turn.ClaimToken {
		t.Fatal("runtime did not issue a separate private capability")
	}
	strictRole := !slices.Contains(operations, "send_message")
	if strictRole || wantAction != "" {
		digest, err := os.ReadFile(digestFile)
		readinessHTTPOK(t, err)
		sum := sha256.Sum256(wire)
		if string(digest) != hex.EncodeToString(sum[:]) {
			t.Fatal("investigation Pi did not receive the digest of the exact issued config")
		}
	}
	if strictRole {
		if !slices.Equal(config.InvestigationTools, operations) {
			t.Fatal("strict Pi did not receive the exact issued tools")
		}
		args := "\n" + string(argv)
		for _, flag := range []string{"--offline", "--no-approve", "--no-prompt-templates", "--no-themes", "--exclude-tools", "--system-prompt", "--append-system-prompt"} {
			if !strings.Contains(args, "\n"+flag+"\n") {
				t.Fatalf("missing independent Pi restriction %s", flag)
			}
		}
		if strings.Contains(args, "\n--session\n") || strings.Contains(args, "\n--continue\n") {
			t.Fatal("investigation could resume an existing session")
		}
	}
	if config.IntakeAssessmentAction != wantAction {
		t.Fatal("private config did not scope the assessment action to issued intake")
	}
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	use(&memberToolHTTPFixture{claim: claim, token: config.Token, path: f.a.path, handler: f.a.handler})
	readinessHTTPOK(t, os.WriteFile(release, nil, 0600))
	want := domain.TurnStateUnknown
	if terminal {
		want = domain.TurnStateSucceeded
	}
	await(string(want), func() bool {
		var err error
		turn, err = db.GetTurn(ctx, f.group.ID, claim.TurnID)
		readinessHTTPOK(t, err)
		return turn.State == want
	})
	files, err = os.ReadDir(f.configRoot)
	readinessHTTPOK(t, err)
	if len(files) != 0 {
		t.Fatal("settled invocation left a capability config on disk")
	}
	return turn, prepared, config.Token
}
