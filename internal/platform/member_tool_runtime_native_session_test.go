package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

func useReleasePiBridge(t *testing.T, f *rolePrepareFixture) {
	t.Helper()
	path, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	prepareRoleOK(t, err)
	if !provider.PiNativeSessionBridgeSupported(path) {
		t.Fatal("native Pi test requires the audited bridge release pair")
	}
	f.opts.PiMemberToolsExtension = path
}

// These are offline trusted-wiring fixtures, not native CLI or UI acceptance.
func nativeSessionPreparation(t *testing.T, purpose string) (*rolePrepareFixture, scheduler.PreparedExecution) {
	t.Helper()
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	useReleasePiBridge(t, f)
	f.opts.EnableNativeSessionResume, f.opts.NativePiThinking, f.opts.RoleThinking = true, "off", "max"
	const model = "fixture/model"
	f.exec(t, "UPDATE members SET model=? WHERE id IN (?, ?)", model, f.coordinator.ID, f.worker.ID)
	f.coordinator.Model, f.worker.Model, f.member.Model = model, model, model
	if purpose == "worker" {
		f.selectWorker(t)
	} else if purpose == "generic" || purpose == domain.ChatIntakePurpose {
		f.pending = f.ordinary
	}
	if purpose == domain.ChatIntakePurpose {
		f.runtimeOptions.ChatIntake = coordination.New(f.s)
		var err error
		f.opts.MemberTools, err = NewMemberToolRuntime(f.s, f.runtimeOptions)
		prepareRoleOK(t, err)
	}
	prepared, err := f.preparer(t).Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	if prepared.Executor.(*memberToolRuntimeRun).nativeSession == nil {
		t.Fatal("explicit known-version text preparation did not freeze a native-session plan")
	}
	return f, prepared
}

type nativeSessionFixtureCall struct {
	binding store.NativeRunBinding
	resume  *provider.NativeSessionReference
	issued  provider.NativeMemberToolConfig
	token   string
	raw     provider.ExecutionReceipt
}

