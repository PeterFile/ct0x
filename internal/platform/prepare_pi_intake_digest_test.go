//go:build unix

package platform

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// The launcher is fake; the selected bridge's parser and Go executor are real.
// No Pi session, tool HTTP call, or model is started. The fake emits lifecycle
// frames only after parsing the config with the environment the adapter supplied.
const piIntakeDigestReader = `
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { pathToFileURL } from "node:url";

const flag = name => process.argv[process.argv.indexOf(name) + 1];
const mode = process.env.FIXTURE_INTAKE_MODE;
const legacy = mode === "legacy-193";
const path = process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
const digest = process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256;
if (legacy ? digest !== undefined : !/^[a-f0-9]{64}$/.test(digest)) process.exit(51);
if (process.argv.includes("--session") || process.argv.includes("--continue") ||
    flag("--tools") !== "list_members,read_channel,send_message,search_history,read_history") process.exit(52);
const { openBridge } = await import(pathToFileURL(join(dirname(flag("--extension")), "bridge.mjs")));
readFileSync(0); // Drain the synthetic prompt; do not echo it.
if (mode === "bridge-recheck") {
  // Keep the same JSON, size, mode and file; change bytes after Go's last read.
  const bytes = readFileSync(path);
  if (bytes.at(-1) !== 10) process.exit(53);
  bytes[bytes.length - 1] = 32;
  writeFileSync(path, bytes);
}
let config;
try { config = openBridge(path, digest); } catch (error) {
  process.exit(mode === "bridge-recheck" && error.message === "成员工具配置无效，未启用任何成员工具。" ? 42 : 54);
}
const combined = mode === "combined";
if (mode === "bridge-recheck" || Boolean(config.discussionOnlyExcerpt) !== !legacy ||
    config.continueWork !== (mode === "excerpt-and-continue" || combined) ||
    Boolean(config.investigationIntake) !== combined || config.investigationTools) process.exit(55);
for (const event of [
  { type: "session", version: 3, id: flag("--session-id"), cwd: process.cwd() },
  { type: "agent_start" },
  { type: "message_end", message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: "离线摘要接线" }] } },
  { type: "agent_settled", aborted: false },
]) console.log(JSON.stringify(event));
`

func installPiIntakeDigestReader(t *testing.T) (string, error) {
	t.Helper()
	node, nodeErr := exec.LookPath("node")
	root := t.TempDir()
	launcher, reader := filepath.Join(root, "pi"), filepath.Join(root, "reader.mjs")
	prepareRoleOK(t, os.WriteFile(launcher, []byte("#!/bin/sh\nexec \"$FIXTURE_NODE\" \"$FIXTURE_READER\" \"$@\"\n"), 0700))
	prepareRoleOK(t, os.WriteFile(reader, []byte(piIntakeDigestReader), 0600))
	t.Setenv("FIXTURE_NODE", node)
	t.Setenv("FIXTURE_READER", reader)
	t.Setenv("NODE_OPTIONS", "")
	t.Setenv("AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256", "must-not-reach-child")
	return launcher, nodeErr
}

