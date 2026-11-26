package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const acpFixtureToken = "fixture-only-token-not-a-credential"

// Fixtures exercise the wire engine, not a claim that a real vendor is safe.
func acpFixture(t *testing.T, mode string) (acpExecutor, ExecuteRequest, acpLaunch) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fixture.py")
	if err := os.WriteFile(path, []byte("#!"+python+"\n"+acpFixtureScript), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "member.json")
	if err := os.WriteFile(configPath, []byte(`{"endpoint":"http://127.0.0.1:12345/api/v1/member-tools","token":"`+acpFixtureToken+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	member, err := readMemberToolConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := ACPConfig{AgentPath: path, Version: "0.68.0", VendorProfile: ACPGemini068, Model: "gemini-explicit", MemberToolConfigPath: configPath, ToolNames: []string{"read_channel"}}
	e := acpExecutor{config: config, timeout: 3 * time.Second}
	request := ExecuteRequest{RunID: "run", CWD: dir, RuntimeDir: dir, Prompt: "/native-command must remain literal"}
	launch := acpLaunch{args: []string{mode}, env: os.Environ(), server: "aw_fixture_unique", agent: "aw_fixture_unique", member: member}
	return e, request, launch
}

func TestACPChildV1HandshakeAndCompletedSnapshot(t *testing.T) {
	e, request, launch := acpFixture(t, "normal")
	var events []Event
	r := e.execute(context.Background(), request, func(event Event) error { events = append(events, event); return nil }, launch)
	if r.Outcome != OutcomeSucceeded || r.Terminal != OutcomeSucceeded || !r.Started || !r.Accepted || !r.OutputComplete || !r.DirectChildReaped || r.NativeSessionID != "native-session" || r.ExitCode != 0 {
		t.Fatalf("matching v1 child lifecycle not settled: %+v", r)
	}
	if len(events) != 4 || events[0].Kind != SessionEvent || events[1].Kind != AcceptedEvent || events[2].Kind != ResponseEvent || events[2].Text != "hello world" || events[3].Kind != TerminalEvent {
		t.Fatalf("not a completed assistant-only snapshot: %+v", events)
	}
	data, err := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("unexpected native methods: %d", len(lines))
	}
	var messages []map[string]any
	for _, line := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			t.Fatal("bad client frame")
		}
		messages = append(messages, m)
	}
	init := messages[0]["params"].(map[string]any)
	caps := init["clientCapabilities"].(map[string]any)
	if init["protocolVersion"] != float64(1) || caps["terminal"] != false || !reflect.DeepEqual(caps["fs"], map[string]any{"readTextFile": false, "writeTextFile": false}) {
		t.Fatal("not fixed v1/no client fs or terminal")
	}
	newParams := messages[1]["params"].(map[string]any)
	server := newParams["mcpServers"].([]any)[0].(map[string]any)
	if newParams["cwd"] != request.CWD || server["type"] != "http" || server["name"] != launch.server || server["url"] != launch.member.endpoint+"/mcp" {
		t.Fatal("not current HTTP MCP/cwd")
	}
	prompt := messages[2]["params"].(map[string]any)
	text := prompt["prompt"].([]any)[0].(map[string]any)["text"].(string)
	if prompt["sessionId"] != "native-session" || strings.HasPrefix(text, "/") || !strings.Contains(text, request.Prompt) {
		t.Fatal("prompt not bound/framed")
	}
	for _, path := range []string{r.StdoutPath, r.StderrPath} {
		raw, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(raw), acpFixtureToken) || strings.Contains(string(raw), "hello") || strings.Contains(string(raw), "rawInput") {
			t.Fatal("native content leaked into summary logs")
		}
	}
	pidRaw, _ := os.ReadFile(filepath.Join(request.CWD, "pid"))
	pid, _ := strconv.Atoi(string(pidRaw))
	if _, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); err != syscall.ECHILD {
		t.Fatal("receipt did not follow real child Wait")
	}
}

