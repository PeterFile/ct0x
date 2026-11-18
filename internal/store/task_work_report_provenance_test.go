package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestB3DelegationReprovesAuthorSource(t *testing.T) {
	f, original, child, _ := workReportOwner(t, 3)
	s, p := f.s, original.Provenance
	terminal := queueReceipt(provider.OutcomeSucceeded)
	assessor, candidate := newWorkInputCandidate(t, f, "Keep the original format.")
	queueEvent(t, s, assessor.Claim, provider.AcceptedEvent, "", "")
	followup, err := s.ApplyChatAssessment(ctx, assessor, candidate.ID, domain.ChatAssessment{
		Disposition: domain.ChatDispositionContinueWork, Reason: "Clarify the original task.",
	}, "Recorded the clarification.", "author-input")
	if err != nil || followup.WorkDeliveryID == "" || followup.TaskID != p.TaskID {
		t.Fatalf("expected a real followup to the original task: %+v %v", followup, err)
	}
	incoming := queueTurn(t, s, assessor.Claim)
	taskWorkTestReceipt(t, s, assessor, terminal, domain.TurnStateSucceeded)
	taskWorkTestReceipt(t, s, original.Member, terminal, domain.TurnStateSucceeded)
	wake, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, followup.WorkDeliveryID)
	if err != nil || wake.Blocked != "" || wake.SourceID != followup.WorkInputID || wake.SourceKind != domain.TaskWorkSourceFollowup {
		t.Fatalf("expected ready followup: %+v %v", wake, err)
	}
	author := taskWorkTestClaim(t, s, wake)
	attempt, err := s.BindTaskWorkContinuation(ctx, author)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcknowledgeWorkInputWithKey(ctx, author, followup.WorkInputID, "ack-author-input"); err != nil {
		t.Fatal(err)
	}
	queueEvent(t, s, author.Claim, provider.AcceptedEvent, "", "")
	in := domain.DelegateWorkInput{MemberID: child.ID, Question: "Check this source.", AllowTopicMessages: true}
	first, err := s.DelegateWork(ctx, author, in, "followup-delegation")
	if err != nil || first.TaskID != p.TaskID || first.AssignmentID != p.AssignmentID || first.AuthorAttemptID != attempt.ID ||
		first.AuthorTurnID != author.Claim.TurnID || first.AuthorDeliveryID != wake.DeliveryID {
		t.Fatalf("expected a delegation from the exact continuation: %+v %v", first, err)
	}
	childWake, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, first.DeliveryID)
	if err != nil || childWake.Blocked != "" {
		t.Fatalf("valid delegation was not ready: %+v %v", childWake, err)
	}
	if _, err := s.RecoverInFlight(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetTaskWorkDelegation(ctx, p.GroupID, first.ID); err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("recovery changed historical author evidence: %+v %v", got, err)
	}
	taskWorkTestPeek(t, s, childWake, "prior_turn_unsettled")
	author.Claim.Token = queueTurn(t, s, author.Claim).ClaimToken
	taskWorkTestReceipt(t, s, author, terminal, domain.TurnStateSucceeded)
	taskWorkTestPeek(t, s, childWake, "")

	// Break only the followup assessor's incoming delivery, not the author or child wake.
	const setBacklink = `UPDATE deliveries SET turn_id=? WHERE id=?`
	coordinationDecideExec(t, s, setBacklink, "", incoming.DeliveryID)
	if _, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, wake.DeliveryID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("fault did not break the exact B1 followup proof: %v", err)
	}
	if got, err := s.GetTaskWorkDelegation(ctx, p.GroupID, first.ID); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
		t.Errorf("delegation preview ignored damaged author source: %+v %v", got, err)
	}
	if got, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, childWake.DeliveryID); !errors.Is(err, ErrMemberToolDenied) || got.DeliveryID != "" {
		t.Errorf("delegation readiness ignored damaged author source: %+v %v", got, err)
	}
	coordinationDecideExec(t, s, setBacklink, incoming.ID, incoming.DeliveryID)
	worker := taskWorkTestClaim(t, s, childWake)
	queueEvent(t, s, worker.Claim, provider.AcceptedEvent, "", "")
	if _, err := s.ReadTaskWorkChannel(ctx, worker, ListOptions{}); err != nil {
		t.Fatalf("valid child could not read the grant: %v", err)
	}
	progress := domain.ReportWorkInput{Kind: domain.TaskWorkReportProgress, Summary: "Checked the format."}
	recordWorkReportProgress(t, s, worker, first, progress, "progress")
	coordinationDecideExec(t, s, setBacklink, "", incoming.DeliveryID)
	t.Run("live-child", func(t *testing.T) {
		before := messageSequenceSnapshot(t, s)
		if _, err := s.GetTaskWorkExecution(ctx, worker); !errors.Is(err, ErrMemberToolDenied) {
			t.Errorf("postclaim execution ignored damaged author source: %v", err)
		}
		if _, err := s.ReadTaskWorkChannel(ctx, worker, ListOptions{}); !errors.Is(err, ErrMemberToolDenied) {
			t.Errorf("child read through damaged author source: %v", err)
		}
		for _, key := range []string{"progress", "new-progress"} {
			if got, err := s.ReportWork(ctx, worker, progress, key); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
				t.Errorf("report %q ignored damaged author source: %+v %v", key, got, err)
			}
		}
		messageSequencePreserved(t, s, before)
	})
	coordinationDecideExec(t, s, setBacklink, incoming.ID, incoming.DeliveryID)

	// A later delegation can be authored by a report continuation, but does not
	// lose the earlier delegation's exact source proof.
	report, err := s.ReportWork(ctx, worker, domain.ReportWorkInput{Kind: domain.TaskWorkReportDelivered, Summary: "Format verified."}, "delivered")
	if err != nil {
		t.Fatal(err)
	}
	taskWorkTestReceipt(t, s, worker, terminal, domain.TurnStateSucceeded)
	nextWake, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, report.DeliveryID)
	if err != nil || nextWake.Blocked != "" {
		t.Fatalf("expected ready report wake: %+v %v", nextWake, err)
	}
	next := taskWorkTestClaim(t, s, nextWake)
	if _, err := s.BindTaskWorkContinuation(ctx, next); err != nil {
		t.Fatal(err)
	}
	queueEvent(t, s, next.Claim, provider.AcceptedEvent, "", "")
	second, err := s.DelegateWork(ctx, next, in, "report-delegation")
	if err != nil || second.Ordinal != first.Ordinal+1 || second.AuthorDeliveryID != report.DeliveryID {
		t.Fatalf("expected a delegation from the exact earlier report: %+v %v", second, err)
	}
	if got, err := s.ResolveTaskWorkDelivery(ctx, p.GroupID, second.DeliveryID); err != nil || got.Blocked != "" {
		t.Fatalf("valid report continuation could not delegate: %+v %v", got, err)
	}
	coordinationDecideExec(t, s, setBacklink, "", incoming.DeliveryID)
	before := messageSequenceSnapshot(t, s)
	if got, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: p.GroupID, DeliveryID: second.DeliveryID, MaxActive: 8}); !errors.Is(err, ErrMemberToolDenied) || got.ID != "" {
		t.Errorf("claim ignored the earlier damaged author source: turn=%q, err=%v", got.ID, err)
	}
	messageSequencePreserved(t, s, before)

	coordinationDecideExec(t, s, setBacklink, incoming.ID, incoming.DeliveryID)
	// Keep the report and its author binding intact but reverse the lineage order.
	coordinationDecideExec(t, s, `DROP TRIGGER task_work_delegations_no_update`)
	coordinationDecideExec(t, s, `UPDATE task_work_delegations SET ordinal=3 WHERE id=?`, first.ID)
	if _, err := s.GetTaskWorkDelegation(ctx, p.GroupID, second.ID); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("followed a report from a later delegation: %v", err)
	}
}

