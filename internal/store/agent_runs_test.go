package store

import (
	"encoding/json"
	"errors"
	"slices"
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
	entry := view.Timeline.Items[0]
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

func TestAgentRunStoreGenericSubmissionsSurviveUnknown(t *testing.T) {
	f, scope, _, _, input := taskSubmissionAgent(t)
	s, claim := f.s, scope.Claim
	first, err := s.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	activation := taskAttemptInput(createTestTask(t, s, f.groupID))
	activation.ActorID, activation.MemberScope = scope.MemberID, &scope
	attempt, err := s.ActivateTaskAttempt(ctx, activation)
	if err != nil {
		t.Fatal(err)
	}
	input = taskSubmissionInput(attempt)
	input.ActorID, input.MemberScope, input.IdempotencyKey = scope.MemberID, &scope, "run-log-second-submission"
	second, err := s.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	// A real local-user submission in the same group has no member Turn.
	manual, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(createTestTask(t, s, f.groupID)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitTask(ctx, taskSubmissionInput(manual)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
		t.Fatal(err)
	}

	want := []domain.Submission{second, first}
	if first.CreatedAt.After(second.CreatedAt) || first.CreatedAt.Equal(second.CreatedAt) && first.ID > second.ID {
		want[0], want[1] = first, second
	}
	options := ListOptions{Limit: 1}
	for _, sub := range want {
		view, err := s.GetAgentRun(ctx, f.groupID, claim.TurnID, options)
		if err != nil || view.Run.State != domain.TurnStateUnknown || view.Receipt == nil || view.Receipt.Outcome != domain.TurnStateUnknown ||
			len(view.Timeline.Items) != 1 || view.Timeline.NextCursor == nil {
			t.Fatalf("expected an unknown Run with committed submission evidence: %+v, %v", view, err)
		}
		entry := view.Timeline.Items[0]
		if entry.Kind != "submission" || entry.ID != runSubmissionPrefix+sub.ID || entry.CreatedAt != sub.CreatedAt ||
			entry.Submission == nil || *entry.Submission != (AgentRunSubmission{
			SubmissionID: sub.ID, TaskID: sub.TaskID, TaskRevision: sub.TaskRevision, AttemptID: sub.AttemptID, Preview: sub.Summary,
		}) {
			t.Fatalf("lost or reordered generic submission: %+v (%+v)", entry, entry.Submission)
		}
		options.Before = view.Timeline.NextCursor
	}
	tail, err := s.GetAgentRun(ctx, f.groupID, claim.TurnID, options)
	if err != nil || len(tail.Timeline.Items) != 1 || tail.Timeline.Items[0].Kind != "trigger_message" || tail.Timeline.NextCursor != nil {
		t.Fatalf("unexpected evidence beyond the two submissions: %+v, %v", tail, err)
	}
}

func TestAgentRunStoreDispatchDecisionSurvivesUnknown(t *testing.T) {
	for _, kind := range []domain.CoordinatorDecisionKind{domain.CoordinatorDecisionKindAssign, domain.CoordinatorDecisionKindDecline} {
		t.Run(string(kind), func(t *testing.T) {
			f, request, scope := coordinationDecideScope(t)
			s, claim := f.s, scope.Member.Claim
			before, err := s.GetAgentRun(ctx, request.GroupID, claim.TurnID, ListOptions{})
			if err != nil || len(before.Timeline.Items) != 1 || before.Timeline.Items[0].Kind != "trigger_message" {
				t.Fatalf("dispatch request without a decision must not create evidence: %+v, %v", before, err)
			}
			const key = "private-run-log-dispatch-key"
			preview := "暂不分配。"
			input := CoordinatorDecisionInput{Kind: string(kind), Reason: preview}
			if kind == domain.CoordinatorDecisionKindAssign {
				preview = strings.Repeat("理", 341) // 1023 bytes; do not split the next rune.
				input.WorkerMemberID, input.Reason = f.worker, preview+"由 unseen-reason-tail"
			}
			receipt, err := s.DecideDispatch(ctx, scope, input, key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
				t.Fatal(err)
			}
			view, err := s.GetAgentRun(ctx, request.GroupID, claim.TurnID, ListOptions{Limit: 1})
			if err != nil || view.Run.State != domain.TurnStateUnknown || view.Receipt == nil || view.Receipt.Outcome != domain.TurnStateUnknown ||
				len(view.Timeline.Items) != 1 || view.Timeline.NextCursor == nil {
				t.Fatalf("expected an unknown Run with committed dispatch evidence: %+v, %v", view, err)
			}
			entry, d := view.Timeline.Items[0], receipt.Decision
			if entry.Kind != "dispatch_decision" || entry.ID != runDecisionPrefix+d.ID || entry.CreatedAt != d.CreatedAt ||
				entry.DispatchDecision == nil || *entry.DispatchDecision != (AgentRunDispatchDecision{
				DecisionID: d.ID, RequestID: request.ID, TaskID: request.TaskID, Kind: kind, WorkerMemberID: input.WorkerMemberID,
				AssignmentID: d.AssignmentID, ReasonPreview: preview, Truncated: preview != input.Reason,
			}) {
				t.Fatalf("lost exact dispatch receipt: %+v (%+v)", entry, entry.DispatchDecision)
			}
			data, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{claim.Token, key, "unseen-reason-tail", `"coordinator_turn_id"`, `"delivery_id"`} {
				if strings.Contains(string(data), forbidden) {
					t.Fatalf("dispatch decision exposed %q", forbidden)
				}
			}
			if kind == domain.CoordinatorDecisionKindDecline && (strings.Contains(string(data), `"worker_member_id"`) || strings.Contains(string(data), `"assignment_id"`)) {
				t.Fatalf("decline did not omit assignment fields: %s", data)
			}
			tail, err := s.GetAgentRun(ctx, request.GroupID, claim.TurnID, ListOptions{Before: view.Timeline.NextCursor})
			if err != nil || len(tail.Timeline.Items) != 1 || tail.Timeline.Items[0].Kind != "trigger_message" || tail.Timeline.NextCursor != nil {
				t.Fatalf("dispatch cursor was not exclusive: %+v, %v", tail, err)
			}

			// Damage only the frozen receipt's source message in this test DB.
			other, err := s.CreateMessage(ctx, MessageRequest{GroupID: request.GroupID, TopicID: request.TopicID, ActorID: "user", Content: "不相关消息"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			coordinationDecideExec(t, s, "DROP TRIGGER coordination_dispatch_state_update")
			coordinationDecideExec(t, s, "UPDATE coordination_dispatch_state SET message_id=? WHERE request_id=?", other.ID, request.ID)
			view, err = s.GetAgentRun(ctx, request.GroupID, claim.TurnID, ListOptions{})
			if err != nil || len(view.Timeline.Items) != 1 || view.Timeline.Items[0].Kind != "trigger_message" {
				t.Fatalf("decision escaped exact dispatch provenance: %+v, %v", view, err)
			}
		})
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

func TestAgentRunStoreToolEvidenceStaysOnExactRun(t *testing.T) {
	f, request, decision, scope := assignmentPending(t)
	s := f.s
	type evidence struct {
		scope                    CoordinationToolScope
		submissionID, decisionID string
	}
	record := func(scope CoordinationToolScope, decision domain.DecisionReceipt) evidence {
		t.Helper()
		claim := scope.Member.Claim
		activation := taskAttemptInput(createTestTask(t, s, claim.GroupID))
		activation.ActorID, activation.MemberScope = scope.Member.MemberID, &scope.Member
		attempt, err := s.ActivateTaskAttempt(ctx, activation)
		if err != nil {
			t.Fatal(err)
		}
		input := taskSubmissionInput(attempt)
		input.ActorID, input.MemberScope, input.IdempotencyKey = scope.Member.MemberID, &scope.Member, "run-log-"+attempt.ID
		sub, err := s.SubmitTask(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		// Release the member and fixture resource without removing the committed evidence.
		if turn, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); err != nil || turn.State != domain.TurnStateSucceeded {
			t.Fatalf("settle fixture Turn %s: %+v, %v", claim.TurnID, turn, err)
		}
		return evidence{scope, sub.ID, decision.Decision.ID}
	}
	target := record(scope, decision)
	compete := func(groupID, coordinator, worker string) evidence {
		t.Helper()
		_, decision, scope := assignmentNew(t, s, groupID, coordinator, worker, request.TopicID, "Competing work", "Verifiable result", "")
		return record(scope, decision)
	}
	sameMember := compete(request.GroupID, f.coord, f.worker)
	otherMember := member(t, s, request.GroupID).ID
	if _, err := s.SetCoordinator(ctx, request.GroupID, otherMember, request.BindingRevision, "run-log-other-coordinator"); err != nil {
		t.Fatal(err)
	}
	otherCoordinator := compete(request.GroupID, otherMember, f.worker)
	foreignGroup, foreignMember, foreignWorker := dispatchConfiguredGroup(t, s)
	foreign := compete(foreignGroup, foreignMember, foreignWorker)

	// A user Attempt may reference the same Turn under the schema, but its submission is not the member's evidence.
	manual, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(createTestTask(t, s, request.GroupID)))
	if err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, s, "DROP TRIGGER task_attempts_no_update")
	coordinationDecideExec(t, s, "UPDATE task_attempts SET turn_id=? WHERE id=?", scope.Member.Claim.TurnID, manual.ID)
	if _, err := s.SubmitTask(ctx, taskSubmissionInput(manual)); err != nil {
		t.Fatal(err)
	}

	check := func(run evidence) {
		t.Helper()
		claim := run.scope.Member.Claim
		view, err := s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, ListOptions{})
		if err != nil || view.Run.RunID != claim.TurnID || view.Run.GroupID != claim.GroupID ||
			view.Run.MemberID != run.scope.Member.MemberID || view.Timeline.NextCursor != nil {
			t.Fatalf("read exact Run %s: %+v, %v", claim.TurnID, view, err)
		}
		want := []string{runMessagePrefix + run.scope.Provenance.MessageID, runSubmissionPrefix + run.submissionID}
		if run.decisionID != "" {
			want = append(want, runDecisionPrefix+run.decisionID)
		}
		got := make([]string, 0, len(view.Timeline.Items))
		for _, entry := range view.Timeline.Items {
			got = append(got, entry.ID)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("Run %s evidence IDs = %v; want %v", claim.TurnID, got, want)
		}
	}
	for _, run := range []evidence{target, sameMember, otherCoordinator, foreign} {
		check(run)
	}
	if _, err := s.GetAgentRun(ctx, foreignGroup, scope.Member.Claim.TurnID, ListOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("target Turn selected from another group: %v", err)
	}

	// Damage only the frozen request coordinator. The genuine decision still names this Turn and delivery.
	coordinationDecideExec(t, s, "DROP TRIGGER coordination_dispatch_requests_no_update")
	coordinationDecideExec(t, s, "UPDATE coordination_dispatch_requests SET coordinator_member_id=? WHERE id=?", otherMember, request.ID)
	dispatchAssertFK(t, s)
	target.decisionID = ""
	check(target)
}

func TestAgentRunStoreTimelinePaginatesEqualTimestamps(t *testing.T) {
	f, scope, _, _, input := taskSubmissionAgent(t)
	s, claim := f.s, scope.Claim
	sub, err := s.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var triggerID string
	if err := s.db.QueryRow("SELECT message_id FROM deliveries WHERE group_id=? AND turn_id=?", claim.GroupID, claim.TurnID).Scan(&triggerID); err != nil {
		t.Fatal(err)
	}
	messageIDs := []string{triggerID}
	for _, key := range []string{"run-log-tie-1", "run-log-tie-2", "run-log-tie-3"} {
		msg, err := s.SendMessageForTool(ctx, scope, MemberToolMessageInput{Content: "Tied publication"}, key)
		if err != nil {
			t.Fatal(err)
		}
		messageIDs = append(messageIDs, msg.ID)
	}

	// Tie the messages' display timestamps to the immutable submission, without changing provenance.
	stamp := sub.CreatedAt.Format(timestampLayout)
	want := []string{runSubmissionPrefix + sub.ID}
	for _, id := range messageIDs {
		coordinationDecideExec(t, s, "UPDATE messages SET created_at=? WHERE group_id=? AND id=?", stamp, claim.GroupID, id)
		want = append(want, runMessagePrefix+id)
	}
	slices.Sort(want)
	slices.Reverse(want)

	seen := make(map[string]bool, len(want))
	options := ListOptions{Limit: 1}
	for i, id := range want {
		view, err := s.GetAgentRun(ctx, claim.GroupID, claim.TurnID, options)
		page := view.Timeline
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("page %d: want one event (%s), got %+v, %v", i+1, id, page, err)
		}
		entry, next := page.Items[0], page.NextCursor
		if entry.ID != id || !entry.CreatedAt.Equal(sub.CreatedAt) || seen[entry.ID] {
			t.Fatalf("page %d: want distinct event %s at %s, got %+v", i+1, id, stamp, entry)
		}
		seen[entry.ID] = true
		if i+1 < len(want) {
			if next == nil || next.ID != id || !next.CreatedAt.Equal(sub.CreatedAt) {
				t.Fatalf("page %d: cursor = %+v; want %s at %s", i+1, next, id, stamp)
			}
		} else if next != nil {
			t.Fatalf("unexpected cursor after the last event: %+v", next)
		}
		options.Before = next
	}
}
