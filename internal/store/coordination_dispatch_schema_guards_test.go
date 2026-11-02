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

func dispatchRecursive(t *testing.T, s *Store, recursive int) {
	t.Helper()
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA recursive_triggers=%d", recursive)); err != nil {
		t.Fatal(err)
	}
	var got, fk int
	var version string
	if err := s.db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("production SQLite %s, recursive_triggers=%d", version, recursive)
	if err := s.db.QueryRow("PRAGMA recursive_triggers").Scan(&got); err != nil || got != recursive {
		t.Fatalf("recursive_triggers=%d: got=%d, %v", recursive, got, err)
	}
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys must be enabled: %d, %v", fk, err)
	}
}

func TestCoordinationDispatchSchemaEveryUniqueInsertConflict(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprint(recursive), func(t *testing.T) {
			s, _ := openTestStore(t)
			g, coordinator, _ := dispatchConfiguredGroup(t, s)
			a, b := dispatchCandidate(t, s, g, coordinator, "unique"), dispatchCandidate(t, s, g, coordinator, "unique")
			coordinatorMustCommit(t, s, dispatchPair(a)...)
			a.state = dispatchReceipt(t, s, a)
			coordinatorMustCommit(t, s, dispatchUpdate(a.state))
			dispatchRecursive(t, s, recursive)
			for _, table := range []struct {
				name, columns, insert, guard string
				original, candidate          []any
				keys                         []string
			}{
				{"coordination_dispatch_requests", dispatchRequestColumns, dispatchRequestInsert, "请求不可替换", a.request, b.request, []string{"id", "id,task_id,group_id"}},
				{"coordination_dispatch_state", dispatchStateColumns, dispatchStateInsert, "状态不可替换", a.state, b.state, []string{"request_id", "request_id,task_id,group_id", "task_id", "message_id", "delivery_id"}},
			} {
				var keys []string
				indexes := messageSequenceRows(t, s.db, fmt.Sprintf(`SELECT name FROM pragma_index_list('%s') WHERE "unique"=1`, table.name))
				for _, index := range indexes {
					columns := messageSequenceRows(t, s.db, fmt.Sprintf("SELECT name FROM pragma_index_info('%s') ORDER BY seqno", index[0]))
					values := slices.Clone(table.candidate)
					var names []string
					for _, column := range columns {
						name := column[0].(string)
						names = append(names, name)
						i := slices.Index(strings.Split(table.columns, ","), name)
						values[i] = table.original[i]
					}
					key := strings.Join(names, ",")
					keys = append(keys, key)
					for _, form := range []string{"INSERT INTO", "INSERT OR IGNORE INTO", "INSERT OR REPLACE INTO"} {
						t.Run(table.name+"/"+key+"/"+form, func(t *testing.T) {
							query := strings.Replace(table.insert, "INSERT INTO", form, 1)
							var statements []coordinatorSQL
							if table.name == "coordination_dispatch_state" && values[0] != a.state[0] {
								statements = append(statements, coordinatorSQL{dispatchRequestInsert, b.request})
							}
							statements = append(statements, coordinatorSQL{query, values})
							dispatchReject(t, s, table.guard, statements...)
						})
					}
				}
				slices.Sort(keys)
				slices.Sort(table.keys)
				if !slices.Equal(keys, table.keys) {
					t.Fatalf("uncovered UNIQUE keys on %s: %v; want %v", table.name, keys, table.keys)
				}
			}
			dispatchAssertFK(t, s)
		})
	}
}

