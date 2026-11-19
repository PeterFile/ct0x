package store

import (
	"slices"
	"testing"
)

// OpenGroupBackgroundFixture runs the real migrator against SQLite while this
// slice awaits registration. Only serial store tests use this fixture.
func OpenGroupBackgroundFixture(t *testing.T, path string) *Store {
	t.Helper()
	original := migrations
	defer func() { migrations = original }()
	if groupBackgroundDispatchSchema != "" && !slices.Contains(migrations, groupBackgroundDispatchSchema) {
		migrations = append(slices.Clone(migrations), groupBackgroundDispatchSchema)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