func TestPiIntakeIssuedConfigDigest(t *testing.T) {
	launcher, nodeErr := installPiIntakeDigestReader(t)
	for _, tc := range []struct {
		mode    string
		outcome provider.Outcome
		exit    int
		reason  string
	}{
		{"excerpt-only", provider.OutcomeSucceeded, 0, ""},
		{"excerpt-and-continue", provider.OutcomeSucceeded, 0, ""},
		{"wrong-digest", provider.OutcomeNotStarted, -1, "无法构造成员工具执行器，执行未启动"},
		{"go-recheck", provider.OutcomeNotStarted, -1, "成员工具配置无效或已变化，执行未启动"},
		{"bridge-recheck", provider.OutcomeUnknown, 42, ""},
		{"legacy-193", provider.OutcomeSucceeded, 0, ""},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			if tc.exit >= 0 && nodeErr != nil {
				t.Skip("offline bridge parser fixture requires Node")
			}
			t.Setenv("FIXTURE_INTAKE_MODE", tc.mode)
			ctx := t.Context()
			f := newRolePrepareFixture(t)
			f.enableRuntime(t)
			f.opts.Providers[1].Path, f.opts.ExecutionTimeout = launcher, 10*time.Second
			f.exec(t, "UPDATE members SET model='fixture/model' WHERE id=?", f.coordinator.ID)
			legacy := tc.mode == "legacy-193"
			if legacy {
				useLegacyNativePiBridge(t, f)
			} else {
				useReleasePiBridge(t, f)
				f.opts.EnableNativeSessionResume, f.opts.NativePiThinking = true, "off"
				f.opts.Investigation = &InvestigationPreparationOptions{PiPath: launcher}
				prepareRoleOK(t, os.Mkdir(filepath.Join(f.opts.PrivateWorkspaceRoot, InvestigationCWDName), 0700))
				f.s.EnableInvestigationExecution() // The topic still has no investigation policy.
			}
			f.runtimeOptions.ChatIntake = coordination.New(f.s)
			if tc.mode == "excerpt-and-continue" {
				// Schema-only stand-in; no continuation or #196 service is invoked.
				f.runtimeOptions.WorkInputs = struct{ MemberWorkInputService }{}
			}
			runtime, err := NewMemberToolRuntime(f.s, f.runtimeOptions)
			prepareRoleOK(t, err)
			f.opts.MemberTools = runtime
			message, err := NewLocalUser(f.s).CreateMessage(ctx, f.group.ID, MessageInput{
				TopicID: "intake-digest", Content: "这次只讨论，不执行。", RecipientIDs: []string{f.coordinator.ID},
			}, "local-intake")
			prepareRoleOK(t, err)
			prepared, err := f.preparer(t).Prepare(ctx, store.PendingDelivery{GroupID: f.group.ID, Delivery: message.Deliveries[0]})
			prepareRoleOK(t, err)
			run := prepared.Executor.(*memberToolRuntimeRun)
			if !run.intake || run.intakeSchema.piDiscussionExcerpt == legacy || run.intakeSchema.investigation || run.nativeSession != nil {
				t.Fatal("expected fresh chat intake without an investigation action")
			}
			var token string
			if legacy {
				factory := run.factory
				run.factory = func(path, id string, names []string) (provider.Executor, error) {
					_, token = memberToolRuntimeConfig(t, path)
					return factory(path, id, names)
				}
			} else if build := run.intakeBuilder; build != nil {
				run.intakeBuilder = func(issued provider.NativeMemberToolConfig) (provider.Executor, error) {
					bytes, err := os.ReadFile(issued.Path)
					prepareRoleOK(t, err)
					var config struct{ Token string }
					if len(bytes) == 0 || bytes[len(bytes)-1] != '\n' || sha256.Sum256(bytes) != issued.SHA256 || json.Unmarshal(bytes, &config) != nil {
						t.Fatal("builder did not receive the digest of the exact issued file")
					}
					token = config.Token
					names, err := runtime.Tools().TransportOperations(ctx, token)
					prepareRoleOK(t, err)
					if !slices.Equal(names, chatIntakeOperations()) {
						t.Fatal("digest binding changed the issued grants")
					}
					if tc.mode == "wrong-digest" {
						issued.SHA256[0] ^= 1
					}
					executor, err := build(issued)
					if tc.mode == "go-recheck" && err == nil {
						bytes[len(bytes)-1] = ' '
						prepareRoleOK(t, os.WriteFile(issued.Path, bytes, 0600))
					}
					return executor, err
				}
			}
			request, claim := claimNativeFenceFixture(t, f, prepared)
			receipt := run.Execute(ctx, request, func(event provider.Event) error { return f.s.RecordTurnEvent(ctx, claim, event) })
			if receipt.Outcome != tc.outcome || receipt.ExitCode != tc.exit || receipt.NativeRef != nil ||
				receipt.Started != (tc.exit >= 0) || receipt.Accepted != (tc.exit == 0) || tc.reason != "" && receipt.Reason != tc.reason {
				t.Fatalf("unexpected intake receipt: outcome=%s exit=%d started=%t accepted=%t", receipt.Outcome, receipt.ExitCode, receipt.Started, receipt.Accepted)
			}
			if _, err := runtime.Tools().TransportOperations(ctx, token); token == "" || !errors.Is(err, store.ErrMemberToolDenied) || len(runtime.tools.capabilities) != 0 {
				t.Fatal("intake did not issue and revoke its capability")
			}
			memberToolRuntimeEmpty(t, f.runtimeOptions.ConfigRoot)
			_, err = f.s.ApplyTurnReceipt(ctx, claim, receipt)
			prepareRoleOK(t, err)
		})
	}
}
