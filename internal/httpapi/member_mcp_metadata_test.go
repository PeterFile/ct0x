package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// Synthetic values, with the keys emitted by Codex 0.166 mcp_tool_call.rs.
const memberMCPCodexMeta = `{"callId":"fixture-call","threadId":"fixture-thread","sessionId":"fixture-session","windowId":"fixture-window","itemId":"fixture-item"}`

func memberMCPWithMeta(body, meta string) string {
	if strings.Contains(body, `"params":{}`) {
		return strings.Replace(body, `"params":{}`, `"params":{"_meta":`+meta+`}`, 1)
	}
	return strings.Replace(body, `"params":{`, `"params":{"_meta":`+meta+`,`, 1)
}

func TestMemberMCPMetadataOrdinaryCallerAndLifecycle(t *testing.T) {
	f := newMemberMCPHTTP(t)
	meta := strings.TrimSuffix(memberMCPCodexMeta, "}") + fmt.Sprintf(`,"caller":"user","group_id":%q,"member_id":%q,"conversation_id":"foreign","turn_id":"foreign","claim":"forged","purpose":"chat_intake/v1","grants":{"operations":["decide_dispatch","submit_result"]},"example.org/context":[null,true,3,{"opaque":"value"}]}`, f.otherID, f.foreign.ID)
	before := memberRoleHTTPState(t, f)
	for _, body := range []string{
		memberMCPInitialize,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":"ping","method":"ping","params":{}}`,
		`{"jsonrpc":"2.0","id":"list","method":"tools/list","params":{}}`,
	} {
		w := memberMCPHTTP(t, f, memberMCPWithMeta(body, meta))
		if w.Code != 200 && w.Code != 202 || strings.Contains(w.Body.String(), `"error":`) {
			t.Fatalf("opaque lifecycle metadata rejected: %d %s", w.Code, w.Body)
		}
	}
	if memberRoleHTTPState(t, f) != before {
		t.Fatal("lifecycle metadata changed durable state")
	}
	body := memberMCPCall(`"ordinary-meta"`, "send_message", `{"content":"forged"}`)
	text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPWithMeta(body, meta)), false)
	var receipt memberToolHTTPSendResult
	readinessHTTPOK(t, json.Unmarshal([]byte(text), &receipt))
	message, err := f.db.GetMessage(context.Background(), f.groupID, receipt.MessageID)
	readinessHTTPOK(t, err)
	if message.GroupID != f.groupID || message.SenderID != f.caller.ID || message.TopicID != "trusted-topic" || message.Content != "forged" {
		t.Fatal("metadata selected an ordinary caller or channel")
	}
	var tasks, submissions, intakes int
	readinessHTTPOK(t, f.raw.QueryRow(`SELECT (SELECT count(*) FROM tasks),(SELECT count(*) FROM task_submissions),(SELECT count(*) FROM chat_intake_executions)`).Scan(&tasks, &submissions, &intakes))
	if tasks != 0 || submissions != 0 || intakes != 0 {
		t.Fatal("opaque purpose upgraded an ordinary call into intake or task authority")
	}
	before = memberRoleHTTPState(t, f)
	for i, call := range []string{
		memberMCPCall(`"ordinary-role"`, "submit_result", `{"summary":"not a worker"}`),
		memberMCPCall(`"ordinary-intake"`, "send_message", `{"content":"not intake","intent_assessment":{"disposition":"discuss","reason":"opaque purpose is not authority"}}`),
	} {
		if text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPWithMeta(call, meta)), true); i == 0 && !strings.Contains(text, `"code":"forbidden"`) {
			t.Fatal("metadata granted worker authority")
		}
	}
	f.tools.Revoke(f.token)
	if text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPWithMeta(body, meta)), true); !strings.Contains(text, `"code":"forbidden"`) {
		t.Fatal("metadata revived a revoked capability or cached its receipt")
	}
	if memberRoleHTTPState(t, f) != before {
		t.Fatal("metadata bypassed ordinary purpose or revocation fences")
	}
}

// Only the executor is synthetic. The existing fixture supplies real SQLite
// provenance, claims, runtime issuance, native handlers and durable receipts.
type memberMCPMetadataExecutor func() provider.ExecutionReceipt