func runNativeSessionFixture(t *testing.T, f *rolePrepareFixture, prepared scheduler.PreparedExecution, inspect func(*nativeSessionFixtureCall), outcome provider.Outcome, capture bool) (store.TurnClaim, provider.ExecutionReceipt, *nativeSessionFixtureCall) {
	t.Helper()
	ctx := context.Background()
	run := prepared.Executor.(*memberToolRuntimeRun)
	factory := run.nativeSession.factory
	call := &nativeSessionFixtureCall{}
	run.nativeSession.factory = func(config provider.NativeMemberToolConfig, names []string, resume *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
		// Construct the actual adapter through its per-turn registry without
		// executing it; only Execute is replaced by an explicit synthetic fixture.
		e, err := factory(config, names, resume, binding)
		if err != nil || e == nil {
			t.Fatalf("actual native-session constructor failed: %v", err)
		}
		call.issued = config
		_, call.token = memberToolRuntimeConfig(t, config.Path)
		prepareRoleOK(t, json.Unmarshal(binding, &call.binding))
		call.resume = cloneMemberToolNativeReference(resume)
		sortedNames := slices.Clone(names)
		slices.Sort(sortedNames)
		if call.binding.ResumedFromTurnID != "" || !slices.Equal(sortedNames, call.binding.Profile.Tools) {
			t.Fatal("prestart metadata claimed actual resume or lost issued tool order")
		}
		if inspect != nil {
			inspect(call)
		}
		return memberToolRuntimeExecutorFunc(func(ctx context.Context, req provider.ExecuteRequest, event func(provider.Event) error) provider.ExecutionReceipt {
			issued, err := f.opts.MemberTools.Tools().TransportOperations(ctx, call.token)
			prepareRoleOK(t, err)
			if !slices.Equal(issued, names) {
				t.Fatal("current token differs from the binding's actually issued tools")
			}
			idDigest := sha256.Sum256([]byte(req.RunID))
			id := fmt.Sprintf("019f0000-0000-7000-8000-%x", idDigest[:6])
			if resume != nil {
				id = memberToolNativeID(resume)
			}
			for _, kind := range []provider.EventKind{provider.SessionEvent, provider.AcceptedEvent} {
				prepareRoleOK(t, event(provider.Event{Kind: kind, NativeSessionID: id}))
			}
			if outcome == provider.OutcomeSucceeded {
				prepareRoleOK(t, event(provider.Event{Kind: provider.ResponseEvent, NativeSessionID: id, Text: "离线接线证据"}))
				prepareRoleOK(t, event(provider.Event{Kind: provider.TerminalEvent, NativeSessionID: id, Terminal: outcome}))
			}
			call.raw = provider.ExecutionReceipt{Started: true, Accepted: true, NativeSessionID: id, Outcome: outcome,
				Terminal: provider.OutcomeSucceeded, OutputComplete: outcome == provider.OutcomeSucceeded, DirectChildReaped: true}
			if capture {
				sha, err := store.NativeToolsProfileSHA256(names)
				prepareRoleOK(t, err)
				// Synthetic file metadata exercises Store's same-file append contract.
				// This fixture does not execute Pi or prove a real file append.
				p := call.binding.Profile
				ref := &provider.PiNativeSessionReference{
					Adapter: p.Adapter, Protocol: p.Protocol, CLIPath: p.CLIPath, CLIVersion: p.CLIVersion, Model: p.Model, Thinking: p.Thinking,
					CWD: p.CWD, ToolsProfileSHA256: sha, NativeSessionID: id,
					SessionFile: filepath.Join(req.RuntimeDir, req.RunID, "native", "session.jsonl"), Device: 1, Inode: 1, Size: 1, MTimeNS: 1,
				}
				if resume != nil {
					ref.SessionFile, ref.Device, ref.Inode = resume.Pi.SessionFile, resume.Pi.Device, resume.Pi.Inode
					ref.Size, ref.MTimeNS = resume.Pi.Size+1, resume.Pi.MTimeNS+1
				}
				ref.FileSHA256 = fmt.Sprintf("%064x", ref.Size)
				ref.LeafID, ref.LastCompletedAssistantID = fmt.Sprintf("leaf-%d", ref.Size), fmt.Sprintf("assistant-%d", ref.Size)
				call.raw.NativeRef = &provider.NativeSessionReference{Version: provider.NativeSessionReferenceVersion, Binding: bytes.Clone(binding), Pi: ref}
			}
			return call.raw
		}), nil
	}
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: run.binding.DeliveryID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
	prepareRoleOK(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	req := prepared.Request
	req.RunID, req.RuntimeDir = turn.ID, f.runtimeOptions.RoleExecutionRoot
	receipt := run.Execute(ctx, req, func(event provider.Event) error { return f.s.RecordTurnEvent(ctx, claim, event) })
	if call.token == "" {
		t.Fatalf("native fixture never received its fresh current config: %s %s", receipt.Outcome, receipt.Reason)
	}
	if _, err := f.opts.MemberTools.Tools().TransportOperations(ctx, call.token); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("finished native invocation retained a usable token")
	}
	memberToolRuntimeEmpty(t, f.runtimeOptions.ConfigRoot)
	_, err = f.s.ApplyTurnReceipt(ctx, claim, receipt)
	prepareRoleOK(t, err)
	return claim, receipt, call
}

func nextNativeSessionPreparation(t *testing.T, f *rolePrepareFixture) scheduler.PreparedExecution {
	t.Helper()
	original, err := f.s.GetMessage(context.Background(), f.group.ID, f.ordinary.Delivery.MessageID)
	prepareRoleOK(t, err)
	message := f.message(t, f.group.ID, original.TopicID, "user", "下一次真实排队投递", f.coordinator.ID)
	f.pending = store.PendingDelivery{GroupID: f.group.ID, Delivery: message.Deliveries[0]}
	prepared, err := f.preparer(t).Prepare(context.Background(), f.pending)
	prepareRoleOK(t, err)
	return prepared
}

