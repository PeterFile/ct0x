package platform

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func threadCorrectionFixtureInstalled(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var version int
	var installed bool
	if err := db.QueryRow(`SELECT (SELECT max(version) FROM schema_migrations),
	 EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='thread_corrections')`).Scan(&version, &installed); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture schema v%d, thread corrections installed=%t", version, installed)
	return installed
}

func TestMemberRoleHistoryKeepsStorageScopeAfterCorrection(t *testing.T) {
	f := memberRoleCoordinatorFixture(t)
	installed := threadCorrectionFixtureInstalled(t, f.raw)
	ctx := t.Context()
	worker := f.assignWorker(t, true)
	coordinatorToken := memberRoleIssue(t, f.tools, f.coordinatorScope)
	workerToken := memberRoleIssue(t, f.tools, worker)
	post := func(key string, input MessageInput) domain.Message {
		t.Helper()
		m, err := f.local.CreateMessage(ctx, f.groupID, input, key)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	anchor := post("role-history-anchor", MessageInput{TopicID: f.request.TopicID, Content: "formal topic anchor", RecipientIDs: []string{}})
	otherTopic := "other-legacy-topic"
	if installed {
		// The fixture's existing formal topic predates E1a's topic catalog.
		if _, err := f.raw.Exec(`INSERT INTO thread_routing_topics(group_id,topic_id,title,title_key,issue_id,origin_message_id)
		 VALUES(?,?,'部署审批记录','部署审批记录','REL-71',?)`, f.groupID, anchor.TopicID, anchor.ID); err != nil {
			t.Fatal(err)
		}
		otherTopic = post("other-origin", MessageInput{Content: "新话题：REL-72 镜像回滚规划", RecipientIDs: []string{}}).TopicID
	}
	incoming := post("incoming", MessageInput{TopicID: otherTopic, Content: "scope-marker incoming", RecipientIDs: []string{}})
	outgoing := post("outgoing", MessageInput{TopicID: anchor.TopicID, Content: "scope-marker outgoing", RecipientIDs: []string{}})
	ordinaryAllow, ordinaryDeny := outgoing, incoming
	if installed {
		for _, move := range []struct {
			issue  string
			m      domain.Message
			wanted string
		}{{"REL-71", incoming, anchor.TopicID}, {"REL-72", outgoing, otherTopic}} {
			correction := post(move.issue, MessageInput{Content: "这句话指的是 " + move.issue,
				ReplyToMessageID: move.m.ID})
			if correction.ThreadCorrection == nil || correction.ThreadCorrection.EffectiveTopicID != move.wanted || len(correction.Deliveries) != 0 {
				t.Fatalf("correction fixture did not create a topical move: %+v", correction)
			}
		}
		ordinaryAllow, ordinaryDeny = incoming, outgoing
	}
	// A normal claim uses the same storage scope unless a correction moves a message.
	peer := f.member(t, f.groupID)
	trigger := post("ordinary", MessageInput{Content: "普通群聊", TopicID: anchor.TopicID, RecipientIDs: []string{peer.ID}})
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: trigger.Deliveries[0].ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := f.tools.Issue(ctx, store.TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken},
		MemberToolGrants{Operations: []string{"read_channel", "search_history", "read_history"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.tools.Revoke(ordinary) })
	for _, tc := range []struct {
		name, token string
		allow, deny domain.Message
	}{
		{"coordination", coordinatorToken, outgoing, incoming},
		{"worker", workerToken, outgoing, incoming},
		{"ordinary", ordinary, ordinaryAllow, ordinaryDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := f.tools.ReadChannel(ctx, tc.token, store.ListOptions{})
			has := func(id string) bool {
				return slices.ContainsFunc(page.Items, func(m domain.Message) bool { return m.ID == id })
			}
			if err != nil || !has(tc.allow.ID) || has(tc.deny.ID) || !has(anchor.ID) {
				t.Errorf("read_channel chose the wrong history scope: %+v (%v)", page, err)
			}
			hits, err := f.tools.SearchHistory(ctx, tc.token, MemberHistorySearchInput{Query: "scope-marker"})
			if err != nil || len(hits.Items) != 1 || hits.Items[0].MessageID != tc.allow.ID {
				t.Errorf("search_history chose the wrong history scope: %+v (%v)", hits, err)
			}
			if got, err := f.tools.ReadHistory(ctx, tc.token, MemberHistoryReadInput{MessageID: tc.allow.ID}); err != nil || got.Content != tc.allow.Content {
				t.Errorf("read_history lost its visible original: %+v (%v)", got, err)
			}
			if got, err := f.tools.ReadHistory(ctx, tc.token, MemberHistoryReadInput{MessageID: tc.deny.ID}); !errors.Is(err, store.ErrNotFound) || got.MessageID != "" {
				t.Errorf("read_history disclosed an original outside its view: %+v (%v)", got, err)
			}
		})
	}
}