func TestB3WorkReportNativeReplyEvidence(t *testing.T) {
	f, owner, child, _ := workReportOwner(t, 2)
	s, p := f.s, owner.Provenance
	start := func(key string) (domain.TaskWorkDelegation, MemberToolScope) {
		t.Helper()
		d, err := s.DelegateWork(ctx, owner.Member, domain.DelegateWorkInput{MemberID: child.ID, Question: "Check the first point."}, key)
		if err != nil {
			t.Fatal(err)
		}
		return d, taskWorkTestClaim(t, s, domain.TaskWorkDelivery{GroupID: p.GroupID, DeliveryID: d.DeliveryID})
	}
	_, worker := start("native-child")
	question := domain.ReportWorkInput{Kind: domain.TaskWorkReportQuestion, Summary: "Is the first point in scope?"}
	r, err := s.ReportWork(ctx, worker, question, "question")
	if err != nil || r.DeliveryID == "" || r.SourceReplyMessageID != "" {
		t.Fatalf("manual report invented a native reply: %+v %v", r, err)
	}
	wake, err := s.GetMessage(ctx, p.GroupID, r.MessageID)
	if err != nil || wake.SourceKind != "platform" || wake.SenderID != worker.MemberID || wake.TopicID != p.TopicID {
		t.Fatalf("expected a same-scope platform wake: %+v %v", wake, err)
	}
	// Use identical text so neither sender/scope nor content can distinguish these messages.
	command, err := s.SendMessageForTool(ctx, worker, MemberToolMessageInput{Content: wake.Content}, "member-message")
	if err != nil || command.SourceKind != "member" || len(command.Deliveries) != 0 {
		t.Fatalf("expected a public-only member command: %+v %v", command, err)
	}
	queueCandidate(t, s, worker.Claim, wake.Content, provider.OutcomeSucceeded)
	taskWorkTestReceipt(t, s, worker, queueReceipt(provider.OutcomeSucceeded), domain.TurnStateSucceeded)
	turn := queueTurn(t, s, worker.Claim)
	reply, err := s.GetMessage(ctx, p.GroupID, turn.ReplyMessageID)
	if err != nil || reply.ID == wake.ID || reply.SourceKind != "member" || reply.SourceID != "turn:"+turn.ID || reply.Content != wake.Content || len(reply.Deliveries) != 0 {
		t.Fatalf("expected actual native response from the child Turn: %+v %v", reply, err)
	}
	manual, worker2 := start("manual-child")
	progress := recordWorkReportProgress(t, s, worker2, manual, domain.ReportWorkInput{Kind: domain.TaskWorkReportProgress, Summary: "Manual progress."}, "manual-progress")
	taskWorkTestReceipt(t, s, worker2, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}, domain.TurnStateNotStarted)
	want := r
	want.SourceReplyMessageID = reply.ID
	if got, err := s.GetTaskWorkReport(ctx, p.GroupID, r.ID); err != nil || got != want {
		t.Fatalf("valid native reply not linked: %+v %v", got, err)
	}
	if page, err := s.ListTaskWorkReports(ctx, p.GroupID, p.TaskID, ListOptions{}); err != nil || !reflect.DeepEqual(page.Items, []domain.TaskWorkReport{progress, want}) {
		t.Fatalf("expected both native and no-reply reports: %+v %v", page, err)
	}
	settlement := queueReceiptJSON(t, s, worker.Claim)
	for _, bad := range []struct{ name, pointer, receipt string }{
		{"platform-wake", wake.ID, settlement},
		{"member-command", command.ID, settlement},
		{"missing-message", "missing-reply", settlement},
		{"missing-terminal-proof", reply.ID, "{}"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			coordinationDecideExec(t, s, `UPDATE turns SET reply_message_id=?,receipt_json=? WHERE id=?`, bad.pointer, bad.receipt, turn.ID)
			if got, err := s.GetTaskWorkReport(ctx, p.GroupID, r.ID); !errors.Is(err, ErrConflict) || got.SourceReplyMessageID != "" {
				t.Errorf("exact lookup accepted damaged reply evidence: %+v %v", got, err)
			}
			if got, err := s.GetWorkReportForMember(ctx, owner.Member, r.ID); !errors.Is(err, ErrConflict) || got.SourceReplyMessageID != "" {
				t.Errorf("scoped lookup accepted damaged reply evidence: %+v %v", got, err)
			}
			if page, err := s.ListTaskWorkReports(ctx, p.GroupID, p.TaskID, ListOptions{}); err != nil || !reflect.DeepEqual(page.Items, []domain.TaskWorkReport{progress, r}) {
				t.Errorf("history lost report facts or exposed the damaged reply: %+v %v", page, err)
			}
		})
	}
}
