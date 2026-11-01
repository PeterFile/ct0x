package store

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestMessageSequenceAtomicWritesAndReplay(t *testing.T) {
	f := setupMessages(t)
	observer := queueHandle(t, f.path, false)
	request := f.request()
	message, err := f.s.CreateMessage(ctx, request, []string{f.a, f.b})
	if err != nil {
		t.Fatal(err)
	}
	before := messageSequenceLog(t, f.s.db)
	if len(before) != 1 || before[0].MessageID != message.ID || before[0].GroupID != message.GroupID || before[0].TopicID != message.TopicID {
		t.Fatalf("normal message did not append one bound position: %+v", before)
	}
	for _, delivery := range message.Deliveries {
		if delivery.State != domain.DeliveryStateQueued || delivery.TurnID != "" {
			t.Fatalf("append changed delivery acceptance/execution: %+v", delivery)
		}
	}
	queueCounts(t, f.s, 0, 0, 0)
	if got, err := f.s.CreateMessage(ctx, request, []string{"invalid-retry-target"}); err != nil || !reflect.DeepEqual(got, message) {
		t.Fatalf("message replay changed: %+v, %v", got, err)
	}
	if got, found, err := f.s.LookupMessageReceipt(ctx, request); err != nil || !found || !reflect.DeepEqual(got, message) {
		t.Fatalf("message receipt changed: %+v, %t, %v", got, found, err)
	}
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rolledBack := domain.Message{ID: "rolled-back", GroupID: f.groupID, TopicID: "direct", SenderID: f.a, Content: "not committed", CreatedAt: time.Now().UTC()}
	if err := insertMessageRow(ctx, tx, rolledBack); err != nil {
		t.Fatal(err)
	}
	inside := messageSequenceLog(t, tx)
	if len(inside) != 2 || inside[1].MessageID != rolledBack.ID || inside[1].Sequence <= before[0].Sequence {
		t.Fatalf("message and log are not in the same outer transaction: %+v", inside)
	}
	if got := messageSequenceLog(t, observer.db); !reflect.DeepEqual(got, before) {
		t.Fatalf("uncommitted positions escaped to another handle: %+v", got)
	}
	var visible int
	if err := observer.db.QueryRow("SELECT count(*) FROM messages WHERE id=?", rolledBack.ID).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("uncommitted source message escaped: %d, %v", visible, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow("SELECT count(*) FROM messages WHERE id=?", rolledBack.ID).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("rolled-back message survived: %d, %v", visible, err)
	}
	// Fail only after the message's log row exists, through the current public write path.
	if _, err := f.s.db.Exec(`CREATE TRIGGER message_sequence_delivery_fail BEFORE INSERT ON deliveries
	 WHEN EXISTS(SELECT 1 FROM message_log WHERE message_id=NEW.message_id)
	 BEGIN SELECT RAISE(ABORT,'sequence delivery fixture'); END`); err != nil {
		t.Fatal(err)
	}
	failed := request
	failed.IdempotencyKey = "failed-after-log"
	if got, err := f.s.CreateMessage(ctx, failed, []string{f.a}); err == nil || got.ID != "" {
		t.Fatalf("late failure returned a partial message: %+v, %v", got, err)
	}
	if _, found, err := f.s.LookupMessageReceipt(ctx, failed); err != nil || found {
		t.Fatalf("rolled-back message left a receipt: %t, %v", found, err)
	}
	assertMessageCounts(t, f.s, 1, 2, 2, 1)
	if got := messageSequenceLog(t, f.s.db); !reflect.DeepEqual(got, before) {
		t.Fatalf("replay or rollback consumed a durable mapping: %+v", got)
	}
	if _, err := f.s.db.Exec("DROP TRIGGER message_sequence_delivery_fail"); err != nil {
		t.Fatal(err)
	}
	committed, err := f.s.CreateMessage(ctx, failed, []string{f.a})
	if err != nil {
		t.Fatal(err)
	}
	after := messageSequenceLog(t, f.s.db)
	// Rolled-back values may be reused; only compare against committed positions.
	if len(after) != 2 || after[1].MessageID != committed.ID || after[1].Sequence <= before[0].Sequence {
		t.Fatalf("retry did not append above the committed frontier: %+v", after)
	}
	messageSequenceCoverage(t, f.s)
}

