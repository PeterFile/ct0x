package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

const (
	automaticTextDiscussion = "这次只讨论，不开工。去年同期列计入本季合计吗？"
	automaticTextWork       = "上一条只讨论；现在开始做这项纯文本交付。请在群里说明本季合计不包括去年同期列。验收是分别说明本季合计和同比对照两行口径。不改文件。"
	automaticTextSubmission = "本季合计：只汇总本季实际发生额。\n同比对照：去年同期列仅用于对照，不计入本季合计。"
)

func TestAutomaticChatTextCompositionFromEmptyGroup(t *testing.T) {
	f := &coordinationCompositionFixture{native: memberToolsFixture(t), dir: t.TempDir()}
	body, err := os.ReadFile(f.native.path)
	if err != nil || bytes.Count(body, []byte("^TestMemberToolsNativeHelper$")) != 1 {
		t.Fatalf("native fixture launcher: %v", err)
	}
	body = bytes.Replace(body, []byte("^TestMemberToolsNativeHelper$"), []byte("^TestCoordinationCompositionNativeHelper$"), 1)
	if err := os.WriteFile(f.native.path, body, 0700); err != nil {
		t.Fatal(err)
	}
	base, stop, _, _ := startCoordinationComposition(t, 30*time.Second, f.args(t, "")...)
	f.db, err = store.Open(filepath.Join(f.dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.raw, err = sql.Open("sqlite", filepath.Join(f.dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.raw.Close() })

	selected := executionProvider(t, base)
	coordinationCompositionHTTP(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "季度统计"}, "group", 201, &f.group)
	path := base + "/api/v1/groups/" + f.group.ID
	const topic = "quotas"
	policyPath := path + "/coordination/chat-policy?topic_id=" + topic
	if coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM members") != 0 {
		t.Fatal("group was not created empty")
	}
	input := map[string]any{"enabled": true, "action": "text_submission", "max_admissions": 1, "expected_revision": 0, "automatic_text_dispatch": true}
	var policy domain.ChatPolicy
	initial := coordinationCompositionHTTP(t, "PUT", policyPath, input, "text-policy", 200, &policy)
	if policy.Revision != 1 || policy.TopicID != topic || policy.ConfigurationSourceID == "" ||
		!bytes.Contains(initial, []byte(`"automatic_text_dispatch":true`)) ||
		!bytes.Equal(initial, coordinationCompositionHTTP(t, "GET", policyPath, nil, "", 200, nil)) {
		t.Fatalf("missing exact local-user policy revision: %s", initial)
	}
	for _, actor := range []struct {
		member    *domain.Member
		name, key string
	}{{&f.lead, "接收人", "initial-chat-recipient"}, {&f.worker, "复核同伴", "user-approved-worker"}} {
		coordinationCompositionHTTP(t, "POST", path+"/members", platform.MemberInput{
			Name: actor.name, Role: "群内协作", ProviderID: selected.ID, Model: coordinationCompositionFixtureModel,
		}, actor.key, 201, actor.member)
	}
	memberToolsWriteJSON(t, filepath.Join(f.native.root, "expected.json"), coordinationCompositionExpectation{
		DataDir: f.dir, GroupID: f.group.ID, CoordinatorID: f.lead.ID, WorkerID: f.worker.ID,
		Extension: memberToolsEntry(t), Mode: "automatic-text",
	})
	var discussion domain.Message
	coordinationCompositionHTTP(t, "POST", path+"/messages", platform.MessageInput{Content: automaticTextDiscussion, TopicID: topic}, "discussion", 201, &discussion)
	first := f.report(t, "ordinary")
	f.turn(t, first.RunID, domain.TurnStateSucceeded)
	for _, table := range []string{"group_coordinator_bindings", "group_dispatch_policies", "tasks", "mandates", "coordination_dispatch_requests"} {
		if coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM "+table) != 0 {
			t.Fatalf("policy or discussion wrote %s before a new work request", table)
		}
	}
	if err := os.Remove(filepath.Join(f.native.root, "ordinary.json")); err != nil {
		t.Fatal(err)
	}
	var source domain.Message
	message := platform.MessageInput{Content: automaticTextWork, TopicID: topic}
	coordinationCompositionHTTP(t, "POST", path+"/messages", message, "ordinary-work", 201, &source)
	second := f.report(t, "ordinary")
	intake := f.turn(t, second.RunID, domain.TurnStateSucceeded)
	if got := coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM tasks"); got != 1 {
		var result string
		_ = f.raw.QueryRow(`SELECT receipt_json FROM chat_assessments a JOIN chat_candidates c ON c.id=a.candidate_id WHERE c.message_id=?`, source.ID).Scan(&result)
		t.Fatalf("authorized ordinary chat produced %d Tasks: %s", got, result)
	}
	co := f.report(t, "coordinator")
	coordinationCompositionAssignmentPrerequisite(t, f, co)
	f.release(t, "coordinator")
	f.turn(t, co.RunID, domain.TurnStateSucceeded)
	worker := f.report(t, "worker")
	f.release(t, "worker")
	f.turn(t, worker.RunID, domain.TurnStateSucceeded)
	assignment, err := f.db.GetAssignment(t.Context(), f.group.ID, co.AssignmentID)
	if err != nil || assignment.Binding == nil || assignment.Binding.TurnID != worker.RunID || assignment.Assignment.WorkerMemberID != f.worker.ID ||
		assignment.Request.CoordinatorMemberID != f.lead.ID || assignment.Request.TopicID != topic {
		t.Fatalf("old scheduler did not use the selected roles: %+v %v", assignment, err)
	}
	request := assignment.Request
	var evidence domain.ChatAssessmentReceipt
	admissionJSON := coordinationCompositionHTTP(t, "GET", path+"/tasks/"+request.TaskID+"/chat-admission", nil, "", 200, &evidence)
	if bytes.Contains(admissionJSON, []byte(`"automatic_text_routing_checked"`)) {
		t.Fatalf("internal replay marker escaped into strict admission wire: %s", admissionJSON)
	}
	if evidence.TurnID != intake.ID || intake.ID == co.RunID || co.RunID == worker.RunID || evidence.AssessedBy != f.lead.ID ||
		evidence.SourceKind != "local_user_chat" || evidence.SourceID != source.SourceID || evidence.MessageID != source.ID ||
		evidence.AuthorizationMessageID != source.ID || evidence.PolicyRevision != policy.Revision || evidence.TopicID != topic ||
		evidence.DispatchRequestID != request.ID || evidence.MandateID != request.MandateID || evidence.Outcome != "admitted" {
		t.Fatalf("lost original user command or binding: %+v", evidence)
	}
	task, err := f.db.GetTask(t.Context(), f.group.ID, request.TaskID)
	if err != nil || task.Status != domain.TaskStatusSubmitted || task.CurrentSubmissionID != worker.SubmissionID {
		t.Fatalf("submission is a separate persisted fact: %+v %v", task, err)
	}
	var noticeID string
	if err := f.raw.QueryRow(`SELECT message_id FROM chat_submission_notices WHERE submission_id=?`, worker.SubmissionID).Scan(&noticeID); err != nil {
		t.Fatal(err)
	}
	notice, err := f.db.GetMessage(t.Context(), f.group.ID, noticeID)
	if err != nil || notice.TopicID != topic || notice.SenderID != f.worker.ID || !strings.Contains(notice.Content, automaticTextSubmission) || len(notice.Deliveries) != 0 {
		t.Fatalf("result did not return to the original topic: %+v %v", notice, err)
	}
	var replay domain.Message
	coordinationCompositionHTTP(t, "POST", path+"/messages", message, "ordinary-work", 201, &replay)
	if replay.ID != source.ID || source.SourceID == "" || source.SourceID == discussion.SourceID {
		t.Fatal("ordinary source identity was not independent and replayable")
	}
	for _, table := range []string{"tasks", "mandates", "coordination_dispatch_requests", "coordination_assignments", "task_attempts", "task_submissions"} {
		if coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM "+table) != 1 {
			t.Fatalf("expected exactly one %s row", table)
		}
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"task_review_decisions", "resource_claims"} {
		if coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM "+table) != 0 {
			t.Fatalf("submission manufactured %s or leaked a claim", table)
		}
	}
}

