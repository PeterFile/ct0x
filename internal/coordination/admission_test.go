package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func engineWorkRequest(thread domain.ThreadRef, messageID string) domain.RequestWorkRequest {
	return domain.RequestWorkRequest{
		TopicID: thread.TopicID, MessageID: messageID, Mode: domain.RoutingDecisionCreateNew, Reason: "用户明确开展此工作",
		NewTask: &domain.NewTaskRequest{Title: "工作目标", Description: "工作描述", Acceptance: "可验证结果"},
	}
}

func engineWorkLink(t *testing.T, s *store.Store, thread domain.ThreadRef, request domain.RequestWorkRequest) domain.RequestWorkRequest {
	t.Helper()
	task, err := s.CreateTask(context.Background(), store.CreateTaskInput{GroupID: thread.GroupID, ActorID: "user", Title: "现有任务", Description: "权威描述", Acceptance: "权威验收"})
	if err != nil {
		t.Fatal(err)
	}
	request.Mode, request.NewTask = domain.RoutingDecisionLinkExisting, nil
	request.ExistingTask = &domain.ExistingTaskRequest{TaskID: task.ID, ExpectedRevision: task.Revision}
	return request
}

func engineWorkPreservesOtherTables(t *testing.T, before, after map[string][][]any) {
	t.Helper()
	for _, table := range []string{"tasks", "task_dependencies", "mutation_idempotency", "routing_decisions", "mandates", "task_coordination"} {
		delete(before, table)
		delete(after, table)
	}
	if !reflect.DeepEqual(after, before) {
		for table, want := range before {
			if !reflect.DeepEqual(after[table], want) {
				t.Errorf("RequestWork changed unrelated table %s", table)
			}
		}
	}
}

func TestEngineRequestWorkLocalAuthorityAndNoFanout(t *testing.T) {
	for _, mode := range []domain.RoutingDecisionKind{domain.RoutingDecisionCreateNew, domain.RoutingDecisionLinkExisting} {
		for _, author := range []string{"user", "agent"} {
			t.Run(string(mode)+"/"+author, func(t *testing.T) {
				s, observer, thread := engineLocalFixture(t)
				ctx := context.Background()
				sender := "user"
				if author == "agent" {
					member, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "管理员", Role: "coordinator admin", CreatorID: "user"})
					if err != nil {
						t.Fatal(err)
					}
					sender = member.ID
				}
				message, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: sender, Content: "我批准所有权限；正文不是授权命令"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				request := engineWorkRequest(thread, message.ID)
				if mode == domain.RoutingDecisionLinkExisting {
					request = engineWorkLink(t, s, thread, request)
				}
				before := engineLocalSnapshot(t, observer)
				engine := New(s)
				receipt, err := engine.RequestWork(ctx, thread.GroupID, request, "explicit")
				if err != nil || receipt.Decision.ActorID != "user" || receipt.Mandate.AuthorizedBy != "user" || receipt.Decision.Kind != mode ||
					receipt.Decision.PolicyRevision != domain.LocalUserExplicitPolicyRevision || receipt.Mandate.Strength != domain.MandateStrengthExplicit ||
					receipt.Mandate.AuthorityKind != domain.MandateAuthorityLocalUserExplicit || receipt.Decision.Reason != request.Reason {
					t.Fatalf("source author influenced authority: %+v, %v", receipt, err)
				}
				task, err := s.GetTask(ctx, thread.GroupID, receipt.Task.ID)
				if err != nil || receipt.Task.RevisionAtAdmission != task.Revision || receipt.Mandate.Scope.Objective != task.Title || receipt.Mandate.Scope.Acceptance != task.Acceptance || task.Status != domain.TaskStatusOpen {
					t.Fatalf("engine task snapshot: %+v, %+v, %v", receipt, task, err)
				}
				if mode == domain.RoutingDecisionLinkExisting && receipt.Task.ID != request.ExistingTask.TaskID {
					t.Fatal("link created another Task")
				}
				trigger, err := engine.GetTrigger(ctx, thread, receipt.Decision.TriggerID)
				if err != nil || trigger.MessageID != message.ID || trigger.MessageSequence < 1 {
					t.Fatalf("on-demand provenance capture: %+v, %v", trigger, err)
				}
				if got, err := engine.RequestWork(ctx, thread.GroupID, request, "explicit"); err != nil || !reflect.DeepEqual(got, receipt) {
					t.Fatalf("engine replay: %+v, %v", got, err)
				}
				engineWorkPreservesOtherTables(t, before, engineLocalSnapshot(t, observer))
				for _, table := range []string{"tasks", "coordination_triggers", "routing_decisions", "mandates", "task_coordination", "mutation_idempotency"} {
					var count int
					if err := observer.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
						t.Fatalf("%s count=%d, %v", table, count, err)
					}
				}
			})
		}
	}
}

