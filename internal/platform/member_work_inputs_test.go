package platform_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if p != nil {
		f.options.WorkInputs = p
	}
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

func assertInvalidWorkMCPUnicode(t *testing.T, f *roleRuntimeFixture, token, id, name, args string) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
	r := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":-32602`) {
		t.Fatalf("invalid %s Unicode was not rejected before MCP binding: %d %s", name, w.Code, w.Body)
	}
}

func assertOpaqueWorkIDBoundary(t *testing.T, f *roleRuntimeFixture, p *workInputProbe, token, op, argsFormat string, forwardedID func(workInputCall) string) {
	t.Helper()
	t.Run("opaque ID whitespace boundary", func(t *testing.T) {
		maxID := strings.Repeat("🙂", 64)
		// Same cases as the Pi fixture. The first ID is deliberately not NFC.
		for i, tc := range []struct {
			id    string
			valid bool
		}{
			{"\ufeffe\u0301\u3000🙂\ufeff", true}, {"\ufeff", true}, {maxID, true},
			{"\u0085handle", false}, {"handle\u0085", false}, {"\u3000handle", false}, {"handle\u2003", false}, {maxID + "x", false},
		} {
			encoded, err := json.Marshal(tc.id)
			roleOK(t, err)
			args := fmt.Sprintf(argsFormat, encoded)
			key := fmt.Sprintf("opaque-id-%d", i)
			before := len(p.calls)
			native := f.http(t, token, op, args, key)
			params := fmt.Sprintf(`{"name":%q,"arguments":%s}`, op, args)
			mcp := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, fmt.Sprintf("%q", key), "tools/call", params), !tc.valid)
			calls := p.calls[before:]
			if tc.valid {
				if native.Code != 201 || len(calls) != 2 || forwardedID(calls[0]) != tc.id || forwardedID(calls[1]) != tc.id {
					t.Fatalf("%s changed or rejected opaque ID %q: %d, %s", op, tc.id, native.Code, mcp)
				}
			} else if native.Code != 400 || !strings.Contains(mcp, `"code":"invalid_input"`) || len(calls) != 0 {
				t.Fatalf("%s did not reject invalid opaque ID %q: %d, %s", op, tc.id, native.Code, mcp)
			}
		}
	})
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
					t.Run("envelope", func(t *testing.T) {
						const readParams = `{"name":"read_work_inputs","arguments":{}}`
						longID := `"` + strings.Repeat(`\u0000`, 128) + `"`
						p.items[0].Content = ""
						empty := memberMCPRoleHTTP(t, f, token, longID, "tools/call", readParams)
						memberMCPRoleText(t, empty, false)
						p.items[0].Content = strings.Repeat("x", 65536-empty.Body.Len())
						full := memberMCPRoleHTTP(t, f, token, longID, "tools/call", readParams)
						if text := memberMCPRoleText(t, full, false); full.Body.Len() != 65536 || !strings.Contains(text, p.items[0].Content) {
							t.Fatal("exact-limit complete MCP work input should fit without clipping")
						}
						for _, content := range []string{strings.Repeat(`"`, 20000), strings.Repeat("<", 12000), p.items[0].Content + "x"} {
							p.items[0].Content = content
							w := f.http(t, token, "read_work_inputs", `{}`, "")
							if w.Code != 413 || !strings.Contains(w.Body.String(), `"code":"tool_result_too_large"`) {
								t.Errorf("native read accepted work input over the full envelope budget (len=%d, status=%d)", len(content), w.Code)
								continue
							}
							text := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, "4", "tools/call", readParams), true)
							if !strings.HasPrefix(text, w.Body.String()) || strings.Contains(text, `"items"`) || strings.Contains(text, "limit") || strings.Contains(text, "length") {
								t.Error("MCP used a different budget or suggested nonexistent pagination")
							}
						}
					})
					if withAck {
						t.Run("hand-back text", func(t *testing.T) {
							p.mutationErr = nil
							const op = "hand_back_work_input"
							maxReason := strings.Repeat("🙂", 512)
							for i, reason := range []string{"", "\u0085", "a\x00b", maxReason + "x", maxReason, "\ufeff"} {
								before, valid := len(p.calls), i >= 4
								raw, err := json.Marshal(map[string]string{"input_id": "input-1", "reason": reason})
								roleOK(t, err)
								w := f.http(t, token, op, string(raw), "hand-back-text")
								params := fmt.Sprintf(`{"name":%q,"arguments":%s}`, op, raw)
								mcp := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, fmt.Sprintf(`"hand-back-text-%d"`, i), "tools/call", params), !valid)
								if valid {
									if w.Code != 201 || len(p.calls) != before+2 || p.calls[before].reason != reason || p.calls[before+1].reason != reason {
										t.Fatalf("valid hand-back text changed or was rejected: %d, %s", w.Code, mcp)
									}
								} else if w.Code != 400 || !strings.Contains(mcp, `"code":"invalid_input"`) || len(p.calls) != before {
									t.Fatalf("invalid hand-back text reached B: %d, %s", w.Code, mcp)
								}
							}
						})
						t.Run("unicode", func(t *testing.T) {
							p.mutationErr = nil
							before := len(p.calls)
							for _, invalid := range []struct{ op, args string }{
								{"ack_work_input", `{"input_id":"\ud800"}`},
								{"hand_back_work_input", `{"input_id":"input-1","reason":"\udfff"}`},
							} {
								suffix := invalid.op + "-unicode"
								if w := f.http(t, token, invalid.op, invalid.args, suffix); w.Code != 400 || len(p.calls) != before {
									t.Errorf("invalid raw Unicode reached B: %d %s", w.Code, w.Body)
								}
							}
							assertInvalidWorkMCPUnicode(t, f, token, `"ack-unicode"`, "ack_work_input", `{"input_id":"\ud801"}`)
							assertInvalidWorkMCPUnicode(t, f, token, `"\ud800"`, "ack_work_input", `{"input_id":"not-the-replacement"}`)
							assertInvalidWorkMCPUnicode(t, f, token, `"hand-back-unicode"`, "hand_back_work_input", `{"input_id":"input-1","reason":"\udfff"}`)
							for _, id := range []string{`"ack-unicode"`, `"\ufffd"`} {
								w := memberMCPRoleHTTP(t, f, token, id, "tools/call", `{"name":"ack_work_input","arguments":{"input_id":"actual-\ufffd"}}`)
								memberMCPRoleText(t, w, false)
							}
							w := memberMCPRoleHTTP(t, f, token, `"hand-back-unicode"`, "tools/call", `{"name":"hand_back_work_input","arguments":{"input_id":"input-1","reason":"\ud83d\ude00"}}`)
							memberMCPRoleText(t, w, false)
							if got := p.calls[before:]; len(got) != 3 || got[0].inputID != "actual-�" || got[1].inputID != "actual-�" || got[2].reason != "😀" {
								t.Fatal("malformed request bound its id or exact valid Unicode was not forwarded")
							}
						})
						assertOpaqueWorkIDBoundary(t, f, p, token, "ack_work_input", `{"input_id":%s}`, func(call workInputCall) string { return call.inputID })
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

