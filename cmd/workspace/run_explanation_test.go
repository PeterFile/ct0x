package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestRunExplanationFlagIsIndependent(t *testing.T) {
	base := []string{"--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", memberToolsEntry(t)}
	for _, on := range []bool{false, true} {
		args := slices.Clone(base)
		if on {
			args = append(args, "--enable-run-explanation")
		}
		c, err := parseConfig(args, io.Discard)
		if err != nil || c.enableRunExplanation != on || c.enableCoordinationRoles || c.enableNativeSessionResume {
			t.Fatalf("flag enabled another profile or failed: %v", err)
		}
	}
	for _, args := range [][]string{
		{"--enable-run-explanation"},
		{"--enable-run-explanation", "--enable-execution"},
		{"--enable-run-explanation", "--enable-execution", "--enable-member-tools", "--enable-coordination-roles", "--enable-codex-app-server"},
	} {
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Fatal("run explanation accepted without the execution/member tool/Pi prerequisites")
		}
	}
}

// Model-free physical CLI; the parent makes real loopback calls while the run is
// held at the named gates. Only synthetic lifecycle/acknowledgement frames are printed.
const runExplanationCLIFixture = `#!/bin/sh
set -eu
root="$RUN_EXPLANATION_FIXTURE"
case "$*" in
--version) printf '1.4.0'; exit 0;;
'auth check --provider openai-codex --json --no-refresh')
  printf '%s' '{"status":"ready","provider":"openai-codex","authType":"oauth"}'; exit 0;;
esac
printf '%s\n' "$@" > "$root/argv"
printf '%s' "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG" > "$root/config"
printf '%s' "${AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256-}" > "$root/digest"
cat > /dev/null
while [ "$1" != '--session-id' ]; do shift; done
printf '{"type":"session","version":3,"id":"%s","cwd":"%s"}\n' "$2" "$PWD"
printf '%s' "$2" > "$root/ready"
while [ ! -f "$root/accept" ]; do sleep 0.01; done
printf '%s\n' '{"type":"agent_start"}'
while [ ! -f "$root/settle" ]; do sleep 0.01; done
for frame in ack start unknown; do
  if [ -f "$root/$frame.json" ]; then cat "$root/$frame.json"; printf '\n'; fi
done
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"正常原生最终回复"}]}}'
if [ -f "$root/abort" ]; then
  printf '%s\n' '{"type":"agent_settled","aborted":true}'
else
  printf '%s\n' '{"type":"agent_settled","aborted":false}'
fi
while [ ! -f "$root/exit" ]; do sleep 0.01; done
`

