package platform_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// This spy checks the C transport boundary only. It does not implement B's
// persistent commands, replay, eligibility or mutation fences.
type workInputProbe struct {
	grants      platform.MemberWorkInputGrants
	issued      []store.CoordinationToolScope
	items       []platform.MemberWorkInput
	calls       []workInputCall
	mutationErr error
}

type workInputCall struct {
	operation, inputID, reason, key, candidateID, content string
	role                                                  store.CoordinationToolScope
	member                                                store.MemberToolScope
	proposal                                              platform.MemberContinueWorkInput
}

func (p *workInputProbe) WorkInputGrants(_ context.Context, scope store.CoordinationToolScope) (platform.MemberWorkInputGrants, error) {
	p.issued = append(p.issued, scope)
	return p.grants, nil
}

func (p *workInputProbe) ReadWorkInputs(_ context.Context, scope store.CoordinationToolScope) ([]platform.MemberWorkInput, error) {
	p.calls = append(p.calls, workInputCall{operation: "read_work_inputs", role: scope})
	return p.items, nil
}

func (p *workInputProbe) AckWorkInput(_ context.Context, scope store.CoordinationToolScope, inputID, key string) error {
	p.calls = append(p.calls, workInputCall{operation: "ack_work_input", role: scope, inputID: inputID, key: key})
	return p.mutationErr
}

func (p *workInputProbe) HandBackWorkInput(_ context.Context, scope store.CoordinationToolScope, inputID, reason, key string) error {
	p.calls = append(p.calls, workInputCall{operation: "hand_back_work_input", role: scope, inputID: inputID, reason: reason, key: key})
	return p.mutationErr
}

func (p *workInputProbe) ContinueWork(_ context.Context, scope store.MemberToolScope, candidateID string, input platform.MemberContinueWorkInput, content, key string) (domain.ChatAssessmentReceipt, error) {
	p.calls = append(p.calls, workInputCall{operation: "continue_work", member: scope, candidateID: candidateID, proposal: input, content: content, key: key})
	return domain.ChatAssessmentReceipt{ID: "assessment-fixture", Disposition: "continue_work", NoticeMessageID: "notice-fixture", CreatedAt: time.Now().UTC()}, p.mutationErr
}

func withWorkInputProbe(t *testing.T, p *workInputProbe) *roleRuntimeFixture {
	t.Helper()
	f := newRoleRuntime(t)
	f.options.WorkInputs = p
	f.options.ChatIntake = coordination.New(f.db)
	var err error
	f.runtime, err = platform.NewMemberToolRuntime(f.db, f.options)
	roleOK(t, err)
	routes := append(httpapi.MemberToolRoutes(f.runtime.Tools()), httpapi.MemberRoleToolRoutes(f.runtime.Tools())...)
	routes = append(routes, httpapi.MemberMCPRoutes(f.runtime.Tools())...)
	server, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
	roleOK(t, err)
	f.handler = server.Handler
	return f
}

func claimWorkInputWorker(t *testing.T, f *roleRuntimeFixture) roleRun {
	t.Helper()
	ctx := context.Background()
	lead := f.coordinator(t)
	factoryCalls, nativeCalls := 0, 0
	receipt := f.wrap(t, lead, func(token string) provider.ExecutionReceipt {
		w := f.http(t, token, "read_work_inputs", `{}`, "")
		if w.Code != 403 {
			t.Fatalf("coordinator gained work-input access: %d %s", w.Code, w.Body)
		}
		w = f.http(t, token, "decide_dispatch", fmt.Sprintf(`{"kind":"assign","worker_member_id":%q,"reason":"fixture"}`, f.worker.ID), "assign")
		if w.Code != 201 {
			t.Fatalf("assign fixture failed: %d %s", w.Code, w.Body)
		}
		return roleNotStarted()
	}, &factoryCalls, &nativeCalls).Execute(ctx, lead.request, nil)
	if receipt != roleNotStarted() || factoryCalls != 1 || nativeCalls != 1 {
		t.Fatal("invalid coordinator fixture")
	}
	_, err := f.db.ApplyTurnReceipt(ctx, lead.claim, receipt)
	roleOK(t, err)
	decision, err := f.db.GetDispatchDecision(ctx, f.group, lead.provenance.RequestID)
	roleOK(t, err)
	assignment, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	roleOK(t, err)
	return f.claim(t, assignment.Delivery.DeliveryID)
}

