package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

// The scripted native fixture proves the real HTTP/capability/queue/receipt
// composition; it is deliberately not evidence of a paid model's semantics.
func TestChatIntentCompositionDiscussionToAutomaticSubmission(t *testing.T) {
	f := newCoordinationCompositionFixture(t, "chat-intake", "")
	thread := domain.ThreadRef{GroupID: f.group.ID, TopicID: "role-topic"}
	if _, err := f.db.SetChatPolicy(context.Background(), thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2}, 0, "chat-policy"); err != nil {
		t.Fatal(err)
	}
	base, stop, _, _ := startCoordinationComposition(t, 30*time.Second, f.args(t, "")...)
	path := base + "/api/v1/groups/" + f.group.ID
	var discussion domain.Message
	coordinationCompositionHTTP(t, "POST", path+"/messages", platform.MessageInput{Content: "先讨论摘要应当包含哪三条结论。", TopicID: thread.TopicID}, "discussion", 201, &discussion)
	first := f.report(t, "ordinary")
	f.turn(t, first.RunID, domain.TurnStateSucceeded)
	if coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM tasks") != 0 {
		t.Fatal("ordinary discussion admitted a task")
	}
	if err := os.Remove(filepath.Join(f.native.root, "ordinary.json")); err != nil {
		t.Fatal(err)
	}
	var source domain.Message
	input := platform.MessageInput{Content: "就按刚才说定的三条，把那份可核对摘要落实吧。", TopicID: thread.TopicID}
	coordinationCompositionHTTP(t, "POST", path+"/messages", input, "contextual-request", 201, &source)
	second := f.report(t, "ordinary")
	f.turn(t, second.RunID, domain.TurnStateSucceeded)
	coordinator := f.report(t, "coordinator")
	// Race instrumentation uses the existing safety prerequisite; ordinary
	// builds still prove liveness through the unchanged one-second intent pass.
	coordinationCompositionAssignmentPrerequisite(t, f, coordinator)
	f.release(t, "coordinator")
	f.turn(t, coordinator.RunID, domain.TurnStateSucceeded)
	worker := f.report(t, "worker")
	f.release(t, "worker")
	f.turn(t, worker.RunID, domain.TurnStateSucceeded)
	var taskID string
	if err := f.raw.QueryRow("SELECT task_id FROM task_submissions WHERE id=?", worker.SubmissionID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.GetTask(context.Background(), f.group.ID, taskID)
	if err != nil || task.Status != domain.TaskStatusSubmitted || task.CurrentSubmissionID != worker.SubmissionID || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM task_review_decisions") != 0 {
		t.Fatalf("concrete Submission was not separate from review: %+v %v", task, err)
	}
	var evidence domain.ChatAssessmentReceipt
	coordinationCompositionHTTP(t, "GET", path+"/tasks/"+taskID+"/chat-admission", nil, "", 200, &evidence)
	if evidence.MessageID != source.ID || evidence.SourceSenderID != "user" || evidence.AssessedBy != f.lead.ID || evidence.TaskID != taskID || evidence.Authority != "explicit_request" || evidence.PolicyRevision != 1 {
		t.Fatalf("chat authority evidence lost: %+v", evidence)
	}
	messages, err := f.db.ListMessages(context.Background(), f.group.ID, store.MessageListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	returned := false
	for _, message := range messages.Items {
		if message.TopicID == thread.TopicID && message.SenderID == f.worker.ID && strings.Contains(message.Content, worker.SubmissionID) && strings.Contains(message.Content, taskID) && len(message.Deliveries) == 0 {
			returned = true
		}
	}
	if !returned {
		t.Fatal("persistent concrete Submission did not return to the originating topic")
	}
	var replay domain.Message
	coordinationCompositionHTTP(t, "POST", path+"/messages", input, "contextual-request", 201, &replay)
	if replay.ID != source.ID || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM tasks") != 1 || coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM chat_assessments") != 2 {
		t.Fatal("message replay duplicated chat admission")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM resource_claims") != 0 {
		t.Fatal("finished text loop leaked claims")
	}
}
