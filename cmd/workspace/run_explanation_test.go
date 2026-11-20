package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
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
for frame in first-start ack start unknown; do
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

func installExplanationFixtureDDL(t *testing.T, db *sql.DB) {
	t.Helper()
	// Resolve F's composed DDL and apply it only to this fixture, not the migration list.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../../internal/store/agent_run_explanation_schema.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := (&types.Config{}).Check(file.Name.Name, fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, ok := pkg.Scope().Lookup("agentRunExplanationSchema").(*types.Const)
	if !ok || schema.Val().Kind() != constant.String {
		t.Fatal("missing string constant agentRunExplanationSchema")
	}
	if _, err := db.Exec(constant.StringVal(schema.Val())); err != nil {
		t.Fatalf("install F fixture DDL: %v", err)
	}
}

func TestRunExplanationNativeHTTP(t *testing.T) {
	const op = provider.PublishRunExplanationTool
	const id = "original-native-call"
	for _, mode := range []string{"confirmed", "receipt-lost", "commit-unconfirmed", "v30", "pre-attempt-loss", "pre-attempt-aborted", "replay-response-lost", "unpaired-replay"} {
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
				installExplanationFixtureDDL(t, db)
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
			journal := filepath.Join(dir, "runtime", runID, "run-explanation-bridge-attempt")
			argv := strings.Split(read("argv"), "\n")
			toolIndex := slices.Index(argv, "--tools")
			if toolIndex < 0 || argv[toolIndex+1] != wantTools || slices.Contains(argv, "--session") || read("digest") != wantDigest ||
				(mode == "v30" && (len(config) != 2 || config["run_explanation"] != "")) ||
				(mode != "v30" && (len(config) != 5 || config["run_explanation"] != runID || config["run_explanation_journal"] != journal || config["run_explanation_claim_fingerprint"] == "")) {
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
			if mode != "v30" {
				// This CLI uses synthetic stdout and parent-issued HTTP calls.
				// The separate physical bridge test exercises real reservation I/O.
				must(os.WriteFile(journal, []byte(key+"\n"+text), 0600))
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
				if mode == "confirmed" || mode == "replay-response-lost" || mode == "unpaired-replay" {
					// In the lost replay the server still returned the exact 201,
					// but the native stream acknowledges only the original call.
					if !bytes.Equal(out, call(input, key, 201, "")) {
						t.Fatal("exact live replay changed the immutable acknowledgement")
					}
					if mode == "confirmed" {
						call(map[string]string{"text": "changed"}, key, 409, "conflict")
						call(input, strings.Repeat("c", 64), 409, "conflict")
					}
					memberToolsWriteJSON(t, filepath.Join(f.root, "first-start.json"), map[string]any{
						"type": "tool_execution_start", "toolName": op, "toolCallId": id, "args": input,
					})
					memberToolsWriteJSON(t, filepath.Join(f.root, "ack.json"), map[string]any{
						"type": "tool_execution_end", "toolName": op, "toolCallId": id, "isError": false,
						"result": map[string]any{"content": []any{}, "details": map[string]string{"agent_workspace_run_explanation_ack_v1": key}},
					})
				}
			}
			if markedUnknown || mode == "unpaired-replay" {
				memberToolsWriteJSON(t, filepath.Join(f.root, "start.json"), map[string]any{
					"type": "tool_execution_start", "toolName": op, "toolCallId": id, "args": input,
				})
			}
			if markedUnknown {
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
			uncertain := markedUnknown || mode == "receipt-lost" || mode == "commit-unconfirmed" || mode == "unpaired-replay"
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
				view.Receipt.OutputComplete == (mode == "unpaired-replay") || !view.Receipt.DirectChildReaped || !view.Receipt.Accepted || (replyID == "") != uncertain {
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
				wire, err := os.ReadFile(journal)
				must(err)
				if string(wire) != key+"\n"+text || counts() != before {
					t.Fatal("uncertainty did not retain the exact bridge attempt, or published a reply")
				}
			} else if _, err := os.Lstat(journal); !os.IsNotExist(err) {
				t.Fatal("settled turn left a bridge reservation")
			}
			if uncertain && !preAttempt {
				var original map[string]string
				memberToolsReadJSON(t, attemptPath, &original)
				info, err := os.Stat(attemptPath)
				must(err)
				if len(original) != 2 || original["idempotency_key"] != key || original["text"] != text || info.Mode() != 0600 {
					t.Fatal("server attempt did not independently retain the original key and text")
				}
			} else if _, err := os.Lstat(attemptPath); !os.IsNotExist(err) {
				t.Fatal("server attempt should be absent")
			}
			must(stop())
		})
	}
}