func TestWorkInputsRealWorkerBindingNativeAndMCP(t *testing.T) {
	for _, withAck := range []bool{false, true} {
		t.Run(fmt.Sprintf("ack=%v", withAck), func(t *testing.T) {
			p := &workInputProbe{grants: platform.MemberWorkInputGrants{Read: true, Ack: withAck, HandBack: true},
				items: []platform.MemberWorkInput{{InputID: "input-1", Content: "正文", Constraint: "仅校对"}}}
			f := withWorkInputProbe(t, p)
			ctx := context.Background()
			run := claimWorkInputWorker(t, f)
			if len(p.issued) != 0 || len(p.calls) != 0 {
				t.Fatal("coordinator or queued work reached the service")
			}
			member, err := f.db.BindMemberTool(ctx, run.claim)
			roleOK(t, err)
			want := []string{"read_channel", "search_history", "read_history", "submit_result", "read_work_inputs"}
			if withAck {
				want = append(want, "ack_work_input")
			}
			want = append(want, "hand_back_work_input")
			var token string
			executor, err := f.runtime.WrapCoordinationRoleWithCapabilities(run.binding, run.provenance, nil, func(path, id string, names []string) (provider.Executor, error) {
				wire, err := os.ReadFile(path)
				roleOK(t, err)
				var config struct{ Token string }
				roleOK(t, json.Unmarshal(wire, &config))
				token = config.Token
				if id != run.claim.TurnID || token == "" || token == run.claim.Token || !slices.Equal(names, want) {
					t.Fatalf("unexpected adapter binding/profile: %v", names)
				}
				return roleNative(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					memberMCPRoleProfile(t, f, token, want...)
					read := f.http(t, token, "read_work_inputs", `{}`, "")
					text := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, "1", "tools/call", `{"name":"read_work_inputs","arguments":{}}`), false)
					if read.Code != 200 || text != read.Body.String() || !strings.Contains(text, `"constraint":"仅校对"`) || len(p.calls) != 2 {
						t.Fatalf("read did not stay a read through both transports: %d %s", read.Code, text)
					}
					for _, invalid := range []struct {
						op, body, key string
						status        int
					}{
						{"read_work_inputs", `{"task_id":"foreign"}`, "", 400},
						{"ack_work_input", `{"input_id":"input-1"}`, "", 400},
						{"ack_work_input", `{"input_id":"input-1","task_id":"foreign"}`, "bad", 400},
						{"hand_back_work_input", `{"input_id":"input-1","reason":null}`, "bad", 400},
						{"hand_back_work_input", `{"input_id":"input-1","reason":"理由","INPUT_ID":"other"}`, "bad", 400},
						{"hand_back_work_input", `{"input_id":"input-1","reason":"理由"}` + strings.Repeat(" ", 65536), "bad", 413},
					} {
						if w := f.http(t, token, invalid.op, invalid.body, invalid.key); w.Code != invalid.status || len(p.calls) != 2 {
							t.Fatalf("invalid %s reached B: %d %s", invalid.op, w.Code, w.Body)
						}
					}
					ack := `{"name":"ack_work_input","arguments":{"input_id":"input-1"}}`
					if !withAck {
						text := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, "2", "tools/call", ack), true)
						if !strings.Contains(text, `"code":"forbidden"`) || len(p.calls) != 2 {
							t.Fatal("work-input read/hand-back grants also authorized ack")
						}
					} else {
						first := memberMCPRoleHTTP(t, f, token, "2", "tools/call", ack)
						text := memberMCPRoleText(t, first, false)
						key := p.calls[2].key
						if !strings.HasPrefix(key, "mcp-") || len(key) != 68 || key == token || text != "{\"input_id\":\"input-1\"}\n" {
							t.Fatal("ack lost the server key or returned a non-minimal receipt")
						}
						replay := memberMCPRoleHTTP(t, f, token, "2", "tools/call", strings.Replace(ack, `"name":`, `"_meta":{"task_id":"foreign","idempotency_key":"forged"},"name":`, 1))
						native := f.http(t, token, "ack_work_input", `{"input_id":"input-1"}`, key)
						if replay.Body.String() != first.Body.String() || native.Code != 201 || native.Body.String() != text || p.calls[2] != p.calls[3] || p.calls[2] != p.calls[4] {
							t.Fatal("unchanged MCP/native replay did not forward the same command identity")
						}
						changed := memberMCPRoleHTTP(t, f, token, "2", "tools/call", strings.Replace(ack, "input-1", "input-2", 1))
						if !strings.Contains(memberMCPRoleText(t, changed, true), `"code":"conflict"`) || len(p.calls) != 5 {
							t.Fatal("altered MCP arguments under the same id reached B")
						}
					}
					back := `{"name":"hand_back_work_input","arguments":{"input_id":"input-1","reason":" 原因\n"}}`
					if text := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, "3", "tools/call", back), false); !strings.Contains(text, `"input_id":"input-1"`) || strings.Contains(text, "原因") {
						t.Fatal("invalid or excessive hand-back receipt")
					}
					if got := p.calls[len(p.calls)-1]; got.reason != " 原因\n" || got.operation != "hand_back_work_input" || !strings.HasPrefix(got.key, "mcp-") {
						t.Fatalf("hand-back request changed before B: %q %q", got.operation, got.reason)
					}
					p.mutationErr = errors.Join(store.ErrConflict, errors.New("PRIVATE outcome unknown"))
					w := f.http(t, token, "hand_back_work_input", `{"input_id":"input-1","reason":"仍需对账"}`, "uncertain")
					if w.Code != 500 || !strings.Contains(w.Body.String(), "未确认") || strings.Contains(w.Body.String(), "PRIVATE") {
						t.Fatal("unconfirmed backend error was classified as a rejection or leaked")
					}
					p.items[0].Content = strings.Repeat("<", 12000)
					if w := f.http(t, token, "read_work_inputs", `{}`, ""); w.Code != 413 || strings.Contains(w.Body.String(), `"items"`) {
						t.Fatal("escaped response overflow did not fail the whole read")
					}
					return roleNotStarted()
				}), nil
			})
			roleOK(t, err)
			if receipt := executor.Execute(ctx, run.request, nil); receipt != roleNotStarted() || len(p.issued) != 1 {
				t.Fatalf("worker failed to use one issued profile: %+v", receipt)
			}
			scope := p.issued[0]
			if scope.Member != member || scope.Provenance != run.provenance || scope.AttemptID == "" {
				t.Fatal("worker operations not bound to the actual turn, role and attempt")
			}
			for _, call := range p.calls {
				if call.role != scope {
					t.Fatal("transport arguments changed worker scope")
				}
			}
			before := len(p.calls)
			if w := f.http(t, token, "read_work_inputs", `{}`, ""); w.Code != 403 || len(p.calls) != before {
				t.Fatal("revoked capability reached B")
			}
		})
	}
}

