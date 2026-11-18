package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testNativeSessionAdapter(name, extension string, config NativeMemberToolConfig, names []string, ref *NativeSessionReference, binding []byte) Adapter {
	if name == Pi {
		return NewPiNativeSessionMemberToolAdapter(extension, config, names, PiSessionOptions{Resume: ref, Binding: binding})
	}
	return NewCodexAppServerNativeSessionMemberToolAdapter(config, names, CodexAppServerSessionOptions{Resume: ref, Binding: binding})
}

func testIssuedMemberConfig(t *testing.T, path string) NativeMemberToolConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return NativeMemberToolConfig{Path: path, SHA256: sha256.Sum256(data)}
}

func testPiReleaseBridge(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	if err != nil || !PiNativeSessionBridgeSupported(path) {
		t.Fatal("checkout's Pi entry/dependency do not match the audited release pair")
	}
	return path
}

func TestNativeSessionAdapterPiChildDigest(t *testing.T) {
	selected, _, path := memberToolFiles(t, `
[ "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256" = "$FIXTURE_EXPECTED_SHA" ] || exit 51
[ -r "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG" ] || exit 52
prev=
for arg do
  if [ "$prev" = --extension ]; then [ "$arg" = "$FIXTURE_PHYSICAL_ENTRY" ] || exit 56; fi
  prev=$arg
  [ "$arg" != "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256" ] || exit 53
  [ "$arg" != "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG" ] || exit 54
done
exit 0`)
	config := testIssuedMemberConfig(t, path)
	expected := hex.EncodeToString(config.SHA256[:])
	t.Setenv("FIXTURE_EXPECTED_SHA", expected)
	t.Setenv(piMemberToolConfigSHAEnv, "inherited-untrusted-digest")
	t.Setenv(piMemberToolConfigEnv, "inherited-untrusted-path")
	release := testPiReleaseBridge(t)
	physical, err := filepath.EvalSymlinks(release)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE_PHYSICAL_ENTRY", physical)
	alias := filepath.Join(t.TempDir(), "alias.ts")
	if err := os.Symlink(release, alias); err != nil {
		t.Fatal(err)
	}
	e, err := testNativeSessionAdapter(Pi, alias, config, []string{"read_channel"}, nil, []byte(`{"current":1}`)).NewExecutor(selected, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request := ExecuteRequest{RunID: "run1", CWD: piSessionPrivateTemp(t), RuntimeDir: piSessionPrivateTemp(t), Model: "fixture/model", Thinking: "off", Prompt: "offline"}
	got := e.Execute(context.Background(), request, nil)
	if !got.Started || !got.DirectChildReaped || got.ExitCode != 0 || got.Outcome != OutcomeUnknown {
		t.Fatal("fixture child did not receive the bound digest via environment only")
	}
	piSessionAssertNoSecret(t, got, nil, expected, memberToolTestToken, path)
	if os.Getenv(piMemberToolConfigSHAEnv) != "inherited-untrusted-digest" || os.Getenv(piMemberToolConfigEnv) != "inherited-untrusted-path" {
		t.Fatal("Pi execution modified the parent bootstrap environment")
	}
}

func TestNativeSessionAdapterPiBridgeGateKeepsLegacy(t *testing.T) {
	t.Setenv(piMemberToolConfigSHAEnv, "must-not-reach-legacy")
	selected, _, path := memberToolFiles(t, `
[ -z "${AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256+x}" ] || exit 41
[ -r "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG" ] || exit 42
exit 0`)
	config := testIssuedMemberConfig(t, path)
	release := filepath.Dir(testPiReleaseBridge(t))
	for _, changed := range []string{"index.ts", "bridge.mjs"} {
		t.Run(changed, func(t *testing.T) {
			root := t.TempDir()
			entry := filepath.Join(root, "index.ts")
			for _, name := range []string{"index.ts", "bridge.mjs"} {
				data, err := os.ReadFile(filepath.Join(release, name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			names := []string{"read_channel"}
			adapter := testNativeSessionAdapter(Pi, entry, config, names, nil, nil)
			e, err := adapter.NewExecutor(selected, 0)
			if err != nil {
				t.Fatal(err)
			}
			// A readable replacement may advertise the same supported version.
			// It does not forward (entry) or verify (dependency) any digest.
			v1 := `// Pi version 1.4.0; claimed digest support` + "\n"
			if changed == "index.ts" {
				v1 += `import { createClient } from "./bridge.mjs"; export default () => createClient(process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG);`
			} else {
				v1 += `export function createClient(_path) { return () => {}; }`
			}
			if err := os.WriteFile(filepath.Join(root, changed), []byte(v1), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Capabilities(selected); err == nil {
				t.Fatal("unaudited release advertised native resume")
			}
			if e, err := adapter.NewExecutor(selected, 0); e != nil || err == nil {
				t.Fatal("unaudited release constructed a native executor")
			}
			request := ExecuteRequest{RunID: "native", CWD: piSessionPrivateTemp(t), RuntimeDir: piSessionPrivateTemp(t), Model: "fixture/model", Thinking: "off", Prompt: "offline"}
			if got := e.Execute(context.Background(), request, nil); got.Started {
				t.Fatal("release drift after construction crossed invocation fence")
			}
			legacyAdapter := NewPiMemberToolAdapter(entry, path, names)
			caps, err := legacyAdapter.Capabilities(selected)
			if err != nil || caps.NativeResume != SupportUnsupported {
				t.Fatal("native qualification changed legacy support")
			}
			legacy, err := legacyAdapter.NewExecutor(selected, 0)
			if err != nil {
				t.Fatal(err)
			}
			request.RunID, request.Thinking = "legacy", ""
			if got := legacy.Execute(context.Background(), request, nil); !got.Started || !got.DirectChildReaped || got.ExitCode != 0 || got.NativeRef != nil {
				t.Fatal("ordinary synthetic V1 invocation lost its path-only bootstrap")
			}
		})
	}
}

func TestNativeSessionAdapterPiActualReaderRejectsChildRewrite(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("offline bridge reader fixture requires a local Node executable")
	}
	// This child is not Pi. It changes one field after Go's final config check,
	// then calls the actual qualified local reader exactly as index.ts does.
	script := filepath.Join(t.TempDir(), "rewrite.mjs")
	if err := os.WriteFile(script, []byte(`
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { pathToFileURL } from "node:url";
const entry = process.argv[process.argv.indexOf("--extension") + 1];
const { createClient, openBridge } = await import(pathToFileURL(join(dirname(entry), "bridge.mjs")));
const path = process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG;
const digest = process.env.AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256;
const original = readFileSync(path);
if (createHash("sha256").update(original).digest("hex") !== digest) process.exit(51);
openBridge(path, digest);
const config = JSON.parse(original);
const field = process.env.FIXTURE_REWRITE;
config[field] = field === "token" ? config.token.replace("LEAK", "PEEK") : config.endpoint.replace(":1/", ":2/");
const changed = JSON.stringify(config) + "\n";
const before = statSync(path);
if (changed.length !== original.length || original.equals(Buffer.from(changed))) process.exit(52);
writeFileSync(path, changed);
const after = statSync(path);
if (before.ino !== after.ino || before.mode !== after.mode || before.size !== after.size) process.exit(53);
createClient(path); // The legacy path-only contract still accepts these valid bytes.
try { openBridge(path, digest); } catch (error) {
  if ([path, digest, config.token].some(value => error.message.includes(value))) process.exit(54);
  process.exit(0);
}
process.exit(55);
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE_NODE", node)
	t.Setenv("FIXTURE_SCRIPT", script)
	t.Setenv("NODE_OPTIONS", "")
	for _, field := range []string{"token", "endpoint"} {
		t.Run(field, func(t *testing.T) {
			t.Setenv("FIXTURE_REWRITE", field)
			selected, _, path := memberToolFiles(t, `exec "$FIXTURE_NODE" "$FIXTURE_SCRIPT" "$@"`)
			config := testIssuedMemberConfig(t, path)
			e, err := testNativeSessionAdapter(Pi, testPiReleaseBridge(t), config, []string{"read_channel"}, nil, nil).NewExecutor(selected, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			request := ExecuteRequest{RunID: "run1", CWD: piSessionPrivateTemp(t), RuntimeDir: piSessionPrivateTemp(t), Model: "fixture/model", Thinking: "off", Prompt: "offline"}
			var events []Event
			got := e.Execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
			if !got.Started || !got.DirectChildReaped || got.ExitCode != 0 || got.Outcome != OutcomeUnknown || len(events) != 0 {
				t.Fatalf("qualified reader did not reject %s drift (exit %d)", field, got.ExitCode)
			}
			piSessionAssertNoSecret(t, got, events, path, hex.EncodeToString(config.SHA256[:]), memberToolTestToken, "SYNTHETIC-MEMBER-TOKEN-MUST-NOT-PEEK")
		})
	}
}

func TestNativeSessionAdapterCodexUsesSnapshotAfterChildStarts(t *testing.T) {
	const alternateToken = "SYNTHETIC-MEMBER-TOKEN-MUST-NOT-PEEK"
	selected, path, request := appServerMemberFixture(t, "normal")
	config := testIssuedMemberConfig(t, path)
	t.Setenv("FIXTURE_CODEX_PROCESS", selected.Path)
	t.Setenv("FIXTURE_REWRITE_PATH", path)
	t.Setenv("FIXTURE_REWRITE_BYTES", strings.NewReplacer(memberToolTestToken, alternateToken, ":1/", ":2/").Replace(memberToolTestConfig))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	// This shell runs only after the final Go file check.
	selected.Path = executable(t, filepath.Join(t.TempDir(), "fixture-codex"), `
printf '%s' "$FIXTURE_REWRITE_BYTES" > "$FIXTURE_REWRITE_PATH" || exit 51
exec "$FIXTURE_CODEX_PROCESS" "$@"`)
	var err error
	request.CWD, err = filepath.EvalSymlinks(request.CWD)
	if err != nil {
		t.Fatal(err)
	}
	e, err := testNativeSessionAdapter(Codex, "", config, []string{"read_channel"}, nil, nil).NewExecutor(selected, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	got := e.Execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil })
	if !got.Started || !got.DirectChildReaped {
		t.Fatal("synthetic AppServer did not observe the native bootstrap")
	}
	// The existing AppServer fixture marks correct token-env binding. Its RPC
	// transcript must likewise retain the original endpoint, not reopen the path.
	if _, err := os.Stat(filepath.Join(request.CWD, "child-token-bound")); err != nil {
		t.Fatal("synthetic child did not receive the issued token snapshot")
	}
	frames, err := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
	if err != nil || !bytes.Contains(frames, []byte(`"url":"http://127.0.0.1:1/api/v1/member-tools/mcp"`)) || bytes.Contains(frames, []byte(`http://127.0.0.1:2/api/`)) {
		t.Fatal("synthetic child did not receive the issued endpoint snapshot")
	}
	visible, err := json.Marshal(struct {
		Receipt ExecutionReceipt
		Events  []Event
	}{got, events})
	if err != nil {
		t.Fatal(err)
	}
	visible = append(visible, frames...)
	for _, log := range []string{got.StdoutPath, got.StderrPath} {
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		visible = append(visible, data...)
	}
	for _, value := range []string{path, memberToolTestToken, alternateToken, hex.EncodeToString(config.SHA256[:])} {
		if bytes.Contains(visible, []byte(value)) {
			t.Fatal("Codex bootstrap leaked config material into the receipt, protocol or lifecycle logs")
		}
	}
}

// Version fixtures only: constructing a native adapter must not launch a CLI.
func TestNativeSessionAdaptersExplicitFreshCaptureAndFrozenInput(t *testing.T) {
	selected, _, path := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	extension := testPiReleaseBridge(t)
	config := testIssuedMemberConfig(t, path)
	for _, name := range []string{Codex, Pi} {
		t.Run(name, func(t *testing.T) {
			p := selected
			selection := AdapterSelection{ID: PiMemberToolAdapterID, Protocol: PiExtensionProtocol}
			if name == Codex {
				p.Name, p.Version = Codex, "0.166.0"
				selection = AdapterSelection{ID: CodexAppServerAdapterID, Protocol: CodexAppServerProtocol}
			}
			names := []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"}
			binding := []byte(`{"current":1}`)
			adapter := testNativeSessionAdapter(name, extension, config, names, nil, binding)
			binding[11] = '9'
			names[0] = "workspace_write"
			registry, err := NewAdapterRegistry(AdapterRegistration{ID: selection.ID, Protocol: selection.Protocol, Adapter: adapter})
			if err != nil {
				t.Fatal(err)
			}
			e, err := registry.NewExecutor(selection, p, 0, CapabilityPlatformTools, CapabilityTerminalEvidence, CapabilityNativeResume)
			if err != nil || e == nil {
				t.Fatalf("fresh-and-capture construction failed: %v", err)
			}
			var captured []byte
			if name == Pi {
				opts := e.(piDigestBoundExecutor).piSession
				if opts == nil || opts.resume != nil {
					t.Fatal("explicit opt-in constructed legacy Pi, not fresh-and-capture")
				}
				captured = opts.binding
			} else {
				opts := e.(codexAppServerMemberToolExecutor).sessionOptions
				if opts == nil || opts.Resume != nil {
					t.Fatal("explicit opt-in constructed legacy Codex, not fresh-and-capture")
				}
				captured = opts.Binding
			}
			if !bytes.Equal(captured, []byte(`{"current":1}`)) {
				t.Fatal("adapter retained caller-owned binding bytes")
			}
			caps, err := registry.Capabilities(selection, p)
			if err != nil || !caps.Supports(CapabilityNativeResume) || caps.Steer != SupportUnsupported {
				t.Fatalf("wrong capability report for the native text builder: %+v, %v", caps, err)
			}
			if _, err := DefaultAdapterRegistry().Capabilities(selection, p); err == nil {
				t.Fatal("opt-in adapter registered by default")
			}
		})
	}
	if _, err := os.Stat(selected.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("adapter construction launched the sentinel CLI")
	}
}

func TestNativeSessionAdaptersNeverFallbackOrAcceptFileProfile(t *testing.T) {
	p, extension, path := memberToolFiles(t, `exit 99`)
	release := testPiReleaseBridge(t)
	config := testIssuedMemberConfig(t, path)
	for _, name := range []string{Codex, Pi} {
		t.Run(name, func(t *testing.T) {
			selected := p
			if name == Codex {
				selected.Name, selected.Version = Codex, "0.166.0"
			}
			names := []string{"read_channel"}
			ref := &NativeSessionReference{Version: NativeSessionReferenceVersion, Binding: []byte(`{"old":1}`)}
			adapter := testNativeSessionAdapter(name, release, config, names, ref, nil)
			ref.Binding[7] = '2'
			frozen := adapter.(nativeSessionMemberToolAdapter)
			if name == Pi && !bytes.Equal(frozen.pi.Resume.Binding, []byte(`{"old":1}`)) || name == Codex && !bytes.Equal(frozen.codex.Resume.Binding, []byte(`{"old":1}`)) {
				t.Fatal("adapter did not copy historical binding")
			}
			if e, err := adapter.NewExecutor(selected, 0); err == nil || e != nil {
				t.Fatal("invalid explicit resume silently constructed a fresh executor")
			}
			file := testNativeSessionAdapter(name, release, config, WorkspaceWorkerToolNames(), nil, nil)
			if _, err := file.Capabilities(selected); err == nil {
				t.Fatal("file worker was mislabeled as a text-session profile")
			}
			if e, err := file.NewExecutor(selected, 0); err == nil || e != nil {
				t.Fatal("direct construction bypassed the text-profile gate")
			}
			a := testNativeSessionAdapter(name, release, config, names, nil, nil)
			badVersion, badPath := selected, selected
			badVersion.Version, badPath.Path = "future", "relative"
			for _, invalid := range []Provider{badVersion, badPath} {
				if _, err := a.Capabilities(invalid); err == nil {
					t.Fatal("unknown version/path promoted native support")
				}
			}
			unsealed := testNativeSessionAdapter(name, release, NativeMemberToolConfig{Path: path}, names, nil, []byte(`{"current":1}`))
			if e, err := unsealed.NewExecutor(selected, 0); err == nil || e != nil {
				t.Fatal("a private path alone was allowed to construct a runtime-bound executor")
			}
			legacy := NewPiMemberToolAdapter(extension, path, names)
			if name == Codex {
				legacy = NewCodexAppServerMemberToolAdapter(path, names)
			}
			caps, err := legacy.Capabilities(selected)
			if err != nil || caps.NativeResume != SupportUnsupported || caps.Steer != SupportUnsupported {
				t.Fatal("new opt-in changed legacy adapter behavior")
			}
		})
	}
}
