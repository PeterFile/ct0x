package store

import (
	"slices"
	"testing"

	"modernc.org/sqlite"
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

// EnableThreadRoutingForBackgroundFixture registers E before the fixture adds
// A1, and keeps E registered for subsequent reads, writes and restarts.
func EnableThreadRoutingForBackgroundFixture(t *testing.T) {
	t.Helper()
	enableThreadRoutingFixture(t)
}

// BeforeGroupBackgroundFixtureWrite runs f immediately before a row write to
// table on the Store fixture's single SQLite connection, until it is closed.
func BeforeGroupBackgroundFixtureWrite(t *testing.T, s *Store, table string, f func()) {
	t.Helper()
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Raw(func(driverConn any) error {
		driverConn.(sqlite.HookRegisterer).RegisterPreUpdateHook(func(data sqlite.SQLitePreUpdateData) {
			if data.TableName == table {
				f()
			}
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