func decodeNativeFixtureBinding(t *testing.T, receipt provider.ExecutionReceipt) store.NativeRunBinding {
	t.Helper()
	if receipt.NativeRef == nil {
		t.Fatal("missing current binding-only/reference sidechannel")
	}
	var binding store.NativeRunBinding
	prepareRoleOK(t, json.Unmarshal(receipt.NativeRef.Binding, &binding))
	return binding
}

func TestNativeSessionRuntimeIssuedPurposeAndAuthority(t *testing.T) {
	for _, purpose := range []string{"generic", domain.ChatIntakePurpose, "coordinator", "worker"} {
		t.Run(purpose, func(t *testing.T) {
			f, prepared := nativeSessionPreparation(t, purpose)
			_, receipt, call := runNativeSessionFixture(t, f, prepared, func(call *nativeSessionFixtureCall) {
				b := call.binding
				maxMembers := 0
				if purpose == "generic" {
					maxMembers = f.runtimeOptions.MaxGroupMembers
					if maxMembers == 0 {
						maxMembers = 4
					}
				}
				if b.Purpose != purpose || b.Profile.MaxGroupMembers != maxMembers || call.resume != nil || b.Profile.WorkspaceIdentity != prepared.ResourceKeys[0] {
					t.Fatalf("purpose/issued grant/physical identity mismatch: %+v", b)
				}
				genericTools := slices.Clone(historyProfileNames)
				slices.Sort(genericTools)
				if purpose == "generic" && !slices.Equal(b.Profile.Tools, genericTools) {
					t.Fatal("generic six-tool issuance was replaced by the legacy four-tool constant")
				}
				a := b.Profile.Authority
				switch purpose {
				case "generic":
					if a != (store.NativeAuthorityScope{}) || b.CandidateID != "" {
						t.Fatal("generic inherited intake/role authority")
					}
				case domain.ChatIntakePurpose:
					if b.CandidateID == "" || a.RequestID != "" || a.TaskID != "" || a.TaskRevision != 0 || a.AssignmentID != "" || a.MandateID != "" || a.AuthorityGeneration != 0 {
						t.Fatal("intake inherited task authority")
					}
				default:
					d := f.request
					if a.RequestID != d.ID || a.TaskID != d.TaskID || a.TaskRevision != d.TaskRevision || a.MandateID != d.MandateID || a.AuthorityGeneration != d.AuthorityGeneration || a.BindingRevision != d.BindingRevision || a.PolicyRevision != d.PolicyRevision || a.ChatPolicyRevision != 0 || (purpose == "worker") != (a.AssignmentID != "") {
						t.Fatal("role authority was not the immutable dispatch/assignment snapshot")
					}
				}
			}, provider.OutcomeSucceeded, true)
			if receipt.Outcome != provider.OutcomeSucceeded || decodeNativeFixtureBinding(t, receipt).ResumedFromTurnID != "" {
				t.Fatal("fresh capture changed native outcome or claimed historical resume")
			}
			if receipt.NativeRef.Pi == nil || receipt.NativeRef.Codex != nil || call.raw.NativeRef == receipt.NativeRef || call.raw.NativeRef.Pi == receipt.NativeRef.Pi {
				t.Fatal("aggregate aliases raw native reference")
			}
		})
	}
}

