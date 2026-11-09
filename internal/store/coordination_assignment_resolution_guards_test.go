package store

import (
	"crypto/rand"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

const resolutionTurnInsert = `INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`

func resolutionOrdinaryTurn(groupID string, d domain.Delivery, id, state string) coordinatorSQL {
	return coordinatorSQL{resolutionTurnInsert, []any{id, groupID, d.ID, d.ConversationID, d.RecipientID, rand.Text(), state, dispatchStamp, dispatchStamp}}
}

func TestCoordinationAssignmentResolutionV14SourceTemporalOrder(t *testing.T) {
	for _, role := range []string{"coordinator", "worker"} {
		for _, mode := range []string{"empty-clean", "empty-existing", "correct-existing", "wrong-pointer", "correct-extra-terminal", "correct-extra-active"} {
			t.Run(role+"/"+mode, func(t *testing.T) {
				s, _ := resolutionStore(t)
				var g string
				var d domain.Delivery
				var write coordinatorSQL
				if role == "coordinator" {
					groupID, c, _ := dispatchConfiguredGroup(t, s)
					g = groupID
					r := dispatchCandidate(t, s, g, c, "source")
					coordinatorMustCommit(t, s, dispatchPair(r)...)
					receipt := dispatchReceipt(t, s, r)
					d = domain.Delivery{ID: receipt[11].(string), ConversationID: receipt[12].(string), RecipientID: c}
					write = dispatchUpdate(receipt)
				} else {
					r := resolutionCandidate(t, s, "assign")
					coordinatorMustCommit(t, s, resolutionStatements(r)...)
					receipt := assignmentReceipt(t, s, r.assignmentSQLRows)
					g = r.turn.GroupID
					d = domain.Delivery{ID: receipt[12].(string), ConversationID: receipt[13].(string), RecipientID: receipt[14].(string)}
					write = assignmentStateUpdate(receipt)
				}
				id := rand.Text()
				if mode != "empty-clean" {
					coordinatorMustCommit(t, s, resolutionOrdinaryTurn(g, d, id, "not_started"))
				}
				if strings.HasPrefix(mode, "correct") || mode == "wrong-pointer" {
					pointer := id
					if mode == "wrong-pointer" {
						pointer = rand.Text()
					}
					coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE deliveries SET turn_id=? WHERE id=?", []any{pointer, d.ID}})
				}
				if strings.HasPrefix(mode, "correct-extra") {
					state := "not_started"
					if mode == "correct-extra-active" {
						state = "unknown"
					}
					coordinatorMustCommit(t, s, resolutionOrdinaryTurn(g, d, rand.Text(), state))
				}
				if mode != "empty-clean" && mode != "correct-existing" {
					dispatchReject(t, s, "轮次来源", write)
					return
				}
				coordinatorMustCommit(t, s, write)
				if mode == "empty-clean" {
					// A raw SQL reservation may commit without a Turn. It is not a deferred binding guarantee.
					coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE deliveries SET turn_id=? WHERE id=?", []any{id, d.ID}})
					dispatchReject(t, s, "轮次来源", resolutionOrdinaryTurn(g, d, rand.Text(), "not_started"))
					coordinatorMustCommit(t, s, resolutionOrdinaryTurn(g, d, id, "not_started"))
				}
				for _, pointer := range []string{"", rand.Text()} {
					dispatchReject(t, s, "回指", coordinatorSQL{"UPDATE deliveries SET turn_id=? WHERE id=?", []any{pointer, d.ID}})
				}
				dispatchAssertFK(t, s)
			})
		}
	}
}

func TestCoordinationAssignmentResolutionV14TurnParentsAndReplaceVictims(t *testing.T) {
	for _, role := range []string{"coordinator", "worker"} {
		for _, recursive := range []int{0, 1} {
			for _, state := range []string{"starting", "running", "cancel_requested", "unknown", "not_started"} {
				t.Run(fmt.Sprintf("%s/recursive=%d/%s", role, recursive, state), func(t *testing.T) {
					s, _ := resolutionStore(t)
					r := resolutionCandidate(t, s, "assign")
					if role == "worker" {
						coordinatorMustCommit(t, s, resolutionStatements(r)...)
						r.state = assignmentReceipt(t, s, r.assignmentSQLRows)
						coordinatorMustCommit(t, s, assignmentStateUpdate(r.state))
						turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.turn.GroupID, DeliveryID: r.state[12].(string), MaxActive: 20})
						if err != nil {
							t.Fatal(err)
						}
						r.turn = turn
					}
					dispatchRecursive(t, s, recursive)
					var keys []string
					for _, index := range messageSequenceRows(t, s.db, `SELECT name FROM pragma_index_list('turns') WHERE "unique"=1`) {
						var columns []string
						for _, column := range messageSequenceRows(t, s.db, fmt.Sprintf("SELECT name FROM pragma_index_info('%s') ORDER BY seqno", index[0])) {
							columns = append(columns, column[0].(string))
						}
						keys = append(keys, strings.Join(columns, ","))
					}
					slices.Sort(keys)
					if !slices.Equal(keys, []string{"conversation_id", "id", "id,group_id", "id,group_id,member_id", "member_id"}) {
						t.Fatalf("uncovered Turn UNIQUE victims: %v", keys)
					}
					coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE turns SET state=? WHERE id=?", []any{state, r.turn.ID}})
					d := queueDelivery(t, s, r.turn.GroupID, r.turn.MemberID, "other-topic")
					ordinary := rand.Text()
					coordinatorMustCommit(t, s, resolutionOrdinaryTurn(r.turn.GroupID, d, ordinary, "not_started"))
					for _, change := range []coordinatorSQL{
						{"UPDATE turns SET delivery_id=?,conversation_id=? WHERE id=?", []any{d.ID, d.ConversationID, r.turn.ID}},
						{"UPDATE turns SET id=? WHERE id=?", []any{rand.Text(), r.turn.ID}},
						{"UPDATE turns SET group_id=? WHERE id=?", []any{rand.Text(), r.turn.ID}},
						{"UPDATE turns SET member_id=? WHERE id=?", []any{rand.Text(), r.turn.ID}},
						{"UPDATE turns SET conversation_id=? WHERE id=?", []any{d.ConversationID, r.turn.ID}},
						{"DELETE FROM turns WHERE id=?", []any{r.turn.ID}},
						// Ordinary -> role identity must have the exact reserved target id, not just the same member.
						{"UPDATE turns SET delivery_id=?,conversation_id=? WHERE id=?", []any{r.turn.DeliveryID, r.turn.ConversationID, ordinary}},
					} {
						dispatchReject(t, s, "轮次", change)
					}
					victim := resolutionOrdinaryTurn(r.turn.GroupID, d, r.turn.ID, "not_started")
					victim.query = strings.Replace(victim.query, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
					dispatchReject(t, s, "轮次不可替换", victim)
					dispatchReject(t, s, "轮次不可替换", coordinatorSQL{"UPDATE OR REPLACE turns SET id=? WHERE id=?", []any{r.turn.ID, ordinary}})
					if state != "not_started" {
						for _, collision := range []string{"member", "conversation"} {
							candidate := d
							if collision == "conversation" {
								candidate.ConversationID = r.turn.ConversationID
								// This attempted tuple is counterfactual: isolate the conversation victim guard before the FK.
								candidate.RecipientID = rand.Text()
							}
							insert := resolutionOrdinaryTurn(r.turn.GroupID, candidate, rand.Text(), "starting")
							insert.query = strings.Replace(insert.query, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
							dispatchReject(t, s, "轮次不可替换", insert)
							if collision == "conversation" {
								dispatchReject(t, s, "轮次不可替换", coordinatorSQL{"UPDATE OR REPLACE turns SET state='starting',conversation_id=?,member_id=? WHERE id=?", []any{candidate.ConversationID, candidate.RecipientID, ordinary}})
							}
						}
						dispatchReject(t, s, "轮次不可替换", coordinatorSQL{"UPDATE OR REPLACE turns SET state='starting' WHERE id=?", []any{ordinary}})
					}
					// Ordinary identity and terminal changes stay legal.
					coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE turns SET id=?,state='failed' WHERE id=?", []any{rand.Text(), ordinary}})
					dispatchAssertFK(t, s)
				})
			}
		}
	}
}

func TestCoordinationAssignmentResolutionV14RoleRuntimeAndWorkerBinding(t *testing.T) {
	s, _ := resolutionStore(t)
	r := resolutionCandidate(t, s, "assign")
	claim := TurnClaim{GroupID: r.turn.GroupID, TurnID: r.turn.ID, Token: r.turn.ClaimToken}
	if err := s.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-process-session"}); err != nil {
		t.Fatal(err)
	}
	queueCandidate(t, s, claim, "role candidate", provider.OutcomeSucceeded)
	if _, err := s.RequestTurnCancel(ctx, claim.GroupID, claim.TurnID); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, s)
	coordinatorMustCommit(t, s, resolutionStatements(r)...)
	for _, table := range []string{"coordination_decisions", "coordination_assignments", "coordination_assignment_dispatch", "coordination_dispatch_state"} {
		delete(before, table)
	}
	messageSequencePreserved(t, s, before)
	if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("role recovery: %d, %v", n, err)
	}
	turn, err := s.GetTurn(ctx, claim.GroupID, claim.TurnID)
	if err != nil || turn.State != domain.TurnStateUnknown || turn.ClaimToken == claim.Token {
		t.Fatalf("role recovery did not preserve unknown/fence token: %+v, %v", turn, err)
	}
	claim.Token = turn.ClaimToken
	if _, err := s.ApplyTurnReceipt(ctx, claim, queueReceipt(provider.OutcomeSucceeded)); err != nil {
		t.Fatal(err)
	}
	r.state = assignmentReceipt(t, s, r.assignmentSQLRows)
	coordinatorMustCommit(t, s, assignmentStateUpdate(r.state))
	binding := assignmentBinding(t, s, r.assignmentSQLRows)
	coordinatorMustCommit(t, s, coordinatorSQL{assignmentBindingInsert, binding})
	before = messageSequenceSnapshot(t, s)
	for _, query := range []string{"UPDATE coordination_assignment_attempts SET turn_id=turn_id", "DELETE FROM coordination_assignment_attempts"} {
		dispatchReject(t, s, "不可", coordinatorSQL{query, nil})
	}
	messageSequencePreserved(t, s, before)
	dispatchAssertFK(t, s)
}