func TestRunExplanationNativeHTTP(t *testing.T) {
	const op = provider.PublishRunExplanationTool
	const id = "original-native-call"
	for _, mode := range []string{"confirmed", "receipt-lost", "commit-unconfirmed", "v30", "pre-attempt-loss", "pre-attempt-aborted", "replay-response-lost"} {
		t.Run(mode, func(t *testing.T) {
			f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
			t.Setenv("RUN_EXPLANATION_FIXTURE", f.root)
			t.Setenv("AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256", "must-not-be-inherited")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(os.WriteFile(f.path, []byte(runExplanationCLIFixture), 0700))
			seed := seedMemberTools(t, f, dir, mode)
			db, err := sql.Open("sqlite", filepath.Join(dir, "workspace.db"))
			must(err)
			defer db.Close()
			exec := func(sql string) { t.Helper(); _, err := db.Exec(sql); must(err) }
			if mode != "v30" {
				// Install F's exact DDL into this disposable fixture only, not the migration list.
				source, err := os.ReadFile("../../internal/store/agent_run_explanation_schema.go")
				must(err)
				_, rest, begin := strings.Cut(string(source), "`")
				ddl, _, end := strings.Cut(rest, "`")
				if !begin || !end {
					t.Fatal("missing F fixture DDL")
				}
				exec(ddl)
			} else {
				var version int
				must(db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version))
				if version != 30 {
					t.Fatalf("old schema fixture must be v30, got v%d", version)
				}
			}
			if mode == "commit-unconfirmed" {
				// The write and readback succeed but SQLite refuses COMMIT. Let F
				// classify that unconfirmed transaction; no mocked Engine/store.
				exec(`CREATE TABLE explanation_test_parent (id INTEGER PRIMARY KEY);
				CREATE TABLE explanation_test_child (id INTEGER REFERENCES explanation_test_parent(id) DEFERRABLE INITIALLY DEFERRED);
				CREATE TRIGGER explanation_test_commit AFTER INSERT ON agent_run_explanations
				BEGIN INSERT INTO explanation_test_child VALUES (1); END;`)
			}
			base, stop := startRun(t, append(memberToolsArgs(t, dir, f.path), "--enable-run-explanation")...)
			read := func(name string) string {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(f.root, name))
				must(err)
				return string(data)
			}
			touch := func(name string) { t.Helper(); must(os.WriteFile(filepath.Join(f.root, name), nil, 0600)) }
			var runID string
			awaitExecution(t, func() bool {
				data, err := os.ReadFile(filepath.Join(f.root, "ready"))
				runID = string(data)
				return err == nil && runID != ""
			})
			path := read("config")
			var config map[string]string
			memberToolsReadJSON(t, path, &config)
			configBytes, err := os.ReadFile(path)
			must(err)
			wantTools := "list_members,read_channel,send_message,create_member,search_history,read_history"
			wantDigest := ""
			if mode != "v30" {
				wantTools += "," + op
				wantDigest = fmt.Sprintf("%x", sha256.Sum256(configBytes))
			}
			argv := strings.Split(read("argv"), "\n")
			toolIndex := slices.Index(argv, "--tools")
			if toolIndex < 0 || argv[toolIndex+1] != wantTools || slices.Contains(argv, "--session") || read("digest") != wantDigest ||
				(mode == "v30" && (len(config) != 2 || config["run_explanation"] != "")) ||
				(mode != "v30" && (len(config) != 3 || config["run_explanation"] != runID)) {
				t.Fatal("native argv and exact issued bytes did not follow schema/claim qualification")
			}
			if config["endpoint"] != base+"/api/v1/member-tools" || config["token"] == "" {
				t.Fatal("issue did not use the active loopback server")
			}
			call := func(input any, key string, status int, code string) []byte {
				t.Helper()
				return memberToolsHTTP(t, config["endpoint"], config["token"], op, input, key, status, code)
			}
			// Include multibyte scalars, leading and trailing whitespace: 1024 bytes, no trimming.
			text := " \n" + strings.Repeat("🙂", 255) + " \t"
			input := map[string]string{"text": text}
			key := fmt.Sprintf("%x", sha256.Sum256([]byte(op+"\x00"+runID+"\x00"+id)))
			call(input, key, 403, "forbidden") // The session header alone isn't acceptance.
			touch("accept")
			awaitExecution(t, func() bool {
				var state string
				return db.QueryRow("SELECT state FROM turns WHERE id=?", runID).Scan(&state) == nil && state == "running"
			})
			counts := func() [2]int {
				t.Helper()
				var c [2]int
				must(db.QueryRow("SELECT (SELECT count(*) FROM messages), (SELECT count(*) FROM deliveries)").Scan(&c[0], &c[1]))
				return c
			}
			before := counts()
			claims := func() int {
				t.Helper()
				var n int
				must(db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", runID).Scan(&n))
				return n
			}
			beforeClaims := claims()
			if beforeClaims == 0 {
				t.Fatal("fixture must hold an execution resource")
			}
			attemptPath := filepath.Join(dir, "runtime", runID, "run-explanation-attempt.json")
			preAttempt := mode == "pre-attempt-loss" || mode == "pre-attempt-aborted"
			markedUnknown := preAttempt || mode == "replay-response-lost"
			observedTerminal := domain.TurnStateSucceeded
			if mode == "pre-attempt-aborted" {
				observedTerminal = domain.TurnStateAborted
				touch("abort")
			}
			var wantPublication bool
			switch mode {
			case "pre-attempt-loss", "pre-attempt-aborted":
				// The SDK disconnect fixture drops the sent request before the
				// application handles it. Here there must be no server attempt.
				if _, err := os.Stat(attemptPath); !os.IsNotExist(err) {
					t.Fatal("pre-attempt loss fixture already has a server journal")
				}
			case "v30":
				call(input, key, 403, "forbidden")
				memberToolsHTTP(t, config["endpoint"], config["token"], "read_channel", map[string]any{}, "", 200, "")
			case "commit-unconfirmed":
				call(input, key, 503, "run_explanation_unknown")
				call(input, key, 503, "run_explanation_unknown")
				call(map[string]string{"text": "不得另写"}, strings.Repeat("b", 64), 503, "run_explanation_unknown")
				memberToolsHTTP(t, config["endpoint"], config["token"], "send_message", map[string]string{"content": "不得发消息"},
					memberToolsKey("send_message", "after-unknown"), 503, "run_explanation_unknown")
			default:
				call(map[string]string{"text": text, "run_id": "forged"}, key, 400, "invalid_input")
				call(map[string]string{"text": text + "!"}, key, 400, "invalid_input")
				call(map[string]string{"text": " \u0085"}, key, 400, "invalid_input")
				wantPublication = true
				out := call(input, key, 201, "")
				if !bytes.Contains(out, []byte("public_explanation:"+runID)) || bytes.Contains(out, []byte(key)) || bytes.Contains(out, []byte("🙂")) {
					t.Fatal("write acknowledgement is not the bounded, text-free projection")
				}
				if mode == "confirmed" || mode == "replay-response-lost" {
					// In the lost replay the server still returned the exact 201,
					// but the native stream acknowledges only the original call.
					if !bytes.Equal(out, call(input, key, 201, "")) {
						t.Fatal("exact live replay changed the immutable acknowledgement")
					}
					if mode == "confirmed" {
						call(map[string]string{"text": "changed"}, key, 409, "conflict")
						call(input, strings.Repeat("c", 64), 409, "conflict")
					}
					memberToolsWriteJSON(t, filepath.Join(f.root, "ack.json"), map[string]any{
						"type": "tool_execution_end", "toolName": op, "toolCallId": id, "isError": false,
						"result": map[string]any{"details": map[string]string{"agent_workspace_run_explanation_ack_v1": key}},
					})
				}
			}
			if markedUnknown {
				memberToolsWriteJSON(t, filepath.Join(f.root, "start.json"), map[string]any{
					"type": "tool_execution_start", "toolName": op, "toolCallId": id, "args": input,
				})
				memberToolsWriteJSON(t, filepath.Join(f.root, "unknown.json"), map[string]any{
					"type": "tool_execution_end", "toolName": op, "toolCallId": id, "isError": true,
					"result": map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "运行说明保存结果未知"}},
						"details": map[string]any{"agent_workspace_run_explanation_unknown_v1": map[string]string{
							"idempotency_key": key, "text_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(text))),
						}}},
				})
			}
			if counts() != before {
				t.Fatal("publication broadcast a normal message or created a delivery")
			}
			type runView struct {
				store.AgentRunDetail
				Timeline struct{ Items []store.AgentRunEntry } `json:"timeline"`
			}
			get := func() runView {
				var view runView
				executionJSON(t, "GET", base+"/api/v1/me/agent-runs/"+runID+"?group_id="+seed.GroupID, nil, &view)
				return view
			}
			view := get()
			n := 0
			for _, item := range view.Timeline.Items {
				if item.Kind == "public_explanation" {
					n++
					if item.ID != "public_explanation:"+runID || item.PublicExplanation == nil || item.PublicExplanation.Text != text {
						t.Fatal("public GET did not expose the exact bound statement")
					}
				}
			}
			if n > 1 || (n == 1) != wantPublication || view.Receipt != nil {
				t.Fatal("run log confused a publication with a receipt")
			}
			touch("settle")
			awaitExecution(t, func() bool {
				var terminal string
				return db.QueryRow("SELECT candidate_terminal FROM turns WHERE id=?", runID).Scan(&terminal) == nil && terminal == string(observedTerminal)
			})
			if mode == "confirmed" {
				// The token is still registered but the claim has observed a terminal.
				call(input, key, 403, "forbidden")
			}
			touch("exit")
			want := domain.TurnStateSucceeded
			uncertain := markedUnknown || mode == "receipt-lost" || mode == "commit-unconfirmed"
			if uncertain {
				want = domain.TurnStateUnknown
			}
			awaitExecution(t, func() bool {
				var receipt string
				return db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", runID).Scan(&receipt) == nil && receipt != ""
			})
			call(input, key, 403, "forbidden") // Same credential is stale after drain.
			view = get()
			var replyID string
			must(db.QueryRow("SELECT reply_message_id FROM turns WHERE id=?", runID).Scan(&replyID))
			if view.Receipt == nil || view.Receipt.Outcome != want || view.Receipt.ObservedTerminal != observedTerminal ||
				!view.Receipt.OutputComplete || !view.Receipt.DirectChildReaped || !view.Receipt.Accepted || (replyID == "") != uncertain {
				t.Fatalf("native terminal substituted for the tool/receipt outcome: %+v", view.Receipt)
			}
			wantClaims := 0
			if uncertain {
				wantClaims = beforeClaims
			}
			if got := claims(); got != wantClaims {
				t.Fatalf("execution resources after settlement: got %d, want %d", got, wantClaims)
			}
			if uncertain {
				var original map[string]string
				memberToolsReadJSON(t, attemptPath, &original)
				info, err := os.Stat(attemptPath)
				must(err)
				if len(original) != 2 || original["idempotency_key"] != key || original["text"] != text || info.Mode() != 0600 || counts() != before {
					t.Fatal("uncertainty did not retain only the original identity/text, or published a reply")
				}
			} else if _, err := os.Stat(attemptPath); !os.IsNotExist(err) {
				t.Fatal("normal execution left an unconfirmed attempt")
			}
			must(stop())
		})
	}
}