func (f memberMCPMetadataExecutor) Execute(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
	return f()
}

func memberMCPMetadataRole(t *testing.T, role *readinessHTTPFixture, use func(*memberToolHTTPFixture, domain.Turn, store.CoordinationRoleProvenance)) store.TurnClaim {
	t.Helper()
	ctx := context.Background()
	state, err := role.a.db.ResolveCoordinationDelivery(ctx, role.group.ID, role.pending.Delivery.ID)
	readinessHTTPOK(t, err)
	if state.Blocked != store.CoordinationReady {
		t.Fatalf("role fixture is not ready: %s", state.Blocked)
	}
	turn, err := role.a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: role.group.ID, DeliveryID: role.pending.Delivery.ID, MaxActive: 1, ResourceKeys: []string{"fixture-role-resource"}})
	readinessHTTPOK(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	tools := role.runtime.Tools()
	f := &memberToolHTTPFixture{db: role.a.db, raw: role.raw, path: role.a.path, groupID: turn.GroupID, claim: claim, tools: tools}
	routes := append(MemberMCPRoutes(tools), MemberRoleToolRoutes(tools)...)
	f.handler = mustServer(t, DefaultAddress, routes...).Handler
	binding := platform.MemberToolRunBinding{GroupID: turn.GroupID, MemberID: turn.MemberID, ConversationID: turn.ConversationID, DeliveryID: turn.DeliveryID, ProviderID: state.ProviderID, ProviderName: provider.Pi, Model: state.Model}
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, ExitCode: 0, Reason: "synthetic executor, not native completion evidence"}
	calls := 0
	executor, err := role.runtime.WrapCoordinationRole(binding, state.Provenance, func(path, id string, names []string) (provider.Executor, error) {
		// This is the private config created for this fixture, not a native credential.
		data, err := os.ReadFile(path)
		readinessHTTPOK(t, err)
		var config struct{ Token string }
		readinessHTTPOK(t, json.Unmarshal(data, &config))
		if id != turn.ID || config.Token == "" || config.Token == claim.Token {
			t.Fatal("runtime did not issue a distinct current-run capability")
		}
		f.token = config.Token
		return memberMCPMetadataExecutor(func() provider.ExecutionReceipt {
			calls++
			use(f, turn, state.Provenance)
			return raw
		}), nil
	})
	readinessHTTPOK(t, err)
	got := executor.Execute(ctx, provider.ExecuteRequest{RunID: turn.ID, RuntimeDir: role.runRoot, CWD: role.runRoot, Model: state.Model}, nil)
	if calls != 1 || got != raw {
		t.Fatal("real runtime did not preserve the single synthetic invocation's unknown outcome")
	}
	_, err = role.a.db.ApplyTurnReceipt(ctx, claim, got)
	readinessHTTPOK(t, err)
	return claim
}

