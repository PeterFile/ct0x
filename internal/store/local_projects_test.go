package store

import (
	"errors"
	"strings"
	"testing"
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