func TestACPChildTerminalSemantics(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want Outcome
	}{{"empty", OutcomeSucceeded}, {"failed-tool", OutcomeSucceeded}, {"refusal", OutcomeFailed}, {"max_tokens", OutcomeTruncated}, {"max_turn_requests", OutcomeTruncated}, {"cancelled", OutcomeAborted}} {
		t.Run(tc.mode, func(t *testing.T) {
			e, request, launch := acpFixture(t, tc.mode)
			responses := 0
			r := e.execute(context.Background(), request, func(event Event) error {
				if event.Kind == ResponseEvent {
					responses++
				}
				return nil
			}, launch)
			if r.Outcome != tc.want || r.Terminal != tc.want || !r.Accepted || !r.OutputComplete || !r.DirectChildReaped {
				t.Fatalf("native terminal mapping failed: %+v", r)
			}
			if tc.mode == "empty" && responses != 0 {
				t.Fatal("empty native success fabricated a response")
			}
		})
	}
}

func TestACPChildUnboundOrIncompleteExecutionStaysUnknown(t *testing.T) {
	for _, mode := range []string{"wrong-init-id", "v2", "missing-http", "wrong-new-id", "cross-session", "provisional-cross-session", "orphan-tool-update", "pending-tool", "unknown-tool", "unknown-stop", "duplicate-terminal", "late-text", "rpc-error", "disconnect", "duplicate-key", "invalid-utf8", "oversize", "nonzero", "agent-version", "unknown-update", "background-tool"} {
		t.Run(mode, func(t *testing.T) {
			e, request, launch := acpFixture(t, mode)
			r := e.execute(context.Background(), request, nil, launch)
			if !r.Started || !r.DirectChildReaped || r.Outcome != OutcomeUnknown {
				t.Fatalf("untrusted native evidence settled: %+v", r)
			}
			if mode == "wrong-init-id" || mode == "v2" || mode == "missing-http" || mode == "wrong-new-id" {
				if r.Accepted {
					t.Fatal("handshake accepted prompt")
				}
			}
		})
	}
}

func TestACPChildPermissionIdentityAndNoPermanentApproval(t *testing.T) {
	for _, mode := range []string{"permission-issued", "permission-cross-server", "permission-native", "permission-no-metadata", "permission-no-once", "oc-permission"} {
		t.Run(mode, func(t *testing.T) {
			e, request, launch := acpFixture(t, mode)
			if mode == "oc-permission" {
				e.config.VendorProfile = ACPOpenCode119
				e.config.Version = "1.19.6"
				e.config.Model = "opencode/big-pickle"
			}
			r := e.execute(context.Background(), request, nil, launch)
			want := OutcomeUnknown
			if mode == "permission-issued" || mode == "permission-no-once" {
				want = OutcomeSucceeded
			}
			if r.Outcome != want || !r.DirectChildReaped {
				t.Fatalf("permission widened or lost native evidence: %+v", r)
			}
			raw, err := os.ReadFile(filepath.Join(request.CWD, "permission-reply.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "never-use-permanent") || !strings.Contains(string(raw), map[bool]string{true: "this-once-47", false: "deny-83"}[mode == "permission-issued"]) {
				t.Fatal("permission did not select this request's exact safe option")
			}
		})
	}
}

func TestACPChildRefusesHostServicesAndUnexpectedModelSelection(t *testing.T) {
	for _, mode := range []string{"fs-request", "terminal-request", "oc-model-fallback", "oc-agent-fallback", "gemini-model-fallback", "provisional-flood"} {
		t.Run(mode, func(t *testing.T) {
			e, request, launch := acpFixture(t, mode)
			if strings.HasPrefix(mode, "oc-") {
				e.config.VendorProfile = ACPOpenCode119
				e.config.Version = "1.19.6"
				e.config.Model = "opencode/big-pickle"
			}
			r := e.execute(context.Background(), request, nil, launch)
			if r.Outcome != OutcomeUnknown || !r.DirectChildReaped {
				t.Fatalf("host capability/fallback was accepted: %+v", r)
			}
			if strings.Contains(mode, "fallback") {
				if r.NativeSessionID != "native-session" || r.Accepted {
					t.Fatal("created session reference lost on rejected selection")
				}
				raw, _ := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
				if strings.Contains(string(raw), `"method":"session/prompt"`) {
					t.Fatal("fallback received prompt")
				}
			}
			if strings.HasSuffix(mode, "request") {
				if _, err := os.Stat(filepath.Join(request.CWD, "host-denied")); err != nil {
					t.Fatal("host request was not explicitly rejected")
				}
			}
		})
	}
	for _, mode := range []string{"provisional", "oc-text"} {
		t.Run(mode, func(t *testing.T) {
			e, request, launch := acpFixture(t, mode)
			if mode == "oc-text" {
				e.config.VendorProfile = ACPOpenCode119
				e.config.Version = "1.19.6"
				e.config.Model = "opencode/big-pickle"
			}
			r := e.execute(context.Background(), request, nil, launch)
			if r.Outcome != OutcomeSucceeded {
				t.Fatalf("valid bound setup rejected: %+v", r)
			}
		})
	}
}

func acpWaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fixture did not reach %s", filepath.Base(path))
}