func TestNativeSessionRuntimeLegacyReceiptWire(t *testing.T) {
	// Frozen v1 diagnostic wire fields from the legacy codec, including real
	// nonzero execution evidence and allowed log file paths, not a zero receipt.
	const legacyRaw = `{"started":true,"accepted":true,"outcome":"succeeded","terminal":"succeeded","delivery_failed":false,"native_session_id":"fixture-id","direct_child_reaped":true,"output_complete":true,"exit_code":0,"stdout_path":"/fixture/stdout.jsonl","stderr_path":"/fixture/stderr.log","logs_truncated":true,"reason":"执行完成"}`
	const source = `{"Kind":"coordinator","GroupID":"g","TurnID":"t","DeliveryID":"d","MessageID":"m","ConversationID":"c","MemberID":"member","TopicID":"topic","RequestID":"request","TaskID":"task","AssignmentID":"","AttemptID":"","ProviderID":"pi","Model":"fixture/model","ClaimFingerprint":"abababababababababababababababababababababababababababababababab","TaskRevision":1}`
	const legacyCapture = `{"Version":1,"Source":` + source + `,"Raw":` + legacyRaw + `,"Aggregate":` + legacyRaw + `}`
	old, ok := memberToolRoleDecodeReceipt(strings.NewReader(legacyCapture))
	if !ok || old.Raw.NativeRef != nil || old.Raw.StdoutPath == "" || !old.Raw.Started {
		t.Fatal("legacy nonzero capture is not a readable fixed point")
	}
	if encoded, ok := memberToolRoleEncodeReceipt(*old); !ok || string(encoded) != legacyCapture {
		t.Fatal("legacy capture encoding changed")
	}

	f, prepared := nativeSessionPreparation(t, "coordinator")
	claim, receipt, call := runNativeSessionFixture(t, f, prepared, nil, provider.OutcomeSucceeded, true)
	if receipt.NativeRef == nil || receipt.NativeRef.Pi == nil {
		t.Fatal("fixture did not create a nonzero native sidecar")
	}
	old.Raw.NativeRef, old.Aggregate.NativeRef = call.raw.NativeRef, receipt.NativeRef
	if _, ok := memberToolRoleEncodeReceipt(*old); ok {
		t.Fatal("diagnostic codec claimed full capture while omitting the private sidecar")
	}

	var stored, binding, reference string
	prepareRoleOK(t, f.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", claim.TurnID).Scan(&stored))
	prepareRoleOK(t, f.db.QueryRow("SELECT binding_json,native_ref_json FROM native_run_bindings WHERE turn_id=?", claim.TurnID).Scan(&binding, &reference))
	want := `{"started":true,"accepted":true,"outcome":"succeeded","terminal":"succeeded","delivery_failed":false,"native_session_id":"` + receipt.NativeSessionID + `","direct_child_reaped":true,"output_complete":true,"exit_code":0,"stdout_path":"","stderr_path":"","logs_truncated":false,"reason":"执行完成"}`
	if stored != want {
		t.Fatal("Store changed the nonzero legacy normalized receipt JSON")
	}
	diagnostic, err := f.opts.MemberTools.InspectCoordinationRoleReceipt(context.Background(), claim.GroupID, claim.TurnID)
	prepareRoleOK(t, err)
	if diagnostic.Capture != nil || diagnostic.CaptureStatus != "missing" || diagnostic.Current.Turn.State != domain.TurnStateSucceeded {
		t.Fatal("diagnostic refusal changed canonical settlement or claimed a complete capture")
	}
	for _, private := range []string{call.token, claim.Token, call.issued.Path, hex.EncodeToString(call.issued.SHA256[:])} {
		if private == "" || strings.Contains(stored+binding+reference, private) {
			t.Fatal("runtime config material leaked into persisted evidence")
		}
	}
}

func TestNativeSessionRuntimeResumeRestartRebindAndUnknownBarrier(t *testing.T) {
	f, prepared := nativeSessionPreparation(t, "generic")
	firstClaim, first, firstCall := runNativeSessionFixture(t, f, prepared, nil, provider.OutcomeSucceeded, true)
	firstBytes, err := json.Marshal(first.NativeRef)
	prepareRoleOK(t, err)
	var canonicalBefore string
	prepareRoleOK(t, f.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", firstClaim.TurnID).Scan(&canonicalBefore))
	// Reopen only this fixture Store and replace its process-local registry.
	var dbPath string
	prepareRoleOK(t, f.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&dbPath))
	prepareRoleOK(t, f.opts.MemberTools.Close())
	prepareRoleOK(t, f.s.Close())
	f.s, err = store.Open(dbPath)
	prepareRoleOK(t, err)
	t.Cleanup(func() { f.s.Close() })
	f.opts.MemberTools, err = NewMemberToolRuntime(f.s, f.runtimeOptions)
	prepareRoleOK(t, err)
	secondClaim, second, secondCall := runNativeSessionFixture(t, f, nextNativeSessionPreparation(t, f), func(call *nativeSessionFixtureCall) {
		if call.resume == nil || call.resume.Pi == nil || call.resume.Pi.NativeSessionID != first.NativeSessionID || call.resume.Pi.SessionFile != first.NativeRef.Pi.SessionFile || call.token == firstCall.token || call.token == firstClaim.Token {
			t.Fatal("restart did not select exact native source with a new token")
		}
		if _, err := f.opts.MemberTools.Tools().TransportOperations(context.Background(), firstCall.token); !errors.Is(err, store.ErrMemberToolDenied) {
			t.Fatal("old process token regained authority after native resume")
		}
	}, provider.OutcomeSucceeded, true)
	if decodeNativeFixtureBinding(t, second).ResumedFromTurnID != firstClaim.TurnID {
		t.Fatal("accepted matching native ID did not preserve source association")
	}
	var resumedFrom string
	var hasRef bool
	prepareRoleOK(t, f.db.QueryRow("SELECT resumed_from_turn_id,native_ref_json IS NOT NULL FROM native_run_bindings WHERE turn_id=?", secondClaim.TurnID).Scan(&resumedFrom, &hasRef))
	if resumedFrom != firstClaim.TurnID || !hasRef {
		t.Fatal("the first Apply did not persist the accepted resume and qualified capture")
	}
	if firstClaim.Token == secondClaim.Token || firstCall.token == secondCall.token {
		t.Fatal("native reference was used to recover old authority")
	}
	second.NativeRef.Binding[0] = '['
	if secondCall.raw.NativeRef.Binding[0] != '{' {
		t.Fatal("aggregate binding mutation changed provider raw receipt")
	}
	var canonicalAfter string
	prepareRoleOK(t, f.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", firstClaim.TurnID).Scan(&canonicalAfter))
	stillFirst, _ := json.Marshal(first.NativeRef)
	if canonicalBefore != canonicalAfter || !bytes.Equal(firstBytes, stillFirst) || strings.Contains(canonicalAfter, "native_ref") {
		t.Fatal("resume changed old canonical receipt JSON or historical reference")
	}
	unknownClaim, unknown, unknownCall := runNativeSessionFixture(t, f, nextNativeSessionPreparation(t, f), nil, provider.OutcomeUnknown, false)
	var retained int
	prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", unknownClaim.TurnID).Scan(&retained))
	if retained != 1 || unknown.NativeRef.Pi != nil || unknown.NativeRef.Codex != nil {
		t.Fatal("unknown capture released a resource or fabricated a reference")
	}
	waiting := nextNativeSessionPreparation(t, f)
	ctx := context.Background()
	// Even with spare process capacity and no competing resource key, the same
	// conversation cannot be claimed again while this external result is unknown.
	claimInput := store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: f.pending.Delivery.ID, MaxActive: 8}
	if _, err := f.s.ClaimTurn(ctx, claimInput); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("same-session replacement passed an unknown Turn: %v", err)
	}
	cancelled, err := f.s.RequestTurnCancel(ctx, f.group.ID, unknownClaim.TurnID)
	prepareRoleOK(t, err)
	prepareRoleOK(t, f.db.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", unknownClaim.TurnID).Scan(&retained))
	if cancelled.CancelRequestedAt == nil || cancelled.State != domain.TurnStateUnknown || retained != 1 {
		t.Fatal("cancellation altered uncertainty or released the physical resource")
	}
	if _, err := f.s.ClaimTurn(ctx, claimInput); !errors.Is(err, store.ErrTurnBusy) {
		t.Fatalf("cancel request allowed a new same-session claim: %v", err)
	}
	// Explicit synthetic reconciliation cannot upgrade the first unknown/noRef
	// barrier, even when later trusted terminal evidence releases this fixture.
	reconciled := unknownCall.raw
	reconciled.Outcome, reconciled.OutputComplete = provider.OutcomeSucceeded, true
	_, err = f.s.ApplyTurnReceipt(context.Background(), unknownClaim, reconciled)
	prepareRoleOK(t, err)
	runNativeSessionFixture(t, f, waiting, func(call *nativeSessionFixtureCall) {
		if call.resume != nil {
			t.Fatal("latest unknown/noRef barrier was skipped in favor of older success")
		}
	}, provider.OutcomeSucceeded, true)
}

func TestNativeSessionRuntimeDoesNotResumeAcrossIssuedToolProfiles(t *testing.T) {
	f, prepared := nativeSessionPreparation(t, "generic")
	_, first, _ := runNativeSessionFixture(t, f, prepared, nil, provider.OutcomeSucceeded, true)
	prepareRoleOK(t, f.opts.MemberTools.Close())
	f.runtimeOptions.Operations = []string{"list_members", "read_channel", "send_message", "create_member"}
	f.runtimeOptions.MaxGroupMembers = 7
	var err error
	f.opts.MemberTools, err = NewMemberToolRuntime(f.s, f.runtimeOptions)
	prepareRoleOK(t, err)
	_, second, _ := runNativeSessionFixture(t, f, nextNativeSessionPreparation(t, f), func(call *nativeSessionFixtureCall) {
		if call.resume != nil || len(call.binding.Profile.Tools) != 4 || call.binding.Profile.MaxGroupMembers != 7 {
			t.Fatal("compatibility was not computed from newly issued grants")
		}
	}, provider.OutcomeSucceeded, true)
	if second.NativeRef.Pi.ToolsProfileSHA256 == first.NativeRef.Pi.ToolsProfileSHA256 {
		t.Fatal("change in issued tool contract was not reflected in provider evidence")
	}
}

func TestNativeSessionRuntimeExplicitResumeRejectionAndFirstNoRefBarrier(t *testing.T) {
	f, prepared := nativeSessionPreparation(t, "generic")
	runNativeSessionFixture(t, f, prepared, nil, provider.OutcomeSucceeded, true)
	prepared = nextNativeSessionPreparation(t, f)
	run := prepared.Executor.(*memberToolRuntimeRun)
	calls := 0
	run.nativeSession.factory = func(_ provider.NativeMemberToolConfig, _ []string, ref *provider.NativeSessionReference, _ json.RawMessage) (provider.Executor, error) {
		calls++
		if ref == nil {
			t.Fatal("qualified source was replaced by fresh construction")
		}
		return nil, errors.New("fixture explicit resume refusal")
	}
	turn, err := f.s.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: run.binding.DeliveryID, MaxActive: 1, ResourceKeys: prepared.ResourceKeys})
	prepareRoleOK(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	req := prepared.Request
	req.RunID, req.RuntimeDir = turn.ID, f.runtimeOptions.RoleExecutionRoot
	receipt := run.Execute(context.Background(), req, nil)
	if calls != 1 || receipt.Started || receipt.Outcome != provider.OutcomeNotStarted {
		t.Fatal("explicit resume refusal retried fresh or changed prestart evidence")
	}
	_, err = f.s.ApplyTurnReceipt(context.Background(), claim, receipt)
	prepareRoleOK(t, err)
	var reference *string
	prepareRoleOK(t, f.db.QueryRow("SELECT native_ref_json FROM native_run_bindings WHERE turn_id=?", turn.ID).Scan(&reference))
	if reference != nil {
		t.Fatal("metadata/factory failure did not persist its first noRef barrier")
	}
	runNativeSessionFixture(t, f, nextNativeSessionPreparation(t, f), func(call *nativeSessionFixtureCall) {
		if call.resume != nil {
			t.Fatal("prestart/noRef barrier allowed selection of an older source")
		}
	}, provider.OutcomeSucceeded, true)
}

