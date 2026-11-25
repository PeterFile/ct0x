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

	"agentworkspace/internal/domain"
)

func TestGroupBackgroundDispatchSchemaStartup(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	legacy := original
	if i := slices.Index(legacy, groupBackgroundDispatchSchema); i >= 0 {
		legacy = legacy[:i]
	}
	// Use the real pre-A registry for legacy. Flank A with unrelated entries
	// only in the registered fixture to exercise schema identity, not position.
	registered := append(slices.Clone(legacy), "SELECT 1;", groupBackgroundDispatchSchema, "SELECT 2;")

	t.Run("legacy", func(t *testing.T) {
		migrations = legacy
		s, _ := openTestStore(t)
		if s.background.state != groupBackgroundSchemaAbsent {
			t.Fatalf("legacy state = %d; want absent (%d)", s.background.state, groupBackgroundSchemaAbsent)
		}
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
		if s.background.state != groupBackgroundSchemaComplete {
			t.Fatalf("complete state = %d; want complete (%d)", s.background.state, groupBackgroundSchemaComplete)
		}
		expected, err := expectedGroupBackgroundObjects()
		if err != nil || len(expected) != 15 {
			t.Fatalf("expected objects = %d, err = %v; want 15", len(expected), err)
		}
		var complete bool
		if err := s.db.QueryRowContext(t.Context(), "SELECT "+groupBackgroundSchemaCompleteSQL).Scan(&complete); err != nil || !complete {
			t.Fatalf("complete probe = %t, err = %v; want true", complete, err)
		}
		if err := s.EnableGroupBackgroundDispatch(t.Context()); err != nil {
			t.Fatalf("authentic migrated schema must enable background dispatch: %v", err)
		}
	})

	for _, tc := range []struct {
		name, ddl      string
		unregistered   bool
		pendingMigrate bool
	}{
		{name: "missing_joins", ddl: `DROP TABLE group_background_joins;`},
		{name: "missing_joins_before_pending_migrate", ddl: `DROP TABLE group_background_joins;`, pendingMigrate: true},
		{name: "missing_delivery_guard", ddl: `DROP TRIGGER group_background_delivery_insert;`},
		{name: "missing_intent_insert_guard", ddl: `DROP TRIGGER group_background_intent_insert;`},
		{name: "missing_join_insert_guard", ddl: `DROP TRIGGER group_background_join_insert;`},
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
		{name: "extra_reserved_view", ddl: `CREATE VIEW GROUP_BACKGROUND_EXTRA AS SELECT 1;`},
		{name: "outside_prefix_trigger_on_background_table", ddl: `
			CREATE TRIGGER custom_bg_hook AFTER INSERT ON group_background_bindings BEGIN SELECT 1; END;`},
		{name: "outside_prefix_view_referencing_background", ddl: `
			CREATE VIEW custom_bg_view AS SELECT group_id FROM group_background_bindings;`},
		{name: "all_absent", ddl: `DROP TRIGGER group_background_delivery_insert;
			DROP INDEX group_background_active_profiles;
			DROP INDEX group_background_queued_topic;
			DROP TABLE group_background_joins;
			DROP TABLE group_background_intents;
			DROP TABLE group_background_bindings;`},
		{name: "all_absent_before_pending_migrate", pendingMigrate: true, ddl: `DROP TRIGGER group_background_delivery_insert;
			DROP INDEX group_background_active_profiles;
			DROP INDEX group_background_queued_topic;
			DROP TABLE group_background_joins;
			DROP TABLE group_background_intents;
			DROP TABLE group_background_bindings;`},
		{name: "orphan_guard", unregistered: true, ddl: `
			CREATE TRIGGER GROUP_BACKGROUND_DELIVERY_INSERT BEFORE INSERT ON deliveries BEGIN SELECT 1; END;`},
		{name: "renamed_orphan_guard", unregistered: true, ddl: `
			CREATE TRIGGER group_background_delivery_insertt BEFORE INSERT ON deliveries
			WHEN EXISTS(SELECT 1 FROM group_background_intents WHERE message_id=NEW.message_id)
			BEGIN SELECT RAISE(ABORT,'orphan delivery guard'); END;`},
		{name: "outside_prefix_renamed_orphan_guard", unregistered: true, ddl: `
			CREATE TRIGGER old_a_guard BEFORE INSERT ON deliveries
			WHEN EXISTS(SELECT 1 FROM group_background_intents WHERE message_id=NEW.message_id)
			BEGIN SELECT RAISE(ABORT,'orphan guard'); END;`},
		{name: "outside_prefix_renamed_orphan_guard_before_pending_migrate", unregistered: true, pendingMigrate: true, ddl: `
			CREATE TRIGGER old_a_guard BEFORE INSERT ON deliveries
			WHEN EXISTS(SELECT 1 FROM group_background_intents WHERE message_id=NEW.message_id)
			BEGIN SELECT RAISE(ABORT,'orphan guard'); END;`},
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
			if err == nil {
				var state groupBackgroundSchemaState
				state, checkErr := checkGroupBackgroundSchema(raw, !tc.pendingMigrate)
				if checkErr == nil || state != groupBackgroundSchemaDamaged {
					t.Errorf("direct check state=%d err=%v; want damaged", state, checkErr)
				}
			}
			if closeErr := raw.Close(); err != nil || closeErr != nil {
				t.Fatalf("prepare damaged schema: %v; close: %v", err, closeErr)
			}
			if tc.pendingMigrate {
				// Simulate opening a damaged older-ledger DB under newer migrations.
				migrations = append(slices.Clone(migrations), "SELECT 3;")
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

	t.Run("partial_schema_does_not_break_message_reads_or_enable_preview", func(t *testing.T) {
		migrations = registered
		s, _ := openTestStore(t)
		ctx := t.Context()
		g, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "普通消息对照", ActorID: "user"})
		if err != nil {
			t.Fatal(err)
		}
		m := member(t, s, g.ID)
		// Drop the delivery trigger and group_background_joins on an open test
		// connection (Open rejects damaged DBs before serving) to prove
		// EnableGroupBackgroundDispatch refuses partial schemas and transaction-local
		// probes never rely solely on group_background_bindings.
		if _, err := s.db.ExecContext(ctx, `DROP TRIGGER group_background_delivery_insert; DROP TABLE group_background_joins;`); err != nil {
			t.Fatal(err)
		}
		if err := s.EnableGroupBackgroundDispatch(ctx); err == nil {
			t.Fatal("EnableGroupBackgroundDispatch must refuse a damaged schema")
		}
		msg, err := s.PostLocalUserMessage(ctx, MessageRequest{GroupID: g.ID, ActorID: "user", Content: "普通消息不受影响"})
		if err != nil || len(msg.Deliveries) != 1 || msg.Deliveries[0].RecipientID != m.ID {
			t.Fatalf("ordinary post under dropped joins: %+v, %v", msg, err)
		}
		page, err := s.ListMessages(ctx, g.ID, MessageListOptions{Limit: 10})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != msg.ID {
			t.Fatalf("list messages under dropped joins: %+v, %v", page.Items, err)
		}
	})
}

