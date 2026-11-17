package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func adapterProvider(t *testing.T) Provider {
	t.Helper()
	path := executable(t, filepath.Join(t.TempDir(), "selected-cli"), "echo invoked > \"$0.invoked\"\n"+executionHeader+executionEmit(executionStart, executionAssistant("stop"), executionSettled))
	return Provider{ID: "installation-id", Name: Pi, Path: path, Version: "1.4.0", Installation: Installed,
		Account: Authenticated, Connectivity: "connected", Capabilities: Capabilities{OneShot: true, Resume: true, Tools: true}}
}

// Inject invalid reports at the registry boundary; construction uses the real factory.
type reportAdapter struct {
	report AdapterCapabilities
	builds int
}

func (a *reportAdapter) Capabilities(Provider) (AdapterCapabilities, error) { return a.report, nil }
func (a *reportAdapter) NewExecutor(p Provider, timeout time.Duration) (Executor, error) {
	a.builds++
	return NewExecutor(p, timeout)
}

func TestAdapterRegistryDefaultContractsAndLegacyFlags(t *testing.T) {
	p := adapterProvider(t)
	r := DefaultAdapterRegistry()
	for _, tc := range []struct{ id, name, version string }{
		{PiOneShotAdapterID, Pi, "1.4.0"}, {CodexOneShotAdapterID, Codex, "0.162.1"}, {CodexOneShotAdapterID, Codex, "0.166.0"},
	} {
		t.Run(tc.name+tc.version, func(t *testing.T) {
			p.Name, p.Version = tc.name, tc.version
			p.Account, p.Connectivity = Authenticated, "connected"
			p.Capabilities = Capabilities{OneShot: true, Resume: true, Tools: true}
			s := AdapterSelection{ID: tc.id, Protocol: OneShotProtocol}
			c, err := r.Capabilities(s, p)
			if err != nil || c.Evidence.Source != BuiltinContractEvidence || c.Evidence.Path != p.Path || c.Evidence.Version != p.Version || c.Evidence.Protocol != s.Protocol || c.Evidence.ProviderID != p.ID || c.Evidence.Name != p.Name {
				t.Fatalf("missing exact implementation-contract binding: %+v, %v", c, err)
			}
			if !c.Supports(CapabilityTerminalEvidence) {
				t.Fatal("existing native terminal parser was not described")
			}
			for _, cap := range []Capability{CapabilityPlatformTools, CapabilityNativeResume, CapabilityPermissionInteraction, CapabilityCancel, CapabilitySteer, "future"} {
				if c.Supports(cap) {
					t.Fatalf("legacy flags/account upgraded capability %q", cap)
				}
				if e, err := r.NewExecutor(s, p, 0, cap); e != nil || err == nil {
					t.Fatalf("unavailable capability %q selected an executor", cap)
				}
			}
			p.Account, p.Connectivity, p.Capabilities = AccountUnknown, Unchecked, Capabilities{}
			if err := ValidateExecutor(p, 0); err != nil {
				t.Fatal(err)
			}
			if e, err := NewExecutor(p, 0); e == nil || err != nil {
				t.Fatalf("legacy baseline changed: %v", err)
			}
			if e, err := r.NewExecutor(s, p, 0, CapabilityTerminalEvidence); e == nil || err != nil {
				t.Fatalf("supported baseline rejected: %v", err)
			}
		})
	}
	if _, err := os.Stat(p.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("capability lookup or construction launched the executable")
	}
}

func TestAdapterRegistryRejectsInvalidRegistrations(t *testing.T) {
	a := &reportAdapter{}
	valid := AdapterRegistration{ID: "explicit", Protocol: OneShotProtocol, Adapter: a}
	var typedNil *reportAdapter
	for _, entries := range [][]AdapterRegistration{
		{valid, valid}, {{ID: " ", Protocol: OneShotProtocol, Adapter: a}},
		{{ID: "explicit", Protocol: OneShotProtocol}}, {{ID: "explicit", Protocol: OneShotProtocol, Adapter: typedNil}},
		{{ID: "explicit", Protocol: "future", Adapter: a}}, {{ID: "explicit", Adapter: a}},
	} {
		if r, err := NewAdapterRegistry(entries...); r != nil || !errors.Is(err, ErrAdapterConfiguration) {
			t.Fatalf("invalid registration accepted: %+v, %v", entries, err)
		}
	}
}