func TestCoordinationDispatchSchemaTaskSlotConcurrentHandles(t *testing.T) {
	s, path := openTestStore(t)
	observer := queueHandle(t, path, false)
	g, coordinator, _ := dispatchConfiguredGroup(t, s)
	a := dispatchCandidate(t, s, g, coordinator, "slot-race")
	b := dispatchSQLRows{slices.Clone(a.request), slices.Clone(a.state)}
	b.request[0] = rand.Text()
	b.state[0] = b.request[0]
	start, results := make(chan struct{}), make(chan error, 2)
	for i, handle := range []*Store{s, observer} {
		rows := []dispatchSQLRows{a, b}[i]
		go func() {
			<-start
			results <- coordinatorCommit(handle, dispatchPair(rows)...)
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-results; err == nil {
			winners++
		} else if !strings.Contains(err.Error(), "状态不可替换") {
			t.Errorf("loser must hit the Task slot guard, not unrelated contention: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("Task slot admitted %d winners", winners)
	}
	if got := messageSequenceRows(t, observer.db, `SELECT r.id,s.request_id FROM coordination_dispatch_requests r
	 JOIN coordination_dispatch_state s ON s.request_id=r.id`); len(got) != 1 || got[0][0] != got[0][1] {
		t.Fatalf("loser leaked a request half or replaced the winner: %v", got)
	}
	dispatchAssertFK(t, observer)
}

func TestCoordinationDispatchSchemaUpdateOrReplaceVictims(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprint(recursive), func(t *testing.T) {
			s, _ := openTestStore(t)
			g, coordinator, _ := dispatchConfiguredGroup(t, s)
			a, b := dispatchCandidate(t, s, g, coordinator, "victim"), dispatchCandidate(t, s, g, coordinator, "victim")
			coordinatorMustCommit(t, s, append(dispatchPair(a), dispatchPair(b)...)...)
			a.state = dispatchReceipt(t, s, a)
			coordinatorMustCommit(t, s, dispatchUpdate(a.state))
			bReceipt := dispatchReceipt(t, s, b)
			dispatchRecursive(t, s, recursive)
			for _, column := range []string{"request_id", "task_id", "message_id", "delivery_id"} {
				t.Run(column, func(t *testing.T) {
					values := dispatchSet(dispatchStateColumns, bReceipt, column, a.state[slices.Index(strings.Split(dispatchStateColumns, ","), column)])
					statement := dispatchUpdate(values)
					statement.query = strings.Replace(statement.query, "UPDATE ", "UPDATE OR REPLACE ", 1)
					statement.args[len(statement.args)-1] = b.request[0]
					// Reject at the UPDATE itself, before FK COMMIT or receipt-parent guards.
					dispatchReject(t, s, "状态不可替换", statement)
				})
			}
			// Model the old bypass: replace a receipt victim, then rebuild it as pending in the same transaction.
			values := slices.Clone(bReceipt)
			values[10], values[11], values[12], values[13] = a.state[10], a.state[11], a.state[12], a.state[13]
			update := dispatchUpdate(values)
			update.query = strings.Replace(update.query, "UPDATE ", "UPDATE OR REPLACE ", 1)
			fresh := slices.Clone(a.state)
			fresh[3], fresh[4], fresh[5], fresh[8], fresh[9] = int64(1), "pending", int64(0), "", ""
			for i := 10; i < 16; i++ {
				fresh[i] = nil
			}
			dispatchReject(t, s, "状态不可替换", update, coordinatorSQL{dispatchStateInsert, fresh})
			// Requests reject all UPDATE forms, including a different row's PK/composite key.
			dispatchReject(t, s, "请求不可修改", coordinatorSQL{"UPDATE OR REPLACE coordination_dispatch_requests SET id=? WHERE id=?", []any{a.request[0], b.request[0]}})
			coordinatorMustCommit(t, s, dispatchUpdate(bReceipt))
			dispatchAssertFK(t, s)
		})
	}
}

func TestCoordinationDispatchSchemaDeliveryParentGuards(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprint(recursive), func(t *testing.T) {
			s, _ := openTestStore(t)
			g, coordinator, worker := dispatchConfiguredGroup(t, s)
			rows := dispatchCandidate(t, s, g, coordinator, "parent")
			coordinatorMustCommit(t, s, dispatchPair(rows)...)
			receipt := dispatchReceipt(t, s, rows)
			coordinatorMustCommit(t, s, dispatchUpdate(receipt))
			other := queueDelivery(t, s, g, coordinator, "parent")
			otherTopic := queueDelivery(t, s, g, coordinator, "other")
			otherMember := queueDelivery(t, s, g, worker, "parent")
			foreignGroup, foreignMember, _ := dispatchConfiguredGroup(t, s)
			foreign := queueDelivery(t, s, foreignGroup, foreignMember, "parent")
			dispatchRecursive(t, s, recursive)
			for _, pair := range []struct {
				column string
				value  any
			}{
				{"id", rand.Text()}, {"message_id", other.MessageID}, {"group_id", foreignGroup},
				{"topic_id", "other"}, {"conversation_id", otherTopic.ConversationID}, {"recipient_id", worker},
			} {
				dispatchReject(t, s, "投递身份不可修改", coordinatorSQL{"UPDATE deliveries SET " + pair.column + "=? WHERE id=?", []any{pair.value, receipt[11]}})
			}
			dispatchReject(t, s, "投递不可删除", coordinatorSQL{"DELETE FROM deliveries WHERE id=?", []any{receipt[11]}})
			// Every actual parent UNIQUE includes either id or (message_id,recipient_id).
			var keys []string
			for _, index := range messageSequenceRows(t, s.db, `SELECT name FROM pragma_index_list('deliveries') WHERE "unique"=1`) {
				var columns []string
				for _, column := range messageSequenceRows(t, s.db, fmt.Sprintf("SELECT name FROM pragma_index_info('%s') ORDER BY seqno", index[0])) {
					columns = append(columns, column[0].(string))
				}
				keys = append(keys, strings.Join(columns, ","))
			}
			slices.Sort(keys)
			if !slices.Equal(keys, []string{"id", "id,group_id,conversation_id,recipient_id", "message_id,recipient_id"}) {
				t.Fatalf("uncovered delivery UNIQUE keys: %v", keys)
			}
			for _, columns := range []string{"id", "message_id,recipient_id", "id,group_id,conversation_id,recipient_id"} {
				t.Run(columns, func(t *testing.T) {
					const deliveryColumns = "id,message_id,conversation_id,recipient_id,group_id,topic_id,state,turn_id,error,created_at"
					original := []any{receipt[11], receipt[10], receipt[12], receipt[13], g, "parent", "queued", "", "", dispatchStamp}
					candidate := []any{rand.Text(), other.MessageID, other.ConversationID, other.RecipientID, g, "parent", "queued", "", "", dispatchStamp}
					for _, column := range strings.Split(columns, ",") {
						i := slices.Index(strings.Split(deliveryColumns, ","), column)
						candidate[i] = original[i]
					}
					for _, args := range [][]any{candidate, original} {
						dispatchReject(t, s, "投递不可替换", coordinatorSQL{"INSERT OR REPLACE INTO deliveries(" + deliveryColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?)", args})
					}
					var assignments []string
					var args []any
					for _, column := range strings.Split(columns, ",") {
						assignments = append(assignments, column+"=?")
						args = append(args, original[slices.Index(strings.Split(deliveryColumns, ","), column)])
					}
					args = append(args, other.ID)
					dispatchReject(t, s, "投递不可替换", coordinatorSQL{"UPDATE OR REPLACE deliveries SET " + strings.Join(assignments, ",") + " WHERE id=?", args})
				})
			}
			// A valid exact tuple from another scope still cannot replace the protected id.
			dispatchReject(t, s, "投递不可替换", coordinatorSQL{`UPDATE OR REPLACE deliveries SET id=?,message_id=?,conversation_id=?,recipient_id=?,group_id=?,topic_id=? WHERE id=?`,
				[]any{receipt[11], foreign.MessageID, foreign.ConversationID, foreign.RecipientID, foreignGroup, "parent", other.ID}})
			// Unreferenced rows remain mutable, replaceable and deletable.
			coordinatorMustCommit(t, s,
				coordinatorSQL{"INSERT OR REPLACE INTO deliveries SELECT * FROM deliveries WHERE id=?", []any{other.ID}},
				coordinatorSQL{"UPDATE deliveries SET error='runtime failure',state='failed' WHERE id=?", []any{otherMember.ID}},
				coordinatorSQL{"UPDATE deliveries SET id=? WHERE id=?", []any{rand.Text(), otherTopic.ID}},
				coordinatorSQL{"DELETE FROM deliveries WHERE id=?", []any{otherMember.ID}},
			)
			dispatchAssertFK(t, s)
		})
	}
}

