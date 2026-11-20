package store

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGroupBackgroundDispatchSchemaStartup(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	legacy := original
	if i := slices.Index(legacy, groupBackgroundDispatchSchema); i >= 0 {
		legacy = legacy[:i]
	}
	// Keep unrelated applied entries to exercise schema identity, not position.
	legacy = append(slices.Clone(legacy), "SELECT 1;")
	registered := append(slices.Clone(legacy), groupBackgroundDispatchSchema, "SELECT 2;")

	t.Run("legacy", func(t *testing.T) {
		migrations = legacy
		s, _ := openTestStore(t)
		ctx := t.Context()
		if err := s.EnableGroupBackgroundDispatch(ctx); !errors.Is(err, ErrInvalid) {
			t.Fatalf("legacy database must not enable background dispatch: %v", err)
		}
		g, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "旧群组", ActorID: "user"})
		if err != nil {
			t.Fatal(err)
		}
		m := member(t, s, g.ID)
		msg, err := s.PostLocalUserMessage(ctx, MessageRequest{GroupID: g.ID, ActorID: "user", Content: "正常消息"})
		if err != nil || len(msg.Deliveries) != 1 || msg.Deliveries[0].RecipientID != m.ID {
			t.Fatalf("legacy default route: %+v, %v", msg.Deliveries, err)
		}
	})

	t.Run("complete", func(t *testing.T) {
		migrations = registered
		s, _ := openTestStore(t)
		if err := s.EnableGroupBackgroundDispatch(t.Context()); err != nil {
			t.Fatalf("authentic migrated schema must enable background dispatch: %v", err)
		}
	})

	for _, tc := range []struct {
		name, ddl    string
		unregistered bool
	}{
		{name: "missing_joins", ddl: `DROP TABLE group_background_joins;`},
		{name: "missing_delivery_guard", ddl: `DROP TRIGGER group_background_delivery_insert;`},
		{name: "missing_index", ddl: `DROP INDEX group_background_active_profiles;`},
		{name: "wrong_type", ddl: `DROP INDEX group_background_pending;
			CREATE VIEW group_background_pending AS SELECT 1;`},
		{name: "wrong_owner", ddl: `DROP TRIGGER group_background_delivery_insert;
			CREATE TRIGGER group_background_delivery_insert BEFORE INSERT ON messages BEGIN SELECT 1; END;`},
		{name: "renamed_notice_column", ddl: `ALTER TABLE group_background_joins RENAME COLUMN notice_message_id TO renamed_notice_message_id;`},
		{name: "noop_delivery_guard", ddl: `DROP TRIGGER group_background_delivery_insert;
			CREATE TRIGGER group_background_delivery_insert BEFORE INSERT ON deliveries BEGIN SELECT 1; END;`},
		{name: "noncanonical_definition", ddl: `DROP INDEX group_background_pending;
			CREATE INDEX group_background_pending ON group_background_intents(sequence) WHERE delivery_id IS null;`},
		{name: "noncanonical_name", ddl: `DROP INDEX group_background_pending;
			CREATE INDEX GROUP_BACKGROUND_PENDING ON group_background_intents(sequence) WHERE delivery_id IS NULL;`},
		{name: "all_absent", ddl: `DROP TRIGGER group_background_delivery_insert;
			DROP INDEX group_background_active_profiles;
			DROP INDEX group_background_queued_topic;
			DROP TABLE group_background_joins;
			DROP TABLE group_background_intents;
			DROP TABLE group_background_bindings;`},
		{name: "orphan_guard", unregistered: true, ddl: `
			CREATE TRIGGER GROUP_BACKGROUND_DELIVERY_INSERT BEFORE INSERT ON deliveries BEGIN SELECT 1; END;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migrations = registered
			if tc.unregistered {
				migrations = legacy
			}
			path := filepath.Join(t.TempDir(), "workspace.sqlite")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			// Damage a closed, empty test database without touching its ledger.
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = raw.Exec(tc.ddl)
			if closeErr := raw.Close(); err != nil || closeErr != nil {
				t.Fatalf("prepare damaged schema: %v; close: %v", err, closeErr)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			s, err = Open(path)
			if s != nil {
				s.Close()
				t.Error("Open returned a Store for a damaged schema")
			}
			const want = "群组后台调度数据库结构不完整"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("Open error = %v; want %q", err, want)
			} else {
				t.Logf("Open rejected the database: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("Open must leave schema, data and migration ledger unchanged: %v", err)
			}
		})
	}
}