func TestCoordinationAssignmentResolutionV14CrossRoleAndDeliveryVictims(t *testing.T) {
	s, _ := resolutionStore(t)
	a, b := resolutionCandidate(t, s, "assign"), resolutionCandidate(t, s, "decline")
	coordinatorMustCommit(t, s, resolutionStatements(a)...)
	// Cross-role rejection must run before the independent sender/recipient join guard.
	cross := slices.Clone(a.state)
	cross[4], cross[5] = int64(2), "enqueued"
	copy(cross[11:], a.request.state[10:])
	dispatchReject(t, s, "角色回执", assignmentStateUpdate(cross))
	a.state = assignmentReceipt(t, s, a.assignmentSQLRows)
	coordinatorMustCommit(t, s, assignmentStateUpdate(a.state))
	pending := dispatchCandidate(t, s, b.turn.GroupID, b.turn.MemberID, "reverse")
	coordinatorMustCommit(t, s, dispatchPair(pending)...)
	crossCoordinator := slices.Clone(pending.state)
	crossCoordinator[3], crossCoordinator[4] = int64(2), "enqueued"
	copy(crossCoordinator[10:], a.state[11:])
	dispatchReject(t, s, "角色回执", dispatchUpdate(crossCoordinator))
	for _, recursive := range []int{0, 1} {
		dispatchRecursive(t, s, recursive)
		for _, receipt := range [][]any{a.request.state[10:], a.state[11:]} {
			var d domain.Delivery
			var groupID, topicID string
			if err := s.db.QueryRow("SELECT id,message_id,group_id,topic_id,conversation_id,recipient_id FROM deliveries WHERE id=?", receipt[1]).Scan(&d.ID, &d.MessageID, &groupID, &topicID, &d.ConversationID, &d.RecipientID); err != nil {
				t.Fatal(err)
			}
			ordinary := queueDelivery(t, s, groupID, d.RecipientID, "ordinary-victim")
			for _, change := range []coordinatorSQL{
				{"DELETE FROM deliveries WHERE id=?", []any{d.ID}},
				{"UPDATE deliveries SET id=? WHERE id=?", []any{rand.Text(), d.ID}},
				{"UPDATE OR REPLACE deliveries SET id=? WHERE id=?", []any{d.ID, ordinary.ID}},
				{"UPDATE OR REPLACE deliveries SET message_id=? WHERE id=?", []any{d.MessageID, ordinary.ID}},
				{`INSERT OR REPLACE INTO deliveries(id,message_id,group_id,topic_id,conversation_id,recipient_id,created_at) VALUES(?,?,?,?,?,?,?)`, []any{rand.Text(), d.MessageID, groupID, topicID, d.ConversationID, d.RecipientID, dispatchStamp}},
				{`INSERT OR REPLACE INTO deliveries(id,message_id,group_id,topic_id,conversation_id,recipient_id,created_at) VALUES(?,?,?,?,?,?,?)`, []any{d.ID, ordinary.MessageID, groupID, "ordinary-victim", ordinary.ConversationID, ordinary.RecipientID, dispatchStamp}},
			} {
				dispatchReject(t, s, "投递", change)
			}
			coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE deliveries SET error='runtime diagnostic' WHERE id=?", []any{d.ID}})
		}
	}
	dispatchAssertFK(t, s)
}

