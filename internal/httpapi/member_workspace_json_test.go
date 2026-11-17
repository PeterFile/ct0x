//go:build unix

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type memberWorkspaceJSONHTTP struct {
	*memberToolHTTPFixture
	role                        *readinessHTTPFixture
	binding                     platform.MemberToolRunBinding
	provenance                  store.CoordinationRoleProvenance
	directory, private, scratch string
}

// Reuse the real HTTP/SQLite fixture, but admit a separate authorized file task.
// Only the executor is synthetic; no provider or credential probe is launched.
func newMemberWorkspaceJSONHTTP(t *testing.T) *memberWorkspaceJSONHTTP {
	t.Helper()
	ctx := context.Background()
	role := newReadinessHTTPFixture(t)
	f := &memberWorkspaceJSONHTTP{role: role, directory: t.TempDir(), private: t.TempDir(), scratch: t.TempDir()}
	for _, path := range []string{f.directory, f.private, f.scratch} {
		readinessHTTPOK(t, os.Chmod(path, 0700))
	}
	owner, err := os.OpenRoot(f.private)
	readinessHTTPOK(t, err)
	blobs, err := artifactfs.New(owner, 64<<20)
	readinessHTTPOK(t, err)
	readinessHTTPOK(t, owner.Close())
	t.Cleanup(func() { readinessHTTPOK(t, blobs.Close()) })
	readinessHTTPOK(t, role.runtime.EnableWorkspace(platform.MemberWorkspaceOptions{Blobs: blobs, ScratchRoot: f.scratch, ProtectedRoots: []string{f.private, role.root}}))
	t.Cleanup(func() { readinessHTTPOK(t, role.runtime.Close()) })
	keys, err := scheduler.WorkspaceResourceKeys([]string{f.directory})
	readinessHTTPOK(t, err)
	grant, err := role.a.db.AuthorizeWorkspaceGrant(ctx, store.AuthorizeWorkspaceGrantInput{GroupID: role.group.ID, MemberID: role.worker.ID, Root: f.directory, Scope: "read_write", IdempotencyKey: "json-directory"}, func([]store.WorkspaceGrantReceipt) (store.WorkspaceGrantDirectory, error) {
		return store.WorkspaceGrantDirectory{ResolvedRoot: f.directory, ResourceKey: keys[0]}, nil
	})
	readinessHTTPOK(t, err)
	_, err = role.a.db.SetChatPolicy(ctx, domain.ThreadRef{GroupID: role.group.ID, TopicID: "json-files"}, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, AllowWorkspaceChanges: true, MaxAdmissions: 1}, 0, "json-policy")
	readinessHTTPOK(t, err)
	source, err := platform.NewLocalUser(role.a.db).CreateMessage(ctx, role.group.ID, platform.MessageInput{TopicID: "json-files", Content: "在授权目录提交真实文件", RecipientIDs: []string{role.lead.ID}}, "json-source")
	readinessHTTPOK(t, err)
	intake, err := role.a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: role.group.ID, DeliveryID: source.Deliveries[0].ID, MaxActive: 1})
	readinessHTTPOK(t, err)
	member, err := role.a.db.BindMemberTool(ctx, store.TurnClaim{GroupID: intake.GroupID, TurnID: intake.ID, Token: intake.ClaimToken})
	readinessHTTPOK(t, err)
	candidate, err := role.a.db.CaptureChatCandidate(ctx, member)
	readinessHTTPOK(t, err)
	admitted, err := role.a.db.ApplyChatAssessment(ctx, member, candidate.ID, domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionWorkspaceChange, Title: "JSON文件", Description: "仅授权目录", Acceptance: "原始字节不被替换", Reason: "明确文件请求"}, "登记文件任务。", "json-admission")
	readinessHTTPOK(t, err)
	_, err = role.a.db.ApplyTurnReceipt(ctx, member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	readinessHTTPOK(t, err)
	materialized, err := role.a.db.MaterializeDispatchRequest(ctx, admitted.DispatchRequestID, 1, time.Now().UTC())
	readinessHTTPOK(t, err)
	state, err := role.a.db.ResolveCoordinationDelivery(ctx, role.group.ID, materialized.Delivery.DeliveryID)
	readinessHTTPOK(t, err)
	lead, err := role.a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: role.group.ID, DeliveryID: materialized.Delivery.DeliveryID, MaxActive: 1})
	readinessHTTPOK(t, err)
	leadMember, err := role.a.db.BindMemberTool(ctx, store.TurnClaim{GroupID: lead.GroupID, TurnID: lead.ID, Token: lead.ClaimToken})
	readinessHTTPOK(t, err)
	fact := store.DispatchWorkerCapabilityFact{GroupID: role.group.ID, MemberID: role.worker.ID, ProviderID: state.ProviderID, Model: state.Model, RequiredProfile: store.DispatchWorkspaceWorkerProfile, TaskID: admitted.TaskID, TaskRevision: admitted.WorkspaceScope.TaskRevision, ResourceKey: keys[0], NodeID: "local", GrantID: grant.ID}
	decision, err := role.a.db.DecideDispatchWithCapabilities(ctx, store.CoordinationToolScope{Member: leadMember, Provenance: state.Provenance}, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: role.worker.ID, Reason: "文件工具能力已确认"}, "json-assign", []store.DispatchWorkerCapabilityFact{fact})
	readinessHTTPOK(t, err)
	_, err = role.a.db.ApplyTurnReceipt(ctx, leadMember.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	readinessHTTPOK(t, err)
	worker, err := role.a.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	readinessHTTPOK(t, err)
	state, err = role.a.db.ResolveCoordinationDelivery(ctx, role.group.ID, worker.Delivery.DeliveryID)
	readinessHTTPOK(t, err)
	turn, err := role.a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: role.group.ID, DeliveryID: worker.Delivery.DeliveryID, MaxActive: 1, ResourceKeys: keys})
	readinessHTTPOK(t, err)
	f.memberToolHTTPFixture = &memberToolHTTPFixture{db: role.a.db, raw: role.raw, path: role.a.path, groupID: role.group.ID, claim: store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}, tools: role.runtime.Tools()}
	f.binding = platform.MemberToolRunBinding{GroupID: turn.GroupID, MemberID: turn.MemberID, ConversationID: turn.ConversationID, DeliveryID: turn.DeliveryID, ProviderID: state.ProviderID, ProviderName: provider.Pi, Model: state.Model}
	f.provenance = state.Provenance
	f.handler = mustServer(t, DefaultAddress, append(MemberRoleToolRoutes(f.tools), MemberMCPRoutes(f.tools)...)...).Handler
	return f
}

