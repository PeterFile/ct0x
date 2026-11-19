package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

// Hold the existing offline Pi fixture after acceptance so the normal startup,
// scheduler, config issuance and native member-tool routes remain under test.
func TestPiDiscussionRelayNativeHandoff(t *testing.T) {
	f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
	script, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	const hold = `while [ -f "$root/hold" ]; do sleep 0.05; done`
	if bytes.Count(script, []byte(hold)) != 1 {
		t.Fatal("fixture no longer exposes a hold point")
	}
	gate := `printf '%s\n' "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG" > "$root/config-$run"
while [ ! -f "$root/release-$run" ]; do sleep 0.05; done`
	if err := os.WriteFile(f.path, bytes.Replace(script, []byte(hold), []byte(gate), 1), 0700); err != nil {
		t.Fatal(err)
	}
	args := append(memberToolsArgs(t, dir, f.path), "--enable-coordination-roles")
	base, stop := startRun(t, args...)
	selected := executionProvider(t, base)
	var group domain.Group
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "普通讨论接力"}, &group)
	groupURL := base + "/api/v1/groups/" + group.ID
	var lead, peer domain.Member
	for _, member := range []*domain.Member{&lead, &peer} {
		executionJSON(t, "POST", groupURL+"/members", platform.MemberInput{
			Name: "讨论成员", Role: "核对证据", ProviderID: selected.ID, Model: "fixture-model",
		}, member)
	}
	var source domain.Message
	coordinationCompositionHTTP(t, "POST", groupURL+"/messages", map[string]any{
		"content": "只讨论：哪条证据支持这个结论？", "topic_id": "relay",
	}, "relay-origin", 201, &source)
	if len(source.Deliveries) != 1 || source.Deliveries[0].RecipientID != lead.ID {
		t.Fatal("ordinary message did not default to one existing member")
	}
	db, err := store.Open(filepath.Join(dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	bind := func(messageID, recipient string) (string, memberToolsNativeConfig) {
		t.Helper()
		var runID, path string
		awaitExecution(t, func() bool {
			message, err := db.GetMessage(ctx, group.ID, messageID)
			if err != nil || len(message.Deliveries) != 1 || message.Deliveries[0].RecipientID != recipient {
				return false
			}
			runID = message.Deliveries[0].TurnID
			turn, err := db.GetTurn(ctx, group.ID, runID)
			if err != nil || turn.State != domain.TurnStateRunning {
				return false
			}
			data, _ := os.ReadFile(filepath.Join(f.root, "config-"+runID))
			path = strings.TrimSpace(string(data))
			return filepath.Dir(path) == filepath.Join(dir, "member-tools")
		})
		var config memberToolsNativeConfig
		memberToolsReadJSON(t, path, &config)
		if config.Endpoint != base+"/api/v1/member-tools" || config.Token == "" {
			t.Fatal("ordinary Pi did not receive a live member tool capability")
		}
		return runID, config
	}
	release := func(runID string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.root, "release-"+runID), nil, 0600); err != nil {
			t.Fatal(err)
		}
		awaitExecution(t, func() bool {
			turn, err := db.GetTurn(ctx, group.ID, runID)
			return err == nil && turn.State == domain.TurnStateSucceeded
		})
	}
	assess := func(config memberToolsNativeConfig, key string) {
		t.Helper()
		memberToolsHTTP(t, config.Endpoint, config.Token, "send_message", map[string]any{
			"content": "先核对依据。", "intent_assessment": domain.ChatAssessment{Disposition: "discuss", Reason: "只核对已有依据。"},
		}, key, 201, "")
	}
	send := func(config memberToolsNativeConfig, kind, target, key, content string, count int) (string, []byte) {
		t.Helper()
		action := map[string]any{"kind": kind}
		if target != "" {
			action["recipient_id"], action["expected_reply"] = target, "一个可核对的来源。"
		}
		body := memberToolsHTTP(t, config.Endpoint, config.Token, "send_message", map[string]any{"content": content, "discussion": action}, key, 201, "")
		var receipt struct {
			MessageID string `json:"message_id"`
			Count     int    `json:"delivery_count"`
			Omitted   bool   `json:"content_omitted"`
		}
		if json.Unmarshal(body, &receipt) != nil || receipt.Count != count || !receipt.Omitted || receipt.MessageID == "" || bytes.Contains(body, []byte(content)) {
			t.Fatalf("unbounded or incorrect %s receipt: %s", kind, body)
		}
		return receipt.MessageID, body
	}
	readArrival := func(config memberToolsNativeConfig, incoming, owner, invitation string) {
		t.Helper()
		body := memberToolsHTTP(t, config.Endpoint, config.Token, "read_channel", map[string]any{"limit": 1}, "", 200, "")
		var page struct {
			Candidate domain.ChatCandidate `json:"chat_candidate"`
			Context   struct {
				Origin     string `json:"origin_message_id"`
				Incoming   string `json:"incoming_message_id"`
				Head       string `json:"head_message_id"`
				Owner      string `json:"active_owner_id"`
				Invitation string `json:"active_invitation_message_id"`
			} `json:"discussion_context"`
		}
		if json.Unmarshal(body, &page) != nil || page.Candidate.MessageID != incoming || page.Candidate.MemberID != owner ||
			page.Context.Origin != source.ID || page.Context.Incoming != incoming || page.Context.Head != incoming ||
			page.Context.Owner != owner || page.Context.Invitation != invitation {
			t.Fatalf("ordinary intake did not receive the bound discussion arrival: %s", body)
		}
	}

	rootRun, root := bind(source.ID, lead.ID)
	assess(root, "root-assessment")
	const inviteKey, question = "Discussion/Exact:1", "请核对讨论依据。"
	invite, first := send(root, "invite", peer.ID, inviteKey, question, 1)
	if _, replay := send(root, "invite", peer.ID, inviteKey, question, 1); !bytes.Equal(first, replay) {
		t.Fatal("same key did not return the original receipt")
	}
	release(rootRun)
	memberToolsHTTP(t, root.Endpoint, root.Token, "read_channel", map[string]any{}, "", 403, "forbidden")

	peerRun, invited := bind(invite, peer.ID)
	readArrival(invited, invite, peer.ID, invite)
	assess(invited, "peer-assessment")
	reply, _ := send(invited, "reply", "", "reply", "记录甲支持结论。", 1)
	release(peerRun)

	returnRun, returned := bind(reply, lead.ID)
	readArrival(returned, reply, lead.ID, "")
	assess(returned, "return-assessment")
	resolve, _ := send(returned, "resolve", "", "resolve", "证据已核对。", 0)
	release(returnRun)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if coordinationCompositionCount(t, raw, "SELECT count(*) FROM tasks") != 0 ||
		coordinationCompositionCount(t, raw, "SELECT count(*) FROM chat_discussion_events WHERE origin_message_id=?", source.ID) != 3 ||
		coordinationCompositionCount(t, raw, "SELECT count(*) FROM mutation_idempotency WHERE record_id=? AND idempotency_key=?", invite, inviteKey) != 1 ||
		coordinationCompositionCount(t, raw, "SELECT count(*) FROM deliveries WHERE message_id=?", resolve) != 0 {
		t.Fatal("ordinary relay was not recorded with one key, two wakes and no Task")
	}
}