func TestNativeSessionRuntimeAcceptedAssociationIsNotCandidate(t *testing.T) {
	f, prepared := nativeSessionPreparation(t, "generic")
	claim, receipt, call := runNativeSessionFixture(t, f, prepared, nil, provider.OutcomeSucceeded, true)
	run := prepared.Executor.(*memberToolRuntimeRun)
	binding := call.binding
	session := &memberToolNativeTurn{binding: binding, canonical: bytes.Clone(call.raw.NativeRef.Binding), selected: store.NativeSessionSelection{Reference: receipt.NativeRef, SourceTurnID: "candidate-only"}}
	for _, change := range []string{"not-started", "not-accepted", "wrong-native-id", "wrong-claim"} {
		t.Run(change, func(t *testing.T) {
			raw, current := cloneMemberToolReceipt(call.raw), claim
			switch change {
			case "not-started":
				raw.Started = false
			case "not-accepted":
				raw.Accepted = false
			case "wrong-native-id":
				raw.NativeSessionID = "different-native"
			case "wrong-claim":
				current.Token = "stale-claim"
			}
			before := bytes.Clone(raw.NativeRef.Binding)
			got := session.receipt(context.Background(), run, current, raw)
			if decodeNativeFixtureBinding(t, got).ResumedFromTurnID != "" || got.NativeRef.Pi != nil || got.NativeRef.Codex != nil || !bytes.Equal(before, raw.NativeRef.Binding) {
				t.Fatal("unaccepted candidate became actual resume or modified raw metadata")
			}
		})
	}
	copy := cloneMemberToolReceipt(receipt)
	copy.NativeRef.Pi.NativeSessionID = "changed"
	copy.NativeRef.Binding[0] = '['
	if reflect.DeepEqual(copy.NativeRef, receipt.NativeRef) || receipt.NativeRef.Binding[0] != '{' || receipt.NativeRef.Pi.NativeSessionID == "changed" {
		t.Fatal("reference deep clone retained payload/RawMessage aliases")
	}
}