func TestMemberMCPCodexMetadataWorkerSubmission(t *testing.T) {
	role := newReadinessHTTPFixture(t)
	role.selectWorker(t)
	ctx := context.Background()
	claim := memberMCPMetadataRole(t, role, func(f *memberToolHTTPFixture, turn domain.Turn, p store.CoordinationRoleProvenance) {
		arguments := `{"summary":"Completed evidence"}`
		body := memberMCPCall(`"worker-meta"`, "submit_result", arguments)
		first := memberMCPHTTP(t, f, memberMCPWithMeta(body, memberMCPCodexMeta))
		if first.Code != 200 {
			t.Fatalf("current worker submit_result with Codex metadata: HTTP %d %s", first.Code, first.Body)
		}
		text := memberMCPText(t, first, false)
		var receipt struct {
			SubmissionID string `json:"submission_id"`
		}
		readinessHTTPOK(t, json.Unmarshal([]byte(text), &receipt))
		submission, err := f.db.GetTaskSubmission(ctx, f.groupID, p.TaskID, receipt.SubmissionID)
		readinessHTTPOK(t, err)
		assignment, err := f.db.GetAssignment(ctx, f.groupID, p.AssignmentID)
		readinessHTTPOK(t, err)
		if assignment.Binding == nil || assignment.Binding.TurnID != turn.ID || submission.AttemptID != assignment.Binding.AttemptID || submission.TaskID != p.TaskID || submission.GroupID != turn.GroupID || submission.SubmittedBy != turn.MemberID || submission.Summary != "Completed evidence" {
			t.Fatal("metadata changed the bound worker, task, claim or attempt")
		}
		task, err := f.db.GetTask(ctx, f.groupID, p.TaskID)
		readinessHTTPOK(t, err)
		if task.Status != domain.TaskStatusSubmitted || task.CurrentSubmissionID != submission.ID {
			t.Fatal("MCP did not persist the authoritative Submission independently of acceptance")
		}
		meta := `{"progressToken":7,"callId":"different-call","threadId":"different-thread","sessionId":"different-session","caller":"user","group_id":"foreign","member_id":"foreign","conversation_id":"foreign","turn_id":"foreign","claim":"forged","purpose":"chat_intake/v1","grants":{"operations":["decide_dispatch"]},"task_id":"foreign","attempt_id":"foreign","idempotency_key":"chosen","example.org/context":{"nested":[null,true,3]}}`
		before := memberRoleHTTPState(t, f)
		replay := memberMCPHTTP(t, f, memberMCPWithMeta(body, meta))
		if replay.Body.String() != first.Body.String() {
			t.Fatal("different metadata changed the original Submission receipt")
		}
		native := f.request(t, "submit_result", arguments, memberMCPKey(f.token, "worker-meta"))
		if native.Code != 201 || native.Body.String() != text {
			t.Fatal("MCP did not use the original Store/native receipt")
		}
		for _, changed := range []string{
			memberMCPCall(`"worker-meta"`, "submit_result", `{"summary":"different evidence"}`),
			memberMCPCall(`"worker-meta"`, "read_channel", `{}`),
		} {
			if text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPWithMeta(changed, meta)), true); !strings.Contains(text, `"code":"conflict"`) {
				t.Fatal("metadata bypassed same-id tool/arguments binding")
			}
		}
		denied := memberMCPCall(`"worker-role"`, "decide_dispatch", `{"kind":"decline","reason":"not a coordinator"}`)
		if text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPWithMeta(denied, meta)), true); !strings.Contains(text, `"code":"forbidden"`) {
			t.Fatal("metadata upgraded a worker into a coordinator")
		}
		for i, field := range []string{"task_id", "purpose"} {
			invalid := memberMCPCall(fmt.Sprint(10+i), "submit_result", fmt.Sprintf(`{"summary":"forged",%q:"foreign"}`, field))
			if text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPWithMeta(invalid, meta)), true); !strings.Contains(text, `"code":"invalid_json"`) {
				t.Fatal("opaque metadata weakened the business arguments schema")
			}
		}
		if memberRoleHTTPState(t, f) != before {
			t.Fatal("replay, conflicts or metadata forgery changed durable role state")
		}
		_, err = f.db.RequestTurnCancel(ctx, f.groupID, turn.ID)
		readinessHTTPOK(t, err)
		before = memberRoleHTTPState(t, f)
		if text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPWithMeta(body, meta)), true); !strings.Contains(text, `"code":"forbidden"`) {
			t.Fatal("metadata or receipt replay bypassed an expired claim")
		}
		if memberRoleHTTPState(t, f) != before {
			t.Fatal("expired replay mutated role state")
		}
	})
	turn, err := role.a.db.GetTurn(ctx, claim.GroupID, claim.TurnID)
	readinessHTTPOK(t, err)
	var resources, submissions int
	readinessHTTPOK(t, role.raw.QueryRow(`SELECT (SELECT count(*) FROM resource_claims WHERE turn_id=?),(SELECT count(*) FROM task_submissions)`, claim.TurnID).Scan(&resources, &submissions))
	if turn.State != domain.TurnStateUnknown || turn.ClaimToken != claim.Token || resources != 1 || submissions != 1 {
		t.Fatal("Submission or cancellation released an unknown execution's claim/resource")
	}
}

