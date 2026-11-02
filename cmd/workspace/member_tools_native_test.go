package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

const memberToolsConfigEnv = "AGENT_WORKSPACE_MEMBER_TOOL_CONFIG"

type memberToolsNativeConfig struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
}

type memberToolsExpectation struct {
	DataDir, GroupID, MemberID, PeerID, ForeignID, ConversationID, DeliveryID, TriggerID, TopicID, ProviderID, Mode string
}

type memberToolsReport struct {
	RunID, ConfigPath, CWD, PublicID, WakeID, CreatedID, JoinID string
}

func memberToolsFixture(t *testing.T) executionFixture {
	t.Helper()
	f := newExecutionFixture(t, "1.4.0")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nset -eu\nroot='" + f.root + "'\n" + `case "$*" in
--version) printf 'version\n' >> "$root/probes"; printf '1.4.0'; exit 0;;
'auth check --provider openai-codex --json --no-refresh')
  printf 'account\n' >> "$root/probes"
  printf '%s' '{"status":"ready","provider":"openai-codex","authType":"oauth"}'; exit 0;;
esac
run=''
next=''
for arg do
  if [ "$next" = yes ]; then run="$arg"; next=''; fi
  if [ "$arg" = --session-id ]; then next=yes; fi
done
printf '%s\n' "$run" >> "$root/calls"
printf '%s\n' "$@" > "$root/args-$run"
` + "MEMBER_TOOLS_FIXTURE_ROOT=\"$root\" MEMBER_TOOLS_FIXTURE_RUN=\"$run\" exec '" + binary + "' -test.run='^TestMemberToolsNativeHelper$'\n"
	if err := os.WriteFile(f.path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

func memberToolsWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

func memberToolsReadJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func memberToolsKey(operation, callID string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(operation+callID)))
}

func memberToolsHTTP(t *testing.T, endpoint, token, operation string, input any, key string, status int, code string) []byte {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint+"/"+operation, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || response.StatusCode != status || len(body) > 65536 || !json.Valid(body) || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("tool %s: status=%d bytes=%d err=%v", operation, response.StatusCode, len(body), err)
	}
	if status >= 400 {
		var failure struct {
			Error struct{ Code, Message string }
		}
		if json.Unmarshal(body, &failure) != nil || failure.Error.Code != code || !chinese(failure.Error.Message) {
			t.Fatalf("tool %s lost Chinese JSON error %s", operation, code)
		}
	}
	if len(token) > 8 && bytes.Contains(body, []byte(token)) {
		t.Fatal("private tool echoed its capability")
	}
	return body
}

// This fixture uses the Go test binary, not Node, Pi, a bridge loader or a model.
// Only synthetic native events are written to stdout; all credentials stay private.
func TestMemberToolsNativeHelper(t *testing.T) {
	root, runID := os.Getenv("MEMBER_TOOLS_FIXTURE_ROOT"), os.Getenv("MEMBER_TOOLS_FIXTURE_RUN")
	if root == "" {
		return
	}
	defer func() {
		if t.Failed() {
			_ = os.WriteFile(filepath.Join(root, "failed"), []byte("native fixture assertion failed"), 0600)
		}
	}()
	var expected memberToolsExpectation
	memberToolsReadJSON(t, filepath.Join(root, "expected.json"), &expected)
	configPath := os.Getenv(memberToolsConfigEnv)
	if !filepath.IsAbs(configPath) || filepath.Dir(configPath) != filepath.Join(expected.DataDir, "member-tools") {
		t.Fatal("child did not receive its private absolute config path")
	}
	info, err := os.Lstat(configPath)
	if err != nil || info.Mode() != 0600 {
		t.Fatal("child config is not a 0600 non-symlink file")
	}
	var config map[string]string
	memberToolsReadJSON(t, configPath, &config)
	if len(config) != 2 || config["token"] == "" {
		t.Fatal("config must contain only endpoint and token")
	}
	endpoint, err := url.Parse(config["endpoint"])
	if err != nil || endpoint.Scheme != "http" || endpoint.Path != "/api/v1/member-tools" || endpoint.Port() == "0" || endpoint.Port() == "" || endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "::1" {
		t.Fatal("child endpoint is not the actual canonical private listener")
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || bytes.Contains(prompt, []byte(config["token"])) || bytes.Contains(prompt, []byte(configPath)) {
		t.Fatal("prompt contains a credential or was not readable")
	}
	if err := os.WriteFile(filepath.Join(root, "prompt-"+runID), prompt, 0600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	workspaceRoot, rootErr := filepath.EvalSymlinks(filepath.Join(expected.DataDir, "workspaces"))
	if err != nil || rootErr != nil || filepath.Dir(cwd) != workspaceRoot {
		t.Fatal("native child obtained an external workspace")
	}
	// Read-only metadata proves the physical claim exists before any native event.
	dsn := url.URL{Scheme: "file", Path: filepath.Join(expected.DataDir, "workspace.db"), RawQuery: "mode=ro"}
	raw, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	var group, member, conversation, delivery, state, native string
	err = raw.QueryRow("SELECT group_id,member_id,conversation_id,delivery_id,state,native_session_id FROM turns WHERE id=?", runID).Scan(&group, &member, &conversation, &delivery, &state, &native)
	if err != nil || group != expected.GroupID || state != "starting" || native != "" {
		t.Fatal("native child started without a fresh durable physical claim")
	}
	primary := member == expected.MemberID
	if primary && (conversation != expected.ConversationID || delivery != expected.DeliveryID) {
		t.Fatal("native child claim differs from the queued delivery")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	emit := func(event any) {
		t.Helper()
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	emit(map[string]any{"type": "session", "version": 3, "id": runID, "cwd": cwd})
	emit(map[string]any{"type": "agent_start"})
	var messages []any
	if primary {
		token := config["token"]
		call := func(operation string, input any, key string, status int, code string) []byte {
			return memberToolsHTTP(t, config["endpoint"], token, operation, input, key, status, code)
		}
		memberToolsHTTP(t, config["endpoint"], "wrong-token", "list_members", map[string]any{}, "", 403, "forbidden")
		call("list_members", map[string]any{"caller_id": "user"}, "", 400, "invalid_json")
		call("list_members", map[string]any{"group_id": "foreign-group"}, "", 400, "invalid_json")
		call("send_message", map[string]any{"content": "cross-group", "recipient_ids": []string{expected.ForeignID}}, memberToolsKey("send_message", "foreign"), 404, "not_found")
		for _, operation := range []string{"request_work", "assign", "coordinate", "submit_task", "search_history", "read_history"} {
			call(operation, map[string]any{}, "", 405, "method_not_allowed")
		}
		list := call("list_members", map[string]any{}, "", 200, "")
		var members struct{ Items []domain.Member }
		if json.Unmarshal(list, &members) != nil || len(members.Items) != 2 {
			t.Fatal("list_members did not use the composition store")
		}
		for _, item := range members.Items {
			if item.GroupID != expected.GroupID || item.ID != expected.MemberID && item.ID != expected.PeerID {
				t.Fatal("list_members crossed the bound group")
			}
		}
		read := call("read_channel", map[string]any{}, "", 200, "")
		var channel struct{ Items []domain.Message }
		if json.Unmarshal(read, &channel) != nil || len(channel.Items) != 1 || channel.Items[0].ID != expected.TriggerID || channel.Items[0].TopicID != expected.TopicID {
			t.Fatal("read_channel crossed the bound topic")
		}
		publicInput := map[string]any{"content": strings.Repeat("公开成员消息", 1000)}
		publicKey := memberToolsKey("send_message", "public")
		public := call("send_message", publicInput, publicKey, 201, "")
		if !bytes.Equal(public, call("send_message", publicInput, publicKey, 201, "")) {
			t.Fatal("send_message changed its stable idempotent receipt")
		}
		wake := call("send_message", map[string]any{"content": "explicit wake", "recipient_ids": []string{expected.PeerID}}, memberToolsKey("send_message", "wake"), 201, "")
		var sent, woken struct {
			MessageID      string `json:"message_id"`
			DeliveryCount  int    `json:"delivery_count"`
			ContentOmitted bool   `json:"content_omitted"`
		}
		if json.Unmarshal(public, &sent) != nil || json.Unmarshal(wake, &woken) != nil || sent.MessageID == "" || sent.DeliveryCount != 0 || !sent.ContentOmitted || woken.DeliveryCount != 1 || len(public) > 512 {
			t.Fatal("send_message did not return a small public/explicit delivery receipt")
		}
		creationInput := map[string]any{"name": "Coordinator", "role": "Coordinator/Assignment/Mandate/RequestWork", "reason": "仅讨论职责，不授予工作准入"}
		creationKey := memberToolsKey("create_member", "create")
		created := call("create_member", creationInput, creationKey, 201, "")
		if !bytes.Equal(created, call("create_member", creationInput, creationKey, 201, "")) || len(created) > 512 {
			t.Fatal("create_member did not preserve its small stable receipt")
		}
		var creation struct {
			MemberID      string `json:"member_id"`
			JoinMessageID string `json:"join_message_id"`
			Reused        bool   `json:"reused"`
		}
		if json.Unmarshal(created, &creation) != nil || creation.MemberID == "" || creation.JoinMessageID == "" || creation.Reused {
			t.Fatal("create_member did not create one member and its join message")
		}
		call("create_member", map[string]any{"name": "overflow", "role": "different role", "reason": "capacity"}, memberToolsKey("create_member", "full"), 409, "member_capacity")
		call("create_member", map[string]any{"name": "forged", "role": "role", "reason": "reason", "provider_id": "attacker", "model": "attacker", "workspace": cwd}, memberToolsKey("create_member", "forged"), 400, "invalid_json")
		for i, result := range []struct {
			name   string
			input  any
			output []byte
		}{{"list_members", map[string]any{}, list}, {"read_channel", map[string]any{}, read}, {"send_message", publicInput, public}, {"create_member", creationInput, created}} {
			id := fmt.Sprintf("fixture-%d", i)
			assistant := memberToolsAssistant([]any{map[string]any{"type": "toolCall", "id": id, "name": result.name, "arguments": result.input}}, "toolUse")
			emit(map[string]any{"type": "message_end", "message": assistant})
			tool := map[string]any{"role": "toolResult", "toolCallId": id, "toolName": result.name, "content": []any{map[string]any{"type": "text", "text": string(result.output)}}, "isError": false, "timestamp": 1}
			emit(map[string]any{"type": "message_end", "message": tool})
			emit(map[string]any{"type": "turn_end", "message": assistant, "toolResults": []any{tool}})
			messages = append(messages, assistant, tool)
		}
		// Send once without ever reading an HTTP response. This fixture has no
		// trusted tool receipt, irrespective of whether the write later commits.
		conn, err := net.DialTimeout("tcp", endpoint.Host, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		lost, _ := http.NewRequest("POST", config["endpoint"]+"/send_message", strings.NewReader(`{"content":"lost HTTP result"}`))
		lost.Header.Set("Content-Type", "application/json")
		lost.Header.Set("Authorization", "Bearer "+token)
		lost.Header.Set("Idempotency-Key", memberToolsKey("send_message", "lost"))
		if err := lost.Write(conn); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		attempt := memberToolsAssistant([]any{map[string]any{"type": "toolCall", "id": "lost", "name": "send_message", "arguments": map[string]any{"content": "lost HTTP result"}}}, "toolUse")
		emit(map[string]any{"type": "message_end", "message": attempt})
		unknown := map[string]any{"role": "toolResult", "toolCallId": "lost", "toolName": "send_message", "content": []any{map[string]any{"type": "text", "text": `{"error":{"code":"request_canceled","message":"结果未知，须对账"}}`}}, "isError": true, "timestamp": 1}
		emit(map[string]any{"type": "message_end", "message": unknown})
		emit(map[string]any{"type": "turn_end", "message": attempt, "toolResults": []any{unknown}})
		messages = append(messages, attempt, unknown)
		memberToolsWriteJSON(t, filepath.Join(root, "primary.json"), memberToolsReport{RunID: runID, ConfigPath: configPath, CWD: cwd, PublicID: sent.MessageID, WakeID: woken.MessageID, CreatedID: creation.MemberID, JoinID: creation.JoinMessageID})
		for {
			if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if expected.Mode == "unknown" {
			fmt.Fprintln(os.Stdout, "synthetic-damaged-native-frame")
		}
	}
	final := memberToolsAssistant([]any{map[string]any{"type": "text", "text": "本地组合回复"}}, "stop")
	emit(map[string]any{"type": "message_end", "message": final})
	emit(map[string]any{"type": "turn_end", "message": final, "toolResults": []any{}})
	messages = append(messages, final)
	emit(map[string]any{"type": "agent_end", "messages": messages, "willRetry": false})
	emit(map[string]any{"type": "agent_settled", "aborted": false})
	os.Exit(0)
}

func memberToolsAssistant(content []any, stop string) map[string]any {
	return map[string]any{"role": "assistant", "content": content, "api": "synthetic", "provider": "synthetic", "model": "fixture-model", "stopReason": stop, "timestamp": 1,
		"usage": map[string]any{"input": 1, "output": 1, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 2, "cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}}
}

func memberToolsExactArgs(t *testing.T, f executionFixture, report memberToolsReport, dataDir, entry string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "args-"+report.RunID))
	want := []string{"--offline", "--mode", "json", "-p", "--no-extensions", "--no-mcp", "--no-builtin-tools", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-approve",
		"--session-dir", filepath.Join(dataDir, "runtime", report.RunID), "--session-id", report.RunID, "--tools", "list_members,read_channel,send_message,create_member", "--extension", entry, "--model", "fixture-model"}
	if err != nil || !reflect.DeepEqual(strings.Split(strings.TrimSpace(string(data)), "\n"), want) {
		t.Fatal("native argv broadened tools, contexts or extension policy")
	}
}
