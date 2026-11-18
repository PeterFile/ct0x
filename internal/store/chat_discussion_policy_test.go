package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
)

func requireDiscussionPolicyFixture(t *testing.T, s *Store) {
	t.Helper()
	if !slices.Contains(migrations, chatDiscussionPolicySchema) {
		t.Skip("requires the discussion policy migration registration")
	}
	if available, err := s.ChatDiscussionPolicyAvailable(ctx); err != nil || !available {
		t.Fatalf("registered policy unavailable: %t %v", available, err)
	}
}

func setDiscussionPolicy(t *testing.T, s *Store, thread domain.ThreadRef, hops int, expected int64, key string) domain.ChatDiscussionPolicy {
	t.Helper()
	got, err := s.SetChatDiscussionPolicy(ctx, thread, hops, expected, key)
	want := domain.ChatDiscussionPolicy{ThreadRef: thread, Revision: expected + 1, MaxInviteHops: hops, MaxTargetedWakes: 2 * hops}
	if err != nil || got != want {
		t.Fatalf("set policy: %+v %v, want %+v", got, err, want)
	}
	return got
}

func TestChatDiscussionPolicyRevisions(t *testing.T) {
	f := setupMessages(t)
	s := f.s
	requireDiscussionPolicyFixture(t, s)
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: " exact\n话题 "}
	defaultPolicy := domain.ChatDiscussionPolicy{ThreadRef: thread, MaxInviteHops: 2, MaxTargetedWakes: 4}
	if got, err := s.GetChatDiscussionPolicy(ctx, thread); err != nil || got != defaultPolicy {
		t.Fatalf("unset policy: %+v %v", got, err)
	}
	before := messageSequenceSnapshot(t, s)
	delete(before, "mutation_idempotency")
	delete(before, "chat_discussion_policy_revisions")
	first := setDiscussionPolicy(t, s, thread, 1, 0, "first")
	second := setDiscussionPolicy(t, s, thread, 0, 1, "second")
	if replay := setDiscussionPolicy(t, s, thread, 1, 0, "first"); replay != first {
		t.Fatalf("replay returned a different revision: %+v", replay)
	}
	for _, attempt := range []struct {
		topic, key string
		hops       int
		expected   int64
	}{
		{thread.TopicID, "stale", 2, 1},
		{thread.TopicID, "first", 0, 0},
		{thread.TopicID, "first", 1, 2},
		{"other topic", "first", 1, 0},
	} {
		if _, err := s.SetChatDiscussionPolicy(ctx, domain.ThreadRef{GroupID: thread.GroupID, TopicID: attempt.topic},
			attempt.hops, attempt.expected, attempt.key); !errors.Is(err, ErrConflict) {
			t.Fatalf("expected conflict for %+v: %v", attempt, err)
		}
	}
	other := domain.ThreadRef{GroupID: f.otherID, TopicID: thread.TopicID}
	setDiscussionPolicy(t, s, other, 1, 0, "first") // Keys are group-scoped.
	for _, stmt := range []string{
		"UPDATE chat_discussion_policy_revisions SET max_invite_hops=2 WHERE group_id=? AND topic_id=? AND revision=1",
		"DELETE FROM chat_discussion_policy_revisions WHERE group_id=? AND topic_id=? AND revision=1",
		"INSERT OR REPLACE INTO chat_discussion_policy_revisions VALUES(?,?,1,2)",
	} {
		if _, err := s.db.Exec(stmt, thread.GroupID, thread.TopicID); err == nil {
			t.Fatalf("revision was mutable: %s", stmt)
		}
	}
	coordinationDecideExec(t, s, `CREATE TRIGGER fail_discussion_policy_key BEFORE INSERT ON mutation_idempotency
	 WHEN NEW.idempotency_key='rollback' BEGIN SELECT RAISE(ABORT,'synthetic'); END`)
	if _, err := s.SetChatDiscussionPolicy(ctx, thread, 2, 2, "rollback"); err == nil {
		t.Fatal("receipt write failure committed a policy revision")
	}
	if got, err := s.GetChatDiscussionPolicy(ctx, thread); err != nil || got != second {
		t.Fatalf("current policy: %+v %v, want %+v", got, err, second)
	}
	coordinationDecideExec(t, s, "DROP TRIGGER fail_discussion_policy_key")
	setDiscussionPolicy(t, s, thread, 2, 2, "rollback")
	messageSequencePreserved(t, s, before)
}