func TestACPChildCancellationWaitsForOriginalPromptAndRealWait(t *testing.T) {
	for _, mode := range []string{"cancel", "cancel-no-terminal"} {
		t.Run(mode, func(t *testing.T) {
			e, request, launch := acpFixture(t, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan ExecutionReceipt, 1)
			go func() { done <- e.execute(ctx, request, nil, launch) }()
			acpWaitFile(t, filepath.Join(request.CWD, "ready"))
			cancel()
			var r ExecutionReceipt
			select {
			case r = <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("cancellation failed to join child")
			}
			if r.Outcome != OutcomeUnknown || !r.Started || !r.DirectChildReaped {
				t.Fatalf("cancel notification masqueraded as settled receipt: %+v", r)
			}
			if (r.Terminal == OutcomeAborted) != (mode == "cancel") || r.Accepted != (mode == "cancel") {
				t.Fatalf("original prompt evidence not preserved: %+v", r)
			}
			raw, _ := os.ReadFile(filepath.Join(request.CWD, "client.jsonl"))
			if strings.Count(string(raw), `"method":"session/cancel"`) != 1 {
				t.Fatal("cancel was not a single notification")
			}
			if _, err := os.Stat(filepath.Join(request.CWD, "exited")); err != nil {
				t.Fatal("returned before fixture normal shutdown")
			}
		})
	}
}

func TestACPChildKillInheritedPipeAndStderrDrain(t *testing.T) {
	for _, mode := range []string{"blocked-stdin", "refuse-exit", "inherited-pipe", "large-stderr"} {
		t.Run(mode, func(t *testing.T) {
			e, request, launch := acpFixture(t, mode)
			var r ExecutionReceipt
			if mode == "blocked-stdin" || mode == "refuse-exit" {
				// Synchronize the intended failure phase, not Python startup speed
				// on HDD. The production cancellation/kill deadlines are unchanged.
				e.timeout = 10 * time.Second
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ready := "exited"
				if mode == "blocked-stdin" {
					request.Prompt = strings.Repeat("x", 1<<20)
					ready = "ready"
				}
				done := make(chan ExecutionReceipt, 1)
				go func() { done <- e.execute(ctx, request, nil, launch) }()
				acpWaitFile(t, filepath.Join(request.CWD, ready))
				cancel()
				select {
				case r = <-done:
				case <-time.After(4 * time.Second):
					t.Fatal("blocked fixture failed to join after cancellation")
				}
			} else {
				r = e.execute(context.Background(), request, nil, launch)
			}
			if !r.DirectChildReaped || !r.Started {
				t.Fatalf("no real wait: %+v", r)
			}
			if mode == "large-stderr" {
				if r.Outcome != OutcomeSucceeded || !r.OutputComplete {
					t.Fatalf("stderr not drained: %+v", r)
				}
			} else if r.Outcome != OutcomeUnknown || r.OutputComplete {
				t.Fatalf("unproven child/output shutdown settled: %+v", r)
			}
			if mode == "refuse-exit" && (!r.Accepted || r.Terminal != OutcomeSucceeded) {
				t.Fatal("kill fixture never reached its intended post-terminal phase")
			}
			if mode == "inherited-pipe" {
				raw, err := os.ReadFile(filepath.Join(request.CWD, "descendant"))
				pid, parseErr := strconv.Atoi(string(raw))
				if err != nil || parseErr != nil || pid <= 1 {
					t.Fatal("missing fixture descendant")
				}
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				if r.Terminal != OutcomeSucceeded || syscall.Kill(pid, 0) != nil {
					t.Fatal("direct wait was mistaken for descendant completion")
				}
			}
			for _, path := range []string{r.StdoutPath, r.StderrPath} {
				raw, err := os.ReadFile(path)
				if err != nil || len(raw) > executionLogLimit || strings.Contains(string(raw), acpFixtureToken) {
					t.Fatal("unbounded/raw token-bearing logs")
				}
			}
		})
	}
}

