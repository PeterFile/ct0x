package store

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestMigrationsV47DeliveryArtifactReadGrantOrder(t *testing.T) {
	if got := slices.Index(migrations, deliveryArtifactReadGrantSQL) + 1; got != 47 {
		t.Fatalf("deliveryArtifactReadGrantSQL migration is v%d, want v47", got)
	}
}

func TestMigrationsV47UpgradeKeepsOrdinaryDelivery(t *testing.T) {
	public := migrations
	if len(public) < 47 {
		t.Fatalf("public migrations stop at v%d, want at least v47", len(public))
	}
	t.Cleanup(func() { migrations = public })
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "v46.sqlite")
	migrations = public[:46]
	before, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = before.Close() })
	group := group(t, before)
	recipient := member(t, before, group.ID)
	req := MessageRequest{
		GroupID: group.ID, ActorID: "user", Content: "请核对导出。",
		RecipientIDs: []string{recipient.ID}, IdempotencyKey: "v46-ordinary-delivery",
	}
	msg, err := before.PostLocalUserMessage(ctx, req)
	if err != nil || len(msg.Deliveries) != 1 {
		t.Fatalf("pre-v47 ordinary delivery failed: %+v, %v", msg, err)
	}
	messageSequenceLedger(t, before, 46)
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	migrations = public
	after, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = after.Close() })
	messageSequenceLedger(t, after, len(public))
	replay, err := after.PostLocalUserMessage(ctx, req)
	if err != nil || replay.ID != msg.ID || len(replay.Deliveries) != 1 || replay.Deliveries[0].ID != msg.Deliveries[0].ID {
		t.Fatalf("ordinary message or delivery changed across v47 upgrade: %+v, %v", replay, err)
	}
	if grants := messageSequenceTableRows(t, after, "delivery_artifact_read_grants"); len(grants) != 0 {
		t.Fatalf("ordinary delivery unexpectedly gained historical artifact grants: %v", grants)
	}
	dispatchAssertFK(t, after)
	assertSQLiteQuickCheckOK(t, after)
}