func (f *memberWorkspaceJSONHTTP) execute(t *testing.T, use func()) {
	t.Helper()
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, ExitCode: 0, Reason: "synthetic executor, not provider completion evidence"}
	executor, err := f.role.runtime.WrapCoordinationRoleWithCapabilities(f.binding, f.provenance, nil, func(path, id string, names []string) (provider.Executor, error) {
		data, err := os.ReadFile(path)
		readinessHTTPOK(t, err)
		var config struct{ Token string }
		readinessHTTPOK(t, json.Unmarshal(data, &config))
		if id != f.claim.TurnID || config.Token == "" || config.Token == f.claim.Token || !slices.Equal(names, provider.WorkspaceWorkerToolNames()) {
			t.Fatal("runtime did not issue the exact bound file-worker capability")
		}
		f.token = config.Token
		return memberMCPMetadataExecutor(func() provider.ExecutionReceipt { use(); return raw }), nil
	})
	readinessHTTPOK(t, err)
	got := executor.Execute(context.Background(), provider.ExecuteRequest{RunID: f.claim.TurnID, RuntimeDir: f.role.runRoot, CWD: f.directory, Model: f.binding.Model}, nil)
	if got != raw {
		t.Fatalf("file tool rejection changed the executor's unknown outcome: %+v", got)
	}
	_, err = f.db.ApplyTurnReceipt(context.Background(), f.claim, got)
	readinessHTTPOK(t, err)
	turn, err := f.db.GetTurn(context.Background(), f.groupID, f.claim.TurnID)
	readinessHTTPOK(t, err)
	var claims int
	readinessHTTPOK(t, f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", f.claim.TurnID).Scan(&claims))
	if turn.State != domain.TurnStateUnknown || turn.ClaimToken != f.claim.Token || claims != 1 {
		t.Fatal("rejection released or replaced the original execution claim/resource")
	}
}

func (f *memberWorkspaceJSONHTTP) files(t *testing.T) map[string]string {
	t.Helper()
	result := make(map[string]string)
	for _, root := range []string{f.directory, f.private, f.scratch} {
		readinessHTTPOK(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			value := fmt.Sprintf("%s|%d|%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
			if !entry.IsDir() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				value += "|" + string(data)
			}
			result[path] = value
			return nil
		}))
	}
	return result
}

