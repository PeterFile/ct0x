package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

type memberToolsHistoryExpectation struct {
	Binding        memberToolsExpectation
	Messages       []domain.Message
	Cursor         string
	ForeignIDs     []string
	ForeignCursors []string
}

func memberToolsHistoryFixture(t *testing.T) executionFixture {
	t.Helper()
	f := memberToolsFixture(t)
	body, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte("^TestMemberToolsNativeHelper$"), []byte("^TestMemberToolsHistoryNativeHelper$"), 1)
	if err := os.WriteFile(f.path, body, 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

func seedMemberToolsHistory(t *testing.T, f executionFixture, dir, topic string) memberToolsHistoryExpectation {
	t.Helper()
	base, stop := startRun(t, "--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path)
	selected := executionProvider(t, base)
	var group, foreign domain.Group
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "历史组合"}, &group)
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "外群"}, &foreign)
	var caller domain.Member
	executionJSON(t, "POST", base+"/api/v1/groups/"+group.ID+"/members", platform.MemberInput{
		Name: "历史调用成员", Role: "只读历史不授予任务权限", ProviderID: selected.ID, Model: "fixture-model",
	}, &caller)
	otherTopic := ""
	if topic == "" {
		otherTopic = "other-topic"
	}
	var expected memberToolsHistoryExpectation
	for i, scope := range []struct{ group, topic string }{{group.ID, topic}, {group.ID, otherTopic}, {foreign.ID, topic}} {
		contents := []string{"H3-needle \t" + strings.Repeat("中😀e\u0301<>&\\\"\n", 111) + " \n", "H3-needle 短😀消息"}
		if i != 0 {
			contents = []string{"H3-needle 外部旧消息", "H3-needle 外部新消息"}
		}
		var messages []domain.Message
		for _, content := range contents {
			var message domain.Message
			executionJSON(t, "POST", base+"/api/v1/groups/"+scope.group+"/messages", platform.MessageInput{
				Content: content, TopicID: scope.topic, RecipientIDs: []string{},
			}, &message)
			messages = append(messages, message)
		}
		query := url.Values{"q": {"H3-needle"}, "topic_id": {scope.topic}, "limit": {"1"}}
		var page store.HistoryPage
		executionJSON(t, "GET", base+"/api/v1/groups/"+scope.group+"/history?"+query.Encode(), nil, &page)
		if len(page.Items) != 1 || page.Items[0].ID != messages[1].ID || page.NextCursor == "" {
			t.Fatal("seed did not produce a real scoped history cursor")
		}
		if i == 0 {
			expected.Messages = []domain.Message{messages[1], messages[0]}
			expected.Cursor = page.NextCursor
		} else {
			expected.ForeignIDs = append(expected.ForeignIDs, messages[1].ID)
			expected.ForeignCursors = append(expected.ForeignCursors, page.NextCursor)
		}
	}
	var trigger domain.Message
	executionJSON(t, "POST", base+"/api/v1/groups/"+group.ID+"/messages", platform.MessageInput{
		Content: "历史读取触发", TopicID: topic, RecipientIDs: []string{caller.ID},
	}, &trigger)
	if len(trigger.Deliveries) != 1 || trigger.Deliveries[0].State != domain.DeliveryStateQueued || trigger.Deliveries[0].TurnID != "" {
		t.Fatal("history seed did not leave one queued, unclaimed delivery")
	}
	expected.Binding = memberToolsExpectation{DataDir: dir, GroupID: group.ID, MemberID: caller.ID, TopicID: topic,
		ConversationID: trigger.Deliveries[0].ConversationID, DeliveryID: trigger.Deliveries[0].ID, TriggerID: trigger.ID, ProviderID: selected.ID}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "member-tools")); !os.IsNotExist(err) {
		t.Fatal("disabled history seed issued a credential root")
	}
	if _, err := os.Stat(filepath.Join(f.root, "calls")); !os.IsNotExist(err) {
		t.Fatal("disabled history seed invoked native execution")
	}
	memberToolsWriteJSON(t, filepath.Join(f.root, "expected.json"), expected)
	return expected
}

func memberToolsHistoryDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	dsn := url.URL{Scheme: "file", Path: filepath.Join(dir, "workspace.db"), RawQuery: "mode=ro"}
	raw, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return raw
}

// Snapshot complete rows, not just counts: reads must not consume deliveries,
// change receipts or release an occupied resource. No native events run between snapshots.
func memberToolsHistoryState(t *testing.T, raw *sql.DB) string {
	t.Helper()
	tx, err := raw.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var state bytes.Buffer
	encoder := json.NewEncoder(&state)
	for _, table := range []string{"messages", "members", "conversations", "deliveries", "turns", "mutation_idempotency", "resource_claims", "tasks", "task_attempts", "task_submissions"} {
		if err := encoder.Encode(table); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if err := encoder.Encode(values); err != nil {
				rows.Close()
				t.Fatal(err)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return state.String()
}

func memberToolsHistoryDecode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func memberToolsHistoryFragment(t *testing.T, got store.MemberHistoryFragment, content string, offset, length int) {
	t.Helper()
	runes := []rune(content)
	next := min(offset+length, len(runes))
	want := store.MemberHistoryFragment{Offset: offset, NextOffset: next, End: next == len(runes), TotalRunes: len(runes),
		Content: string(runes[offset:next]), ContentComplete: offset == 0 && next == len(runes)}
	if !utf8.ValidString(got.Content) || got != want {
		t.Fatalf("history fragment at rune %d: got=%+v want=%+v", offset, got, want)
	}
}

// This child uses actual cmd credentials and HTTP, not the Pi loader/bridge or a model.
func TestMemberToolsHistoryNativeHelper(t *testing.T) {
	root, runID := os.Getenv("MEMBER_TOOLS_FIXTURE_ROOT"), os.Getenv("MEMBER_TOOLS_FIXTURE_RUN")
	if root == "" {
		return
	}
	defer func() {
		if t.Failed() {
			_ = os.WriteFile(filepath.Join(root, "failed"), []byte("history native fixture assertion failed"), 0600)
		}
	}()
	var expected memberToolsHistoryExpectation
	memberToolsReadJSON(t, filepath.Join(root, "expected.json"), &expected)
	binding := expected.Binding
	configPath := os.Getenv(memberToolsConfigEnv)
	info, err := os.Lstat(configPath)
	if err != nil || info.Mode() != 0600 || !filepath.IsAbs(configPath) || filepath.Dir(configPath) != filepath.Join(binding.DataDir, "member-tools") {
		t.Fatal("history child has no private 0600 capability config")
	}
	var config map[string]string
	memberToolsReadJSON(t, configPath, &config)
	if len(config) != 2 || config["token"] == "" || config["endpoint"] == "" {
		t.Fatal("history config must contain only endpoint and token")
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || bytes.Contains(prompt, []byte(config["token"])) || bytes.Contains(prompt, []byte(configPath)) {
		t.Fatal("history prompt is unreadable or contains private credentials")
	}
	if err := os.WriteFile(filepath.Join(root, "prompt-"+runID), prompt, 0600); err != nil {
		t.Fatal(err)
	}
	raw := memberToolsHistoryDB(t, binding.DataDir)
	var group, member, conversation, delivery, topic, state, native, claim string
	err = raw.QueryRow(`SELECT t.group_id,t.member_id,t.conversation_id,t.delivery_id,c.topic_id,t.state,t.native_session_id,t.claim_token
	 FROM turns t JOIN conversations c ON c.id=t.conversation_id WHERE t.id=?`, runID).
		Scan(&group, &member, &conversation, &delivery, &topic, &state, &native, &claim)
	if err != nil || group != binding.GroupID || member != binding.MemberID || conversation != binding.ConversationID || delivery != binding.DeliveryID || topic != binding.TopicID || state != "starting" || native != "" || claim == "" {
		t.Fatal("history child started without the exact fresh durable claim")
	}
	var claims int
	if err := raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", runID).Scan(&claims); err != nil || claims != 1 {
		t.Fatal("history child must hold its actual workspace resource")
	}
	before := memberToolsHistoryState(t, raw)
	// Every history request deliberately omits Idempotency-Key.
	call := func(operation string, input any, status int, code string) []byte {
		return memberToolsHTTP(t, config["endpoint"], config["token"], operation, input, "", status, code)
	}
	page := memberToolsHistoryDecode[store.MemberHistorySearchResult](t, call("search_history", map[string]any{"query": "H3-needle", "limit": 1}, 200, ""))
	if page.NextCursor != expected.Cursor || page.NextCursor == "" {
		t.Fatal("cmd wrapped or lost the real opaque Store cursor")
	}
	cursor := page.NextCursor
	if !bytes.Contains(prompt, []byte("基础平台工具仅有 list_members、read_channel、send_message、create_member、search_history、read_history；")) {
		t.Fatal("claimed prompt did not advertise the canonical six-tool profile")
	}
	for i, message := range expected.Messages {
		if i > 0 {
			page = memberToolsHistoryDecode[store.MemberHistorySearchResult](t, call("search_history", map[string]any{"query": "H3-needle", "limit": 1, "before": cursor}, 200, ""))
		}
		if len(page.Items) != 1 || page.Items[0].MessageID != message.ID || page.Items[0].SenderID != message.SenderID || !page.Items[0].CreatedAt.Equal(message.CreatedAt) || (page.NextCursor == "") != (i == len(expected.Messages)-1) {
			t.Fatalf("history page lost exact scoped message identity: %+v", page)
		}
		memberToolsHistoryFragment(t, page.Items[0].Excerpt, message.Content, 0, 256)
		var reconstructed strings.Builder
		for offset := 0; offset < utf8.RuneCountInString(message.Content); {
			fragment := memberToolsHistoryDecode[store.MemberHistoryReadResult](t, call("read_history", map[string]any{"message_id": message.ID, "offset": offset, "length": 331}, 200, ""))
			if fragment.MessageID != message.ID {
				t.Fatal("history read changed message identity")
			}
			memberToolsHistoryFragment(t, fragment.MemberHistoryFragment, message.Content, offset, 331)
			reconstructed.WriteString(fragment.Content)
			offset = fragment.NextOffset
		}
		if reconstructed.String() != message.Content || !utf8.ValidString(reconstructed.String()) {
			t.Fatal("history fragments did not reconstruct the precise original UTF-8 bytes")
		}
	}
	last := expected.Messages[len(expected.Messages)-1]
	end := memberToolsHistoryDecode[store.MemberHistoryReadResult](t, call("read_history", map[string]any{"message_id": last.ID, "offset": utf8.RuneCountInString(last.Content), "length": 1}, 200, ""))
	if end.MessageID != last.ID {
		t.Fatal("end fragment changed message identity")
	}
	memberToolsHistoryFragment(t, end.MemberHistoryFragment, last.Content, utf8.RuneCountInString(last.Content), 1)
	for _, foreignCursor := range expected.ForeignCursors {
		call("search_history", map[string]any{"query": "H3-needle", "before": foreignCursor}, 400, "invalid_input")
	}
	call("search_history", map[string]any{"query": "different-query", "before": cursor}, 400, "invalid_input")
	for _, foreignID := range expected.ForeignIDs {
		call("read_history", map[string]any{"message_id": foreignID}, 404, "not_found")
	}
	for _, field := range []string{"actor_id", "group_id", "topic_id", "grants"} {
		call("search_history", map[string]any{"query": "H3-needle", field: "forged"}, 400, "invalid_json")
		call("read_history", map[string]any{"message_id": last.ID, field: "forged"}, 400, "invalid_json")
	}
	memberToolsHTTP(t, config["endpoint"], "wrong-token", "search_history", map[string]any{"query": "H3-needle"}, "", 403, "forbidden")
	memberToolsHTTP(t, config["endpoint"], "wrong-token", "read_history", map[string]any{"message_id": last.ID}, "", 403, "forbidden")
	if after := memberToolsHistoryState(t, raw); after != before {
		t.Fatal("history reads changed messages, deliveries, tasks, mutation receipts, consumed state or held claims")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	emit := func(event any) {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	emit(map[string]any{"type": "session", "version": 3, "id": runID, "cwd": cwd})
	emit(map[string]any{"type": "agent_start"})
	memberToolsWriteJSON(t, filepath.Join(root, "primary.json"), memberToolsReport{RunID: runID, ConfigPath: configPath, CWD: cwd})
	for {
		if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	final := memberToolsAssistant([]any{map[string]any{"type": "text", "text": "历史组合读取完成"}}, "stop")
	emit(map[string]any{"type": "message_end", "message": final})
	emit(map[string]any{"type": "turn_end", "message": final, "toolResults": []any{}})
	emit(map[string]any{"type": "agent_end", "messages": []any{final}, "willRetry": false})
	emit(map[string]any{"type": "agent_settled", "aborted": false})
	os.Exit(0)
}

func TestMemberToolsHistoryExecutionHTTP(t *testing.T) {
	for _, tt := range []struct{ name, topic string }{{"main", ""}, {"exact-topic", "历史/精确话题"}} {
		t.Run(tt.name, func(t *testing.T) {
			f, dir := memberToolsHistoryFixture(t), t.TempDir()
			expected := seedMemberToolsHistory(t, f, dir, tt.topic)
			base, stop := startRun(t, memberToolsArgs(t, dir, f.path)...)
			report := awaitMemberToolsReport(t, f, dir)
			memberToolsExactArgs(t, f, report, dir, memberToolsEntry(t))
			var config memberToolsNativeConfig
			memberToolsReadJSON(t, report.ConfigPath, &config)
			if config.Endpoint != base+"/api/v1/member-tools" {
				t.Fatal("history child did not use the actual cmd listener")
			}
			raw := memberToolsHistoryDB(t, dir)
			if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			var reply string
			awaitExecution(t, func() bool {
				var state string
				err := raw.QueryRow("SELECT state,reply_message_id FROM turns WHERE id=? AND group_id=?", report.RunID, expected.Binding.GroupID).Scan(&state, &reply)
				return err == nil && state == "succeeded" && reply != ""
			})
			before := memberToolsHistoryState(t, raw)
			memberToolsHTTP(t, config.Endpoint, config.Token, "search_history", map[string]any{"query": "H3-needle"}, "", 403, "forbidden")
			memberToolsHTTP(t, config.Endpoint, config.Token, "read_history", map[string]any{"message_id": expected.Messages[0].ID}, "", 403, "forbidden")
			if memberToolsHistoryState(t, raw) != before {
				t.Fatal("revoked history reads changed durable state")
			}
			if _, err := os.Stat(report.ConfigPath); !os.IsNotExist(err) {
				t.Fatal("history completion retained its private config")
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(filepath.Join(f.root, "calls"))
			if err != nil || string(calls) != report.RunID+"\n" {
				t.Fatal("history reads woke or duplicated native execution")
			}
			t.Log("six-tool cmd profile, scoped opaque pagination, exact Unicode fragments, read-only state and revoke passed with a synthetic Go child")
		})
	}
}

func TestMemberToolsHistoryStartupCapabilities(t *testing.T) {
	f := newExecutionFixture(t, "1.4.0")
	failure := errors.New("history startup output fixture")
	var output bytes.Buffer
	err := runWithSearch(context.Background(), memberToolsArgs(t, t.TempDir(), f.path), io.MultiWriter(&output, executionFailWriter{failure}), provider.Search{})
	if !errors.Is(err, failure) {
		t.Fatalf("startup output failure was lost: %v", err)
	}
	if !strings.Contains(output.String(), "可用六项成员工具（含只读历史搜索与分段读取）") || strings.Contains(output.String(), "可用四项") {
		t.Fatalf("startup capability description is stale: %s", &output)
	}
}