func TestMessageSequenceTerminalReplyAndReplay(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	before := messageSequenceLog(t, f.s.db)
	queueCandidate(t, f.s, claim, "durable final reply", provider.OutcomeSucceeded)
	unknown := queueReceipt(provider.OutcomeSucceeded)
	unknown.Outcome = provider.OutcomeUnknown
	if _, err := f.s.ApplyTurnReceipt(ctx, claim, unknown); err != nil {
		t.Fatal(err)
	}
	if got := messageSequenceLog(t, f.s.db); !reflect.DeepEqual(got, before) {
		t.Fatalf("private native evidence became a message event: %+v", got)
	}
	turnBefore := queueTurn(t, f.s, claim)
	receiptBefore := queueReceiptJSON(t, f.s, claim)
	queueCounts(t, f.s, 1, 1, 1)
	if _, err := f.s.db.Exec(`CREATE TRIGGER message_sequence_reply_fail BEFORE UPDATE ON turns
	 WHEN NEW.state='succeeded' AND EXISTS(SELECT 1 FROM message_log WHERE message_id=NEW.reply_message_id)
	 BEGIN SELECT RAISE(ABORT,'sequence reply fixture'); END`); err != nil {
		t.Fatal(err)
	}
	receipt := queueReceipt(provider.OutcomeSucceeded)
	if got, err := f.s.ApplyTurnReceipt(ctx, claim, receipt); err == nil || got.ID != "" {
		t.Fatalf("reply/log insertion did not roll back with settlement: %+v, %v", got, err)
	}
	if got := queueTurn(t, f.s, claim); !reflect.DeepEqual(got, turnBefore) || queueReceiptJSON(t, f.s, claim) != receiptBefore {
		t.Fatalf("failed reply changed native evidence: %+v", got)
	}
	queueCounts(t, f.s, 1, 1, 1)
	if got := messageSequenceLog(t, f.s.db); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed terminal reply left a log row: %+v", got)
	}
	messageSequenceCoverage(t, f.s)
	if _, err := f.s.db.Exec("DROP TRIGGER message_sequence_reply_fail"); err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ApplyTurnReceipt(ctx, claim, receipt)
	if err != nil || turn.ReplyMessageID == "" {
		t.Fatalf("terminal reply failed: %+v, %v", turn, err)
	}
	after := messageSequenceLog(t, f.s.db)
	if len(after) != 2 || after[1].MessageID != turn.ReplyMessageID || after[1].GroupID != f.groupID || after[1].TopicID != "bound-topic" || after[1].Sequence <= before[0].Sequence {
		t.Fatalf("terminal reply did not append one bound position: %+v", after)
	}
	queueCounts(t, f.s, 1, 1, 0)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	if got, err := reopened.ApplyTurnReceipt(ctx, claim, receipt); err != nil || !reflect.DeepEqual(got, turn) {
		t.Fatalf("terminal replay changed after reopen: %+v, %v", got, err)
	}
	if got := messageSequenceLog(t, reopened.db); !reflect.DeepEqual(got, after) {
		t.Fatalf("terminal replay appended or rewrote a position: %+v", got)
	}
	messageSequenceCoverage(t, reopened)
}

func TestMessageSequenceConcurrentHandles(t *testing.T) {
	f := setupMessages(t)
	second := queueHandle(t, f.path, false)
	type result struct {
		message domain.Message
		err     error
	}
	const writers = 12
	start, results := make(chan struct{}), make(chan result, writers)
	for i := range writers {
		go func() {
			<-start
			request := MessageRequest{GroupID: []string{f.groupID, f.otherID}[i%2], ActorID: "user", TopicID: fmt.Sprintf("topic-%d", i%3), Content: "concurrent", IdempotencyKey: fmt.Sprintf("message-%d", i)}
			message, err := []*Store{f.s, second}[i%2].CreateMessage(ctx, request, nil)
			results <- result{message, err}
		}()
	}
	close(start)
	messages := make(map[string]domain.Message, writers)
	for range writers {
		r := <-results
		if r.err != nil {
			t.Errorf("concurrent message: %v", r.err)
			continue
		}
		messages[r.message.ID] = r.message
	}
	entries := messageSequenceLog(t, f.s.db)
	if len(entries) != writers || len(messages) != writers {
		t.Fatalf("lost or duplicate committed mappings: %d entries, %d messages", len(entries), len(messages))
	}
	for _, entry := range entries {
		message, found := messages[entry.MessageID]
		if !found || entry.GroupID != message.GroupID || entry.TopicID != message.TopicID {
			t.Fatalf("concurrent scope/mapping changed: %+v", entry)
		}
	}
	later, err := second.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, ActorID: "user", Content: "after all commits"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := messageSequenceLog(t, second.db)
	if len(after) != writers+1 || !reflect.DeepEqual(after[:writers], entries) || after[writers].MessageID != later.ID || after[writers].Sequence <= entries[writers-1].Sequence {
		t.Fatalf("later commit did not advance the committed frontier: %+v", after)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	if got := messageSequenceLog(t, reopened.db); !reflect.DeepEqual(got, after) {
		t.Fatalf("reopen changed concurrent mappings: %+v", got)
	}
	messageSequenceCoverage(t, reopened)
}