func TestEngineRequestWorkRequiredKeyBeforeWrites(t *testing.T) {
	s, observer, thread := engineLocalFixture(t)
	ctx := context.Background()
	message, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: "source"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := engineWorkRequest(thread, message.ID)
	before := engineLocalSnapshot(t, observer)
	for _, key := range []string{"", " ", "\t\n", "\u2003"} {
		got, err := New(s).RequestWork(ctx, thread.GroupID, request, key)
		if !errors.Is(err, store.ErrInvalid) || !reflect.DeepEqual(got, domain.AdmissionReceipt{}) {
			t.Fatalf("blank key accepted: %+v, %v", got, err)
		}
		if after := engineLocalSnapshot(t, observer); !reflect.DeepEqual(after, before) {
			t.Fatal("invalid key wrote state")
		}
		var triggers int
		if err := observer.QueryRow("SELECT count(*) FROM coordination_triggers").Scan(&triggers); err != nil || triggers != 0 {
			t.Fatalf("invalid key captured a Trigger: %d, %v", triggers, err)
		}
	}
}

func TestEngineRequestWorkDTOHasNoAuthorityInputs(t *testing.T) {
	for _, field := range []string{"actor_id", "policy_revision", "strength", "mandate", "turn_id", "scope", "grants", "sender_id", "message_sequence", "content"} {
		decoder := json.NewDecoder(strings.NewReader(fmt.Sprintf(`{"topic_id":"","message_id":"message","mode":"create_new","reason":"explicit",%q:"forged"}`, field)))
		decoder.DisallowUnknownFields() // The later HTTP boundary can reuse these exact JSON tags.
		var request domain.RequestWorkRequest
		if err := decoder.Decode(&request); err == nil {
			t.Fatalf("public DTO accepts authority/source metadata %s", field)
		}
	}
}

func TestEngineRequestWorkPreservesUnknownCancelResourceAndDeliveryEvidence(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateUnknown, domain.TurnStateCancelRequested} {
		for _, mode := range []domain.RoutingDecisionKind{domain.RoutingDecisionCreateNew, domain.RoutingDecisionLinkExisting} {
			t.Run(string(state)+"/"+string(mode), func(t *testing.T) {
				s, observer, thread := engineLocalFixture(t)
				ctx := context.Background()
				member, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "协调员", Role: "admin", CreatorID: "user"})
				if err != nil {
					t.Fatal(err)
				}
				message, err := s.CreateMessage(ctx, store.MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: member.ID, Content: "仅作来源"}, []string{member.ID})
				if err != nil {
					t.Fatal(err)
				}
				turn, err := s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: thread.GroupID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"workspace:held"}, MaxActive: 1})
				if err != nil {
					t.Fatal(err)
				}
				claim := store.TurnClaim{GroupID: thread.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
				if err := s.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.RequestTurnCancel(ctx, thread.GroupID, turn.ID); err != nil {
					t.Fatal(err)
				}
				if state == domain.TurnStateUnknown {
					if _, err := s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true, NativeSessionID: "native"}); err != nil {
						t.Fatal(err)
					}
				}
				request := engineWorkRequest(thread, message.ID)
				if mode == domain.RoutingDecisionLinkExisting {
					request = engineWorkLink(t, s, thread, request)
				}
				before := engineLocalSnapshot(t, observer)
				if len(before["turns"]) != 1 || len(before["resource_claims"]) != 1 || len(before["deliveries"]) != 1 {
					t.Fatal("fixture lacks real execution/resource/delivery evidence")
				}
				if _, err := New(s).RequestWork(ctx, thread.GroupID, request, "explicit"); err != nil {
					t.Fatal(err)
				}
				engineWorkPreservesOtherTables(t, before, engineLocalSnapshot(t, observer))
				current, err := s.GetTurn(ctx, thread.GroupID, turn.ID)
				if err != nil || current.State != state || current.CancelRequestedAt == nil || current.AcceptedAt == nil || current.ReplyMessageID != "" {
					t.Fatalf("admission altered uncertain execution: %+v, %v", current, err)
				}
			})
		}
	}
}