func TestACPChildDeliveryFailureAndInFlightJoin(t *testing.T) {
	for _, at := range []EventKind{SessionEvent, AcceptedEvent, ResponseEvent, TerminalEvent} {
		t.Run(string(at), func(t *testing.T) {
			e, request, launch := acpFixture(t, "normal")
			failed := false
			r := e.execute(context.Background(), request, func(event Event) error {
				if failed {
					t.Error("callback after failure")
				}
				if event.Kind == at {
					failed = true
					return errors.New(acpFixtureToken)
				}
				return nil
			}, launch)
			if !failed || !r.DeliveryFailed || r.Outcome != OutcomeUnknown || !r.DirectChildReaped || strings.Contains(r.Reason, acpFixtureToken) {
				t.Fatalf("callback failure was hidden/leaked: %+v", r)
			}
		})
	}
	e, request, launch := acpFixture(t, "normal")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan ExecutionReceipt, 1)
	go func() {
		done <- e.execute(context.Background(), request, func(event Event) error {
			if event.Kind == ResponseEvent {
				close(entered)
				<-release
			}
			return nil
		}, launch)
	}()
	select {
	case <-entered:
	case <-time.After(4 * time.Second):
		t.Fatal("callback not reached")
	}
	acpWaitFile(t, filepath.Join(request.CWD, "exited"))
	select {
	case <-done:
		t.Fatal("Execute abandoned in-flight callback")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case r := <-done:
		if r.Outcome != OutcomeSucceeded {
			t.Fatalf("joined successful callback failed: %+v", r)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("callback not joined")
	}
}

