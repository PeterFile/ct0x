package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProjectChannelOwnershipForToolUsesLiveGroup(t *testing.T) {
	f, scope := memberOperationFixture(t)
	bound, err := f.s.GetGroupOwnership(ctx, f.groupID)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.s.GetGroupOwnership(ctx, f.otherID)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := f.s.CreateChannel(ctx, CreateChannelInput{ProjectID: bound.ProjectID, Name: "同项目的其他频道"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetChannelOwnershipForTool(ctx, scope, bound.ProjectID, bound.ChannelID); err != nil || got != bound {
		t.Fatalf("live scope did not resolve its own group: %+v, %v", got, err)
	}
	wrongMember := scope
	wrongMember.MemberID = f.b // A member in the correct group, but not the claimed member.
	wrongTopic := scope
	wrongTopic.TopicID += "-wrong"
	for _, tc := range []struct {
		name                 string
		scope                MemberToolScope
		projectID, channelID string
	}{
		{"missing-scope", MemberToolScope{}, bound.ProjectID, bound.ChannelID},
		{"different-member", wrongMember, bound.ProjectID, bound.ChannelID},
		{"different-topic", wrongTopic, bound.ProjectID, bound.ChannelID},
		{"sibling-channel", scope, bound.ProjectID, sibling.ID},
		{"foreign-channel", scope, foreign.ProjectID, foreign.ChannelID},
		{"wrong-project", scope, foreign.ProjectID, bound.ChannelID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.s.GetChannelOwnershipForTool(ctx, tc.scope, tc.projectID, tc.channelID)
			requireMemberToolDenied(t, err)
			if got != (GroupOwnership{}) {
				t.Fatalf("denied lookup returned ownership: %+v", got)
			}
		})
	}
	if _, err := f.s.RequestTurnCancel(ctx, scope.Claim.GroupID, scope.Claim.TurnID); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.GetChannelOwnershipForTool(ctx, scope, bound.ProjectID, bound.ChannelID)
	requireMemberToolDenied(t, err)
	if got != (GroupOwnership{}) {
		t.Fatalf("revoked scope returned ownership: %+v", got)
	}
}

func TestProjectChannelOwnershipForToolRequiresWorkspace(t *testing.T) {
	f, scope := memberOperationFixture(t)
	bound, err := f.s.GetGroupOwnership(ctx, f.groupID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetChannelOwnershipForTool(ctx, scope, bound.ProjectID, bound.ChannelID); err != nil || got != bound {
		t.Fatalf("fixture must start with a live scope: %+v, %v", got, err)
	}
	// Damage only the Workspace; keep the Project, Group and live claim intact.
	coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF; DROP TRIGGER workspaces_no_delete")
	coordinationDecideExec(t, f.s, "DELETE FROM workspaces WHERE id = ?", bound.WorkspaceID)
	coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
	got, err := f.s.GetChannelOwnershipForTool(ctx, scope, bound.ProjectID, bound.ChannelID)
	requireMemberToolDenied(t, err)
	if got != (GroupOwnership{}) {
		t.Fatalf("dangling project returned ownership: %+v", got)
	}
}

func TestProjectChannelReadsFailClosedOnDanglingGroup(t *testing.T) {
	s, _ := openTestStore(t)
	project, err := s.CreateProject(ctx, CreateProjectInput{Name: "测试项目"})
	if err != nil {
		t.Fatal(err)
	}
	input := CreateChannelInput{ProjectID: project.ID, Name: "告警", IdempotencyKey: "dangling"}
	dangling, err := s.CreateChannel(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel(ctx, CreateChannelInput{ProjectID: project.ID, Name: "讨论"}); err != nil {
		t.Fatal(err)
	}
	// Verify the guard, then bypass it only here to leave the Channel and its
	// receipt intact while simulating out-of-band damage.
	const deleteGroup = "DELETE FROM groups WHERE id = ?"
	coordinationDecideExec(t, s, "PRAGMA foreign_keys=OFF")
	if _, err := s.db.ExecContext(ctx, deleteGroup, dangling.GroupID); err == nil {
		t.Fatal("groups_no_delete allowed deletion")
	}
	coordinationDecideExec(t, s, "DROP TRIGGER groups_no_delete")
	coordinationDecideExec(t, s, deleteGroup, dangling.GroupID)
	coordinationDecideExec(t, s, "PRAGMA foreign_keys=ON")
	t.Run("detail", func(t *testing.T) {
		got, err := s.GetChannel(ctx, project.ID, dangling.ID)
		if !errors.Is(err, ErrNotFound) || got != (ChannelView{}) {
			t.Fatalf("detail returned a dangling channel: %+v, %v", got, err)
		}
	})
	t.Run("list", func(t *testing.T) {
		got, err := s.ListChannels(ctx, project.ID, ListOptions{Limit: 1})
		if !errors.Is(err, ErrNotFound) || got.Items != nil || got.NextCursor != nil {
			t.Fatalf("list must reject a page with a dangling channel: %+v, %v", got, err)
		}
	})
	t.Run("replay", func(t *testing.T) {
		got, err := s.CreateChannel(ctx, input)
		if !errors.Is(err, ErrNotFound) || got != (ChannelView{}) {
			t.Fatalf("replay returned a dangling channel: %+v, %v", got, err)
		}
	})
}

func TestProjectChannelPagePlan(t *testing.T) {
	s, _ := openTestStore(t)
	for _, tc := range []struct {
		name      string
		before    *Cursor
		seekRange string
	}{
		{"head", nil, "(project_id=?)"},
		{"before", &Cursor{CreatedAt: time.Unix(0, 0), ID: "cursor"}, "(project_id=? AND (created_at,id)<(?,?))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, args := channelPageQuery("project", tc.before, 51)
			rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(plan) != 2 || !strings.HasPrefix(plan[0], "SEARCH c ") ||
				!strings.HasSuffix(plan[0], " channels_project_created_id "+tc.seekRange) ||
				!strings.HasPrefix(plan[1], "SEARCH g ") || !strings.HasSuffix(strings.TrimSuffix(plan[1], " LEFT-JOIN"), " (id=?)") {
				t.Fatalf("want project index range %s followed by a Group PK lookup, without scans or sorting: %q", tc.seekRange, plan)
			}
		})
	}
}

func TestProjectDirectoryProbeRequiresDefaultWorkspaceWithoutKey(t *testing.T) {
	s, _ := openTestStore(t)
	dir := t.TempDir()
	coordinationDecideExec(t, s, "PRAGMA foreign_keys=OFF; DROP TRIGGER workspaces_no_delete")
	coordinationDecideExec(t, s, "DELETE FROM workspaces WHERE is_default = 1")
	coordinationDecideExec(t, s, "PRAGMA foreign_keys=ON")

	input := CreateProjectInput{Name: "目录项目", Directory: &dir}
	got, err := s.createProject(ctx, input, func(string) (os.FileInfo, error) {
		t.Error("directory probe ran without a default Workspace")
		return nil, os.ErrNotExist
	})
	if !errors.Is(err, ErrNotFound) || got != (ProjectView{}) {
		t.Fatalf("missing workspace: %+v, %v", got, err)
	}
}

func TestProjectDirectoryProbeDoesNotHoldStore(t *testing.T) {
	s, _ := openTestStore(t)
	dir := t.TempDir()
	input := CreateProjectInput{Name: "目录项目", Directory: &dir, IdempotencyKey: "pending"}
	probeCtx, cancel := context.WithCancel(ctx)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var probeErr error
	go func() {
		defer close(done)
		_, probeErr = s.createProject(probeCtx, input, func(string) (os.FileInfo, error) {
			close(started)
			<-release
			return nil, os.ErrNotExist
		})
	}()
	defer func() {
		cancel()
		close(release)
		<-done
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("directory probe did not start")
	}

	// The probe has not returned, but a directoryless write can use the
	// single Store connection and reserve this still-unclaimed key.
	writeCtx, stopWrite := context.WithTimeout(ctx, 2*time.Second)
	defer stopWrite()
	if _, err := s.CreateProject(writeCtx, CreateProjectInput{Name: "无需目录", IdempotencyKey: input.IdempotencyKey}); err != nil {
		t.Errorf("directory probe blocked Store progress: %v", err)
	}
	cancel()
	select {
	case <-done:
		if !errors.Is(probeErr, context.Canceled) {
			t.Errorf("canceled directory check returned %v", probeErr)
		}
	case <-time.After(2 * time.Second):
		t.Error("directory probe blocked cancellation")
	}
}

func TestProjectDirectoryRechecksReceiptAfterProbe(t *testing.T) {
	s, _ := openTestStore(t)
	dir := t.TempDir()
	input := CreateProjectInput{Name: "目录项目", Directory: &dir, IdempotencyKey: "race"}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	type outcome struct {
		project ProjectView
		err     error
	}
	committed := make(chan outcome, 1)
	project, err := s.createProject(callCtx, input, func(string) (os.FileInfo, error) {
		other, err := s.CreateProject(callCtx, input)
		committed <- outcome{other, err}
		return nil, os.ErrNotExist // The directory may vanish after the competing request checks it.
	})
	if err != nil || project.ID == "" {
		t.Fatalf("matching receipt must win over a failed probe: %+v, %v", project, err)
	}
	select {
	case other := <-committed:
		if other.err != nil || other.project.ID != project.ID {
			t.Fatalf("probe race did not replay the competing request: %+v, %v", other.project, other.err)
		}
	default:
		t.Fatal("directory probe did not run")
	}

	noProbe := func(string) (os.FileInfo, error) {
		t.Error("a committed receipt must be resolved before any filesystem probe")
		return nil, os.ErrNotExist
	}
	if got, err := s.createProject(ctx, input, noProbe); err != nil || got.ID != project.ID {
		t.Fatalf("committed replay: %+v, %v", got, err)
	}
	input.Description = "变更内容"
	if got, err := s.createProject(ctx, input, noProbe); !errors.Is(err, ErrConflict) || got != (ProjectView{}) {
		t.Fatalf("changed payload: %+v, %v", got, err)
	}
}

func TestProjectChannelCreationIsAtomic(t *testing.T) {
	s, _ := openTestStore(t)
	project, err := s.CreateProject(ctx, CreateProjectInput{Name: "目录无关的项目"})
	if err != nil {
		t.Fatal(err)
	}
	input := CreateChannelInput{ProjectID: project.ID, Name: "告警", IdempotencyKey: "retry"}
	// Fail after the group row has been inserted and only if the channel and
	// reservation were already written. RAISE(FAIL) leaves the rollback to us.
	if _, err := s.db.Exec(`CREATE TRIGGER test_fail_channel_group AFTER INSERT ON groups
	 WHEN NEW.name = '告警' AND EXISTS(
	    SELECT 1 FROM channels c JOIN mutation_idempotency r ON r.record_id = c.id
	     WHERE c.legacy_group_id = NEW.id AND r.operation = 'create_channel' AND r.idempotency_key = 'retry')
	 BEGIN SELECT RAISE(FAIL, 'injected channel group failure'); END`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CreateChannel(ctx, input); err == nil || !strings.Contains(err.Error(), "injected channel group failure") || got != (ChannelView{}) {
		t.Fatalf("injected failure returned %+v, %v", got, err)
	}
	counts := func(want int) {
		t.Helper()
		for _, table := range []string{"projects", "channels", "groups", "mutation_idempotency"} {
			expected := want
			if table == "projects" {
				expected = 1
			}
			var got int
			if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != expected {
				t.Fatalf("%s count = %d, want %d: %v", table, got, expected, err)
			}
		}
	}
	counts(0)
	if _, err := s.db.Exec("DROP TRIGGER test_fail_channel_group"); err != nil {
		t.Fatal(err)
	}
	channel, err := s.CreateChannel(ctx, input)
	if err != nil || channel.ProjectID != project.ID || channel.ID == "" || channel.GroupID == "" {
		t.Fatalf("retry could not create the exact channel: %+v, %v", channel, err)
	}
	if got, err := s.CreateChannel(ctx, input); err != nil || got != channel {
		t.Fatalf("committed retry did not replay: %+v, %v", got, err)
	}
	counts(1)
	var redundant int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_schema
	 WHERE name GLOB 'project_channel_order*' OR name = 'local_projects_workspace_page'`).Scan(&redundant); err != nil || redundant != 0 {
		t.Fatalf("redundant paging objects = %d, want 0: %v", redundant, err)
	}
}

func TestProjectChannelScopePrecedesReceiptLookup(t *testing.T) {
	s, _ := openTestStore(t)
	// A stale receipt with a different payload must not disclose a conflict
	// under a project that isn't available to this entry point.
	if _, err := s.db.Exec(`INSERT INTO mutation_idempotency(operation, actor_id, group_id, idempotency_key, payload, record_id)
	 VALUES('create_channel', 'user', 'missing-project', 'existing-key', ?, 'opaque-id')`, mutationPayload("different payload")); err != nil {
		t.Fatal(err)
	}
	got, err := s.CreateChannel(ctx, CreateChannelInput{ProjectID: "missing-project", Name: "告警", IdempotencyKey: "existing-key"})
	if !errors.Is(err, ErrNotFound) || got != (ChannelView{}) {
		t.Fatalf("missing project disclosed a receipt: %+v, %v", got, err)
	}
}