func TestAutomaticChatTextCompositionOneLineStartImport(t *testing.T) {
	f := &coordinationCompositionFixture{native: memberToolsFixture(t), dir: t.TempDir()}
	body, err := os.ReadFile(f.native.path)
	if err != nil || bytes.Count(body, []byte("^TestMemberToolsNativeHelper$")) != 1 {
		t.Fatalf("native fixture launcher: %v", err)
	}
	body = bytes.Replace(body, []byte("^TestMemberToolsNativeHelper$"), []byte("^TestCoordinationCompositionNativeHelper$"), 1)
	if err := os.WriteFile(f.native.path, body, 0700); err != nil {
		t.Fatal(err)
	}
	base, stop, _, _ := startCoordinationComposition(t, 30*time.Second, f.args(t, "")...)
	f.db, err = store.Open(filepath.Join(f.dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.raw, err = sql.Open("sqlite", filepath.Join(f.dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.raw.Close() })

	selected := executionProvider(t, base)
	var srcGroup domain.Group
	coordinationCompositionHTTP(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "已授权源群"}, "src-group", 201, &srcGroup)
	const srcTopic = "src-topic"
	var srcPolicy domain.ChatPolicy
	coordinationCompositionHTTP(t, "PUT", base+"/api/v1/groups/"+srcGroup.ID+"/coordination/chat-policy?topic_id="+srcTopic, map[string]any{
		"enabled": true, "action": "text_submission", "max_admissions": 2, "expected_revision": 0, "automatic_text_dispatch": true,
	}, "src-policy", 200, &srcPolicy)
	if srcPolicy.Revision != 1 || !srcPolicy.AutomaticTextDispatch {
		t.Fatalf("unexpected source policy: %+v", srcPolicy)
	}

	// Create new group importing source authorization; NO PUT chat-policy or coordinator setup on f.group.
	coordinationCompositionHTTP(t, "POST", base+"/api/v1/groups", platform.GroupInput{
		Name: "导入授权一句直达群", ImportAuthorizationFromGroupID: srcGroup.ID, ImportAuthorizationFromTopicID: srcTopic,
	}, "imported-group", 201, &f.group)
	path := base + "/api/v1/groups/" + f.group.ID
	const newTopic = "brand-new-topic"
	var beforePolicy domain.ChatPolicy
	coordinationCompositionHTTP(t, "GET", path+"/coordination/chat-policy?topic_id="+newTopic, nil, "", 200, &beforePolicy)
	if beforePolicy.Revision != 0 {
		t.Fatalf("new topic policy materialized before first user chat: %+v", beforePolicy)
	}

	for _, actor := range []struct {
		member    *domain.Member
		name, key string
	}{{&f.lead, "接收人", "import-lead"}, {&f.worker, "执行同伴", "import-worker"}} {
		coordinationCompositionHTTP(t, "POST", path+"/members", platform.MemberInput{
			Name: actor.name, Role: "群内协作", ProviderID: selected.ID, Model: coordinationCompositionFixtureModel,
		}, actor.key, 201, actor.member)
	}
	memberToolsWriteJSON(t, filepath.Join(f.native.root, "expected.json"), coordinationCompositionExpectation{
		DataDir: f.dir, GroupID: f.group.ID, CoordinatorID: f.lead.ID, WorkerID: f.worker.ID,
		Extension: memberToolsEntry(t), Mode: "automatic-text",
	})

	var source domain.Message
	coordinationCompositionHTTP(t, "POST", path+"/messages", platform.MessageInput{Content: automaticTextWork, TopicID: newTopic}, "one-line-work", 201, &source)
	ordinary := f.report(t, "ordinary")
	f.turn(t, ordinary.RunID, domain.TurnStateSucceeded)
	co := f.report(t, "coordinator")
	coordinationCompositionAssignmentPrerequisite(t, f, co)
	f.release(t, "coordinator")
	f.turn(t, co.RunID, domain.TurnStateSucceeded)
	worker := f.report(t, "worker")
	f.release(t, "worker")
	f.turn(t, worker.RunID, domain.TurnStateSucceeded)

	assignment, err := f.db.GetAssignment(t.Context(), f.group.ID, co.AssignmentID)
	if err != nil || assignment.Request.CoordinatorMemberID != f.lead.ID || assignment.Assignment.WorkerMemberID != f.worker.ID || assignment.Request.TopicID != newTopic {
		t.Fatalf("unexpected automatic assignment on imported group: %+v %v", assignment, err)
	}
	var matPolicy domain.ChatPolicy
	coordinationCompositionHTTP(t, "GET", path+"/coordination/chat-policy?topic_id="+newTopic, nil, "", 200, &matPolicy)
	if matPolicy.Revision != 1 || !matPolicy.Enabled || !matPolicy.AutomaticTextDispatch || matPolicy.MaxAdmissions != 2 || matPolicy.ConfigurationSourceID == "" {
		t.Fatalf("expected auto-materialized rev-1 topic policy: %+v", matPolicy)
	}
	var evidence domain.ChatAssessmentReceipt
	coordinationCompositionHTTP(t, "GET", path+"/tasks/"+assignment.Request.TaskID+"/chat-admission", nil, "", 200, &evidence)
	if evidence.Outcome != "admitted" || evidence.PolicyRevision != 1 || evidence.TopicID != newTopic {
		t.Fatalf("unexpected imported group admission evidence: %+v", evidence)
	}
	var noticeID string
	if err := f.raw.QueryRow(`SELECT message_id FROM chat_submission_notices WHERE submission_id=?`, worker.SubmissionID).Scan(&noticeID); err != nil {
		t.Fatal(err)
	}
	notice, err := f.db.GetMessage(t.Context(), f.group.ID, noticeID)
	if err != nil || notice.TopicID != newTopic || !strings.Contains(notice.Content, automaticTextSubmission) {
		t.Fatalf("notice missing on imported topic: %+v %v", notice, err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}
