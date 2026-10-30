package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

var ctx = context.Background()

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "core ?#&_pragma=foreign_keys(0).sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, path
}

func group(t *testing.T, s *Store) domain.Group {
	t.Helper()
	g, err := s.CreateGroup(ctx, CreateGroupInput{Name: "讨论组", Description: "说明"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func member(t *testing.T, s *Store, groupID string) domain.Member {
	t.Helper()
	m, err := s.CreateMember(ctx, CreateMemberInput{
		GroupID: groupID, Name: "研究员", Role: "研究", ProviderID: "pi", Model: "model",
		CreatorID: "external-creator", OwnerID: "lifecycle-owner", Workspace: "/shared/workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOpenMigratesAndConfiguresConnections(t *testing.T) {
	s, path := openTestStore(t)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{
		"PRAGMA foreign_keys": 1, "PRAGMA busy_timeout": 5000,
		"SELECT count(*) FROM schema_migrations WHERE version = 1": 1,
	} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", query, got, err, want)
		}
	}
	if s.db.Stats().MaxOpenConnections != 1 {
		t.Fatal("expected one connection")
	}
	s.db.SetMaxIdleConns(0)
	var enabled int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("replacement connection lost foreign keys: %d, %v", enabled, err)
	}
}

func TestReopenPreservesIdentities(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	m := member(t, s, g.ID)
	c, err := s.GetOrCreateConversation(ctx, g.ID, m.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == "" || m.ID == "" || c.ID == "" || m.Status != domain.MemberStatusActive {
		t.Fatal("missing identities or initial status")
	}
	if g.Name != "讨论组" || g.Description != "说明" || m.GroupID != g.ID ||
		m.Name != "研究员" || m.Role != "研究" || m.ProviderID != "pi" || m.Model != "model" ||
		m.CreatorID != "external-creator" || m.OwnerID != "lifecycle-owner" || m.Workspace != "/shared/workspace" {
		t.Fatalf("creation lost input fields: %+v, %+v", g, m)
	}
	for _, stamp := range []time.Time{g.CreatedAt, m.CreatedAt, c.CreatedAt} {
		if stamp.IsZero() || stamp.Location() != time.UTC {
			t.Fatalf("timestamp is not UTC: %v", stamp)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	gotGroup, err := reopened.GetGroup(ctx, g.ID)
	if err != nil || !reflect.DeepEqual(gotGroup, g) {
		t.Fatalf("group changed: %+v, %v", gotGroup, err)
	}
	gotMember, err := reopened.GetMember(ctx, g.ID, m.ID)
	if err != nil || !reflect.DeepEqual(gotMember, m) {
		t.Fatalf("member changed: %+v, %v", gotMember, err)
	}
	gotConversation, err := reopened.GetConversation(ctx, g.ID, c.ID)
	if err != nil || !reflect.DeepEqual(gotConversation, c) {
		t.Fatalf("conversation changed: %+v, %v", gotConversation, err)
	}
	if m.CreatorID == m.OwnerID || m.CreatorID == m.GroupID || m.OwnerID == m.Workspace {
		t.Fatal("creator, owner, membership and workspace were conflated")
	}
}

func TestEmptyListsAreArraysWithNoNextPage(t *testing.T) {
	s, _ := openTestStore(t)
	groups, err := s.ListGroups(ctx, ListOptions{})
	data, _ := json.Marshal(groups.Items)
	if err != nil || string(data) != "[]" || groups.NextCursor != nil {
		t.Fatalf("empty groups: %+v, %v", groups, err)
	}
	g := group(t, s)
	members, err := s.ListMembers(ctx, g.ID, ListOptions{})
	data, _ = json.Marshal(members.Items)
	if err != nil || string(data) != "[]" || members.NextCursor != nil {
		t.Fatalf("empty members: %+v, %v", members, err)
	}
}

func TestMissingAndCrossGroupReferences(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	m := member(t, s, g.ID)
	c, err := s.GetOrCreateConversation(ctx, g.ID, m.ID, "topic")
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"get group": func() error { _, e := s.GetGroup(ctx, "missing"); return e },
		"create member": func() error {
			_, e := s.CreateMember(ctx, CreateMemberInput{GroupID: "missing", Name: "name", Role: "role"})
			return e
		},
		"list members":         func() error { _, e := s.ListMembers(ctx, "missing", ListOptions{}); return e },
		"get member":           func() error { _, e := s.GetMember(ctx, g.ID, "missing"); return e },
		"foreign member":       func() error { _, e := s.GetMember(ctx, other.ID, m.ID); return e },
		"missing conversation": func() error { _, e := s.GetConversation(ctx, g.ID, "missing"); return e },
		"foreign conversation": func() error { _, e := s.GetConversation(ctx, other.ID, c.ID); return e },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("want ErrNotFound, got %v", err)
			}
		})
	}
	for _, pair := range [][2]string{{"missing", m.ID}, {g.ID, "missing"}, {other.ID, m.ID}} {
		if _, err := s.GetOrCreateConversation(ctx, pair[0], pair[1], "topic"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid conversation reference %v: %v", pair, err)
		}
	}
}

func TestConversationIdentityAcrossConnections(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	m := member(t, s, g.ID)
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, topic := range []string{"", "topic-a", "topic-b"} {
		c, err := s.GetOrCreateConversation(ctx, g.ID, m.ID, topic)
		if err != nil {
			t.Fatal(err)
		}
		again, err := second.GetOrCreateConversation(ctx, g.ID, m.ID, topic)
		if err != nil || !reflect.DeepEqual(c, again) || c.TopicID != topic {
			t.Fatalf("unstable conversation: %+v, %+v, %v", c, again, err)
		}
	}
	type result struct {
		c   domain.Conversation
		err error
	}
	results := make(chan result, 12)
	start := make(chan struct{})
	for i := range 12 {
		go func() {
			<-start
			target := []*Store{s, second}[i%2]
			c, err := target.GetOrCreateConversation(ctx, g.ID, m.ID, "concurrent")
			results <- result{c, err}
		}()
	}
	close(start)
	id := ""
	for range 12 {
		r := <-results
		if r.err != nil || (id != "" && r.c.ID != id) {
			t.Errorf("concurrent identity: %+v, %v", r.c, r.err)
		}
		id = r.c.ID
	}
	another := member(t, s, g.ID)
	separate, err := s.GetOrCreateConversation(ctx, g.ID, another.ID, "concurrent")
	if err != nil || separate.ID == id || separate.MemberID != another.ID || separate.GroupID != g.ID {
		t.Fatalf("different members shared a conversation: %+v, %v", separate, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM conversations").Scan(&count); err != nil || count != 5 {
		t.Fatalf("topic uniqueness failed: %d, %v", count, err)
	}
}

func TestDatabaseEnforcesOwnershipAndCreatorIndependence(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	creator := member(t, s, g.ID)
	child, err := s.CreateMember(ctx, CreateMemberInput{
		GroupID: g.ID, Name: "child", Role: "helper", CreatorID: creator.ID,
		OwnerID: "user-owner", Workspace: creator.Workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.GetOrCreateConversation(ctx, g.ID, child.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO members(id, group_id, name, role, created_at) VALUES('bad', 'missing', 'n', 'r', '')", nil},
		{"INSERT INTO conversations(id, group_id, member_id, topic_id, created_at) VALUES('bad', ?, ?, '', '')", []any{other.ID, child.ID}},
		{"INSERT INTO conversations(id, group_id, member_id, topic_id, created_at) VALUES('duplicate', ?, ?, '', '')", []any{g.ID, child.ID}},
		{"INSERT INTO conversations(id, group_id, member_id, topic_id, created_at) VALUES('null-topic', ?, ?, NULL, '')", []any{g.ID, child.ID}},
		{"UPDATE members SET group_id = ? WHERE id = ?", []any{other.ID, child.ID}},
		{"UPDATE members SET status = 'invented' WHERE id = ?", []any{child.ID}},
	} {
		if _, err := s.db.Exec(statement.query, statement.args...); err == nil {
			t.Fatalf("constraint accepted: %s", statement.query)
		}
	}
	if child.CreatorID != creator.ID || child.OwnerID != "user-owner" || child.Workspace != creator.Workspace {
		t.Fatalf("creator, owner and workspace did not remain independent: %+v", child)
	}
	if _, err := s.db.Exec("UPDATE members SET status = 'left' WHERE id = ?", creator.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMember(ctx, g.ID, child.ID)
	if err != nil || !reflect.DeepEqual(got, child) {
		t.Fatalf("creator leaving changed child: %+v, %v", got, err)
	}
	if _, err := s.GetConversation(ctx, g.ID, c.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("CREATE TABLE members(legacy TEXT)"); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil || s != nil {
		t.Fatalf("expected migration failure, got %v, %v", s, err)
	}
	var count int
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('groups', 'schema_migrations')").Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration was partially committed: %d, %v", count, err)
	}
}

func TestUnknownMigrationIsRejected(t *testing.T) {
	s, path := openTestStore(t)
	if _, err := s.db.Exec("UPDATE schema_migrations SET version = 99 WHERE version = (SELECT max(version) FROM schema_migrations)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if reopened, err := Open(path); err == nil || reopened != nil {
		t.Fatalf("unknown schema accepted: %v, %v", reopened, err)
	}
}

func TestValidationCancellationAndClose(t *testing.T) {
	for _, path := range []string{"", " \t", t.TempDir(), filepath.Join(t.TempDir(), "absent", "core.sqlite")} {
		if s, err := Open(path); err == nil || s != nil {
			t.Fatalf("invalid path %q: %v, %v", path, s, err)
		}
	}
	badPath := filepath.Join(t.TempDir(), "corrupt.sqlite")
	if err := os.WriteFile(badPath, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(badPath); err == nil || s != nil {
		t.Fatalf("corrupt database accepted: %v, %v", s, err)
	}
	s, _ := openTestStore(t)
	g := group(t, s)
	for _, input := range []CreateGroupInput{{}, {Name: " \t"}} {
		if _, err := s.CreateGroup(ctx, input); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid group: %v", err)
		}
	}
	for _, input := range []CreateMemberInput{{GroupID: g.ID, Role: "r"}, {GroupID: g.ID, Name: "n", Role: " "}} {
		if _, err := s.CreateMember(ctx, input); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid member: %v", err)
		}
	}
	for _, limit := range []int{-1, 201} {
		if _, err := s.ListGroups(ctx, ListOptions{Limit: limit}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid group limit: %v", err)
		}
		if _, err := s.ListMembers(ctx, g.ID, ListOptions{Limit: limit}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid member limit: %v", err)
		}
	}
	for _, cursor := range []*Cursor{{ID: "id"}, {CreatedAt: time.Now()}, {ID: "id", CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}} {
		if _, err := s.ListGroups(ctx, ListOptions{Before: cursor}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid group cursor: %v", err)
		}
		if _, err := s.ListMembers(ctx, g.ID, ListOptions{Before: cursor}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid member cursor: %v", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetGroup(cancelled, g.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGroup(ctx, g.ID); err == nil {
		t.Fatal("closed store remained usable")
	}
}
