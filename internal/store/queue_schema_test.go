package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestQueueSchemaBindsMemberAndDelivery(t *testing.T) {
	f := setupMessages(t)
	r := f.request()
	r.IdempotencyKey = ""
	first, err := f.s.CreateMessage(ctx, r, []string{f.a, f.b})
	if err != nil {
		t.Fatal(err)
	}
	r.TopicID = "another-topic"
	second, err := f.s.CreateMessage(ctx, r, []string{f.a})
	if err != nil {
		t.Fatal(err)
	}
	a, b := first.Deliveries[0], first.Deliveries[1]
	if a.RecipientID != f.a {
		a, b = b, a
	}
	insert := func(id, groupID, deliveryID, conversationID, memberID string) error {
		_, err := f.s.db.Exec(`INSERT INTO turns(id,group_id,delivery_id,conversation_id,member_id,claim_token,state,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,'starting',?,?)`, id, groupID, deliveryID, conversationID, memberID, "token-"+id, first.CreatedAt.Format(timestampLayout), first.CreatedAt.Format(timestampLayout))
		return err
	}
	if err := insert("first", f.groupID, a.ID, a.ConversationID, a.RecipientID); err != nil {
		t.Fatalf("valid member-bound turn: %v", err)
	}
	for _, state := range []string{"starting", "running", "cancel_requested", "unknown"} {
		if _, err := f.s.db.Exec("UPDATE turns SET state=? WHERE id='first'", state); err != nil {
			t.Fatal(err)
		}
		d := second.Deliveries[0]
		if err := insert("same-member", f.groupID, d.ID, d.ConversationID, d.RecipientID); err == nil {
			t.Fatalf("%s allowed same member in a different topic", state)
		}
	}
	// Leave no active uniqueness conflict that could mask the composite FK checks.
	if _, err := f.s.db.Exec("UPDATE turns SET state='succeeded' WHERE id='first'"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][4]string{
		{f.otherID, b.ID, b.ConversationID, b.RecipientID},
		{f.groupID, b.ID, b.ConversationID, a.RecipientID},
		{f.groupID, b.ID, a.ConversationID, b.RecipientID},
	} {
		if err := insert("forged", args[0], args[1], args[2], args[3]); err == nil {
			t.Fatalf("mixed delivery identity accepted: %v", args)
		}
	}
	if _, err := f.s.db.Exec("UPDATE turns SET state='unknown' WHERE id='first'"); err != nil {
		t.Fatal(err)
	}
	if err := insert("different-member", f.groupID, b.ID, b.ConversationID, b.RecipientID); err != nil {
		t.Fatalf("different member blocked: %v", err)
	}
	if _, err := f.s.db.Exec("INSERT INTO resource_claims VALUES('shared','first')"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("INSERT INTO resource_claims VALUES('shared','different-member')"); err == nil {
		t.Fatal("duplicate global resource accepted")
	}
	for _, state := range []string{"not_started", "succeeded", "failed", "aborted", "truncated"} {
		if _, err := f.s.db.Exec("UPDATE turns SET state=? WHERE id='first'", state); err != nil {
			t.Fatal(err)
		}
		d := second.Deliveries[0]
		if err := insert("next", f.groupID, d.ID, d.ConversationID, d.RecipientID); err != nil {
			t.Fatalf("terminal %s still occupies member: %v", state, err)
		}
		if _, err := f.s.db.Exec("DELETE FROM turns WHERE id='next'"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.db.Exec("UPDATE turns SET state='invented' WHERE id='first'"); err == nil {
		t.Fatal("invalid turn state accepted")
	}
}

func TestQueueV3UpgradePreservesMessagesAndReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY);` + migrations[0] + migrations[1] + migrations[2] + `INSERT INTO schema_migrations VALUES(1),(2),(3);`); err != nil {
		t.Fatal(err)
	}
	old := &Store{db: raw}
	g, err := old.CreateGroup(ctx, keyedGroup())
	if err != nil {
		t.Fatal(err)
	}
	m := member(t, old, g.ID)
	r := MessageRequest{GroupID: g.ID, ActorID: "user", Content: "durable input", IdempotencyKey: "input"}
	before, err := old.CreateMessage(ctx, r, []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.db.Query(`SELECT candidate_text,candidate_terminal,native_session_id,accepted_at,cancel_requested_at,reply_message_id,receipt_json FROM turns LIMIT 0`)
	if err != nil {
		t.Fatalf("v4 must persist all agreed settlement fields: %v", err)
	}
	rows.Close()
	var version int
	if err := s.db.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=4").Scan(&version); err != nil || version != 1 {
		t.Fatalf("missing v4: %d, %v", version, err)
	}
	after, found, err := s.LookupMessageReceipt(ctx, r)
	if err != nil || !found || !reflect.DeepEqual(before, after) {
		t.Fatalf("upgrade changed message receipt: %+v, %v, %v", after, found, err)
	}
	if got, err := s.CreateGroup(ctx, keyedGroup()); err != nil || got.ID != g.ID {
		t.Fatalf("upgrade changed mutation receipt: %+v, %v", got, err)
	}
}