func TestCoordinationAssignmentResolutionV14OwnershipDeferredWindow(t *testing.T) {
	s, _ := resolutionStore(t)
	r := resolutionCandidate(t, s, "assign")
	r2 := dispatchSQLRows{request: slices.Clone(r.request.request), state: slices.Clone(r.request.state)}
	r2.request[0] = rand.Text()
	r2.state = []any{r2.request[0], r2.request[3], r2.request[1], int64(1), "pending", int64(0), dispatchStamp, dispatchStamp, "", "", nil, nil, nil, nil, nil, nil}
	attack := []coordinatorSQL{{dispatchRequestInsert, r2.request}}
	attack = append(attack, resolutionStatements(r)...)
	attack = append(attack, coordinatorSQL{dispatchStateInsert, r2.state})
	dispatchReject(t, s, "所有权", attack...)
	coordinatorMustCommit(t, s, resolutionStatements(r)...)
	dispatchReject(t, s, "所有权", dispatchPair(r2)...)
	// Decline keeps its unfinished slot and does not create an Assignment.
	d := resolutionCandidate(t, s, "decline")
	coordinatorMustCommit(t, s, resolutionStatements(d)...)
	r2.request, r2.state = slices.Clone(d.request.request), slices.Clone(d.request.state)
	r2.request[0], r2.state[0] = rand.Text(), nil
	r2.state = []any{r2.request[0], r2.request[3], r2.request[1], int64(1), "pending", int64(0), dispatchStamp, dispatchStamp, "", "", nil, nil, nil, nil, nil, nil}
	dispatchReject(t, s, "状态不可替换", dispatchPair(r2)...)
}

