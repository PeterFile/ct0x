package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestAgentRunStoreSubmissionSurvivesUnknownAndDeparture(t *testing.T) {
	f, request, worker := assignmentBoundWorker(t)
	claim := worker.Member.Claim
	sub, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: strings.Repeat("正式成果", 400)}, "run-log-submission")
	if err != nil {
		t.Fatal(err)
	}
	queueCandidate(t, f.s, claim, "UNPUBLISHED", provider.OutcomeSucceeded)
	receipt := queueReceipt(provider.OutcomeSucceeded)
	receipt.OutputComplete = false
	turn, err := f.s.ApplyTurnReceipt(ctx, claim, receipt)
	if err != nil || turn.State != domain.TurnStateUnknown {
		t.Fatalf("unconfirmed Turn: %+v, %v", turn, err)
	}
	coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE group_id=? AND id=?", request.GroupID, worker.Member.MemberID)

	page, err := f.s.ListAgentRuns(ctx, worker.Member.MemberID, ListOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].RunID != turn.ID || page.Items[0].MemberName == "" {
		t.Fatalf("lost departed member's run: %+v, %v", page, err)
	}
	view, err := f.s.GetAgentRun(ctx, request.GroupID, turn.ID, ListOptions{})
	if err != nil || view.Run != page.Items[0] || view.Receipt == nil || view.Receipt.Outcome != domain.TurnStateUnknown || len(view.Timeline.Items) != 2 {
		t.Fatalf("run unknown and committed evidence must coexist: %+v, %v", view, err)
	}
	entry := view.Timeline.Items[1]
	result := entry.Submission
	if entry.Kind != "submission" || entry.CreatedAt != sub.CreatedAt || result == nil || result.SubmissionID != sub.ID ||
		result.TaskID != sub.TaskID || result.TaskRevision != sub.TaskRevision || result.AttemptID != sub.AttemptID ||
		!result.Truncated || result.Preview == "" || !strings.HasPrefix(sub.Summary, result.Preview) {
		t.Fatalf("lost exact formal Submission: %+v, %+v", entry, result)
	}
	full, err := f.s.GetTaskSubmission(ctx, request.GroupID, result.TaskID, result.SubmissionID)
	if err != nil || full.Summary != sub.Summary {
		t.Fatalf("exact Submission read lost full evidence: %+v, %v", full, err)
	}
}

func TestAgentRunStoreWorkspaceReceiptAndPendingIntentPagination(t *testing.T) {
	f, request, worker := workspaceOperationFixture(t)
	s := f.s
	claim := worker.Member.Claim
	done := workspaceOperationDone(t, s, worker, "run-log-done")
	pending := workspaceOperationBegin(t, s, worker, "run-log-pending")
	turn, err := s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown})
	if err != nil || turn.State != domain.TurnStateUnknown {
		t.Fatalf("fixture: %+v, %v", turn, err)
	}
	if _, err := s.GetAgentRun(ctx, "wrong-group", claim.TurnID, ListOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign Turn selected: %v", err)
	}
	options := ListOptions{Limit: 1}
	for _, want := range []WorkspaceOperation{pending, done} {
		view, err := s.GetAgentRun(ctx, request.GroupID, claim.TurnID, options)
		if err != nil || view.Run.State != domain.TurnStateUnknown || len(view.Timeline.Items) != 1 || view.Timeline.NextCursor == nil {
			t.Fatalf("workspace page: %+v, %v", view, err)
		}
		entry := view.Timeline.Items[0]
		o := entry.WorkspaceOperation
		if entry.Kind != "workspace_operation" || entry.CreatedAt != want.CreatedAt || o == nil || o.OperationID != want.ID ||
			o.TaskID != want.TaskID || o.AttemptID != want.AttemptID || o.Kind != want.Input.Kind || o.RelativePath != want.Input.RelativePath {
			t.Fatalf("action lost its durable identity or timestamp: %+v, %+v", entry, o)
		}
		if want.Result == nil {
			if o.State != "pending" || o.Result != nil {
				t.Fatalf("intent without receipt presented as a result: %+v", o)
			}
		} else if o.State != want.Result.Outcome || o.Result == nil || *o.Result != (AgentRunWorkspaceResult{
			ObservedHash: want.Result.ObservedHash, ObservedSize: want.Result.ObservedSize,
			ErrorCode: want.Result.ErrorCode, FinishedAt: *want.FinishedAt,
		}) {
			t.Fatalf("committed tool receipt surrendered or reinterpreted: %+v, %+v", o, o.Result)
		}
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{want.ResourceKey, want.GrantID, want.claimDigest, want.Preimage.BlobHash, "/fixture/workspace", "grant_id", "claim_digest"} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("workspace projection leaked %q", forbidden)
			}
		}
		options.Before = view.Timeline.NextCursor
	}
	older, err := s.GetAgentRun(ctx, request.GroupID, claim.TurnID, options)
	if err != nil || len(older.Timeline.Items) != 1 || older.Timeline.Items[0].Kind != "trigger_message" || older.Timeline.NextCursor != nil {
		t.Fatalf("older trigger page: %+v, %v", older, err)
	}
}

func TestAgentRunStorePublicationChecksGroupTopicAndSender(t *testing.T) {
	f, turn, claim := queueClaimFixture(t)
	for _, src := range []struct{ group, topic, sender string }{
		{f.otherID, "bound-topic", f.foreign}, {f.groupID, "not-bound-topic", f.a}, {f.groupID, "bound-topic", f.b},
	} {
		m, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: src.group, TopicID: src.topic, ActorID: src.sender, Content: "unrelated publication"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Deliberately damage the stored source association (not the message).
		// The published message must still pass exact group/topic/sender joins.
		coordinationDecideExec(t, f.s, "UPDATE turns SET reply_message_id=? WHERE id=?", m.ID, turn.ID)
		coordinationDecideExec(t, f.s, `INSERT INTO message_command_sources(id,message_id,kind,payload,idempotency_key,turn_id,claim_digest)
		 VALUES(?,?,'member',x'01','fixture',?,zeroblob(32))`, "source-"+m.ID, m.ID, turn.ID)
	}
	view, err := f.s.GetAgentRun(ctx, turn.GroupID, claim.TurnID, ListOptions{})
	if err != nil || len(view.Timeline.Items) != 1 || view.Timeline.Items[0].Kind != "trigger_message" || view.AcceptedAt != nil || view.Receipt != nil {
		t.Fatalf("unrelated message or queued execution leaked into exact Turn: %+v, %v", view, err)
	}
}