func TestWorkContinuationUsesCapturedIntakeCandidate(t *testing.T) {
	p := &workInputProbe{}
	f := withWorkInputProbe(t, p)
	ctx := context.Background()
	msg, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: "topic", Content: "请继续原工作，仅校对。", RecipientIDs: []string{f.worker.ID}}, "source")
	roleOK(t, err)
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: msg.Deliveries[0].ID, MaxActive: 1})
	roleOK(t, err)
	claim := store.TurnClaim{GroupID: f.group, TurnID: turn.ID, Token: turn.ClaimToken}
	member, err := f.db.BindMemberTool(ctx, claim)
	roleOK(t, err)
	if token, err := f.runtime.Tools().Issue(ctx, claim, platform.MemberToolGrants{Operations: []string{"read_work_inputs"}}); token != "" || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("generic Issue accepted the work-input operation")
	}
	ordinary, err := f.runtime.Tools().Issue(ctx, claim, platform.MemberToolGrants{Operations: []string{"send_message"}})
	roleOK(t, err)
	body := `{"content":"记录续办。","intent_assessment":{"disposition":"continue_work","reason":"用户续办","followup":{"task_id":"reference-only","constraint":" 仅校对\n"}}}`
	if w := f.http(t, ordinary, "send_message", body, "generic"); w.Code != 403 || len(p.calls) != 0 {
		t.Fatal("ordinary capability assessed a continuation")
	}
	f.runtime.Tools().Revoke(ordinary)
	token, err := f.runtime.Tools().IssueChatIntake(ctx, claim)
	roleOK(t, err)
	t.Cleanup(func() { f.runtime.Tools().Revoke(token) })
	candidate, err := f.runtime.Tools().ChatCandidate(ctx, token)
	roleOK(t, err)
	if candidate == nil || candidate.MessageID != msg.ID {
		t.Fatal("fixture lacks a real bound candidate")
	}
	for _, invalid := range []string{
		strings.Replace(body, `"task_id":`, `"group_id":"foreign","task_id":`, 1),
		strings.Replace(body, `"task_id":`, `"TASK_ID":"foreign","task_id":`, 1),
		strings.Replace(body, `"reference-only"`, `null`, 1),
		strings.Replace(body, `"followup":{`, `"action":"workspace_change","followup":{`, 1),
		strings.Replace(body, `"continue_work"`, `"discuss"`, 1),
		strings.Replace(body, `"intent_assessment":`, `"recipient_ids":[],"intent_assessment":`, 1),
	} {
		if w := f.http(t, token, "send_message", invalid, "invalid"); w.Code != 400 || len(p.calls) != 0 {
			t.Fatalf("forged continuation reached B: %d %s", w.Code, w.Body)
		}
	}
	params := fmt.Sprintf(`{"name":"send_message","arguments":%s}`, body)
	text := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, `"continue"`, "tools/call", params), false)
	if len(p.issued) != 0 || len(p.calls) != 1 || !strings.Contains(text, `"disposition":"continue_work"`) {
		t.Fatal("intake did not route to the separate continuation operation")
	}
	got := p.calls[0]
	if got.operation != "continue_work" || got.member != member || got.candidateID != candidate.ID || got.content != "记录续办。" ||
		got.proposal != (platform.MemberContinueWorkInput{Reason: "用户续办", TaskID: "reference-only", Constraint: " 仅校对\n"}) || !strings.HasPrefix(got.key, "mcp-") {
		t.Fatal("intake changed the authorized candidate, proposal or key")
	}
}