func TestAdapterRegistryUnknownSelectionsAndNativeDefaultsStayClosed(t *testing.T) {
	p := adapterProvider(t)
	for _, r := range []*AdapterRegistry{nil, {}, DefaultAdapterRegistry()} {
		for _, s := range []AdapterSelection{
			{}, {ID: "missing", Protocol: OneShotProtocol}, {ID: PiOneShotAdapterID},
			{ID: PiOneShotAdapterID, Protocol: "future"}, {ID: PiOneShotAdapterID, Protocol: PiExtensionProtocol},
			{ID: CodexOneShotAdapterID, Protocol: CodexAppServerProtocol},
			{ID: "pi_extension", Protocol: PiExtensionProtocol}, {ID: "codex_app_server", Protocol: CodexAppServerProtocol},
		} {
			if _, err := r.Capabilities(s, p); !errors.Is(err, ErrAdapterConfiguration) {
				t.Fatalf("unknown selection reported capabilities: %+v, %v", s, err)
			}
			if e, err := r.NewExecutor(s, p, 0); e != nil || !errors.Is(err, ErrAdapterConfiguration) {
				t.Fatalf("unknown selection fell back: %+v, %v", s, err)
			}
		}
	}
	for _, change := range []func(*Provider){
		func(p *Provider) { p.Version = "1.4.1" }, func(p *Provider) { p.Version = "" },
		func(p *Provider) { p.Name = Codex }, func(p *Provider) { p.ID = "" },
		func(p *Provider) { p.Installation = "" }, func(p *Provider) { p.Path = "relative" },
	} {
		q := p
		change(&q)
		s := AdapterSelection{ID: PiOneShotAdapterID, Protocol: OneShotProtocol}
		if e, err := DefaultAdapterRegistry().NewExecutor(s, q, 0); e != nil || err == nil {
			t.Fatal("unknown or insufficient provider metadata selected an executor")
		}
	}
}

func TestAdapterRegistryRequiresMatchingCapabilityEvidence(t *testing.T) {
	p := adapterProvider(t)
	s := AdapterSelection{ID: PiOneShotAdapterID, Protocol: OneShotProtocol}
	base, err := DefaultAdapterRegistry().Capabilities(s, p)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*CapabilityEvidence){
		"missing":                         func(e *CapabilityEvidence) { *e = CapabilityEvidence{} },
		"path":                            func(e *CapabilityEvidence) { e.Path += ".other" },
		"version":                         func(e *CapabilityEvidence) { e.Version = "0.166.0" },
		"protocol":                        func(e *CapabilityEvidence) { e.Protocol = PiExtensionProtocol },
		"identity":                        func(e *CapabilityEvidence) { e.ProviderID = "other" },
		"name":                            func(e *CapabilityEvidence) { e.Name = Codex },
		"account is not capability proof": func(e *CapabilityEvidence) { e.Source = "account" },
		"unperformed handshake":           func(e *CapabilityEvidence) { e.Source = "native_handshake" },
	} {
		t.Run(name, func(t *testing.T) {
			a := &reportAdapter{report: base}
			change(&a.report.Evidence)
			r, err := NewAdapterRegistry(AdapterRegistration{ID: s.ID, Protocol: s.Protocol, Adapter: a})
			if err != nil {
				t.Fatal(err)
			}
			if c, err := r.Capabilities(s, p); err == nil || c.Supports(CapabilityTerminalEvidence) {
				t.Fatal("mismatched evidence escaped as usable capabilities")
			}
			if e, err := r.NewExecutor(s, p, 0, CapabilityTerminalEvidence); e != nil || err == nil || a.builds != 0 {
				t.Fatal("invalid evidence reached executor construction")
			}
		})
	}
}

func TestAdapterRegistryUnknownCapabilityNeverReachesFactory(t *testing.T) {
	p := adapterProvider(t)
	s := AdapterSelection{ID: PiOneShotAdapterID, Protocol: OneShotProtocol}
	base, err := DefaultAdapterRegistry().Capabilities(s, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []Support{"", SupportUnknown, SupportUnsupported, "future", SupportSupported} {
		a := &reportAdapter{report: base}
		a.report.PlatformTools = state
		entries := []AdapterRegistration{{ID: s.ID, Protocol: s.Protocol, Adapter: a}}
		r, err := NewAdapterRegistry(entries...)
		if err != nil {
			t.Fatal(err)
		}
		entries[0].ID = "mutated"
		c, err := r.Capabilities(s, p)
		if err != nil {
			t.Fatal(err)
		}
		c.PlatformTools = SupportSupported // A returned value cannot mutate registry reports.
		e, err := r.NewExecutor(s, p, 0, CapabilityPlatformTools)
		if state == SupportSupported {
			if e == nil || err != nil || a.builds != 1 {
				t.Fatal("explicit trusted support could not construct existing executor")
			}
		} else if e != nil || !errors.Is(err, ErrAdapterConfiguration) || a.builds != 0 {
			t.Fatalf("state %q participated in selection", state)
		}
	}
}

func TestAdapterRegistryExecutesExistingBaselineAndRevalidatesPath(t *testing.T) {
	p := adapterProvider(t)
	r := DefaultAdapterRegistry()
	s := AdapterSelection{ID: PiOneShotAdapterID, Protocol: OneShotProtocol}
	e, err := r.NewExecutor(s, p, time.Second, CapabilityTerminalEvidence)
	if err != nil {
		t.Fatal(err)
	}
	req := ExecuteRequest{RunID: "run1", CWD: t.TempDir(), RuntimeDir: t.TempDir(), Prompt: "fixture only"}
	if err := os.Chmod(req.RuntimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if receipt := e.Execute(context.Background(), req, nil); receipt.Outcome != OutcomeSucceeded || receipt.Terminal != OutcomeSucceeded {
		t.Fatalf("registry did not produce the existing executable adapter: %+v", receipt)
	}
	if _, err := r.Capabilities(s, p); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.Path); err != nil {
		t.Fatal(err)
	}
	if e, err := r.NewExecutor(s, p, 0); e != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capability read cached a stale installation or lost the actual stat failure")
	}
}
