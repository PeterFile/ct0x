package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
)

// Hold the existing offline Pi fixture after acceptance so the normal startup,
// scheduler, config issuance and native member-tool routes remain under test.
func TestPiDiscussionRelayNativeHandoff(t *testing.T) {
	f := newExecutionFixture(t, "1.4.0")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	const hold = `while [ -f "$root/hold" ]; do sleep 0.05; done`
	if bytes.Count(script, []byte(hold)) != 1 {
		t.Fatal("fixture no longer exposes a hold point")
	}
	gate := `printf '%s\n%s\n' "${AGENT_WORKSPACE_MEMBER_TOOL_CONFIG-}" "${AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256-}" > "$root/ready-$run.tmp"
mv "$root/ready-$run.tmp" "$root/ready-$run"
while [ ! -f "$root/release-$run" ]; do sleep 0.05; done`
	if err := os.WriteFile(f.path, bytes.Replace(script, []byte(hold), []byte(gate), 1), 0700); err != nil {
		t.Fatal(err)
	}
	entry, err := filepath.EvalSymlinks(memberToolsEntry(t))
	if err != nil || !provider.PiDiscussionRelayBridgeSupported(entry) {
		t.Fatalf("relay fixture requires the current physical bridge pair: %s (%v)", entry, err)
	}
	args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path,
		"--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", entry,
		"--enable-coordination-roles", "--enable-codex-app-server"}
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
	// Observe one delivery/Turn snapshot without opening a second migrating
	// Store or polling through the full message/discussion projection.
	dsn := url.URL{Scheme: "file", Path: filepath.Join(dir, "workspace.db"), RawQuery: "mode=ro"}
	raw, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ctx := t.Context()
	bind := func(messageID, recipient string) (string, memberToolsNativeConfig) {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		var delivery, arrived, runID, reason, nativeID string
		var state domain.TurnState
		var accepted bool
		var queryErr, readyErr error
		fail := func() {
			t.Helper()
			_, invoked := os.Stat(filepath.Join(f.root, "calls"))
			t.Fatalf("relay %s did not reach native ready: delivery=%s run=%q state=%s reason=%.240q accepted=%t provider=%s/%s invoked=%t db_err=%.160v ready_err=%.160v",
				messageID, delivery, runID, state, reason, accepted, selected.Name, selected.Version, invoked == nil, queryErr, readyErr)
		}
		for {
			queryErr = raw.QueryRowContext(wait, `SELECT d.state,d.recipient_id,d.turn_id,coalesce(t.state,''),coalesce(t.reason,d.error),
				coalesce(t.native_session_id,''),t.accepted_at IS NOT NULL FROM deliveries d
				LEFT JOIN turns t ON t.id=d.turn_id AND t.delivery_id=d.id AND t.group_id=d.group_id
				WHERE d.group_id=? AND d.message_id=?`, group.ID, messageID).Scan(&delivery, &arrived, &runID, &state, &reason, &nativeID, &accepted)
			if queryErr == nil {
				if arrived != recipient {
					t.Fatal("relay message did not target the expected member")
				}
				switch state {
				case "", domain.TurnStateStarting:
				case domain.TurnStateRunning:
					var ready []byte
					ready, readyErr = os.ReadFile(filepath.Join(f.root, "ready-"+runID))
					if readyErr == nil {
						if !accepted || nativeID != runID {
							t.Fatal("relay fixture did not report matching native acceptance")
						}
						return runID, relayFixtureConfig(t, f, runID, ready, dir, entry, base)
					}
				default:
					fail()
				}
			}
			select {
			case <-tick.C:
			case <-wait.Done():
				fail()
			}
		}
	}
	release := func(runID string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.root, "release-"+runID), nil, 0600); err != nil {
			t.Fatal(err)
		}
		awaitExecution(t, func() bool {
			var state domain.TurnState
			err := raw.QueryRowContext(ctx, "SELECT state FROM turns WHERE group_id=? AND id=?", group.ID, runID).Scan(&state)
			return err == nil && state == domain.TurnStateSucceeded
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
	assertNativeRelayRejectedByMCP(t, root, peer.ID)
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
	if coordinationCompositionCount(t, raw, "SELECT count(*) FROM tasks") != 0 ||
		coordinationCompositionCount(t, raw, "SELECT count(*) FROM chat_discussion_events WHERE origin_message_id=?", source.ID) != 3 ||
		coordinationCompositionCount(t, raw, "SELECT count(*) FROM mutation_idempotency WHERE record_id=? AND idempotency_key=?", invite, inviteKey) != 1 ||
		coordinationCompositionCount(t, raw, "SELECT count(*) FROM deliveries WHERE message_id=?", resolve) != 0 {
		t.Fatal("ordinary relay was not recorded with one key, two wakes and no Task")
	}
}

// Use the actual issued Pi relay capability, after its assessment. If routed
// through the native handler, this invite would otherwise be authorized.
func assertNativeRelayRejectedByMCP(t *testing.T, config memberToolsNativeConfig, recipient string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "relay-not-mcp", "method": "tools/call",
		"params": map[string]any{"name": "send_message", "arguments": map[string]any{
			"content": "请核对。", "discussion": map[string]string{"kind": "invite", "recipient_id": recipient},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, config.Endpoint+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+config.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2025-06-18")
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2048))
	if err != nil || response.StatusCode != http.StatusBadRequest || !bytes.Contains(data, []byte(`"code":-32602`)) {
		t.Fatalf("MCP did not reject the real Pi relay capability's invite: status=%d read=%v", response.StatusCode, err)
	}
}

func relayFixtureConfig(t *testing.T, f executionFixture, runID string, ready []byte, dir, entry, base string) memberToolsNativeConfig {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(string(ready), "\n"), "\n")
	if len(lines) != 2 || filepath.Dir(lines[0]) != filepath.Join(dir, "member-tools") {
		t.Fatalf("run %s lacked a private issued config", runID)
	}
	data, err := os.ReadFile(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != lines[1] {
		t.Fatalf("run %s did not receive the digest of its exact issued config", runID)
	}
	var config struct {
		memberToolsNativeConfig
		DiscussionRelay bool `json:"discussion_relay"`
	}
	if err := json.Unmarshal(data, &config); err != nil || !config.DiscussionRelay || config.Token == "" || config.Endpoint != base+"/api/v1/member-tools" {
		t.Fatalf("run %s did not receive a relay capability (config parse: %v)", runID, err)
	}
	args, err := os.ReadFile(filepath.Join(f.root, "args"))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
	hasPair := func(flag, value string) bool {
		i := slices.Index(argv, flag)
		return i >= 0 && i+1 < len(argv) && argv[i+1] == value
	}
	const tools = "list_members,read_channel,send_message,search_history,read_history"
	if !hasPair("--extension", entry) || !hasPair("--tools", tools) || !hasPair("--session-id", runID) ||
		!slices.Contains(argv, "--no-builtin-tools") || !slices.Contains(argv, "--no-mcp") || slices.Contains(argv, "--session") {
		t.Fatalf("run %s did not select fresh native Pi with the qualified relay tools", runID)
	}
	return config.memberToolsNativeConfig
}