func TestGroupBackgroundDispatchQuoteSuppressFallbackAndComparison(t *testing.T) {
	original := migrations
	defer func() { migrations = original }()
	enableThreadRoutingFixture(t)
	if !slices.Contains(migrations, messageQuoteSchema) {
		migrations = append(slices.Clone(migrations), messageQuoteSchema)
	}
	if !slices.Contains(migrations, groupBackgroundDispatchSchema) {
		migrations = append(slices.Clone(migrations), groupBackgroundDispatchSchema)
	}
	s, _ := openTestStore(t)
	ctx := t.Context()
	if err := s.EnableGroupBackgroundDispatch(ctx); err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateLocalUserGroup(ctx, CreateGroupInput{Name: "引用与后台兼容", ActorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	// 1. Direct quote of a user message sets suppressFallback (user cannot be
	// woken as an Agent author) and must NOT defer to background dispatch.
	userOrig, err := s.PostLocalUserMessage(ctx, MessageRequest{
		GroupID: g.ID, ActorID: "user", Content: "用户的原始说明", RecipientIDs: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	quotedUser, err := s.PostLocalUserMessage(ctx, MessageRequest{
		GroupID: g.ID, ActorID: "user", Content: "直接引用用户原话",
		ThreadLinks: domain.ThreadLinks{ReplyToMessageID: userOrig.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(quotedUser.Deliveries) != 0 {
		t.Fatalf("direct quote of user message woke recipients: %+v", quotedUser.Deliveries)
	}
	if quotedUser.ReplyTo == nil || quotedUser.ReplyTo.MessageID != userOrig.ID {
		t.Fatalf("direct quote lost message_quotes link: %+v", quotedUser.ReplyTo)
	}
	var intentCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM group_background_intents WHERE message_id=?`, quotedUser.ID).Scan(&intentCount); err != nil {
		t.Fatal(err)
	}
	if intentCount != 0 {
		t.Fatalf("direct quote of user message created %d background intents; want 0", intentCount)
	}

	// 2. Comparison-only quote stored in message_quotes must still defer to A1
	// and exclude the quoted member author when routing.
	author, err := s.CreateMember(ctx, CreateMemberInput{
		GroupID: g.ID, Name: "原作者", Role: "讨论", ProviderID: "pi", Model: "model-a",
		CreatorID: "user", OwnerID: "user", ActorID: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	authorMsg, err := s.CreateMessage(ctx, MessageRequest{
		GroupID: g.ID, ActorID: author.ID, Content: "我的方案是先验证边界。", RecipientIDs: []string{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	comp, err := s.PostLocalUserMessage(ctx, MessageRequest{
		GroupID: g.ID, ActorID: "user", Content: "引用仅作对照，请其他人给出第二意见。",
		ThreadLinks: domain.ThreadLinks{ReplyToMessageID: authorMsg.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(comp.Deliveries) != 0 || comp.ReplyTo == nil || comp.ReplyTo.MessageID != authorMsg.ID {
		t.Fatalf("comparison quote should defer with quote preserved: deliveries=%+v replyTo=%+v", comp.Deliveries, comp.ReplyTo)
	}
	opts := BackgroundDispatchOptions{
		MaxActiveMembers: 3,
		ReadyProfiles:    []BackgroundDispatchProfile{{ProviderID: "pi", Model: "model-a"}},
		Presets: []BackgroundDispatchTemplate{{
			ID: "second-opinion", Name: "第二视角", Role: "评审",
			Profile: BackgroundDispatchProfile{ProviderID: "pi", Model: "model-a"},
		}},
	}
	if err := s.RunGroupBackgroundDispatch(ctx, opts); err != nil {
		t.Fatal(err)
	}
	var routedRecipient, routeKind string
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(recipient_id,''), coalesce(route_kind,'') FROM group_background_intents WHERE message_id=?`, comp.ID).
		Scan(&routedRecipient, &routeKind); err != nil {
		t.Fatal(err)
	}
	if routedRecipient == "" || routedRecipient == author.ID || routeKind != "preset" {
		t.Fatalf("comparison quote routed to recipient=%q kind=%q (author=%q); want preset join excluding author", routedRecipient, routeKind, author.ID)
	}
}