func TestCoordinationDispatchSchemaExistingMessageAndConversationGuards(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprint(recursive), func(t *testing.T) {
			s, _ := openTestStore(t)
			g, coordinator, worker := dispatchConfiguredGroup(t, s)
			rows := dispatchCandidate(t, s, g, coordinator, "parents")
			coordinatorMustCommit(t, s, dispatchPair(rows)...)
			receipt := dispatchReceipt(t, s, rows)
			coordinatorMustCommit(t, s, dispatchUpdate(receipt))
			other := queueDelivery(t, s, g, coordinator, "other-topic")
			otherMember := queueDelivery(t, s, g, worker, "parents")
			unoccupied := member(t, s, g)
			foreignGroup, foreignMember, _ := dispatchConfiguredGroup(t, s)
			foreign := queueDelivery(t, s, foreignGroup, foreignMember, "parents")
			dispatchRecursive(t, s, recursive)
			for _, column := range []string{"id", "group_id", "topic_id", "sender_id", "content"} {
				dispatchReject(t, s, "消息身份", coordinatorSQL{"UPDATE messages SET " + column + "='changed' WHERE id=?", []any{receipt[10]}})
			}
			dispatchReject(t, s, "消息不可删除", coordinatorSQL{"DELETE FROM messages WHERE id=?", []any{receipt[10]}})
			dispatchReject(t, s, "消息身份不可替换", coordinatorSQL{"INSERT OR REPLACE INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES(?,?,'parents','user','replacement',?)", []any{receipt[10], g, dispatchStamp}})
			dispatchReject(t, s, "消息身份", coordinatorSQL{"UPDATE OR REPLACE messages SET id=? WHERE id=?", []any{receipt[10], other.MessageID}})
			var cases []coordinatorSQL
			for _, pair := range []struct {
				column string
				value  any
			}{
				{"id", "new-conversation"}, {"group_id", foreignGroup}, {"member_id", unoccupied.ID}, {"topic_id", "changed"},
			} {
				cases = append(cases, coordinatorSQL{"UPDATE conversations SET " + pair.column + "=? WHERE id=?", []any{pair.value, receipt[12]}})
			}
			cases = append(cases, coordinatorSQL{"DELETE FROM conversations WHERE id=?", []any{receipt[12]}})
			// All tuples exist; FK parent scope, not a missing member, must reject these replacements.
			for _, parent := range []struct {
				delivery         domain.Delivery
				groupID, topicID string
			}{{other, g, "other-topic"}, {otherMember, g, "parents"}, {foreign, foreignGroup, "parents"}} {
				d := parent.delivery
				cases = append(cases,
					coordinatorSQL{"INSERT OR REPLACE INTO conversations(id,group_id,member_id,topic_id,created_at) VALUES(?,?,?,?,?)", []any{receipt[12], parent.groupID, d.RecipientID, parent.topicID, dispatchStamp}},
					coordinatorSQL{"UPDATE OR REPLACE conversations SET id=?,group_id=?,member_id=?,topic_id=? WHERE id=?", []any{receipt[12], parent.groupID, d.RecipientID, parent.topicID, d.ConversationID}},
					coordinatorSQL{"INSERT OR REPLACE INTO conversations(id,group_id,member_id,topic_id,created_at) VALUES(?,? ,?,'parents',?)", []any{rand.Text(), g, coordinator, dispatchStamp}},
				)
			}
			if len(cases) != 14 {
				t.Fatalf("expected 14 conversation counterexamples, got %d", len(cases))
			}
			for i, statement := range cases {
				t.Run(fmt.Sprintf("conversation/%d", i), func(t *testing.T) {
					dispatchReject(t, s, "FOREIGN KEY", statement)
				})
			}
			// Display timestamps remain mutable; no v12 message guard/index may be added.
			coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE messages SET created_at=? WHERE id=?", []any{dispatchStamp, receipt[10]}})
			if got := messageSequenceRows(t, s.db, "SELECT name FROM sqlite_master WHERE tbl_name IN ('messages','conversations') AND name LIKE 'coordination_dispatch%'"); len(got) != 0 {
				t.Fatalf("v12 must reuse existing message/conversation guards: %v", got)
			}
			dispatchAssertFK(t, s)
		})
	}
}