func TestNativeSessionRuntimeConfigDriftAndCurrentClaim(t *testing.T) {
	for _, change := range []string{"member-model", "member-workspace", "claim", "claimed-resource", "config-contents"} {
		t.Run(change, func(t *testing.T) {
			f, prepared := nativeSessionPreparation(t, "generic")
			run := prepared.Executor.(*memberToolRuntimeRun)
			factory, calls := run.nativeSession.factory, 0
			run.nativeSession.factory = func(issued provider.NativeMemberToolConfig, names []string, ref *provider.NativeSessionReference, binding json.RawMessage) (provider.Executor, error) {
				calls++
				e, err := factory(issued, names, ref, binding)
				if change == "config-contents" && err == nil {
					path := issued.Path
					data, readErr := os.ReadFile(path)
					prepareRoleOK(t, readErr)
					var config map[string]string
					prepareRoleOK(t, json.Unmarshal(data, &config))
					config["token"] = "different-current-token"
					data, err = json.Marshal(config)
					prepareRoleOK(t, err)
					prepareRoleOK(t, os.WriteFile(path, data, 0600))
				}
				return e, err
			}
			keys := prepared.ResourceKeys
			if change == "claimed-resource" {
				keys = []string{"fixture-wrong-resource"}
			}
			turn, err := f.s.ClaimTurn(context.Background(), store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: run.binding.DeliveryID, MaxActive: 1, ResourceKeys: keys})
			prepareRoleOK(t, err)
			run.factory = func(string, string, []string) (provider.Executor, error) {
				t.Fatal("selector/preflight failure attempted legacy fallback")
				return nil, nil
			}
			switch change {
			case "member-model":
				f.exec(t, "UPDATE members SET model='changed' WHERE id=?", run.binding.MemberID)
			case "member-workspace":
				f.exec(t, "UPDATE members SET workspace=? WHERE id=?", t.TempDir(), run.binding.MemberID)
			case "claim":
				_, err = f.s.RecoverInFlight(context.Background())
				prepareRoleOK(t, err)
			}
			req := prepared.Request
			req.RunID, req.RuntimeDir = turn.ID, f.runtimeOptions.RoleExecutionRoot
			receipt := run.Execute(context.Background(), req, nil)
			if receipt.Started || change != "config-contents" && calls != 0 || change == "config-contents" && calls != 1 {
				t.Fatal("stale config/claim reached native execution or bypassed the actual immutable config gate")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(f.opts.PrivateWorkspaceRoot), "invoked")); !os.IsNotExist(err) {
				t.Fatal("drift rejection launched the sentinel CLI")
			}
		})
	}
}
