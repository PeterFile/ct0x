package provider

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
)

// Reconstruct the 0.166.0 stdio no-model diagnostic's thread/start frame with
// synthetic IDs, CWD and model. The trace records an empty turns array as a count.
const codex166StdioThreadStartFrame = `{"id":3,"result":{
  "activePermissionProfile":null,"approvalPolicy":"never","approvalsReviewer":"user",
  "cwd":"/synthetic/workspace","disabledPluginIds":[],"instructionSources":[],
  "model":"synthetic-model","modelProvider":"openai","multiAgentMode":"explicitRequestOnly","reasoningEffort":"max",
  "runtimeWorkspaceRoots":["/synthetic/workspace"],"sandbox":{"type":"readOnly","networkAccess":false},"serviceTier":null,
  "thread":{"id":"synthetic-thread","sessionId":"synthetic-session","forkedFromId":null,"parentThreadId":null,
    "preview":"","ephemeral":false,"projectId":null,"modelProvider":"openai","createdAt":1,"updatedAt":1,
    "status":{"type":"idle"},"cwd":"/synthetic/workspace","cliVersion":"0.166.0","source":"vscode","turns":[]}
}}`

func newCodex166SourceProtocol(t *testing.T) *codexAppServerProtocol {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &codexAppServerProtocol{
		ctx: ctx, cancel: cancel, awaiting: 3,
		request: ExecuteRequest{CWD: "/synthetic/workspace", Model: "synthetic-model", Prompt: "synthetic prompt"},
		receipt: &ExecutionReceipt{}, session: &codexAppServerSession{provider: "openai", effort: "max"},
		member: &codexAppServerMemberSession{}, send: func(int, string, any) bool { return true },
	}
}

func receiveCodex166SourceFrame(t *testing.T, p *codexAppServerProtocol, raw string) {
	t.Helper()
	frame, err := decodeCodexRPCFrame([]byte(raw))
	if err != nil {
		t.Fatal("invalid synthetic protocol frame")
	}
	p.receive(frame)
}

func TestCodexSession166ThreadResponseSource(t *testing.T) {
	for _, tt := range []struct{ name, from, to, want string }{
		{name: "installed-stdio", want: "vscode"},
		{"appServer", `"source":"vscode",`, `"source":"appServer",`, "appServer"},
		{"unknown", `"source":"vscode",`, `"source":"unknown",`, ""},
		{"cli", `"source":"vscode",`, `"source":"cli",`, ""},
		{"exec", `"source":"vscode",`, `"source":"exec",`, ""},
		{"subagent", `"source":"vscode",`, `"source":{"subAgent":"review"},`, ""},
		{"custom", `"source":"vscode",`, `"source":{"custom":"vscode"},`, ""},
		{"missing", `"source":"vscode",`, ``, ""},
		{"null", `"source":"vscode",`, `"source":null,`, ""},
		{"wrong-case", `"source":"vscode",`, `"source":"VSCode",`, ""},
		{"duplicate", `"source":"vscode",`, `"source":"vscode","source":"appServer",`, ""},
		{"fork", `"forkedFromId":null`, `"forkedFromId":"other-thread"`, ""},
		{"parent", `"parentThreadId":null`, `"parentThreadId":"other-thread"`, ""},
		{"version", `"cliVersion":"0.166.0"`, `"cliVersion":"0.166.1"`, ""},
		{"thread-cwd", `"cwd":"/synthetic/workspace","cliVersion"`, `"cwd":"/synthetic/workspace/.","cliVersion"`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newCodex166SourceProtocol(t)
			receiveCodex166SourceFrame(t, p, strings.Replace(codex166StdioThreadStartFrame, tt.from, tt.to, 1))
			if tt.want != "" {
				if p.damaged || p.awaiting != 4 || p.session.source != tt.want || p.receipt.NativeSessionID != "synthetic-thread" || p.receipt.Accepted {
					t.Fatal("valid source did not bind the native thread and reach config recheck")
				}
			} else if !p.damaged || p.awaiting != 0 || p.session.source != "" || p.receipt.NativeSessionID != "" {
				t.Fatal("invalid provenance bound a thread or advanced the protocol")
			}
		})
	}
}

func TestCodexSession166ThreadHistorySource(t *testing.T) {
	// Synthetic thread/read, not part of the installed no-model trace. Test just
	// the read gates after the other readiness/terminal checks have run.
	const history = `{"thread":{"id":"synthetic-thread","source":"vscode","cliVersion":"0.166.0",
	  "ephemeral":false,"cwd":"/synthetic/workspace","modelProvider":"openai","status":{"type":"idle"},
	  "turns":[{"id":"synthetic-turn","status":"completed","items":[
	    {"id":"user","type":"userMessage","content":[{"type":"text","text":"synthetic prompt"}]},
	    {"id":"answer","type":"agentMessage","text":"synthetic answer"}]}]}}`
	capture := func(t *testing.T, source string) *codexAppServerProtocol {
		t.Helper()
		p := newCodex166SourceProtocol(t)
		receiveCodex166SourceFrame(t, p, codex166StdioThreadStartFrame)
		if p.awaiting != 4 || p.damaged {
			t.Fatal("failed to bind source from thread/start")
		}
		p.awaiting, p.turnID = 9, "synthetic-turn"
		p.items = map[string]codexAppServerItem{
			"answer": {kind: "agentMessage", completed: true, textHash: sha256.Sum256([]byte("synthetic answer"))},
		}
		receiveCodex166SourceFrame(t, p, `{"id":9,"result":`+strings.Replace(history, `"vscode"`, source, 1)+`}`)
		return p
	}
	fresh := capture(t, `"vscode"`)
	if fresh.external || fresh.session.pending == nil || fresh.session.pending.Codex == nil {
		t.Fatal("matching source failed history capture")
	}
	if switched := capture(t, `"appServer"`); !switched.external || switched.session.pending != nil {
		t.Fatal("capture with a different allowed source did not fence settlement")
	}

	for _, source := range []string{"vscode", "appServer", "cli"} {
		t.Run("resume/"+source, func(t *testing.T) {
			p := newCodex166SourceProtocol(t)
			p.session.resume = fresh.session.pending.Codex
			// The tied response also has empty turns on resume (excludeTurns).
			receiveCodex166SourceFrame(t, p, codex166StdioThreadStartFrame)
			if p.awaiting != 4 || p.damaged {
				t.Fatal("failed to bind source from thread/resume")
			}
			p.awaiting = 8
			receiveCodex166SourceFrame(t, p, `{"id":8,"result":`+strings.Replace(history, `"vscode"`, `"`+source+`"`, 1)+`}`)
			if source == "vscode" {
				if _, verified := p.session.priorTurns["synthetic-turn"]; p.damaged || p.awaiting != 5 || !verified {
					t.Fatal("matching resume history did not enable turn/start")
				}
			} else if !p.damaged || p.awaiting != 0 || len(p.session.priorTurns) != 0 {
				t.Fatal("resume with a mismatched source advanced past the history gate")
			}
		})
	}
}
