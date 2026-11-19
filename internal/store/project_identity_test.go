package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func projectIdentityVersion(t testing.TB) int {
	t.Helper()
	version := slices.Index(migrations, projectIdentitySchema) + 1
	if version <= 30 || slices.Contains(migrations[version:], projectIdentitySchema) {
		t.Fatal("expected one project identity migration after the frozen v30 prefix")
	}
	return version
}

func openProjectIdentityPrefix(t testing.TB, path string, version int) *Store {
	t.Helper()
	all := migrations
	migrations = all[:version]
	defer func() { migrations = all }()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func projectIdentityLocation(t *testing.T, s *Store, groupID string) GroupOwnership {
	t.Helper()
	got, err := s.GetGroupOwnership(ctx, groupID)
	if err != nil || got.GroupID != groupID || got.WorkspaceID == "" || got.ProjectID == "" || got.ChannelID == "" ||
		got.ProjectID == groupID || got.ChannelID == groupID || got.ChannelID == got.ProjectID {
		t.Fatalf("invalid location for %q: %+v, %v", groupID, got, err)
	}
	return got
}

func assertProjectIdentityMissing(t *testing.T, s *Store, groupID string) {
	t.Helper()
	if got, err := s.GetGroupOwnership(ctx, groupID); got != (GroupOwnership{}) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("location for %q: %+v, %v; want ErrNotFound", groupID, got, err)
	}
}

func assertProjectIdentityCounts(t *testing.T, s *Store, groups, projects int) {
	t.Helper()
	var got [4]int
	want := [4]int{1, groups, projects, groups}
	err := s.db.QueryRow(`SELECT (SELECT count(*) FROM workspaces WHERE is_default=1), (SELECT count(*) FROM groups),
		(SELECT count(*) FROM projects), (SELECT count(*) FROM channels)`).Scan(&got[0], &got[1], &got[2], &got[3])
	if err != nil || got != want {
		t.Fatalf("default workspace/group/project/channel counts: %v, %v; want %v", got, err, want)
	}
}

func TestProjectIdentityUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	version := projectIdentityVersion(t)
	old := openProjectIdentityPrefix(t, path, version-1)
	g := group(t, old)
	input := keyedGroup()
	input.Name, input.Description = g.Name, g.Description
	other, err := old.CreateGroup(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	m := member(t, old, g.ID)
	grant := AuthorizeWorkspaceGrantInput{GroupID: g.ID, MemberID: m.ID, Root: t.TempDir(), Scope: "read_only", IdempotencyKey: "legacy-grant"}
	if _, err := old.AuthorizeWorkspaceGrant(ctx, grant, workspaceGrantValidation(grant)); err != nil {
		t.Fatal(err)
	}
	delivery := queueDelivery(t, old, g.ID, m.ID, "topic")
	if _, err := old.ClaimTurn(ctx, ClaimTurnInput{GroupID: g.ID, DeliveryID: delivery.ID, MaxActive: 1, ResourceKeys: []string{"legacy-held"}}); err != nil {
		t.Fatal(err)
	}
	task := createTestTask(t, old, g.ID)
	artifact := artifactPublishInput(g.ID)
	artifact.SourceTaskID, artifact.IdempotencyKey = task.ID, "legacy-artifact"
	mustPublishArtifact(t, old, artifact)
	before := messageSequenceSnapshot(t, old)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s := openProjectIdentityPrefix(t, path, version)
	var workspace, project, channel string
	err = s.db.QueryRow(`SELECT w.id, p.id, c.id FROM groups g
		JOIN channels c ON c.legacy_group_id=g.id
		JOIN projects p ON p.id=c.project_id
		JOIN workspaces w ON w.id=p.workspace_id AND w.is_default=1
		WHERE g.id=?`, g.ID).Scan(&workspace, &project, &channel)
	if err != nil || workspace == "" || project == "" || channel == "" {
		t.Fatalf("upgrade did not persist a default workspace and mapped project/channel: %q/%q/%q, %v", workspace, project, channel, err)
	}
	first, second := projectIdentityLocation(t, s, g.ID), projectIdentityLocation(t, s, other.ID)
	if first != (GroupOwnership{WorkspaceID: workspace, ProjectID: project, ChannelID: channel, GroupID: g.ID}) ||
		second.WorkspaceID != workspace || second.ProjectID == project || second.ChannelID == channel {
		t.Fatalf("same-name legacy Groups merged or mapped incorrectly: %+v, %+v", first, second)
	}
	var copied int
	err = s.db.QueryRow(`SELECT count(*) FROM groups g JOIN channels c ON c.legacy_group_id=g.id
		JOIN projects p ON p.id=c.project_id
		WHERE c.name=g.name AND p.name=g.name AND c.description=g.description AND p.description=g.description
		AND c.created_at=g.created_at AND p.created_at=g.created_at`).Scan(&copied)
	if err != nil || copied != 2 {
		t.Fatalf("legacy display data was not copied: %d, %v", copied, err)
	}
	messageSequenceLedger(t, s, version)
	assertProjectIdentityCounts(t, s, 2, 2)
	messageSequencePreserved(t, s, before)
	dispatchAssertFK(t, s)
	if _, err := s.GetConversation(ctx, other.ID, delivery.ConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("same workspace granted cross-group conversation access: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openProjectIdentityPrefix(t, path, version)
	if got, err := s.CreateGroup(ctx, input); err != nil || got != other {
		t.Fatalf("legacy key no longer replays: %+v, %v", got, err)
	}
	if projectIdentityLocation(t, s, g.ID) != first || projectIdentityLocation(t, s, other.ID) != second {
		t.Fatal("reopen changed legacy ownership")
	}
	messageSequencePreserved(t, s, before)
}

func TestProjectIdentityCreateReplay(t *testing.T) {
	s, path := openTestStore(t)
	assertProjectIdentityCounts(t, s, 0, 0)
	var workspace string
	if err := s.db.QueryRow(`SELECT id FROM workspaces WHERE is_default=1`).Scan(&workspace); err != nil {
		t.Fatal(err)
	}
	input := keyedGroup()
	g, err := s.CreateGroup(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	first := projectIdentityLocation(t, s, g.ID)
	if first.WorkspaceID != workspace {
		t.Fatal("legacy create did not use the persisted default workspace")
	}
	for _, id := range []string{"missing", g.ID + " ", first.WorkspaceID, first.ProjectID, first.ChannelID} {
		assertProjectIdentityMissing(t, s, id)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = queueHandle(t, path, false)
	if got, err := s.CreateGroup(ctx, input); err != nil || got != g {
		t.Fatalf("keyed replay changed Group: %+v, %v", got, err)
	}
	changed := input
	changed.Description += " "
	if got, err := s.CreateGroup(ctx, changed); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("changed payload reused key: %+v, %v", got, err)
	}
	if got := projectIdentityLocation(t, s, g.ID); got != first {
		t.Fatalf("replay changed ownership: %+v; want %+v", got, first)
	}
	assertProjectIdentityCounts(t, s, 1, 1)
	assertMutationCount(t, s, 1)
}

func TestProjectIdentityMigrationAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.sqlite")
	version := projectIdentityVersion(t)
	old := openProjectIdentityPrefix(t, path, version-1)
	g := group(t, old)
	before := messageSequenceSnapshot(t, old)
	coordinationDecideExec(t, old, fmt.Sprintf(`CREATE TRIGGER fail_identity_ledger BEFORE INSERT ON schema_migrations
		WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected identity ledger failure'); END`, version))
	const objects = `SELECT type, name, sql FROM sqlite_schema ORDER BY name`
	schema := messageSequenceRows(t, old.db, objects)
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("migration committed despite failing its version write")
	} else if !strings.Contains(err.Error(), "injected identity ledger failure") {
		t.Fatalf("migration did not reach its version write: %v", err)
	}
	messageSequenceLedger(t, old, version-1)
	messageSequencePreserved(t, old, before)
	if got := messageSequenceRows(t, old.db, objects); !reflect.DeepEqual(got, schema) {
		t.Fatal("failed migration leaked schema objects")
	}
	coordinationDecideExec(t, old, "DROP TRIGGER fail_identity_ledger")
	s := openProjectIdentityPrefix(t, path, version)
	projectIdentityLocation(t, s, g.ID)
	messageSequenceLedger(t, s, version)
}

func TestProjectIdentityCreateAtomic(t *testing.T) {
	s, _ := openTestStore(t)
	input := keyedGroup()
	for _, fault := range []string{"RAISE(ABORT, 'injected channel write failure')", "RAISE(IGNORE)"} {
		coordinationDecideExec(t, s, `CREATE TRIGGER fail_identity_channel BEFORE INSERT ON channels BEGIN SELECT `+fault+`; END`)
		if got, err := s.CreateGroup(ctx, input); err == nil || got.ID != "" {
			t.Fatalf("%s allowed a partial Group: %+v, %v", fault, got, err)
		}
		assertProjectIdentityCounts(t, s, 0, 0)
		assertMutationCount(t, s, 0)
		coordinationDecideExec(t, s, "DROP TRIGGER fail_identity_channel")
	}
	g, err := s.CreateGroup(ctx, input)
	if err != nil {
		t.Fatalf("failed write burned the legacy key: %v", err)
	}
	projectIdentityLocation(t, s, g.ID)
	assertProjectIdentityCounts(t, s, 1, 1)
	assertMutationCount(t, s, 1)
}

func TestProjectIdentityExplicitChannel(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	first := projectIdentityLocation(t, s, g.ID)
	m := member(t, s, g.ID)
	const groupID, channelID = " group:opaque|群\tinside ", "explicit-channel"
	const insert = `INSERT INTO channels(id, project_id, legacy_group_id, name, created_at) VALUES(?,?,?,'channel',?)`
	stamp := g.CreatedAt.Format(timestampLayout)
	coordinatorMustCommit(t, s,
		coordinatorSQL{insert, []any{channelID, first.ProjectID, groupID, stamp}},
		coordinatorSQL{`INSERT INTO groups(id, name, created_at) VALUES(?,'legacy',?)`, []any{groupID, stamp}})
	got := projectIdentityLocation(t, s, groupID)
	if got != (GroupOwnership{WorkspaceID: first.WorkspaceID, ProjectID: first.ProjectID, ChannelID: channelID, GroupID: groupID}) {
		t.Fatalf("explicit Channel was replaced or ignored: %+v", got)
	}
	assertProjectIdentityMissing(t, s, strings.TrimSpace(groupID))
	if _, err := s.GetMember(ctx, groupID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("shared Project granted cross-Channel membership: %v", err)
	}
	t.Run("cannot commit a dangling Channel", func(t *testing.T) {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			t.Fatal(err)
		}
		defer conn.ExecContext(ctx, "ROLLBACK")
		if _, err := conn.ExecContext(ctx, insert, "dangling", first.ProjectID, "missing-group", stamp); err != nil {
			t.Fatalf("legacy Group FK was not deferred: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err == nil {
			t.Fatal("dangling Channel committed")
		}
	})
	assertProjectIdentityCounts(t, s, 2, 1)
	dispatchAssertFK(t, s)
}

func TestProjectIdentityImmutable(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	a, b := projectIdentityLocation(t, s, g.ID), projectIdentityLocation(t, s, other.ID)
	coordinationDecideExec(t, s, "PRAGMA recursive_triggers=OFF")
	coordinationDecideExec(t, s, `INSERT INTO workspaces(id, name) VALUES('other-workspace', 'other')`)
	before := messageSequenceSnapshot(t, s)
	for _, attempt := range []coordinatorSQL{
		{`INSERT OR REPLACE INTO workspaces(id, name, is_default) VALUES('new-default','replacement',1)`, nil},
		{`INSERT OR REPLACE INTO workspaces(id, name) VALUES(?,'replacement')`, []any{a.WorkspaceID}},
		{`UPDATE OR REPLACE workspaces SET is_default=1 WHERE id='other-workspace'`, nil},
		{`DELETE FROM workspaces WHERE id=?`, []any{a.WorkspaceID}},
		{`UPDATE projects SET workspace_id='other-workspace' WHERE id=?`, []any{a.ProjectID}},
		{`INSERT OR REPLACE INTO projects(id, workspace_id, name, created_at, legacy_origin_group_id)
			SELECT 'new-project', workspace_id, name, created_at, legacy_origin_group_id FROM projects WHERE id=?`, []any{a.ProjectID}},
		{`DELETE FROM projects WHERE id=?`, []any{a.ProjectID}},
		{`UPDATE channels SET project_id=? WHERE id=?`, []any{b.ProjectID, a.ChannelID}},
		{`UPDATE OR REPLACE channels SET legacy_group_id=? WHERE id=?`, []any{b.GroupID, a.ChannelID}},
		{`UPDATE channels SET id='new-channel' WHERE id=?`, []any{a.ChannelID}},
		{`INSERT OR REPLACE INTO channels(id, project_id, legacy_group_id, name, created_at)
			SELECT 'new-channel', ?, legacy_group_id, name, created_at FROM channels WHERE id=?`, []any{b.ProjectID, a.ChannelID}},
		{`DELETE FROM channels WHERE id=?`, []any{a.ChannelID}},
		{`UPDATE groups SET id='new-group' WHERE id=?`, []any{a.GroupID}},
		{`DELETE FROM groups WHERE id=?`, []any{a.GroupID}},
		{`INSERT OR REPLACE INTO groups(id, name, created_at)
			SELECT id, 'replacement', created_at FROM groups WHERE id=?`, []any{a.GroupID}},
		{`INSERT OR REPLACE INTO groups(rowid, id, name, created_at)
			SELECT rowid, 'new-group', 'replacement', created_at FROM groups WHERE id=?`, []any{a.GroupID}},
	} {
		if _, err := s.db.Exec(attempt.query, attempt.args...); err == nil {
			t.Fatalf("immutable identity accepted %s", attempt.query)
		}
	}
	messageSequencePreserved(t, s, before)
	dispatchAssertFK(t, s)
}

func TestProjectIdentityMissingChainFailsClosed(t *testing.T) {
	s, _ := openTestStore(t)
	g, other := group(t, s), group(t, s)
	location := projectIdentityLocation(t, s, other.ID)
	// Simulate out-of-band damage, not a supported write path.
	coordinationDecideExec(t, s, "PRAGMA foreign_keys=OFF; DROP TRIGGER projects_no_delete")
	coordinationDecideExec(t, s, "DELETE FROM groups WHERE id=?", g.ID)
	coordinationDecideExec(t, s, "DELETE FROM projects WHERE id=?", location.ProjectID)
	coordinationDecideExec(t, s, "PRAGMA foreign_keys=ON")
	before := messageSequenceSnapshot(t, s)
	assertProjectIdentityMissing(t, s, g.ID)
	assertProjectIdentityMissing(t, s, other.ID)
	messageSequencePreserved(t, s, before)
}

func BenchmarkProjectIdentityCreateGroup(b *testing.B) {
	version := projectIdentityVersion(b)
	for _, tc := range []struct {
		name    string
		version int
	}{{"legacy", version - 1}, {"mapped", version}} {
		b.Run(tc.name, func(b *testing.B) {
			s := openProjectIdentityPrefix(b, filepath.Join(b.TempDir(), "create.sqlite"), tc.version)
			input := keyedGroup()
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				input.IdempotencyKey = strconv.Itoa(i)
				if _, err := s.CreateGroup(ctx, input); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
