package store

import (
	"reflect"
	"slices"
	"testing"
)

func TestMessageSequenceSnapshotCompleteAndDeterministic(t *testing.T) {
	s, _ := openTestStore(t)
	coordinatorMustCommit(t, s, coordinatorSQL{`
CREATE TABLE snapshot_rowid_fixture(value, payload BLOB, note TEXT COLLATE NOCASE);
CREATE TABLE snapshot_without_rowid_fixture(id TEXT PRIMARY KEY, payload BLOB, note TEXT) WITHOUT ROWID;
`, nil})
	rows := [][]any{
		{int64(1), []byte{0, 255, 1}, nil},
		{float64(1), []byte{}, "A"},
		{int64(1), []byte{0, 255, 1}, nil}, // Identical rows must not be deduplicated.
		{nil, nil, "a"},
		{"1", []byte{0}, "a"},
		{[]byte("1"), nil, "A"},
		{"case", nil, "a"},
		{"case", nil, "A"},
	}
	insert := func(rows [][]any) {
		t.Helper()
		var statements []coordinatorSQL
		for _, row := range rows {
			statements = append(statements, coordinatorSQL{"INSERT INTO snapshot_rowid_fixture VALUES(?,?,?)", row})
		}
		coordinatorMustCommit(t, s, statements...)
	}
	insert(rows)
	coordinatorMustCommit(t, s,
		coordinatorSQL{"INSERT INTO snapshot_without_rowid_fixture VALUES(?,?,?)", []any{"b", []byte{}, ""}},
		coordinatorSQL{"INSERT INTO snapshot_without_rowid_fixture VALUES(?,?,?)", []any{"a", []byte{0, 255}, nil}},
	)
	before := messageSequenceSnapshot(t, s)
	want := [][]any{
		{[]byte("1"), nil, "A"},
		{int64(1), []byte{0, 255, 1}, nil},
		{int64(1), []byte{0, 255, 1}, nil},
		{nil, nil, "a"},
		{float64(1), []byte(nil), "A"}, // The driver represents an empty BLOB as a typed nil slice, not SQL NULL.
		{"1", []byte{0}, "a"},
		{"case", nil, "A"},
		{"case", nil, "a"},
	}
	if !reflect.DeepEqual(before["snapshot_rowid_fixture"], want) {
		t.Fatalf("snapshot lost types, columns, duplicates, BLOB or NULL: %#v; want %#v", before["snapshot_rowid_fixture"], want)
	}
	if got := before["snapshot_without_rowid_fixture"]; !reflect.DeepEqual(got, [][]any{{"a", []byte{0, 255}, nil}, {"b", []byte(nil), ""}}) {
		t.Fatalf("WITHOUT ROWID snapshot is incomplete: %#v", got)
	}
	var blobType string
	if err := s.db.QueryRow("SELECT typeof(payload) FROM snapshot_without_rowid_fixture WHERE id='b'").Scan(&blobType); err != nil || blobType != "blob" {
		t.Fatalf("empty BLOB became SQL NULL: %q, %v", blobType, err)
	}
	for _, table := range []string{"group_coordinator_binding_revisions", "group_dispatch_policy_revisions", "group_coordinator_bindings", "group_dispatch_policies"} {
		if got, ok := before[table]; !ok || !reflect.DeepEqual(got, [][]any{}) {
			t.Fatalf("empty typed table was skipped or altered: %s: %#v, present=%t", table, got, ok)
		}
	}
	messageSequencePreserved(t, s, before)
	coordinatorMustCommit(t, s, coordinatorSQL{"DELETE FROM snapshot_rowid_fixture", nil})
	reversed := slices.Clone(rows)
	slices.Reverse(reversed)
	insert(reversed)
	if after := messageSequenceSnapshot(t, s); !reflect.DeepEqual(after, before) {
		t.Fatal("physical row order changed a logically identical complete snapshot")
	}
	messageSequencePreserved(t, s, before)
	coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE snapshot_without_rowid_fixture SET payload=x'01' WHERE id='a'", nil})
	if after := messageSequenceSnapshot(t, s); reflect.DeepEqual(after, before) {
		t.Fatal("snapshot failed to detect a changed BLOB value")
	}
	coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE snapshot_without_rowid_fixture SET payload=x'00ff' WHERE id='a'", nil})
	messageSequencePreserved(t, s, before)
	coordinatorMustCommit(t, s, coordinatorSQL{`DELETE FROM snapshot_rowid_fixture WHERE rowid=(
	 SELECT min(rowid) FROM snapshot_rowid_fixture WHERE typeof(value)='integer')`, nil})
	after := messageSequenceSnapshot(t, s)
	if reflect.DeepEqual(after, before) || len(after["snapshot_rowid_fixture"]) != len(want)-1 {
		t.Fatal("snapshot failed to detect removal of one duplicate row")
	}
}
