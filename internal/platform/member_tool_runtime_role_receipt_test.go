package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
	"modernc.org/sqlite"
)

func roleReceiptRuntime(t *testing.T) (memberRoleFixture, *MemberToolRuntime, provider.ExecuteRequest, MemberToolRunBinding) {
	t.Helper()
	f := memberRoleCoordinatorFixture(t)
	base := t.TempDir()
	config, root := filepath.Join(base, "config"), filepath.Join(base, "runtime")
	for _, path := range []string{config, root} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewMemberToolRuntime(f.db, MemberToolRuntimeOptions{ConfigRoot: config, Endpoint: "http://127.0.0.1:7310/api/v1/member-tools", Operations: []string{"list_members"}, RoleMaxActive: 8, RoleExecutionRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	scope := f.coordinatorScope
	req := provider.ExecuteRequest{RunID: scope.Member.Claim.TurnID, RuntimeDir: root, CWD: root, Model: "selected"}
	binding := MemberToolRunBinding{GroupID: f.groupID, MemberID: f.coordinator.ID, ConversationID: scope.Member.ConversationID, DeliveryID: scope.Provenance.DeliveryID, ProviderID: "pi", Model: "selected", ProviderName: provider.Pi}
	return f, r, req, binding
}
func roleReceiptInvoke(t *testing.T, r *MemberToolRuntime, req provider.ExecuteRequest, binding MemberToolRunBinding, p store.CoordinationRoleProvenance, invoke func() provider.ExecutionReceipt) provider.ExecutionReceipt {
	t.Helper()
	executor, err := r.WrapCoordinationRole(binding, p, func(string, string, []string) (provider.Executor, error) {
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			return invoke()
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor.Execute(context.Background(), req, nil)
}

func TestRoleReceiptCaptureSeparatesRawAggregateAndCurrent(t *testing.T) {
	f, r, req, binding := roleReceiptRuntime(t)
	ctx := context.Background()
	claim := f.coordinatorScope.Member.Claim
	before, err := r.InspectCoordinationRoleReceipt(ctx, f.groupID, claim.TurnID)
	if err != nil || before.Capture != nil || before.CaptureStatus != "missing" || before.Current.Receipt != nil {
		t.Fatalf("missing diagnostic fabricated evidence: %v", err)
	}
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1, Reason: "PRIVATE-original-reason", StdoutPath: "/not-created/stdout", LogsTruncated: true}
	aggregate := roleReceiptInvoke(t, r, req, binding, f.coordinatorScope.Provenance, func() provider.ExecutionReceipt {
		// A real same-claim unknown receipt conflicts with proving whole-Turn nonexecution.
		_, err := f.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	})
	if aggregate.Outcome != provider.OutcomeUnknown || aggregate.Reason == raw.Reason || aggregate.ExitCode != raw.ExitCode || aggregate.StdoutPath != raw.StdoutPath || aggregate.LogsTruncated != raw.LogsTruncated {
		t.Fatal("aggregate forged/dropped evidence")
	}
	got, err := r.InspectCoordinationRoleReceipt(ctx, f.groupID, claim.TurnID)
	if err != nil || got.Capture == nil || got.CaptureStatus != "captured" || got.Capture.Raw != raw || got.Capture.Aggregate != aggregate || got.Current.Receipt == nil || got.Current.Receipt.Reason == raw.Reason {
		t.Fatalf("raw/aggregate/current not independent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(req.RuntimeDir, req.RunID)); !os.IsNotExist(err) {
		t.Fatal("sidecar depended on adapter RunID directory")
	}
	encoded, err := json.Marshal(got)
	if err != nil || string(encoded) != "{}" {
		t.Fatal("local diagnostic became normal DTO")
	}
	ordinary, err := f.db.GetTurn(ctx, f.groupID, claim.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(ordinary)
	if strings.Contains(string(encoded), raw.Reason) || strings.Contains(string(encoded), claim.Token) {
		t.Fatal("ordinary DTO leaked raw receipt/token")
	}
	// Current normalized data may come from a later Apply, never the capture instant.
	_, err = f.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: 27, Reason: "PRIVATE-later"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = r.InspectCoordinationRoleReceipt(ctx, f.groupID, claim.TurnID)
	if err != nil || got.Capture == nil || got.Capture.Raw != raw || got.Current.Receipt.ExitCode != 27 {
		t.Fatal("later Apply overwrote/impersonated first capture")
	}
	if _, err = r.InspectCoordinationRoleReceipt(ctx, f.otherID, claim.TurnID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-group query: %v", err)
	}
	// Normal reopen, no recovery or provider process is implied.
	reopened, err := store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	options := MemberToolRuntimeOptions{ConfigRoot: r.configRoot, Endpoint: r.endpoint, Operations: []string{"list_members"}, RoleMaxActive: 8, RoleExecutionRoot: req.RuntimeDir}
	reader, err := NewMemberToolRuntime(reopened, options)
	if err != nil {
		t.Fatal(err)
	}
	got, err = reader.InspectCoordinationRoleReceipt(ctx, f.groupID, claim.TurnID)
	if err != nil || got.Capture == nil || got.Capture.Raw != raw {
		t.Fatal("normal reopen lost capture")
	}
}

func TestRoleReceiptRecoveryPreservesHistoricalCapture(t *testing.T) {
	f, oldRuntime, req, binding := roleReceiptRuntime(t)
	ctx := context.Background()
	claim := f.coordinatorScope.Member.Claim
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, ExitCode: 23, Reason: "PRIVATE-first-unknown"}
	natives := 0
	aggregate := roleReceiptInvoke(t, oldRuntime, req, binding, f.coordinatorScope.Provenance, func() provider.ExecutionReceipt { natives++; return raw })
	first, err := oldRuntime.InspectCoordinationRoleReceipt(ctx, claim.GroupID, claim.TurnID)
	if err != nil || natives != 1 || aggregate != raw || first.Capture == nil || first.Capture.Raw != raw || first.Capture.Aggregate != aggregate {
		t.Fatal("real issuance/native/capture precondition failed")
	}
	if _, err := f.db.ApplyTurnReceipt(ctx, claim, aggregate); err != nil {
		t.Fatal(err)
	}
	oldRuntime.roleFence.mu.Lock()
	owner := oldRuntime.roleFence.records[claim.TurnID]
	oldRuntime.roleFence.mu.Unlock()
	if owner == nil {
		t.Fatal("unknown receipt did not retain its local owner")
	}
	if _, err := oldRuntime.InspectCoordinationRoleReceipt(ctx, claim.GroupID, claim.TurnID); err != nil {
		t.Fatal(err)
	}
	oldRuntime.roleFence.mu.Lock()
	retained := oldRuntime.roleFence.records[claim.TurnID] == owner
	oldRuntime.roleFence.mu.Unlock()
	if !retained {
		t.Fatal("inspection released an unknown owner's fence")
	}
	path := filepath.Join(req.RuntimeDir, memberToolRoleReceiptDirectory, memberToolRoleReceiptName(claim.GroupID, claim.TurnID))
	firstBytes, err := os.ReadFile(path)
	if err != nil || bytes.Contains(firstBytes, []byte(claim.Token)) {
		t.Fatal("missing sidecar or raw token in capture")
	}
	options := MemberToolRuntimeOptions{ConfigRoot: oldRuntime.configRoot, Endpoint: oldRuntime.endpoint, Operations: []string{"list_members"}, RoleMaxActive: 8, RoleExecutionRoot: req.RuntimeDir}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	oldRuntime = nil // No in-memory fence survival is assumed across restart.
	reopened, err := store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.RecoverInFlight(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("actual singleton recovery failed: count=%d error=%v", recovered, err)
	}
	current, err := reopened.GetTurn(ctx, claim.GroupID, claim.TurnID)
	if err != nil || current.State != domain.TurnStateUnknown || current.ClaimToken == "" || current.ClaimToken == claim.Token {
		t.Fatal("recovery did not rotate unknown claim")
	}
	reader, err := NewMemberToolRuntime(reopened, options)
	if err != nil {
		t.Fatal(err)
	}
	fileRecord, status := reader.readRoleReceipt(claim.GroupID, claim.TurnID)
	if status != "captured" || fileRecord == nil || fileRecord.Source != memberToolRoleReceiptSourceFor(binding, f.coordinatorScope) {
		t.Fatal("first historical record not intact")
	}
	var claims int
	if err := f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", claim.TurnID).Scan(&claims); err != nil || claims != 1 {
		t.Fatal("real recovery failed to retain unknown resources")
	}
	t.Log("one real issuance/native/capture; close/reopen/Recover rotated unknown claim, retained resource and unchanged readable first record")
	before := memberRoleSnapshot(t, f.localMessageFixture)
	got, err := reader.InspectCoordinationRoleReceipt(ctx, claim.GroupID, claim.TurnID)
	if err != nil || got.CaptureStatus != "captured" || got.Capture == nil || got.Capture.Raw != raw || got.Capture.Aggregate != aggregate || got.Capture.Source != fileRecord.Source {
		t.Fatal("recovery hid intact historical raw/aggregate evidence from trusted inspection")
	}
	if !first.CaptureClaimMatchesCurrent || got.CaptureClaimMatchesCurrent || got.Current.Turn.ClaimToken != current.ClaimToken || got.Current.Turn.State != domain.TurnStateUnknown ||
		got.Current.Receipt == nil || got.Current.Receipt.Outcome != provider.OutcomeUnknown || got.Current.Receipt.Reason == raw.Reason {
		t.Fatal("historical capture was confused with current claim/normalized evidence")
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(firstBytes, afterBytes) || bytes.Contains(afterBytes, []byte(current.ClaimToken)) || !reflect.DeepEqual(before, memberRoleSnapshot(t, f.localMessageFixture)) {
		t.Fatal("inspection changed file/source or durable rows")
	}
	if err := f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", claim.TurnID).Scan(&claims); err != nil || claims != 1 {
		t.Fatal("recovery inspection released durable unknown resources")
	}
	if len(reader.roleFence.records) != 0 {
		t.Fatal("inspection created a new runtime owner")
	}
	// Execution evidence acceptance still requires the original exact claim.
	e := memberToolRuntimeRun{runtime: reader, binding: binding, role: &f.coordinatorScope.Provenance}
	if e.roleHistorical(ctx, req, f.coordinatorScope) {
		t.Fatal("Execute evidence acceptance accepted a recovered claim")
	}
	factoriesAfterRecovery := 0
	executor, err := reader.WrapCoordinationRole(binding, f.coordinatorScope.Provenance, func(string, string, []string) (provider.Executor, error) {
		factoriesAfterRecovery++
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			natives++
			return raw
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if executor.Execute(ctx, req, nil).Outcome != provider.OutcomeUnknown || factoriesAfterRecovery != 0 || natives != 1 || len(reader.roleFence.records) != 0 {
		t.Fatal("historical inspection enabled recovered execution")
	}
	if _, err := reader.InspectCoordinationRoleReceipt(ctx, f.otherID, claim.TurnID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-group inspection gained history")
	}
	message, err := NewLocalUser(reopened).CreateMessage(ctx, f.groupID, MessageInput{TopicID: "other-topic", Content: "other turn", RecipientIDs: []string{f.worker.ID}}, "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := reopened.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 8, ResourceKeys: []string{"other-turn"}})
	if err != nil {
		t.Fatal(err)
	}
	wrongPath := filepath.Join(req.RuntimeDir, memberToolRoleReceiptDirectory, memberToolRoleReceiptName(f.groupID, other.ID))
	if err := os.WriteFile(wrongPath, firstBytes, 0600); err != nil {
		t.Fatal(err)
	} // Explicit test-owned wrong-turn file.
	wrong, err := reader.InspectCoordinationRoleReceipt(ctx, f.groupID, other.ID)
	if err != nil || wrong.Capture != nil || wrong.CaptureStatus != "unconfirmed" {
		t.Fatal("wrong-turn sidecar gained historical authority")
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	} // Explicit test-owned damage, never product repair.
	damaged, err := reader.InspectCoordinationRoleReceipt(ctx, claim.GroupID, claim.TurnID)
	if err != nil || damaged.Capture != nil || damaged.CaptureStatus != "unconfirmed" {
		t.Fatal("damaged first record was returned as complete")
	}
	if err := f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", claim.TurnID).Scan(&claims); err != nil || claims != 1 || natives != 1 || len(reader.roleFence.records) != 0 {
		t.Fatal("negative inspection changed retained execution ownership")
	}
}

type roleReceiptReadFailure struct{ data []byte }

func (r roleReceiptReadFailure) Read(p []byte) (int, error) {
	return copy(p, r.data), errors.New("PRIVATE-READ-FAILURE")
}

func TestRoleReceiptBoundariesAndDamagedFirstRecord(t *testing.T) {
	f, r, req, binding := roleReceiptRuntime(t)
	claim := f.coordinatorScope.Member.Claim
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1, NativeSessionID: strings.Repeat("<", 65536), StdoutPath: strings.Repeat("<", 4096), StderrPath: strings.Repeat("\\", 4096), Reason: strings.Repeat("<", 72)}
	record := memberToolRoleReceiptRecord{Version: 1, Source: memberToolRoleReceiptSourceFor(binding, f.coordinatorScope), Raw: raw, Aggregate: raw}
	if _, ok := memberToolRoleJSONBound(math.MaxInt, math.MaxInt); ok {
		t.Fatal("overflow accepted without allocation")
	}
	if _, ok := memberToolRoleJSONBound(memberToolRoleReceiptMax); ok {
		t.Fatal("JSON expansion precheck accepted oversize")
	}
	encoded, ok := memberToolRoleEncodeReceipt(record)
	if !ok || len(encoded) > memberToolRoleReceiptMax {
		t.Fatal("supported maximum/worst-case escaping rejected")
	}
	decoded, ok := memberToolRoleDecodeReceipt(bytes.NewReader(encoded))
	if !ok || decoded.Raw != record.Raw || decoded.Aggregate != record.Aggregate {
		t.Fatal("complete bounded reader rejected a valid record")
	}
	if _, ok := memberToolRoleDecodeReceipt(roleReceiptReadFailure{data: encoded}); ok {
		t.Fatal("reader failure accepted partial evidence")
	}
	maximum := record
	remaining := 16384
	for _, value := range maximum.Source.strings() {
		remaining -= len(value)
	}
	maximum.Source.Model = strings.Repeat("<", remaining+len(maximum.Source.Model))
	maximum.Aggregate.Reason = strings.Repeat("<", 1024)
	if data, ok := memberToolRoleEncodeReceipt(maximum); !ok || len(data) > memberToolRoleReceiptMax {
		t.Fatal("supported total provenance/aggregate boundary rejected")
	}
	for _, change := range []func(*memberToolRoleReceiptRecord){
		func(x *memberToolRoleReceiptRecord) { x.Raw.NativeSessionID += "x" },
		func(x *memberToolRoleReceiptRecord) { x.Raw.StdoutPath += "x" },
		func(x *memberToolRoleReceiptRecord) { x.Raw.Reason += "x" },
		func(x *memberToolRoleReceiptRecord) { x.Aggregate.Reason = strings.Repeat("x", 1025) },
		func(x *memberToolRoleReceiptRecord) { x.Source.Model = strings.Repeat("x", 16385) },
		func(x *memberToolRoleReceiptRecord) { x.Source.ProviderID = "" },
		func(x *memberToolRoleReceiptRecord) { x.Source.ProviderID = "\xff" },
		func(x *memberToolRoleReceiptRecord) { x.Raw.Reason = "\xff" },
	} {
		bad := record
		change(&bad)
		if _, ok := memberToolRoleEncodeReceipt(bad); ok {
			t.Fatal("out-of-bound/invalid UTF-8 capture accepted")
		}
	}
	path := filepath.Join(req.RuntimeDir, ".role-diagnostics", memberToolRoleReceiptName(claim.GroupID, claim.TurnID))
	if r.writeRoleReceipt(record) != true {
		t.Fatal("first-record write failed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode() != 0600 {
		t.Fatal("diagnostic not private regular file")
	}
	info, err = os.Stat(filepath.Dir(path))
	if err != nil || info.Mode() != os.ModeDir|0700 {
		t.Fatal("diagnostic directory not private")
	}
	if strings.Contains(path, claim.Token) {
		t.Fatal("filename contains token")
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record.Raw.Reason = "different"
	if r.writeRoleReceipt(record) {
		t.Fatal("existing first record overwritten")
	}
	again, err := os.ReadFile(path)
	if err != nil || string(first) != string(again) {
		t.Fatal("existing bytes changed")
	}
	// Mutations below are explicit test-owned corruption, never automatic repair.
	foreign := record
	// Fingerprint mismatch can be legitimate recovery history; a foreign
	// immutable request remains invalid regardless of current claim matching.
	foreign.Source.RequestID = "foreign-request"
	foreignBytes, ok := memberToolRoleEncodeReceipt(foreign)
	if !ok {
		t.Fatal("foreign source fixture not encodable")
	}
	for _, body := range [][]byte{[]byte("{"), []byte("null"), []byte(`{"Version":1}`), append(append([]byte{}, first...), []byte("{}")...), []byte("\xff"), []byte(strings.Repeat("x", memberToolRoleReceiptMax+1)), foreignBytes,
		bytes.Replace(first, []byte(`"Version":1`), []byte(`"Version":1,"Version":1`), 1),
		bytes.Replace(first, []byte(`"started":false`), []byte(`"started":null`), 1)} {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := r.InspectCoordinationRoleReceipt(context.Background(), f.groupID, claim.TurnID)
		if err != nil || got.Capture != nil || got.CaptureStatus != "unconfirmed" {
			t.Fatal("partial/corrupt/oversize diagnostic treated as complete")
		}
	}
	if err := os.WriteFile(path, first, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	got, err := r.InspectCoordinationRoleReceipt(context.Background(), f.groupID, claim.TurnID)
	if err != nil || got.Capture != nil || got.CaptureStatus != "unconfirmed" {
		t.Fatal("unsafe read permissions accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0600); err != nil {
		t.Fatal(err)
	}
	got, err = r.InspectCoordinationRoleReceipt(context.Background(), f.groupID, claim.TurnID)
	if err != nil || got.Capture != nil || got.CaptureStatus != "unconfirmed" {
		t.Fatal("nonregular/read error accepted")
	}
}

func TestRoleReceiptWriteFailureDoesNotChangeExecution(t *testing.T) {
	f, r, req, binding := roleReceiptRuntime(t)
	// A non-directory collision is a real filesystem write failure, not chmod-root assumptions.
	if err := os.WriteFile(filepath.Join(req.RuntimeDir, ".role-diagnostics"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1, Reason: "PRIVATE-native"}
	got := roleReceiptInvoke(t, r, req, binding, f.coordinatorScope.Provenance, func() provider.ExecutionReceipt { return raw })
	if got != raw {
		t.Fatal("diagnostic failure changed native evidence")
	}
	diagnostic, err := r.InspectCoordinationRoleReceipt(context.Background(), f.groupID, req.RunID)
	if err != nil || diagnostic.Capture != nil || diagnostic.CaptureStatus != "unconfirmed" {
		t.Fatal("failed diagnostic falsely reported saved")
	}
}

func TestRoleReceiptCancelledCallerAndUnsupportedCapture(t *testing.T) {
	f, r, req, binding := roleReceiptRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claim := f.coordinatorScope.Member.Claim
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1, Reason: "PRIVATE-cancelled-native", StdoutPath: "/raw/stdout", StderrPath: "/raw/stderr", LogsTruncated: true}
	executor, err := r.WrapCoordinationRole(binding, f.coordinatorScope.Provenance, func(string, string, []string) (provider.Executor, error) {
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			cancel()
			if _, err := f.db.RequestTurnCancel(context.Background(), claim.GroupID, claim.TurnID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.raw.Exec("UPDATE members SET status='left',model='changed' WHERE id=?", binding.MemberID); err != nil {
				t.Fatal(err)
			}
			return raw
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := executor.Execute(ctx, req, nil); got != raw {
		t.Fatal("cancel/live permission change denied produced native evidence")
	}
	diagnostic, err := r.InspectCoordinationRoleReceipt(context.Background(), claim.GroupID, claim.TurnID)
	if err != nil || diagnostic.Capture == nil || diagnostic.Capture.Raw != raw {
		t.Fatal("cancelled caller lost full raw evidence")
	}
	if _, err := f.db.ApplyTurnReceipt(context.Background(), claim, raw); err != nil {
		t.Fatal(err)
	}

	t.Run("unsupported capture does not constrain execution", func(t *testing.T) {
		f, r, req, binding := roleReceiptRuntime(t)
		raw := provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Terminal: provider.OutcomeFailed, Started: true, Accepted: true, DeliveryFailed: true, NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true, ExitCode: 7, StdoutPath: "/raw/stdout", StderrPath: "/raw/stderr", LogsTruncated: true, Reason: strings.Repeat("x", 73)}
		got := roleReceiptInvoke(t, r, req, binding, f.coordinatorScope.Provenance, func() provider.ExecutionReceipt { return raw })
		if got != raw {
			t.Fatal("capture boundary rewrote execution fields")
		}
		diagnostic, err := r.InspectCoordinationRoleReceipt(context.Background(), f.groupID, req.RunID)
		if err != nil || diagnostic.Capture != nil || diagnostic.CaptureStatus != "missing" {
			t.Fatal("unsupported receipt claimed complete capture")
		}
	})
}

func TestRoleRuntimePostclaimAndFinalStartupRechecks(t *testing.T) {
	for _, phase := range []string{"post-reserve fresh", "startup blocked", "final selection", "revoked grants", "factory failure", "wrong root"} {
		t.Run(phase, func(t *testing.T) {
			f, r, req, binding := roleReceiptRuntime(t)
			claim := f.coordinatorScope.Member.Claim
			factories, natives := 0, 0
			if phase == "post-reserve fresh" {
				reads := 0
				r.roleFence.readTurn = func(ctx context.Context, g, id string) (domain.Turn, error) {
					turn, err := f.db.GetTurn(ctx, g, id)
					reads++
					if reads == 2 && err == nil {
						_, err = f.db.RequestTurnCancel(context.Background(), g, id)
					}
					return turn, err
				}
			}
			if phase == "startup blocked" {
				if _, err := f.db.SetDispatchPolicy(context.Background(), f.groupID, []string{f.worker.ID}, 1, "changed-policy"); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "wrong root" {
				req.RuntimeDir = t.TempDir()
			}
			executor, err := r.WrapCoordinationRole(binding, f.coordinatorScope.Provenance, func(path, _ string, _ []string) (provider.Executor, error) {
				factories++
				switch phase {
				case "final selection":
					if _, err := f.raw.Exec("UPDATE members SET model='changed' WHERE id=?", binding.MemberID); err != nil {
						t.Fatal(err)
					}
				case "revoked grants":
					_, token := memberToolRuntimeConfig(t, path)
					r.Tools().Revoke(token)
				case "factory failure":
					return nil, errors.New("PRIVATE-FACTORY-ERROR")
				}
				return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
					natives++
					return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}
				}), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got := executor.Execute(context.Background(), req, nil)
			if got.Outcome != provider.OutcomeUnknown || natives != 0 {
				t.Fatal("failed startup invoked native or fabricated not_started")
			}
			wantFactories := 1
			if phase == "post-reserve fresh" || phase == "startup blocked" || phase == "wrong root" {
				wantFactories = 0
			}
			if factories != wantFactories {
				t.Fatalf("factory calls=%d, want %d", factories, wantFactories)
			}
			diagnostic, err := r.InspectCoordinationRoleReceipt(context.Background(), claim.GroupID, claim.TurnID)
			if err != nil || diagnostic.Capture != nil || diagnostic.CaptureStatus != "missing" {
				t.Fatal("no native invoke invented a raw receipt")
			}
			memberToolRuntimeEmpty(t, r.configRoot)
			var claims int
			if err := f.raw.QueryRow("SELECT count(*) FROM resource_claims WHERE turn_id=?", claim.TurnID).Scan(&claims); err != nil || claims != 1 {
				t.Fatal("pre-native failure released resources")
			}
		})
	}
}

func TestRoleRuntimePreflightBoundsDatabasePoolWait(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	function := "role_preflight_hold_" + rand.Text()
	// Register before Store.Open: the test-only SQL function holds a real Store
	// transaction's sole connection. No Store DB accessor or issuance bypass.
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		once.Do(func() { close(entered); <-release })
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	f, r, req, binding := roleReceiptRuntime(t)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	caller := context.Background()
	if _, hasDeadline := caller.Deadline(); hasDeadline {
		t.Fatal("caller must have no deadline")
	}
	var run *memberToolRuntimeRun
	executor, err := r.WrapCoordinationRole(binding, f.coordinatorScope.Provenance, func(path, _ string, _ []string) (provider.Executor, error) {
		_, token := memberToolRuntimeConfig(t, path)
		root, err := os.OpenRoot(r.configRoot)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		config := &memberToolRuntimeConfigFile{root: root, name: filepath.Base(path), info: info}
		_, err = f.raw.Exec("CREATE TRIGGER role_preflight_hold_connection AFTER UPDATE OF claim_token ON turns BEGIN SELECT " + function + "(); END")
		if err != nil {
			t.Fatal(err)
		}
		holder := make(chan error, 1)
		go func() { holder <- f.db.ValidateMemberTool(caller, f.coordinatorScope.Member) }()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("real Store transaction never entered SQL hold")
		}
		finished := make(chan bool, 1)
		go func() { finished <- run.rolePreflight(caller, req, f.coordinatorScope, token, config) }()
		// Store deliberately has no public pool handle. Observe the pinned Go
		// toolchain's actual rolePreflight -> DB.conn stack while its sole
		// connection is held above. This is a SQL pool wait, NOT a fence gate wait.
		stack := make([]byte, 64<<10)
		barrier := time.After(3 * time.Second)
		waiting := false
		for !waiting {
			n := runtime.Stack(stack, true)
			for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
				if strings.Contains(goroutine, "(*memberToolRuntimeRun).rolePreflight(") && strings.Contains(goroutine, "database/sql.(*DB).conn(") {
					waiting = true
					break
				}
			}
			if !waiting {
				select {
				case <-barrier:
					t.Fatal("rolePreflight was not observed waiting for the Store pool")
				default:
					runtime.Gosched()
				}
			}
		}
		t.Log("observed rolePreflight database/sql pool wait with a deadline-free caller")
		select {
		case allowed := <-finished:
			if allowed {
				t.Error("blocked database verification authorized startup")
			}
		case <-time.After(memberToolRuntimeVerificationTimeout + time.Second):
			t.Error("rolePreflight did not bound the actual Store pool wait to its verification timeout")
			close(release)
			<-finished
		}
		select {
		case <-release:
		default:
			close(release)
		}
		if err := <-holder; err != nil {
			t.Fatal(err)
		}
		return nil, errors.New("test stops before native execution")
	})
	if err != nil {
		t.Fatal(err)
	}
	run = executor.(*memberToolRuntimeRun)
	if receipt := executor.Execute(caller, req, nil); receipt.Outcome != provider.OutcomeUnknown {
		t.Fatal("bounded pre-native failure was not unknown")
	}
}

func TestRoleRuntimeConcurrentWrappersShareOneOwner(t *testing.T) {
	f, r, req, binding := roleReceiptRuntime(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan provider.ExecutionReceipt, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var factories, natives atomic.Int32
	factory := func(string, string, []string) (provider.Executor, error) {
		if factories.Add(1) == 1 {
			close(entered)
			<-release
		}
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			natives.Add(1)
			return provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
		}), nil
	}
	first, err := r.WrapCoordinationRole(binding, f.coordinatorScope.Provenance, factory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.WrapCoordinationRole(binding, f.coordinatorScope.Provenance, factory)
	if err != nil {
		t.Fatal(err)
	}
	go func() { finished <- first.Execute(context.Background(), req, nil) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first wrapper did not reach factory")
	}
	if got := second.Execute(context.Background(), req, nil); got.Outcome != provider.OutcomeUnknown || factories.Load() != 1 || natives.Load() != 0 {
		t.Fatal("second wrapper bypassed shared owner while first factory was in progress")
	}
	close(release)
	select {
	case got := <-finished:
		if got.Outcome != provider.OutcomeNotStarted {
			t.Fatal("owner lost native evidence")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first wrapper did not finish")
	}
	if factories.Load() != 1 || natives.Load() != 1 {
		t.Fatal("same Turn invoked more than once")
	}
}

func TestRoleRuntimeRevokeDrainKeepsFenceUntilCleanup(t *testing.T) {
	f, r, req, binding := roleReceiptRuntime(t)
	claim := f.coordinatorScope.Member.Claim
	// Fill the other slots with existing private-core fixture helpers and real
	// fresh claims, using the same M=8. No capability/native invocation is issued
	// for these test owners, so capacity must actually examine the drain owner.
	fenceFixture := memberToolRoleFenceFixture{localMessageFixture: f.localMessageFixture, runtime: r, fence: r.roleFence, root: req.RuntimeDir, maxActive: 8}
	for i := 0; i < 7; i++ {
		fenceFixture.reserve(t, fenceFixture.claim(t, f.groupID))
	}
	raw := provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan provider.ExecutionReceipt, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var configPath string
	executor, err := r.WrapCoordinationRole(binding, f.coordinatorScope.Provenance, func(path, _ string, _ []string) (provider.Executor, error) {
		configPath = path
		_, token := memberToolRuntimeConfig(t, path)
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			go func() {
				_ = r.tools.withAuthorized(context.Background(), token, "read_channel", func(store.MemberToolScope, MemberToolGrants) error { close(entered); <-release; return nil })
			}()
			<-entered
			return raw
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { returned <- executor.Execute(context.Background(), req, nil) }()
	<-entered
	// Only a complete, valid sidecar proves the wrapper has actually returned
	// from native.Execute; owner.invoked is set BEFORE native entry.
	captureDeadline := time.After(3 * time.Second)
	for {
		record, status := r.readRoleReceipt(claim.GroupID, claim.TurnID)
		if status == "captured" && record.Raw == raw && record.Aggregate == raw && record.Source == memberToolRoleReceiptSourceFor(binding, f.coordinatorScope) {
			break
		}
		select {
		case <-captureDeadline:
			t.Fatal("no complete post-native capture while callback was blocked")
		default:
			runtime.Gosched()
		}
	}
	t.Log("complete post-native sidecar observed before releasing the real registry callback")
	// The callback holds RLock. TryRLock only fails once the sole writer,
	// wrapper Revoke, is actually waiting. No elapsed-time guess is evidence.
	deadline := time.After(3 * time.Second)
	for r.tools.mu.TryRLock() {
		r.tools.mu.RUnlock()
		select {
		case <-deadline:
			t.Fatal("Revoke never began waiting for callback drain")
		default:
			runtime.Gosched()
		}
	}
	terminal, err := f.db.ApplyTurnReceipt(context.Background(), claim, raw)
	if err != nil || terminal.State != domain.TurnStateNotStarted {
		t.Fatalf("real committed terminal missing: %v", err)
	}
	r.roleFence.mu.Lock()
	owner := r.roleFence.records[claim.TurnID]
	done := owner.localDone
	r.roleFence.mu.Unlock()
	if done {
		t.Fatal("localDone before callback drain")
	}
	if err := r.CheckCoordinationRoleCapacity(context.Background()); !errors.Is(err, errMemberToolRoleFenceCapacity) {
		t.Fatalf("committed terminal freed the fence before callback drain: %v", err)
	}
	r.roleFence.mu.Lock()
	retained := r.roleFence.records[claim.TurnID] == owner
	r.roleFence.mu.Unlock()
	if !retained {
		t.Fatal("draining owner disappeared")
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatal("config cleaned before callback drain")
	}
	select {
	case <-returned:
		t.Fatal("Execute returned before Revoke drained")
	default:
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Revoke did not finish")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatal("config cleanup incomplete")
	}
	r.roleFence.mu.Lock()
	done = owner.localDone
	r.roleFence.mu.Unlock()
	if !done {
		t.Fatal("owner not marked done after cleanup")
	}
	if err := r.CheckCoordinationRoleCapacity(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.roleFence.mu.Lock()
	retained = r.roleFence.records[claim.TurnID] == owner
	r.roleFence.mu.Unlock()
	if retained {
		t.Fatal("committed terminal plus completed drain/cleanup did not release the owner")
	}
}

func TestRoleFenceExactErrorsAndWaitingQueryCancellation(t *testing.T) {
	f := setupMemberToolRoleFence(t, 1)
	r, err := NewMemberToolRuntime(f.db, MemberToolRuntimeOptions{ConfigRoot: f.runtime.configRoot, Endpoint: f.runtime.endpoint, Operations: []string{"list_members"}, RoleMaxActive: 1, RoleExecutionRoot: f.root})
	if err != nil {
		t.Fatal(err)
	}
	f.runtime, f.fence = r, r.roleFence
	ctx := context.Background()
	reads := 0
	f.fence.readTurn = func(context.Context, string, string) (domain.Turn, error) {
		reads++
		return domain.Turn{}, errors.New("unexpected empty-slot read")
	}
	if err := r.CheckCoordinationRoleCapacity(ctx); err != nil || reads != 0 || len(f.fence.records) != 0 {
		t.Fatal("empty capacity check read Store or reserved a claim")
	}
	f.fence.readTurn = f.db.GetTurn
	claim := f.claim(t, f.groupID)
	owner := f.reserve(t, claim)
	if err := r.CheckCoordinationRoleCapacity(ctx); !errors.Is(err, errMemberToolRoleFenceCapacity) || errors.Is(err, errMemberToolRoleFenceDenied) {
		t.Fatalf("capacity category lost: %v", err)
	}
	if _, err := f.fence.reserve(ctx, claim, f.root); !errors.Is(err, errMemberToolRoleFenceDenied) || errors.Is(err, errMemberToolRoleFenceCapacity) {
		t.Fatalf("duplicate category lost: %v", err)
	}
	if _, err := f.fence.reserve(ctx, claim, f.runtime.configRoot); !errors.Is(err, errMemberToolRoleFenceConfig) {
		t.Fatalf("root category lost: %v", err)
	}
	owner.markLocalDone()
	failure := errors.New("PRIVATE-DB-CAUSE")
	f.fence.readTurn = func(context.Context, string, string) (domain.Turn, error) {
		return domain.Turn{}, fmt.Errorf("wrapped: %w", failure)
	}
	if err := r.CheckCoordinationRoleCapacity(ctx); !errors.Is(err, failure) || errors.Is(err, errMemberToolRoleFenceCapacity) {
		t.Fatalf("DB error became capacity/no_work: %v", err)
	}
	f.fence.readTurn = func(context.Context, string, string) (domain.Turn, error) {
		return domain.Turn{}, fmt.Errorf("wrapped: %w", context.DeadlineExceeded)
	}
	if err := r.CheckCoordinationRoleCapacity(ctx); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errMemberToolRoleFenceCapacity) {
		t.Fatalf("deadline cause became capacity/no_work: %v", err)
	}
	// Force a real SQLite query to wait for a pooled connection inside the read
	// seam, after Store.GetTurn has read the exact real row. Stats is the observed
	// barrier: cancellation occurs only after the query actually starts waiting.
	f.raw.SetMaxOpenConns(1)
	held, err := f.raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	baseline := f.raw.Stats().WaitCount
	f.fence.readTurn = func(ctx context.Context, g, id string) (domain.Turn, error) {
		turn, err := f.db.GetTurn(ctx, g, id)
		if err != nil {
			return turn, err
		}
		var one int
		err = f.raw.QueryRowContext(ctx, "SELECT 1").Scan(&one)
		return turn, err
	}
	waiting, cancel := context.WithCancel(ctx)
	defer cancel()
	ended := make(chan error, 1)
	go func() { ended <- r.CheckCoordinationRoleCapacity(waiting) }()
	deadline := time.After(3 * time.Second)
	for f.raw.Stats().WaitCount == baseline {
		select {
		case <-deadline:
			t.Fatal("query never began waiting")
		default:
			runtime.Gosched()
		}
	}
	cancel()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) || errors.Is(err, errMemberToolRoleFenceCapacity) {
			t.Fatalf("waiting query cancellation misclassified: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("query cancellation did not return")
	}
	f.fence.mu.Lock()
	retained := f.fence.records[claim.TurnID] == owner
	f.fence.mu.Unlock()
	if !retained {
		t.Fatal("query cancellation freed unknown claim")
	}
}