func TestMemberWorkspaceJSONTransport(t *testing.T) {
	f := newMemberWorkspaceJSONHTTP(t)
	f.execute(t, func() {
		ctx := context.Background()
		original, err := f.tools.WorkspaceWrite(ctx, f.token, platform.MemberWorkspaceWriteInput{Path: "original.txt", Create: true, Text: "original bytes"}, "original-write")
		readinessHTTPOK(t, err)
		_, err = f.tools.PublishWorkspaceArtifact(ctx, f.token, platform.MemberWorkspaceArtifactInput{OperationID: original.OperationID, Name: "原成果"}, "original-publish")
		readinessHTTPOK(t, err)
		for _, transport := range []string{"native", "mcp"} {
			t.Run(transport, func(t *testing.T) {
				call := func(t *testing.T, operation, arguments, key string) *httptest.ResponseRecorder {
					t.Helper()
					if transport == "native" {
						return f.request(t, operation, arguments, key)
					}
					// Opaque metadata must not inherit file-argument string restrictions.
					body := memberMCPWithMeta(memberMCPCall(fmt.Sprintf("%q", key), operation, arguments), `{"opaque":"\ud800","nested":["\udc00"]}`)
					return memberMCPHTTP(t, f.memberToolHTTPFixture, body)
				}
				t.Run("reject", func(t *testing.T) {
					before, files := coordinationHTTPSnapshot(t, f.raw), f.files(t)
					var version int
					readinessHTTPOK(t, f.raw.QueryRow("PRAGMA data_version").Scan(&version))
					for _, unit := range []string{`\ud800`, `\udc00`} {
						label := "high"
						if unit == `\udc00` {
							label = "low"
						}
						for _, test := range []struct{ name, operation, body string }{
							{label + "-create", "workspace_write", fmt.Sprintf(`{"path":"%s-%s.txt","create":true,"text":"%s"}`, transport, label, unit)},
							{label + "-replace", "workspace_write", fmt.Sprintf(`{"path":"original.txt","create":false,"expected_sha256":%q,"text":"%s"}`, original.AfterSHA256, unit)},
							{label + "-write-path", "workspace_write", fmt.Sprintf(`{"path":"%s.txt","create":true,"text":"safe"}`, unit)},
							{label + "-read-path", "workspace_read", fmt.Sprintf(`{"path":"%s.txt"}`, unit)},
							{label + "-list-path", "workspace_list", fmt.Sprintf(`{"path":"%s"}`, unit)},
							{label + "-publish-name", "publish_workspace_artifact", fmt.Sprintf(`{"operation_id":%q,"name":"%s"}`, original.OperationID, unit)},
						} {
							t.Run(test.name, func(t *testing.T) {
								w := call(t, test.operation, test.body, transport+"-"+test.name)
								if transport == "native" {
									assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
								} else if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":-32602`) {
									t.Errorf("invalid raw file arguments were accepted: %d %s", w.Code, w.Body)
								}
								coordinationHTTPUnchanged(t, f.raw, before)
								if !reflect.DeepEqual(files, f.files(t)) {
									t.Error("rejection changed original file, staging, or immutable blobs")
								}
								var after int
								readinessHTTPOK(t, f.raw.QueryRow("PRAGMA data_version").Scan(&after))
								if after != version {
									t.Error("rejection committed a write before returning")
								}
								t.Logf("status=%d; full database rows, original file, staging and blobs unchanged", w.Code)
							})
						}
					}
				})
				t.Run("accept", func(t *testing.T) {
					for _, test := range []struct{ name, raw, text string }{
						{"pair", `"\ud83d\ude00"`, "😀"},
						{"pair-limits", `"\uD800\uDC00\uDBFF\uDFFF"`, "\U00010000\U0010ffff"},
						{"utf8", `"中文😀"`, "中文😀"},
						{"replacement", `"�"`, "�"},
						{"replacement-escape", `"\ufffd"`, "�"},
						{"literal-backslash", `"\\ud800|\\udc00|\\uD83D\\uDE00"`, `\ud800|\udc00|\uD83D\uDE00`},
						{"escaped-quote", `"\"\\ud800"`, `"\ud800`},
						{"escaped-backslash-pair", `"\\\ud83d\ude00"`, "\\😀"},
					} {
						t.Run(test.name, func(t *testing.T) {
							name, key := transport+"-"+test.name+".txt", transport+"-"+test.name
							path := fmt.Sprintf("%q", name)
							if test.name == "pair" {
								name = transport + "-pair-😀.txt"
								path = `"` + transport + `-pair-\ud83d\ude00.txt"`
							}
							if test.name == "replacement" {
								// Rejected input must consume neither Begin nor an MCP ID binding.
								key = transport + "-high-create"
							}
							w := call(t, "workspace_write", fmt.Sprintf(`{"path":%s,"create":true,"text":%s}`, path, test.raw), key)
							text := w.Body.String()
							if transport == "mcp" {
								text = memberMCPText(t, w, false)
							} else if w.Code != 201 {
								t.Fatalf("valid raw string rejected: %d %s", w.Code, w.Body)
							}
							var result platform.MemberWorkspaceWriteResult
							readinessHTTPOK(t, json.Unmarshal([]byte(text), &result))
							data, err := os.ReadFile(filepath.Join(f.directory, name))
							readinessHTTPOK(t, err)
							if result.Outcome != "done" || string(data) != test.text {
								t.Fatalf("valid raw string changed: outcome=%s file=%x want=%x", result.Outcome, data, test.text)
							}
							t.Logf("status=%d outcome=done file_hex=%x", w.Code, data)
						})
					}
					for _, test := range []struct{ operation, body string }{
						{"workspace_read", `{"path":"` + transport + `-pair-\ud83d\ude00.txt"}`},
						{"workspace_list", `{"path":"."}`},
						{"publish_workspace_artifact", fmt.Sprintf(`{"operation_id":%q,"name":"合法 \ud83d\ude00 \ufffd \\ud800"}`, original.OperationID)},
					} {
						w := call(t, test.operation, test.body, transport+"-valid-"+test.operation)
						text := w.Body.String()
						if transport == "mcp" {
							text = memberMCPText(t, w, false)
						} else if w.Code != 200 && w.Code != 201 {
							t.Fatalf("valid file arguments rejected: %s %d %s", test.operation, w.Code, w.Body)
						}
						if test.operation == "publish_workspace_artifact" {
							var result platform.MemberWorkspaceArtifactResult
							readinessHTTPOK(t, json.Unmarshal([]byte(text), &result))
							artifact, err := f.db.GetArtifact(ctx, f.groupID, result.ArtifactID)
							readinessHTTPOK(t, err)
							if artifact.Name != `合法 😀 � \ud800` {
								t.Fatal("valid publication name was changed")
							}
						}
					}
				})
			})
		}
	})
}