func TestChatDiscussionPolicyDisabled(t *testing.T) {
	f := setupMessages(t)
	s := f.s
	requireDiscussionPolicyFixture(t, s)
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: " scoped "}
	old := discussionUser(t, f, thread.TopicID, f.a)
	a, ca := discussionArrive(t, s, f.groupID, old.Deliveries[0].ID)
	discussionAssess(t, s, a, ca)
	legacy := MemberToolMessageInput{Content: "请核对原结论。", RecipientIDs: []string{f.b}}
	prior, err := s.SendMessageForChatIntake(ctx, a, ca, legacy, "legacy")
	if err != nil || len(prior.Deliveries) != 1 {
		t.Fatalf("default legacy directed send: %+v %v", prior, err)
	}
	setDiscussionPolicy(t, s, thread, 0, 0, "disable")
	if replay, err := s.SendMessageForChatIntake(ctx, a, ca, legacy, "legacy"); err != nil || !reflect.DeepEqual(replay, prior) {
		t.Fatalf("previous directed send no longer replays: %+v %v", replay, err)
	}

	origin := discussionUser(t, f, thread.TopicID, f.b)
	if len(origin.Deliveries) != 1 || origin.Deliveries[0].RecipientID != f.b {
		t.Fatalf("local-user direct delivery was changed: %+v", origin)
	}
	b, cb := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
	discussionAssess(t, s, b, cb)
	invite := domain.ChatDiscussionAction{Kind: domain.ChatDiscussionInvite, Content: "请核对结论。", RecipientID: f.a}
	legacy.RecipientIDs = []string{f.a}
	before := messageSequenceSnapshot(t, s)
	if _, err := s.ApplyChatDiscussion(ctx, b, cb, invite, "denied"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("zero policy allowed a structured invite: %v", err)
	}
	if _, err := s.SendMessageForChatIntake(ctx, b, cb, legacy, "denied"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("zero policy allowed a fresh directed send: %v", err)
	}
	messageSequencePreserved(t, s, before)
	public, err := s.SendMessageForChatIntake(ctx, b, cb, MemberToolMessageInput{Content: "公开讨论仍然可见。"}, "public")
	if err != nil || public.ID == "" || len(public.Deliveries) != 0 {
		t.Fatalf("zero policy suppressed public discussion: %+v %v", public, err)
	}
	discussionEnd(t, s, b)
	origin = discussionUser(t, f, thread.TopicID, f.b)
	b, cb = discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
	discussionAssess(t, s, b, cb)
	resolve := discussionApply(t, s, b, cb, "public-resolution", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionResolve, Content: "无需邀请，公开结论。"})
	if len(resolve.Message.Deliveries) != 0 {
		t.Fatal("public resolution caused a wake")
	}
	checkDiscussionCaps(t, s, origin.ID, 0)
	discussionEnd(t, s, b)

	other := messageFixture{s: s, groupID: f.otherID, a: f.foreign, b: member(t, s, f.otherID).ID}
	for _, route := range []struct {
		fixture messageFixture
		topic   string
		root    string
		target  string
	}{
		{f, "scoped", f.b, f.a},
		{other, thread.TopicID, other.a, other.b},
	} {
		m := discussionUser(t, route.fixture, route.topic, route.root)
		scope, c := discussionArrive(t, s, m.GroupID, m.Deliveries[0].ID)
		discussionAssess(t, s, scope, c)
		invite.RecipientID = route.target
		r := discussionApply(t, s, scope, c, "isolated", invite)
		if r.Message.GroupID != m.GroupID || r.Message.TopicID != m.TopicID || r.Event.RecipientID != route.target {
			t.Fatalf("invitation escaped bound scope: %+v", r)
		}
		checkDiscussionCaps(t, s, m.ID, 2)
	}
}

func TestChatDiscussionPolicySnappedCaps(t *testing.T) {
	f := setupMessages(t)
	s := f.s
	requireDiscussionPolicyFixture(t, s)
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: "one-hop"}
	origin := discussionUser(t, f, thread.TopicID, f.a)
	a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
	discussionAssess(t, s, a, ca)
	setDiscussionPolicy(t, s, thread, 1, 0, "one") // The policy is read at publication, not capture.
	invite := domain.ChatDiscussionAction{Kind: domain.ChatDiscussionInvite, Content: "请核对结论。", RecipientID: f.b}
	first := discussionApply(t, s, a, ca, "invite", invite)
	checkDiscussionCaps(t, s, origin.ID, 1)
	setDiscussionPolicy(t, s, thread, 2, 1, "two")
	b, cb := discussionArrive(t, s, f.groupID, first.Event.DeliveryID)
	discussionAssess(t, s, b, cb)
	delegate := domain.ChatDiscussionAction{Kind: domain.ChatDiscussionInvite, Content: "再请一位同群成员。", RecipientID: member(t, s, f.groupID).ID}
	if _, err := s.ApplyChatDiscussion(ctx, b, cb, delegate, "second-hop"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("new revision widened an active chain: %v", err)
	}
	setDiscussionPolicy(t, s, thread, 0, 2, "zero")
	if replay := discussionApply(t, s, a, ca, "invite", invite); !reflect.DeepEqual(replay, first) {
		t.Fatal("new revision changed the original discussion receipt")
	}
	answered := discussionApply(t, s, b, cb, "return", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionReply, Content: "已核对。"})
	if answered.Event.InvitationMessageID != first.Message.ID || answered.Event.DeliveryID == "" || answered.Event.RecipientID != f.a {
		t.Fatalf("new revision stranded the reserved return: %+v", answered)
	}
	discussionEnd(t, s, a)
	a, ca = discussionArrive(t, s, f.groupID, answered.Event.DeliveryID)
	discussionAssess(t, s, a, ca)
	closed := discussionApply(t, s, a, ca, "resolution", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionResolve, Content: "已整理结论。"})
	discussionCheckContext(t, s, a, ca, origin, f.a, "", "", answered.Message.ID, closed.Message.ID, true, first, answered, closed)
	checkDiscussionCaps(t, s, origin.ID, 1)
}

