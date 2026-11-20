package store

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
)

// NativeThreadHandoffFixture shares the actual production Store, migration
// code and F fixtures with the external C wire tests. Disable fsync only on
// this private fixture (these tests do not simulate a host crash).
func NativeThreadHandoffFixture(t *testing.T, installF bool) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "handoff.sqlite")
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(OFF)&_txlock=immediate"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	t.Cleanup(func() { _ = s.Close() })
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if installF {
		installDecisionAnswerFixture(t, s)
		installThreadDecisionAnswerFixture(t, s)
	}
	return s, path
}