func TestMemberWorkspaceJSONBoundariesAndLegacy(t *testing.T) {
	f := newMemberMCPHTTP(t)
	for _, text := range []string{`\ud800`, `\udc00`, `\ud800x`, `\ud800\ud800`, `\udc00\ud800`, `\ud800\\udc00`, `\ud800\n\udc00`} {
		w := f.request(t, "workspace_write", `{"path":"x","create":true,"text":"`+text+`"}`, "boundary")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
	}
	for _, body := range []string{
		`{"path":"x","create":true,"text":"\ud83d"` + `}`,
		`{"path":"x","create":true,"text":"\u12xz"}`,
		`{"path":"x","create":true,"text":"\u123"}`,
		`{"path":"x","create":true,"text":"\x00"}`,
		`{"path":"x","create":true,"text":"ok","Text":"duplicate"}`,
		`{"path":"x","create":true,"text":"ok","caller":"forged"}`,
		`{"path":"x","create":true,"text":null}`,
		`{"path":"x","create":true,"text":"ok"} {}`,
		"{\"path\":\"x\",\"create\":true,\"text\":\"\xff\"}",
	} {
		w := f.request(t, "workspace_write", body, "boundary")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
	}
	body := `{"path":"x","create":true,"text":"\\ud800"}`
	body += strings.Repeat(" ", memberToolJSONBytes-len(body))
	w := f.request(t, "workspace_write", body, "boundary")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
	w = f.request(t, "workspace_write", body+" ", "boundary")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "body_too_large")
	for i, transport := range []string{"native", "mcp"} {
		body := `{"content":"\ud800"}`
		var w *httptest.ResponseRecorder
		var text string
		if transport == "mcp" {
			w = memberMCPHTTP(t, f, memberMCPCall(`"legacy"`, "send_message", body))
			text = memberMCPText(t, w, false)
		} else {
			w = f.request(t, "send_message", body, fmt.Sprint("legacy-", i))
			if w.Code != 201 {
				t.Fatalf("legacy non-file contract changed: %d %s", w.Code, w.Body)
			}
			text = w.Body.String()
		}
		var result memberToolHTTPSendResult
		readinessHTTPOK(t, json.Unmarshal([]byte(text), &result))
		message, err := f.db.GetMessage(context.Background(), f.groupID, result.MessageID)
		readinessHTTPOK(t, err)
		if message.Content != "�" {
			t.Fatal("file-only validation changed the legacy non-file decoder")
		}
	}
}
