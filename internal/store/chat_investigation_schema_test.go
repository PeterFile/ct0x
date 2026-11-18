package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestChatInvestigationSQLBindsCapturedSourceAndDispatch(t *testing.T) {
	for _, source := range []string{"local-command", "native-reply"} {
		t.Run(source, func(t *testing.T) {
			f := chatSetup(t, "user", false)
			policy := investigationPolicy(t, f, domain.ChatInvestigationPolicy{Objective: investigationObjective, AllowTopicMessages: true}, 1)
			priorCandidate := f.candidate.ID
			cases := []string{"wrong-command", "binding", "dispatch-policy", "task", "mandate", "dispatch"}
			if source == "local-command" {
				chatNextCandidate(t, &f, true)
			} else {
				claim := f.scope.Claim
				queueCandidate(t, f.s, claim, "建议调查这些未决问题。", provider.OutcomeSucceeded)
				turn, err := f.s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded))
				if err != nil || turn.ReplyMessageID == "" {
					t.Fatalf("no native reply: %+v %v", turn, err)
				}
				// Native replies are public-only; explicitly queue the existing reply for this fixture.
				conversation, err := f.s.GetOrCreateConversation(ctx, f.groupID, f.b, f.scope.TopicID)
				if err != nil {
					t.Fatal(err)
				}
				coordinationDecideExec(t, f.s, `INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
				 SELECT 'native-reply',id,?,?,group_id,topic_id,created_at FROM messages WHERE id=?`, conversation.ID, f.b, turn.ReplyMessageID)
				investigationBindCandidate(t, &f, domain.Delivery{ID: "native-reply"})
				if f.candidate.SourceKind != "member" || f.candidate.SourceID != "turn:"+turn.ID {
					t.Fatalf("fixture did not capture native source: %+v", f.candidate)
				}
				cases = []string{"wrong-turn", "platform-notice"}
			}
			other := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: "another-topic", ActorID: "user", Content: "另一话题的命令。"}, nil)
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			receipt, decision := investigationPendingSQL(t, tx, f.candidate)
			insert := func(t *testing.T, r domain.ChatAssessmentReceipt) error {
				t.Helper()
				data, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				_, err = tx.Exec(`INSERT INTO chat_assessments VALUES(?,?,?,?,?,?,?)`, r.ID, r.CandidateID, decision,
					receipt.DispatchRequestID, r.NoticeMessageID, []byte("guard fixture"), string(data))
				return err
			}
			for _, scenario := range cases {
				t.Run(scenario, func(t *testing.T) {
					exec := func(query string, args ...any) {
						t.Helper()
						if _, err := tx.Exec(query, args...); err != nil {
							t.Fatal(err)
						}
					}
					exec("SAVEPOINT guard_case")
					defer func() { exec("ROLLBACK TO guard_case"); exec("RELEASE guard_case") }()
					r := receipt
					switch scenario {
					case "wrong-command", "wrong-turn":
						r.SourceID = other.SourceID
						if scenario == "wrong-turn" {
							r.SourceID = "turn:" + r.TurnID
						}
						// Simulate bad captured evidence, not just a mismatch with the receipt.
						exec("DROP TRIGGER chat_intake_executions_update")
						exec("UPDATE chat_intake_executions SET source_id=? WHERE candidate_id=?", r.SourceID, r.CandidateID)
					case "platform-notice":
						exec(`INSERT INTO chat_assessments(id,candidate_id,notice_message_id,payload,receipt_json)
						 VALUES('platform',?,?,x'00','{}')`, priorCandidate, r.MessageID)
					case "binding", "dispatch-policy":
						exec("DROP TRIGGER chat_candidates_no_update")
						if scenario == "binding" {
							exec("UPDATE chat_candidates SET binding_revision=0 WHERE id=?", r.CandidateID)
						} else {
							exec("UPDATE chat_candidates SET dispatch_policy_revision=0 WHERE id=?", r.CandidateID)
						}
					case "task":
						r.TaskID = "other-task"
					case "mandate":
						r.MandateID = "other-mandate"
					case "dispatch":
						r.DispatchRequestID = "other-dispatch"
					}
					if err := insert(t, r); err == nil || !strings.Contains(err.Error(), "只读调查必须绑定当前本机用户策略和真实来源") {
						t.Fatalf("expected investigation guard rejection, got %v", err)
					}
				})
			}
			if receipt.PolicyRevision != policy.Revision {
				t.Fatal("fixture did not capture current policy")
			}
			if err := insert(t, receipt); err != nil {
				t.Fatalf("guard rejected real %s evidence: %v", source, err)
			}
		})
	}
}

func investigationPendingSQL(t *testing.T, tx *sql.Tx, c domain.ChatCandidate) (domain.ChatAssessmentReceipt, string) {
	t.Helper()
	trigger, err := recordLocalMessageTriggerInTx(ctx, tx, c.ThreadRef, c.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	const decision = "investigation-decision"
	title, description, acceptance := chatInvestigationTask(c)
	if err := admitWorkInTx(ctx, tx, trigger, domain.RequestWorkRequest{Mode: domain.RoutingDecisionCreateNew, Reason: "只读调查",
		NewTask: &domain.NewTaskRequest{Title: title, Description: description, Acceptance: acceptance}}, decision); err != nil {
		t.Fatal(err)
	}
	admitted, err := admissionReceiptInTx(ctx, tx, c.GroupID, decision)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := coordinationDispatchCreateInTx(ctx, tx, c.GroupID, admitted.Task.ID, admitted.Task.RevisionAtAdmission, 1, "只读调查")
	if err != nil {
		t.Fatal(err)
	}
	var authorization string
	if err := tx.QueryRow(`SELECT message_id FROM message_command_sources WHERE id=?`, c.Policy.ConfigurationSourceID).Scan(&authorization); err != nil {
		t.Fatal(err)
	}
	notice, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: c.GroupID, TopicID: c.TopicID, ActorID: c.MemberID, Content: "调查行动说明。"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return domain.ChatAssessmentReceipt{ID: "investigation-assessment", CandidateID: c.ID, ThreadRef: c.ThreadRef,
		MessageID: c.MessageID, SourceSenderID: c.SourceSenderID, AssessedBy: c.MemberID, TurnID: c.TurnID,
		PolicyRevision: c.Policy.Revision, CandidateGeneration: c.Generation, Disposition: "prepare_work", Authority: "written_preauthorization",
		AuthorizationMessageID: authorization, Action: domain.ChatActionReadOnlyInvestigation, TaskID: admitted.Task.ID, MandateID: admitted.Mandate.ID,
		DispatchRequestID: dispatch.ID, NoticeMessageID: notice.ID, Purpose: c.Purpose, SourceID: c.SourceID, SourceKind: c.SourceKind,
		Outcome: "admitted", AuthorityEvidenceVerified: true, CreatedAt: notice.CreatedAt}, decision
}