func TestMemberMCPCodexMetadataCoordinatorDecision(t *testing.T) {
	role := newReadinessHTTPFixture(t)
	memberMCPMetadataRole(t, role, func(f *memberToolHTTPFixture, turn domain.Turn, p store.CoordinationRoleProvenance) {
		meta := strings.TrimSuffix(memberMCPCodexMeta, "}") + fmt.Sprintf(`,"purpose":"worker","member_id":%q,"worker_member_id":%q,"task_id":"foreign","grants":{"operations":["submit_result"]}}`, role.worker.ID, role.lead.ID)
		body := memberMCPCall(`"coordinator-meta"`, "decide_dispatch", fmt.Sprintf(`{"kind":"assign","worker_member_id":%q,"reason":"eligible worker"}`, role.worker.ID))
		memberMCPText(t, memberMCPHTTP(t, f, memberMCPWithMeta(body, meta)), false)
		decision, err := f.db.GetDispatchDecision(context.Background(), f.groupID, p.RequestID)
		readinessHTTPOK(t, err)
		if decision.Decision.CoordinatorMemberID != turn.MemberID || decision.Decision.CoordinatorTurnID != turn.ID || decision.Decision.TaskID != p.TaskID || decision.Assignment == nil || decision.Assignment.WorkerMemberID != role.worker.ID || decision.Assignment.GroupID != f.groupID {
			t.Fatal("metadata selected coordinator authority or the assignment worker/task")
		}
		before := memberRoleHTTPState(t, f)
		denied := memberMCPCall(`"coordinator-role"`, "submit_result", `{"summary":"not a worker"}`)
		if text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPWithMeta(denied, meta)), true); !strings.Contains(text, `"code":"forbidden"`) || memberRoleHTTPState(t, f) != before {
			t.Fatal("metadata granted a coordinator worker authority")
		}
	})
}

func TestMemberMCPMetadataValidationBoundaries(t *testing.T) {
	f := newMemberMCPHTTP(t)
	before := memberRoleHTTPState(t, f)
	body := memberMCPCall("1", "read_channel", `{}`)
	for _, meta := range []string{"null", "[]", "[{}]", "false", "7", `"opaque"`,
		`{"progressToken":null}`, `{"progressToken":{}}`, `{"progressToken":[]}`, `{"progressToken":true}`, `{"progressToken":1.5}`, `{"progressToken":9007199254740992}`, `{"progressToken":"` + strings.Repeat("x", 129) + `"}`} {
		w := memberMCPHTTP(t, f, memberMCPWithMeta(body, meta))
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":-32602`) {
			t.Fatalf("invalid metadata/progress token accepted: %d %s", w.Code, w.Body)
		}
	}
	for _, invalid := range []string{
		memberMCPWithMeta(body, `{"callId":"a","\u0063allId":"b"}`),
		memberMCPWithMeta(body, `{"example.org/context":[{"key":1,"Key":2}]}`),
		memberMCPWithMeta(body, `{"progressToken":1,"ProgressToken":2}`),
		memberMCPWithMeta(memberMCPWithMeta(body, `{}`), `{}`),
		memberMCPWithMeta(body, "{\"opaque\":\"\xff\"}"),
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"callId":}}}`,
	} {
		w := memberMCPHTTP(t, f, invalid)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":-32700`) {
			t.Fatalf("metadata bypassed strict full-body JSON decoding: %d %s", w.Code, w.Body)
		}
	}
	w := memberMCPHTTP(t, f, `{"jsonrpc":"2.0","id":1,"method":"ping","_meta":{}}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":-32600`) {
		t.Fatal("params metadata allowance leaked into the RPC top level")
	}
	bounded := `{"jsonrpc":"2.0","id":2,"method":"ping","params":{"_meta":{"opaque":""}}}`
	bounded = strings.Replace(bounded, `"opaque":""`, `"opaque":"`+strings.Repeat("x", memberToolJSONBytes-len(bounded))+`"`, 1)
	if memberMCPHTTP(t, f, bounded).Code != 200 || memberMCPHTTP(t, f, bounded+" ").Code != 413 {
		t.Fatal("opaque metadata escaped the existing complete-body byte cap")
	}
	if memberRoleHTTPState(t, f) != before {
		t.Fatal("metadata validation or read-only probes mutated durable state")
	}
}