func TestChatDiscussionPolicyEarlierSchema(t *testing.T) {
	const version = 25
	if len(migrations) < version {
		t.Fatalf("missing v%d migration in registry", version)
	}
	// Open without auto-migrating so neither the v26+ markers nor their tables exist.
	s := queueHandle(t, filepath.Join(t.TempDir(), "v25.sqlite"), true)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY CHECK(version > 0))"); err != nil {
		t.Fatal(err)
	}
	for i, migration := range migrations[:version] {
		if _, err := tx.Exec(migration); err != nil {
			t.Fatalf("apply v%d: %v", i+1, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_migrations(version) VALUES(?)", i+1); err != nil {
			t.Fatalf("record v%d: %v", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := s.db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&got); err != nil || got != version {
		t.Fatalf("legacy schema version: %d (%v), want %d", got, err, version)
	}
	if available, err := s.ChatDiscussionPolicyAvailable(ctx); err != nil || available {
		t.Fatalf("earlier schema claimed editable settings: %t %v", available, err)
	}
	groupID := group(t, s).ID
	f := messageFixture{s: s, groupID: groupID, a: member(t, s, groupID).ID, b: member(t, s, groupID).ID}
	origin := discussionUser(t, f, "", f.a)
	a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
	discussionAssess(t, s, a, ca)
	discussionApply(t, s, a, ca, "legacy-default", domain.ChatDiscussionAction{Kind: domain.ChatDiscussionInvite, Content: "请核对结论。", RecipientID: f.b})
	checkDiscussionCaps(t, s, origin.ID, 2)
}

func TestChatDiscussionPolicyMissingMigrationMarker(t *testing.T) {
	for _, test := range []struct {
		name         string
		laterVersion bool
	}{
		{"surviving_table", false},
		{"later_version", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := openTestStore(t) // No policy revisions are needed to detect damage.
			requireDiscussionPolicyFixture(t, s)
			// Isolate each sign of damage, even if other migrations follow this one.
			coordinationDecideExec(t, s, "DELETE FROM schema_migrations WHERE version>=?", chatDiscussionPolicyVersion)
			if test.laterVersion {
				coordinationDecideExec(t, s, "DROP TABLE chat_discussion_policy_revisions")
				coordinationDecideExec(t, s, "INSERT INTO schema_migrations(version) VALUES(?)", chatDiscussionPolicyVersion+1)
			}
			if available, err := s.ChatDiscussionPolicyAvailable(ctx); err == nil || available || errors.Is(err, ErrNotFound) {
				t.Fatalf("missing migration marker must surface a storage error: %t %v", available, err)
			}
			thread := domain.ThreadRef{GroupID: group(t, s).ID}
			if got, err := s.GetChatDiscussionPolicy(ctx, thread); err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("damaged policy read fell back or hid the storage error: %+v %v", got, err)
			}
		})
	}
}

func TestChatDiscussionPolicyDamagedSchema(t *testing.T) {
	f := setupMessages(t)
	s := f.s
	requireDiscussionPolicyFixture(t, s)
	origin := discussionUser(t, f, "", f.a)
	a, ca := discussionArrive(t, s, f.groupID, origin.Deliveries[0].ID)
	discussionAssess(t, s, a, ca)
	coordinationDecideExec(t, s, "ALTER TABLE chat_discussion_policy_revisions RENAME COLUMN max_invite_hops TO damaged")
	before := messageSequenceSnapshot(t, s)
	_, structured := s.ApplyChatDiscussion(ctx, a, ca, domain.ChatDiscussionAction{Kind: domain.ChatDiscussionInvite, Content: "请核对结论。", RecipientID: f.b}, "structured")
	_, directed := s.SendMessageForChatIntake(ctx, a, ca, MemberToolMessageInput{Content: "请核对结论。", RecipientIDs: []string{f.b}}, "legacy")
	for _, err := range []error{structured, directed} {
		if err == nil || errors.Is(err, ErrMemberToolDenied) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
			t.Fatalf("damaged policy schema fell back or hid the failure: %v", err)
		}
	}
	messageSequencePreserved(t, s, before)
}

func checkDiscussionCaps(t *testing.T, s *Store, origin string, hops int) {
	t.Helper()
	var got, wakes int
	if err := s.db.QueryRow("SELECT max_invite_hops,max_targeted_wakes FROM chat_discussions WHERE origin_message_id=?", origin).Scan(&got, &wakes); err != nil || got != hops || wakes != 2*hops {
		t.Fatalf("snapped caps: %d/%d (%v), want %d/%d", got, wakes, err, hops, 2*hops)
	}
}