func TestCoordinationDispatchSchemaQueueRuntimeKeepsReceiptAndUnknownClaims(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprint(recursive), func(t *testing.T) {
			s, path := openTestStore(t)
			g, coordinator, _ := dispatchConfiguredGroup(t, s)
			rows := dispatchCandidate(t, s, g, coordinator, "runtime")
			coordinatorMustCommit(t, s, dispatchPair(rows)...)
			receipt := dispatchReceipt(t, s, rows)
			coordinatorMustCommit(t, s, dispatchUpdate(receipt))
			dispatchRecursive(t, s, recursive)
			before := map[string][][]any{
				"coordination_dispatch_requests": messageSequenceTableRows(t, s, "coordination_dispatch_requests"),
				"coordination_dispatch_state":    messageSequenceTableRows(t, s, "coordination_dispatch_state"),
			}
			turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: g, DeliveryID: receipt[11].(string), ResourceKeys: []string{"dispatch-runtime-held"}, MaxActive: 1})
			if err != nil {
				t.Fatalf("receipt guard blocked normal turn_id binding: %v", err)
			}
			claim := TurnClaim{GroupID: g, TurnID: turn.ID, Token: turn.ClaimToken}
			queueCandidate(t, s, claim, "private candidate", provider.OutcomeSucceeded)
			cancelled, err := s.RequestTurnCancel(ctx, g, turn.ID)
			if err != nil || cancelled.State != domain.TurnStateCancelRequested {
				t.Fatalf("cancellation: %+v, %v", cancelled, err)
			}
			unknown := queueReceipt(provider.OutcomeSucceeded)
			unknown.Outcome, unknown.OutputComplete = provider.OutcomeUnknown, false
			got, err := s.ApplyTurnReceipt(ctx, claim, unknown)
			if err != nil || got.State != domain.TurnStateUnknown {
				t.Fatalf("unknown outcome: %+v, %v", got, err)
			}
			coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE deliveries SET error='runtime diagnostic' WHERE id=?", []any{receipt[11]}})
			messageSequencePreserved(t, s, before)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := queueHandle(t, path, false)
			messageSequencePreserved(t, reopened, before)
			if locks := messageSequenceRows(t, reopened.db, "SELECT resource_key,turn_id FROM resource_claims WHERE resource_key='dispatch-runtime-held'"); !reflect.DeepEqual(locks, [][]any{{"dispatch-runtime-held", turn.ID}}) {
				t.Fatalf("unknown released resource: %v", locks)
			}
			dispatchReject(t, reopened, "", coordinatorSQL{"UPDATE coordination_dispatch_state SET status='pending',generation=generation+1 WHERE request_id=?", []any{rows.request[0]}})
			dispatchAssertFK(t, reopened)
		})
	}
}