// The native child uses the actual bridge and HTTP transport. This loopback
// sink consumes the body but never hands it to C's server or returns a receipt.
// Cancelling the scheduler must kill and join the child, not run a JS abort hook.
func TestRunExplanationParentCancelAfterSendKeepsExactJournal(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("model-free bridge subprocess requires Node")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type sent struct{ key, body string }
	requests := make(chan sent, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/api/v1/member-tools/publish_run_explanation" {
			t.Error("unexpected explanation transport request")
		}
		select {
		case requests <- sent{r.Header.Get("Idempotency-Key"), string(body)}:
		default:
			t.Error("unexpected extra send")
		}
		<-r.Context().Done()
	}))
	defer sink.Close()

	f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
	t.Setenv("RUN_EXPLANATION_FIXTURE", f.root)
	t.Setenv("RUN_EXPLANATION_NODE", node)
	t.Setenv("RUN_EXPLANATION_BRIDGE", filepath.Join(filepath.Dir(memberToolsEntry(t)), "bridge.mjs"))
	must(os.WriteFile(f.path, []byte(`#!/bin/sh
case "$*" in
--version) printf '1.4.0'; exit 0;;
'auth check --provider openai-codex --json --no-refresh')
  printf '%s' '{"status":"ready","provider":"openai-codex","authType":"oauth"}'; exit 0;;
esac
exec "$RUN_EXPLANATION_NODE" "$RUN_EXPLANATION_FIXTURE/child.mjs" "$@"
`), 0700))
	const id, text = "actual-native-call|0", " \n精确原文🙂\t\r "
	t.Setenv("RUN_EXPLANATION_TEXT", text)
	must(os.WriteFile(filepath.Join(f.root, "child.mjs"), []byte(`
import { existsSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
const env = process.env;
const { invoke } = (await import(pathToFileURL(env.RUN_EXPLANATION_BRIDGE))).openBridge(
    env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG, env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256);
for await (const _ of process.stdin) {} // Drain the prompt.
const run = process.argv[process.argv.indexOf('--session-id') + 1];
console.log(JSON.stringify({type:'session',version:3,id:run,cwd:process.cwd()}));
console.log(JSON.stringify({type:'agent_start'}));
writeFileSync(join(env.RUN_EXPLANATION_FIXTURE,'ready'), run);
while (!existsSync(join(env.RUN_EXPLANATION_FIXTURE,'send'))) await delay(5);
// Deliberately do not provide tool events or the statement on stdout.
await invoke('publish_run_explanation', {text:env.RUN_EXPLANATION_TEXT}, 'actual-native-call|0');
`), 0600))

	seed := seedMemberTools(t, f, dir, "parent-interrupted")
	dbPath := filepath.Join(dir, "workspace.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	must(err)
	defer sqlDB.Close()
	installExplanationFixtureDDL(t, sqlDB)
	db, err := store.Open(dbPath)
	must(err)
	defer db.Close()
	cfg, err := parseConfig(append(memberToolsArgs(t, dir, f.path), "--enable-run-explanation"), io.Discard)
	must(err)
	catalog := platform.DiscoverProviders(ctx, provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: f.path}}}, provider.Runner{Timeout: 2 * time.Second, OutputLimit: 4096})
	items, _, err := catalog.List(0, "")
	must(err)
	if len(items) != 1 || items[0].ID != seed.ProviderID {
		t.Fatal("fixture catalog does not match the seeded member")
	}
	run, tools, _, err := executionRunner(cfg, db, coordination.New(db), catalog, sink.URL+"/api/v1/member-tools", io.Discard)
	must(err)
	defer tools.Close()
	done := make(chan error, 1)
	go func() { done <- run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
		if t.Failed() {
			matches, _ := filepath.Glob(filepath.Join(dir, "runtime", "*", "stderr.log"))
			for _, path := range matches {
				output, _ := os.ReadFile(path)
				t.Logf("synthetic child stderr: %s", output)
			}
		}
	}()

	var runID string
	awaitExecution(t, func() bool {
		ready, err := os.ReadFile(filepath.Join(f.root, "ready"))
		if err != nil || len(ready) == 0 {
			return false
		}
		runID = string(ready)
		turn, err := db.GetTurn(ctx, seed.GroupID, runID)
		return err == nil && turn.State == domain.TurnStateRunning
	})
	must(os.WriteFile(filepath.Join(f.root, "send"), nil, 0600))
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(provider.PublishRunExplanationTool+"\x00"+runID+"\x00"+id)))
	select {
	case request := <-requests:
		if request.key != key || request.body != `{"text":" \n精确原文🙂\t\r "}` {
			t.Fatal("sink did not receive the exact expected key and statement")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not send the fixture explanation")
	}
	runDir := filepath.Join(dir, "runtime", runID)
	assertJournal := func() {
		t.Helper()
		path := filepath.Join(runDir, "run-explanation-bridge-attempt")
		wire, err := os.ReadFile(path)
		if err != nil || string(wire) != key+"\n"+text {
			t.Fatalf("sent request lacks exact, independent bridge evidence: %v", err)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode() != 0600 {
			t.Fatal("bridge evidence is not private")
		}
	}
	// Capture at the send boundary, but still exercise cancellation on the RED run.
	before, beforeErr := os.ReadFile(filepath.Join(runDir, "run-explanation-bridge-attempt"))
	cancel()
	must(shutdownError(<-done))
	assertJournal()
	must(beforeErr)
	if string(before) != key+"\n"+text {
		t.Fatal("exact evidence was not present on disk at the send boundary")
	}
	if _, err := os.Lstat(filepath.Join(runDir, "run-explanation-attempt.json")); !os.IsNotExist(err) {
		t.Fatal("the sink must not produce a server attempt")
	}
	out, err := os.ReadFile(filepath.Join(runDir, "stdout.jsonl"))
	must(err)
	if bytes.Contains(out, []byte("tool_execution_end")) || bytes.Contains(out, []byte("精确原文")) {
		t.Fatal("stdout accidentally supplied outcome or exact request evidence")
	}
	receipt, err := db.GetAgentRun(t.Context(), seed.GroupID, runID, store.ListOptions{})
	must(err)
	var claims, exitCode, publications int
	must(sqlDB.QueryRow("SELECT count(*) FROM agent_run_explanations").Scan(&publications))
	must(sqlDB.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", runID).Scan(&claims))
	must(sqlDB.QueryRow("SELECT json_extract(receipt_json,'$.exit_code') FROM turns WHERE id=?", runID).Scan(&exitCode))
	if receipt.Receipt == nil || receipt.Receipt.Outcome != domain.TurnStateUnknown || !receipt.Receipt.DirectChildReaped ||
		exitCode != -1 || receipt.Receipt.ObservedTerminal != "" || receipt.Receipt.OutputComplete || claims == 0 || publications != 0 {
		t.Fatal("real parent cancellation did not reap the child and hold the unresolved claim")
	}
	configs, err := os.ReadDir(filepath.Join(dir, "member-tools"))
	must(err)
	if len(configs) != 0 {
		t.Fatal("capability config survived the drain")
	}
}
