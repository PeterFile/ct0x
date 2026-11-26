package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentworkspace/internal/preflight"
)

func TestACPPreflightCancellationBeforeStart(t *testing.T) {
	for _, stage := range []string{"opencode_dependency_walk", "acp_os_start"} {
		t.Run(stage, func(t *testing.T) {
			e, request, launch := acpPreparedFixture(t, "oc-prepared")
			e.timeout = time.Minute // Cancellation is deterministic even on slow test storage.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []preflight.Observation
			ctx = preflight.WithObserver(ctx, func(event preflight.Observation) {
				events = append(events, event)
				if event.Stage == stage && event.Status == "begin" {
					cancel()
				}
			})
			r := e.execute(ctx, request, func(Event) error {
				t.Error("preflight produced a native event")
				return nil
			}, launch)
			if r.Outcome != OutcomeNotStarted || r.Started || r.Accepted || r.Terminal != "" || r.ExitCode != -1 || r.DirectChildReaped || r.NativeSessionID != "" || r.OutputComplete || r.DeliveryFailed {
				t.Fatal("canceled preflight returned native execution evidence")
			}
			if _, err := os.Lstat(filepath.Join(request.CWD, "pid")); !os.IsNotExist(err) {
				t.Fatal("canceled preflight reached the fixture child")
			}
			var canceled bool
			for _, event := range events {
				if event.Stage == "acp_os_start" && (stage != event.Stage || event.Status == "ok") {
					t.Fatal("canceled preflight advanced to OS Start")
				}
				canceled = canceled || event.Stage == stage && event.Status == "canceled"
			}
			if !canceled {
				t.Fatal("missing sanitized cancellation stage")
			}
		})
	}
}

func TestACPPreflightDuplicateRunIDStaysUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		cancel      bool
	}{
		{name: "already-canceled", cancel: true},
		{name: "cancel-during-member-check", stage: "acp_execute_member_config", cancel: true},
		{name: "cancel-at-mkdir", stage: "acp_run_dir", cancel: true},
		{name: "mkdir-collision", stage: "acp_run_dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, request, prepared := acpFixture(t, "normal")
			e.config.VendorProfile, e.config.Version, e.config.Model = ACPOpenCode119, "1.19.6", "opencode/big-pickle"
			e.member, e.timeout = prepared.member, time.Minute
			marker := filepath.Join(request.RuntimeDir, request.RunID, "owner-marker")
			reserve := func() {
				ownerCtx, cancelOwner := context.WithCancel(context.Background())
				defer cancelOwner()
				ownerCtx = preflight.WithObserver(ownerCtx, func(o preflight.Observation) {
					if o.Stage == "acp_run_dir" && o.Status == "ok" {
						cancelOwner() // The other path owns the exact RunID, but never starts a child.
					}
					if o.Stage == "acp_os_start" {
						t.Fatal("owner crossed the preflight boundary")
					}
				})
				r := e.execute(ownerCtx, request, nil, prepared)
				if r.Outcome != OutcomeNotStarted || r.Started || r.Accepted {
					t.Fatalf("exclusive owner did not return a pre-start cancellation: %s", r.Outcome)
				}
				if err := os.WriteFile(marker, []byte("owner"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.stage == "" {
				reserve()
				cancel()
			}
			ctx = preflight.WithObserver(ctx, func(o preflight.Observation) {
				if o.Stage == "acp_os_start" {
					t.Fatal("duplicate crossed the preflight boundary")
				}
				if o.Stage == tc.stage && o.Status == "begin" {
					// Deterministically let the other path win as this invocation
					// approaches its own reservation, not just on sequential replay.
					reserve()
					if tc.cancel {
						cancel()
					}
				}
			})
			r := e.Execute(ctx, request, func(Event) error {
				t.Error("duplicate emitted a native event")
				return nil
			})
			if r.Outcome != OutcomeUnknown || r.Started || r.Accepted || r.Terminal != "" || r.NativeSessionID != "" ||
				r.DirectChildReaped || r.OutputComplete || r.ExitCode != -1 || r.StdoutPath != "" || r.StderrPath != "" {
				t.Fatalf("unreserved duplicate returned %s or fabricated native evidence", r.Outcome)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "owner" {
				t.Fatal("duplicate changed or removed the winning reservation")
			}
		})
	}
}

// Offline only: no Prepare/Execute, spawned child, HTTP, or real member config.
// Start with the whole constructor so this test does not prime either pin first.
func TestACPOpenCodePinnedConstructorPhases(t *testing.T) {
	if os.Getenv("AW_ACP_PREFLIGHT_TIMING") != "1" {
		t.Skip("offline timing not explicitly selected")
	}
	if !testing.Verbose() {
		t.Fatal("use -v for live phase timing output")
	}
	const (
		lock       = "ba2d7afec8f52eb0ced8ccad88076a0634ce9023a949014fdfea0d1c868cfed5"
		coldBudget = 12 * time.Minute
		warmBudget = 12 * time.Minute
	)
	image, deps := os.Getenv("AW_ACP_PINNED_OPENCODE_BINARY"), os.Getenv("AW_ACP_PREPARED_CONFIG_DIR")
	if !filepath.IsAbs(image) || !filepath.IsAbs(deps) || os.Getenv("AW_ACP_PREPARED_LOCK_SHA256") != lock {
		t.Fatal("expected explicit audited image, dependency and lock pins")
	}
	member := filepath.Join(t.TempDir(), "member.json")
	if os.WriteFile(member, []byte(`{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"fixture-offline-only"}`), 0600) != nil {
		t.Fatal("synthetic config creation failed")
	}
	c := ACPConfig{
		AgentPath: image, Version: "1.19.6", VendorProfile: ACPOpenCode119, Model: "opencode/big-pickle",
		MemberToolConfigPath: member, ToolNames: []string{"read_channel"}, OpenCode: &ACPOpenCodeProfile{ConfigDir: deps, LockSHA256: lock},
	}
	// In this offline test only, emit each sanitized line through the live
	// verbose logger rather than saving it until the inspection returns.
	trace := preflight.WithObserver(context.Background(), func(phase preflight.Observation) {
		t.Logf("stage=%s status=%s elapsed_ms=%d", phase.Stage, phase.Status, phase.Elapsed.Milliseconds())
	})
	// Run with a separate test timeout (e.g. -timeout=26m). The phase budgets
	// are cooperative: they cannot interrupt a blocked filesystem syscall.
	cold, cancelCold := context.WithTimeout(trace, coldBudget)
	defer cancelCold()
	var frozen acpExecutor
	if !preflight.Check(cold, "offline_constructor_total", func() bool {
		executor, err := NewACPExecutorContext(cold, c, 180*time.Second)
		var ok bool
		frozen, ok = executor.(acpExecutor)
		return err == nil && ok && frozen.openCodeAgentSHA == acpOpenCodeImageSHA256
	}) {
		t.Fatal("offline pinned constructor failed")
	}
	cancelCold()
	warm, cancelWarm := context.WithTimeout(trace, warmBudget)
	defer cancelWarm()
	if !preflight.Check(warm, "offline_warm_image_hash", func() bool {
		return acpOpenCodeExecutableSHA256Context(warm, frozen.agentReal, frozen.agentFile) == frozen.openCodeAgentSHA
	}) || !preflight.Check(warm, "offline_warm_dependencies", func() bool {
		digest, err := acpOpenCodeDependenciesContext(warm, *c.OpenCode)
		return err == nil && digest == frozen.openCodeDigest
	}) {
		t.Fatal("offline warm pin recheck failed")
	}
}