const acpFixtureScript = `import json, os, sys, time
mode=os.environ.get('AW_ACP_FIXTURE_MODE',sys.argv[1])
open('argv.json','w').write(json.dumps(sys.argv))
if mode=='gemini-profile':
    import fnmatch
    args=sys.argv[1:]
    assert args[0]=='--acp' and args[args.index('--extensions')+1]=='none' and args[args.index('--approval-mode')+1]=='default'
    configured_server=args[args.index('--allowed-mcp-server-names')+1]
    settings=json.load(open(os.environ['GEMINI_CLI_SYSTEM_SETTINGS_PATH']))
    core=settings['tools']['core']
    exact='mcp_'+configured_server+'_read_channel'
    assert core==[exact] and not any('*' in rule for rule in core)
    builtins=['read_file','write_file','run_shell_command','invoke_agent','web_fetch','activate_skill']
    assert not any(tool in core for tool in builtins)
    def decision(tool,rules):
        policies=[('*','DENY',4.24),('mcp_'+configured_server+'_*','ALLOW',4.1),('mcp_'+configured_server+'_*','ALLOW',4.2)]+[(rule,'ALLOW',4.25) for rule in rules]
        return max((p for p in policies if fnmatch.fnmatchcase(tool,p[0])),key=lambda p:p[2])[1]
    assert decision(exact,core)=='ALLOW' and decision(exact,[])=='DENY'
    assert all(decision(tool,core)=='DENY' for tool in builtins+['mcp_foreign_read_channel'])
    # computeMergedSettings reconstructs admin; only the independent skills field is effective.
    effective_admin={'skills':{'enabled':True}}
    skills_support=settings.get('skills',{}).get('enabled',True)
    assert skills_support==False and effective_admin['skills']['enabled']==True and settings['hooksConfig']['enabled']==False
    open('profile-checked','w').write('exact-core-priority-and-registration')
if mode=='oc-prepared-cache':
    # Mirror only the v1.19.6 disk/snapshot selection prerequisites, without loading an SDK.
    cache=os.path.join(os.environ['XDG_CACHE_HOME'],'opencode')
    path=os.environ.get('OPENCODE_MODELS_PATH',os.path.join(cache,'models.json'))
    assert path==os.path.join(cache,'models.json') and os.environ['OPENCODE_DISABLE_MODELS_FETCH']=='true' and not os.path.lexists(path)
    os.makedirs(os.path.join(cache,'bin'),exist_ok=True)  # native global bootstrap
    open('catalog-source','w').write('snapshot')
open('pid','w').write(str(os.getpid()))
def read():
    line=sys.stdin.readline()
    if not line: return None
    with open('client.jsonl','a') as f: f.write(line)
    return json.loads(line)
def send(m):
    print(json.dumps(m,separators=(',',':')),flush=True)
def result(id,value): send({'jsonrpc':'2.0','id':id,'result':value})
def update(u,session='native-session'): send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':session,'update':u}})
def chunk(text,kind='agent_message_chunk'): update({'sessionUpdate':kind,'content':{'type':'text','text':text}})
msg=read()
assert msg['method']=='initialize'
result(71 if mode=='wrong-init-id' else msg['id'],{'protocolVersion':2 if mode=='v2' else 1,'agentCapabilities':{'mcpCapabilities':{} if mode=='missing-http' else {'http':True}},'agentInfo':{'name':'fixture','version':'1.19.6' if mode.startswith('oc-') else ('unsupported' if mode=='agent-version' else '0.68.0')}})
msg=read()
if not msg: sys.exit(0)
assert msg['method']=='session/new'
if mode=='oc-prepared-cloudflare-gateway':
    # Mirror the pinned gateway loader with the catalog entry present, no auth and no baseURL override.
    # The final enabled_providers filter does not guard the custom loader invocation.
    config=json.loads(os.environ['OPENCODE_CONFIG_CONTENT'])
    assert config['enabled_providers']==['opencode']
    token=os.environ.get('CLOUDFLARE_API_TOKEN') or os.environ.get('CF_AIG_TOKEN')
    if ('cloudflare-ai-gateway' not in config.get('disabled_providers', []) and
        os.environ.get('CLOUDFLARE_ACCOUNT_ID') and os.environ.get('CLOUDFLARE_GATEWAY_ID') and not token):
        send({'jsonrpc':'2.0','id':msg['id'],'error':{'code':-32603,'message':'fixture: Cloudflare AI Gateway requires a token'}})
        sys.stdin.read()
        sys.exit(0)
server=msg['params']['mcpServers'][0]['name']
if mode=='gemini-profile': assert server==configured_server
if mode=='provisional-cross-session': update({'sessionUpdate':'available_commands_update','availableCommands':[]},'other')
if mode=='provisional': update({'sessionUpdate':'available_commands_update','availableCommands':[]})
if mode=='provisional-flood':
    for i in range(17): update({'sessionUpdate':'available_commands_update','availableCommands':[]})
session_result={'sessionId':'native-session'}
if mode.startswith('oc-'):
    selected_agent=json.loads(os.environ['OPENCODE_CONFIG_CONTENT'])['default_agent'] if mode.startswith('oc-prepared') else 'aw_fixture_unique'
    session_result['configOptions']=[{'id':'model','currentValue':'other/model' if mode=='oc-model-fallback' else 'opencode/big-pickle'},{'id':'mode','currentValue':'unrestricted' if mode=='oc-agent-fallback' else selected_agent}]
    if mode.endswith('-effort') or mode.endswith('-effort-change'): session_result['configOptions'].append({'id':'effort','currentValue':'default'})
if mode.startswith('gemini-prepared') or mode=='gemini-profile':
    session_result['models']={'currentModelId':'gemini-explicit'}
    session_result['modes']={'currentModeId':'default'}
    if mode.endswith('-no-model'): session_result.pop('models')
    if mode.endswith('-model-null'): session_result['models']=None
if mode=='gemini-model-fallback': session_result['models']={'currentModelId':'other-model'}
if mode=='oc-prepared-cache-drift':
    with open(os.environ['OPENCODE_MODELS_PATH'],'x') as f: f.write('{}')
result(72 if mode=='wrong-new-id' else msg['id'],session_result)
if mode=='blocked-stdin': open('ready','w').write('blocked phase'); time.sleep(30); sys.exit(0)
msg=read()
if not msg: sys.exit(0)
assert msg['method']=='session/prompt'
if mode in ('cancel','cancel-no-terminal'):
    open('ready','w').write('ready')
    cancel=read()
    assert cancel['method']=='session/cancel' and 'id' not in cancel and cancel['params']['sessionId']=='native-session'
    if mode=='cancel': result(msg['id'],{'stopReason':'cancelled'})
    sys.stdin.read();open('exited','w').write('done');sys.exit(0)
if mode=='disconnect': sys.exit(0)
if mode.startswith('permission-') or mode=='oc-permission' or mode.endswith('-permission'):
    call={'toolCallId':'permission-call','title':'read_channel (untrusted title)','kind':'other','status':'pending'}
    update(dict(call,sessionUpdate='tool_call'))
    call['_meta']={'gemini-cli':{'mcp':{'server_name':'foreign' if mode=='permission-cross-server' else server,'tool_name':'shell' if mode=='permission-native' else 'read_channel'}}}
    if mode=='permission-native': call['kind']='execute'
    if mode=='permission-no-metadata' or mode.endswith('-permission'): call.pop('_meta')
    options=[{'optionId':'never-use-permanent','kind':'allow_always','name':'Always'},{'optionId':'this-once-47','kind':'allow_once','name':'Once'},{'optionId':'deny-83','kind':'reject_once','name':'No'}]
    if mode=='permission-no-once': options=[options[0],options[2]]
    send({'jsonrpc':'2.0','id':'permission-1','method':'session/request_permission','params':{'sessionId':'native-session','toolCall':call,'options':options}})
    permission=read()
    open('permission-reply.json','w').write(json.dumps(permission))
    outcome=permission['result']['outcome']
    assert outcome['optionId']==('this-once-47' if mode=='permission-issued' else 'deny-83')
    update({'sessionUpdate':'tool_call_update','toolCallId':'permission-call','status':'completed' if mode=='permission-issued' else 'failed'})
if mode in ('fs-request','terminal-request') or mode.endswith('-fs-request') or mode.endswith('-terminal-request'):
    send({'jsonrpc':'2.0','id':101,'method':'fs/read_text_file' if mode=='fs-request' or mode.endswith('-fs-request') else 'terminal/create','params':{'sessionId':'native-session','path':'/not-readable','command':'not-executed'}})
    denied=read();assert denied['error']['code']==-32601
    open('host-denied','w').write('yes')
if mode=='rpc-error':
    send({'jsonrpc':'2.0','id':msg['id'],'error':{'code':-32603,'message':'fixture-only-token-not-a-credential'}})
    sys.stdin.read(); sys.exit(0)
if mode=='duplicate-key': print('{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"a","text":"b"}}}}',flush=True)
elif mode=='invalid-utf8': sys.stdout.buffer.write(b'{"jsonrpc":"2.0","method":"x","params":{"bad":"\xff"}}\n'); sys.stdout.buffer.flush()
elif mode=='oversize': print('x'*(1048576+1),flush=True)
elif mode=='cross-session': update({'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'foreign'}},'other')
elif mode=='orphan-tool-update': update({'sessionUpdate':'tool_call_update','toolCallId':'orphan','status':'completed'})
elif mode=='unknown-update': update({'sessionUpdate':'unclassified_extension','background':True})
elif mode=='background-tool':
    update({'sessionUpdate':'tool_call','toolCallId':'background','title':'do not infer from title','kind':'execute','status':'completed','_meta':{'gemini-cli':{'mcp':{'server_name':server,'tool_name':'read_channel'}}}})
else:
    if mode!='empty':
        chunk('hello '); chunk('not final','agent_thought_chunk'); update({'sessionUpdate':'plan','entries':[]}); chunk('world')
    if mode in ('normal','failed-tool','pending-tool','unknown-tool'):
        update({'sessionUpdate':'tool_call','toolCallId':'call-1','title':'not an identity','kind':'other','status':'pending','_meta':{'gemini-cli':{'mcp':{'server_name':server,'tool_name':'shell' if mode=='unknown-tool' else 'read_channel'}}}})
        update({'sessionUpdate':'tool_call_update','toolCallId':'call-1','content':[{'type':'content','content':{'type':'text','text':'tool output is not final'}}]})
        if mode!='pending-tool': update({'sessionUpdate':'tool_call_update','toolCallId':'call-1','status':'failed' if mode=='failed-tool' else 'completed'})
if '-prepared' in mode and not mode.endswith('-permission'):
    call={'sessionUpdate':'tool_call','toolCallId':'ordinary-1','title':'must not infer identity','status':'in_progress','kind':'execute' if mode.endswith('-native') else 'other'}
    if mode.endswith('-foreign'): call['_meta']={'gemini-cli':{'mcp':{'server_name':'foreign','tool_name':'read_channel'}}}
    if mode.endswith('-diff'): call['content']=[{'type':'diff','path':'/not-authorized','newText':'not written'}]
    if mode.endswith('-location'): call['locations']=[{'path':'/not-authorized'}]
    update(call, 'foreign-session' if mode.endswith('-cross-session') else 'native-session')
    if mode.endswith('-duplicate'): update(call)
    if not mode.endswith('-pending'): update({'sessionUpdate':'tool_call_update','toolCallId':'orphan' if mode.endswith('-orphan') else 'ordinary-1','status':'completed'})
    if mode.endswith('-background'): update({'sessionUpdate':'unknown_background','running':True})
    if mode.endswith('-config'): update({'sessionUpdate':'config_option_update','configOptions':[{'id':'model','currentValue':'wrong/model'}]})
    if mode.endswith('-mode'): update({'sessionUpdate':'current_mode_update','currentModeId':'unrestricted'})
    if mode.endswith('-effort-change'):
        update({'sessionUpdate':'config_option_update','configOptions':[{'id':'model','currentValue':'opencode/big-pickle'},{'id':'mode','currentValue':selected_agent},{'id':'effort','currentValue':'high'}]})
stop=mode if mode in ('refusal','max_tokens','max_turn_requests','cancelled') else ('new_stop' if mode=='unknown-stop' else 'end_turn')
result(msg['id'],{'stopReason':stop})
if mode=='duplicate-terminal' or mode.endswith('-duplicate-terminal'): result(msg['id'],{'stopReason':stop})
if mode=='late-text' or mode.endswith('-late-text'): chunk('late text')
if mode=='inherited-pipe':
    pid=os.fork()
    if pid==0: time.sleep(30);os._exit(0)
    open('descendant','w').write(str(pid))
if mode=='large-stderr': sys.stderr.write('fixture-only-token-not-a-credential'*100000)
sys.stderr.write('fixture-only-token-not-a-credential\n');sys.stderr.flush()
sys.stdin.read()
open('exited','w').write('done')
if mode=='refuse-exit': time.sleep(30)
sys.exit(7 if mode=='nonzero' or mode.endswith('-nonzero') else 0)
`