func TestWorkInputsRejectTypedNilAtRuntimeConstruction(t *testing.T) {
	f := newRoleRuntime(t)
	var service *workInputProbe
	f.options.WorkInputs = service
	f.options.ChatIntake = coordination.New(f.db)
	if runtime, err := platform.NewMemberToolRuntime(f.db, f.options); err == nil || runtime != nil {
		t.Fatal("typed-nil WorkInputs must not produce a runtime or issue capabilities")
	}
}

func TestWorkContinuationDiscoveryAndConfigFollowIssuedPurpose(t *testing.T) {
	for _, tc := range []struct {
		name            string
		service, intake bool
	}{
		{"no-service", false, true}, {"generic-with-service", true, false}, {"intake-with-service", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p *workInputProbe
			if tc.service {
				p = &workInputProbe{}
			}
			f := withWorkInputProbe(t, p)
			ctx := context.Background()
			msg, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: "topic", Content: "请续办。", RecipientIDs: []string{f.worker.ID}}, "source")
			roleOK(t, err)
			turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: msg.Deliveries[0].ID, MaxActive: 1})
			roleOK(t, err)
			binding := platform.MemberToolRunBinding{GroupID: turn.GroupID, MemberID: turn.MemberID, ConversationID: turn.ConversationID, DeliveryID: turn.DeliveryID, ProviderID: f.worker.ProviderID, Model: f.worker.Model}
			wrap := f.runtime.Wrap
			if tc.intake {
				wrap = f.runtime.WrapChatIntake
			}
			want := tc.service && tc.intake
			calls := 0
			e, err := wrap(binding, func(path, _ string, _ []string) (provider.Executor, error) {
				wire, err := os.ReadFile(path)
				roleOK(t, err)
				var fields map[string]json.RawMessage
				roleOK(t, json.Unmarshal(wire, &fields))
				var token string
				roleOK(t, json.Unmarshal(fields["token"], &token))
				marker, marked := fields["continue_work"]
				if marked != want || marked && string(marker) != "true" || !marked && len(fields) != 2 || token == "" || token == turn.ClaimToken {
					t.Error("config is not projected from the exact issued capability")
				}
				return roleNative(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					calls++
					list := memberMCPRoleHTTP(t, f, token, `"list"`, "tools/list", `{"_meta":{"continue_work":true,"purpose":"chat_intake/v1"}}`)
					text := list.Body.String()
					if !strings.Contains(text, `"name":"send_message"`) || strings.Contains(text, `"continue_work"`) != want || strings.Contains(text, `"followup"`) != want ||
						want && (!strings.Contains(text, `"task_id"`) || !strings.Contains(text, `"constraint"`)) {
						t.Errorf("discovery ignored configured service or issued purpose: %s", text)
					}
					return roleNotStarted()
				}), nil
			})
			roleOK(t, err)
			if receipt := e.Execute(ctx, provider.ExecuteRequest{RunID: turn.ID, RuntimeDir: f.root, Model: binding.Model}, nil); receipt != roleNotStarted() || calls != 1 {
				t.Fatalf("unexpected runtime outcome: %v", receipt.Outcome)
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
	params := func(args string) string { return fmt.Sprintf(`{"name":"send_message","arguments":%s}`, args) }
	t.Run("text", func(t *testing.T) {
		for _, tc := range []struct {
			name, value string
			limit       int
			emptyOK     bool
		}{
			{"reason", "用户续办", 4096, false},
			{"content", "记录续办。", 16384, false},
			{"constraint", ` 仅校对\n`, 2048, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				invalid := []string{`"\u0085"`, `"a\u0000b"`, "null", "1", fmt.Sprintf("%q", strings.Repeat("🙂", tc.limit/4)+"x")}
				if !tc.emptyOK {
					invalid = append(invalid, `""`)
				}
				field := fmt.Sprintf(`"%s":"%s"`, tc.name, tc.value)
				for i, bad := range append(invalid, "missing") {
					var request string
					if bad == "missing" {
						if tc.emptyOK {
							continue
						}
						request = strings.Replace(body, field+",", "", 1)
					} else {
						request = strings.Replace(body, field, fmt.Sprintf(`"%s":%s`, tc.name, bad), 1)
					}
					before := len(p.calls)
					if w := f.http(t, token, "send_message", request, "invalid-text"); w.Code != 400 || len(p.calls) != before {
						t.Fatalf("invalid continuation %s case %d reached B: %d %s", tc.name, i, w.Code, w.Body)
					}
					id := fmt.Sprintf(`"invalid-%s-%d"`, tc.name, i)
					w := memberMCPRoleHTTP(t, f, token, id, "tools/call", params(request))
					if text := memberMCPRoleText(t, w, true); (!strings.Contains(text, `"code":"invalid_input"`) && !strings.Contains(text, `"code":"invalid_json"`)) || len(p.calls) != before {
						t.Fatalf("MCP did not reject invalid continuation %s case %d: %s", tc.name, i, text)
					}
				}
			})
		}
	})
	t.Run("unicode", func(t *testing.T) {
		for _, invalid := range []string{
			`{"CoNtEnT":"\ud800","INTENT_ASSESSMENT":{"DiSpOsItIoN":"\u0063ontinue_work","reason":"reason"}}`,
			strings.Replace(body, "用户续办", `\ud800`, 1),
			strings.Replace(body, "reference-only", `\udfff`, 1),
			strings.Replace(body, "仅校对", `\ud800`, 1),
		} {
			before := len(p.calls)
			if w := f.http(t, token, "send_message", invalid, "invalid-unicode"); w.Code != 400 || len(p.calls) != before {
				t.Fatalf("invalid Unicode reached B: %d %s", w.Code, w.Body)
			}
			assertInvalidWorkMCPUnicode(t, f, token, `"continue"`, "send_message", invalid)
		}
	})
	if t.Failed() {
		return
	}
	text := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, `"continue"`, "tools/call", params(body)), false)
	if len(p.issued) != 0 || len(p.calls) != 1 || !strings.Contains(text, `"disposition":"continue_work"`) {
		t.Fatal("intake did not route to the separate continuation operation")
	}
	got := p.calls[0]
	if got.operation != "continue_work" || got.member != member || got.candidateID != candidate.ID || got.content != "记录续办。" ||
		got.proposal != (platform.MemberContinueWorkInput{Reason: "用户续办", TaskID: "reference-only", Constraint: " 仅校对\n"}) || !strings.HasPrefix(got.key, "mcp-") {
		t.Fatal("intake changed the authorized candidate, proposal or key")
	}
	t.Run("exact limits", func(t *testing.T) {
		reason, content, constraint := strings.Repeat("🙂", 1024), strings.Repeat("🙂", 4096), strings.Repeat("🙂", 512)
		boundary := fmt.Sprintf(`{"content":%q,"intent_assessment":{"disposition":"continue_work","reason":%q,"followup":{"constraint":%q}}}`, content, reason, constraint)
		before := len(p.calls)
		if w := f.http(t, token, "send_message", boundary, "exact-text"); w.Code != 201 {
			t.Fatalf("exact text limits rejected: %d %s", w.Code, w.Body)
		}
		memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, `"exact-text"`, "tools/call", params(boundary)), false)
		if calls := p.calls[before:]; len(calls) != 2 || calls[0].content != content || calls[1].content != content ||
			calls[0].proposal != (platform.MemberContinueWorkInput{Reason: reason, Constraint: constraint}) || calls[0].proposal != calls[1].proposal {
			t.Fatal("transports did not forward exact byte-limit text to B")
		}
	})
	assertOpaqueWorkIDBoundary(t, f, p, token, "send_message", strings.Replace(body, `"reference-only"`, "%s", 1),
		func(call workInputCall) string { return call.proposal.TaskID })
	t.Run("empty followup replay", func(t *testing.T) {
		const base = `{"content":" 记录续办\n","intent_assessment":{"disposition":"continue_work","reason":" 依原要求\n"%s}}`
		for _, taskID := range []string{"", "reference-only"} {
			var omitEmpty string
			emptyForms := []string{``, `,"followup":{}`, `,"Followup":{"CONSTRAINT":""}`}
			if taskID != "" {
				omitEmpty = `,"followup":{"task_id":"reference-only"}`
				emptyForms = []string{omitEmpty, `,"followup":{"constraint":"","task_id":"reference-only"}`}
			}
			id := fmt.Sprintf("%q", "empty-followup-"+taskID)
			before := len(p.calls)
			for _, form := range emptyForms {
				memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, id, "tools/call", params(fmt.Sprintf(base, form))), false)
			}
			first := p.calls[before]
			if w := f.http(t, token, "send_message", fmt.Sprintf(base, emptyForms[len(emptyForms)-1]), first.key); w.Code != 201 {
				t.Fatalf("native handoff of the MCP key failed: %d %s", w.Code, w.Body)
			}
			if first.proposal != (platform.MemberContinueWorkInput{Reason: " 依原要求\n", TaskID: taskID}) || first.content != " 记录续办\n" || len(p.calls) != before+len(emptyForms)+1 {
				t.Fatal("optional-equivalent requests did not preserve the actual proposal and content")
			}
			for _, got := range p.calls[before:] {
				if got != first {
					t.Fatal("empty followup changed the key, scope or proposal reaching B")
				}
			}
			for _, changed := range []string{
				fmt.Sprintf(base, `,"followup":{"task_id":"other"}`),
				fmt.Sprintf(base, `,"followup":{"constraint":"另一条原文"}`),
				fmt.Sprintf(base, `,"followup":{"constraint":null}`),
				fmt.Sprintf(base, `,"followup":{"constraint":"","group_id":"foreign"}`),
				fmt.Sprintf(strings.Replace(base, " 依原要求", "依原要求", 1), omitEmpty),
				fmt.Sprintf(strings.Replace(base, " 记录续办", "记录续办", 1), omitEmpty),
			} {
				text := memberMCPRoleText(t, memberMCPRoleHTTP(t, f, token, id, "tools/call", params(changed)), true)
				if !strings.Contains(text, `"code":"conflict"`) || len(p.calls) != before+len(emptyForms)+1 {
					t.Fatal("different or invalid arguments reused the bound MCP identity")
				}
			}
		}
	})
}