func TestCoordinationAssignmentResolutionV14ResolvedReplayIgnoresCurrentEligibility(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := resolutionStore(t)
			r := resolutionCandidate(t, s, kind)
			coordinatorMustCommit(t, s, resolutionStatements(r)...)
			beforeReceipt := messageSequenceRows(t, s.db, "SELECT "+dispatchStateColumns+" FROM coordination_dispatch_state")
			task, err := s.GetTask(ctx, r.turn.GroupID, r.decision[2].(string))
			if err != nil {
				t.Fatal(err)
			}
			alternate := coordinationAdmissionRows(task, coordinationAdmissionSource(t, s, domain.ThreadRef{GroupID: task.GroupID, TopicID: r.request.request[2].(string)}))
			alternate.current = nil
			if err := coordinationAdmissionCommit(s, alternate); err != nil {
				t.Fatal(err)
			}
			coordinatorMustCommit(t, s,
				coordinatorSQL{"UPDATE tasks SET revision=revision+1 WHERE id=?", []any{r.decision[2]}},
				coordinatorSQL{"UPDATE task_coordination SET authority_generation=authority_generation+1,current_mandate_id=? WHERE task_id=?", []any{alternate.mandate[0], r.decision[2]}},
				coordinatorSQL{coordinatorBindingRevisionInsert, []any{r.decision[3], 2, r.decision[4]}},
				coordinatorSQL{coordinatorPolicyRevisionInsert, []any{r.decision[3], 2, "[]"}},
				coordinatorSQL{"UPDATE group_coordinator_bindings SET revision=2 WHERE group_id=?", []any{r.decision[3]}},
				coordinatorSQL{"UPDATE group_dispatch_policies SET revision=2 WHERE group_id=?", []any{r.decision[3]}},
				coordinatorSQL{"UPDATE members SET status='left' WHERE group_id=?", []any{r.decision[3]}},
			)
			before := messageSequenceSnapshot(t, s)
			got, err := s.MaterializeDispatchRequest(ctx, r.decision[1].(string), 1, materializeAt)
			if err != nil || got.Generation != 3 || got.Disposition != map[string]string{"assign": "assigned", "decline": "declined"}[kind] || got.Delivery == nil || got.Delivery.DeliveryID != r.request.state[11] {
				t.Fatalf("current eligibility incorrectly replaced historical materialization: %+v, %v", got, err)
			}
			if after := messageSequenceRows(t, s.db, "SELECT "+dispatchStateColumns+" FROM coordination_dispatch_state"); !reflect.DeepEqual(after, beforeReceipt) {
				t.Fatal("historical replay modified original state/receipt projection")
			}
			messageSequencePreserved(t, s, before)
		})
	}
}
