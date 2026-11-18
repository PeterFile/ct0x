package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/provider"
)

func nativeV22Fixture(t *testing.T) messageFixture {
	t.Helper()
	original := migrations
	defer func() { migrations = original }()
	migrations = original[:22]
	return setupMessages(t)
}

// Exercise the v23 schema transition in isolation even after later appends.
func nativeV23Migrations(t *testing.T) {
	t.Helper()
	if len(migrations) < 23 {
		t.Fatalf("V23 not registered: %d", len(migrations))
	}
	original := migrations
	migrations = original[:23]
	t.Cleanup(func() { migrations = original })
}

func TestNativeSessionV23FrozenPrefixAndRegistration(t *testing.T) {
	// These are exact SQL hashes from the immutable F5 dependency, including
	// the composed workspace-operation and repair schemas at V21/V22.
	frozen := []string{
		"0c4b8cb8a1b1dde7a98596f45e8c6996d892392deac0481a83bc9f73f8eca64c",
		"15f9694d11b3e60c893dd633e815a97ada9a1b219c621c157eb7eab76dff1b28",
		"6161295e75c2dcd7435d451f2a8359fb1aff445da0209aa2e42f599f25bb8399",
		"a5e0d499caaa68a606af016f72b1022679639eab38594a12759976075a761714",
		"f843e64ea2d1b7611c781f513a102e3d52fbb67317c7a8e23953a2d28a9538e4",
		"d73117e5319acdd6884ff57df747dd67cd2f2958efac5318799cd1677a1ab32c",
		"f68370d3140ed156c84567f4cd99f82381293b656486ba1dd18691cfdf08a77f",
		"f8f82d4a122e630608f5e78760882a1ac025ed38bd6f76edb7013cff1c6f8e5f",
		"efa3930f60093437d002bf41c748f987066821fa6036aacaec60de78e30ee659",
		"4b6c8c8252d7968a083fdffb19fa0216a17813e90716b485d6b64c78eb3d27fe",
		"899b3bbe85055d44ecefbb5176c8c98d157ed1edbba10815ba2765bc61f1cc41",
		"9dd7c579f374cf961802aea6066e6c62a787186de10ecd8c285010439607bf7c",
		"517f592c721740e49a31af2b4ccdbb660c3670714d1ecd6d0b34050157f5d215",
		"613d4dd94149cd829ecd053dc5348d813b3cf0d910e7cd7ce72a7557d7039fd7",
		"4bf1683f149cbd60f9fb6a2b0c89ece32557ee12a31b03b47fc9aeaf40950df0",
		"bc087e7b26ced96d1dac352de73daa4f04a36322f9d1597fe360d7823401cc49",
		"434be565ddd0581a0d7c6c0b262f69f49a356e3c29f95f65d348c090b0e2b06b",
		"c1aa6eda7fa893ff674768cd8849e450b14a443ebc561579d78632228be6e3a9",
		"c446da877ec9f857e47d4bb6d1772bb6577e0952cc5cd642cc4bc85e8abe37d0",
		"610de63a87be479e43ea8460979419c124f61ccfacd8a89874760a831c632098",
		"dc7b9b8edda63a758948f58bf6617d5d70edbad0256e45dad585dde760316c7e",
		"7eba2522f2ecb7affee53ac953ead939060ff6884004a69279a070481e2d8098",
	}
	if len(migrations) < 23 || migrations[22] != nativeRunBindingsSchemaV23 {
		t.Fatalf("native schema must be registered at V23: registered=%d", len(migrations))
	}
	for i, want := range frozen {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations[i]))); got != want {
			t.Fatalf("immutable migration %d changed: %s", i+1, got)
		}
	}
	count := 0
	for _, migration := range migrations {
		if migration == nativeRunBindingsSchemaV23 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("native schema registered %d times", count)
	}
}

func TestNativeSessionV23MigrationKeepsUnknownResourcesAndCanonicalReceipt(t *testing.T) {
	nativeV23Migrations(t)
	f := nativeV22Fixture(t)
	for i, state := range []string{"succeeded", "unknown", "starting"} {
		m := []string{f.a, f.b, f.foreign}[i]
		g := f.groupID
		if i == 2 {
			g = f.otherID
		}
		d := queueDelivery(t, f.s, g, m, "v22-fixture")
		turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: g, DeliveryID: d.ID, ResourceKeys: []string{"v22-" + state}, MaxActive: 8})
		if err != nil {
			t.Fatal(err)
		}
		if state != "starting" {
			r := normalizeTurnReceipt(turn, queueReceipt(provider.Outcome(state)))
			// Old receipt fixtures predate the new association hook. Keep their
			// canonical bytes and actual held/released resource state explicit.
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec(`UPDATE turns SET state=?,native_session_id=?,accepted_at=created_at,receipt_json=? WHERE id=?`,
				state, r.NativeSessionID, string(data), turn.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec("UPDATE deliveries SET state='accepted' WHERE id=?", d.ID); err != nil {
				t.Fatal(err)
			}
			if state == "succeeded" {
				if _, err := f.s.db.Exec("DELETE FROM resource_claims WHERE turn_id=?", turn.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	before := messageSequenceSnapshot(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = queueHandle(t, f.path, false)
	messageSequenceLedger(t, f.s, 23)
	messageSequencePreserved(t, f.s, before)
	var total, barriers int
	if err := f.s.db.QueryRow(`SELECT count(*),count(CASE WHEN purpose='unknown' AND binding_version=0
	 AND binding_json IS NULL AND native_ref_json IS NULL AND resumed_from_turn_id IS NULL AND claim_digest IS NULL THEN 1 END) FROM native_run_bindings`).Scan(&total, &barriers); err != nil || total != 3 || barriers != total {
		t.Fatalf("V22 turns did not become pure barriers: total=%d barriers=%d err=%v", total, barriers, err)
	}
	for _, resource := range []string{"v22-unknown", "v22-starting"} {
		if held, err := f.s.HasResourceClaim(ctx, resource); err != nil || !held {
			t.Fatalf("migration replayed/released %s: %t, %v", resource, held, err)
		}
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = queueHandle(t, f.path, false)
	messageSequenceLedger(t, f.s, 23)
	messageSequencePreserved(t, f.s, before)
	dispatchAssertFK(t, f.s)
}

func TestNativeSessionV23MigrationRollbackAndRetry(t *testing.T) {
	nativeV23Migrations(t)
	f := nativeV22Fixture(t)
	queueDelivery(t, f.s, f.groupID, f.a, "v22-pending")
	before := messageSequenceSnapshot(t, f.s)
	original := migrations
	defer func() { migrations = original }()
	migrations = slices.Clone(original)
	migrations[22] += "\nINSERT INTO native_migration_missing_fixture VALUES(1);"
	if err := migrate(f.s.db); err == nil || !strings.Contains(err.Error(), "迁移版本 23") {
		t.Fatalf("partial native migration committed: %v", err)
	}
	messageSequenceLedger(t, f.s, 22)
	messageSequencePreserved(t, f.s, before)
	var leaked int
	if err := f.s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('native_run_bindings','turns_native_binding_identity')").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("failed V23 leaked objects: %d, %v", leaked, err)
	}
	migrations = original
	if err := migrate(f.s.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, f.s, 23)
	messageSequencePreserved(t, f.s, before)
}
