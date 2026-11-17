package provider

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Protocol identifies an integration, not an enabled feature or an authorization.
type Protocol string

const (
	OneShotProtocol        Protocol = "one_shot"
	PiExtensionProtocol    Protocol = "pi_extension"
	CodexAppServerProtocol Protocol = "codex_app_server"
	PiOneShotAdapterID              = "pi_one_shot"
	CodexOneShotAdapterID           = "codex_one_shot"
)

type Support string

const (
	SupportUnknown     Support = "unknown"
	SupportUnsupported Support = "unsupported"
	SupportSupported   Support = "supported"
)

type Capability string

const (
	CapabilityPlatformTools         Capability = "platform_tools"
	CapabilityNativeResume          Capability = "native_resume"
	CapabilityPermissionInteraction Capability = "permission_interaction"
	CapabilityCancel                Capability = "cancel"
	CapabilityTerminalEvidence      Capability = "terminal_evidence"
	CapabilitySteer                 Capability = "steer"
)

// BuiltinContractEvidence describes implementation support only. It proves no
// native handshake, model request, installation, account or connectivity state.
const BuiltinContractEvidence = "builtin_contract"

// CapabilityEvidence binds a report to trusted selection metadata, not file
// contents. The host must revalidate mutable paths and keep executors immutable.
// Provider installation/account/connectivity evidence remains independent.
type CapabilityEvidence struct {
	Source     string   `json:"source"`
	ProviderID string   `json:"provider_id"`
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Version    string   `json:"version"`
	Protocol   Protocol `json:"protocol"`
}

// AdapterCapabilities is a value-only report, separate from legacy Capabilities.
// Empty/unknown/unrecognized support never satisfies a required capability.
// Cancel means acknowledged native cancellation, not killing a direct child.
// TerminalEvidence means native turn evidence, not descendant shutdown or
// permission to release resource claims. PlatformTools excludes CLI read tools.
type AdapterCapabilities struct {
	PlatformTools         Support            `json:"platform_tools"`
	NativeResume          Support            `json:"native_resume"`
	PermissionInteraction Support            `json:"permission_interaction"`
	Cancel                Support            `json:"cancel"`
	TerminalEvidence      Support            `json:"terminal_evidence"`
	Steer                 Support            `json:"steer"`
	Evidence              CapabilityEvidence `json:"evidence"`
}

func (c AdapterCapabilities) Supports(required Capability) bool {
	var state Support
	switch required {
	case CapabilityPlatformTools:
		state = c.PlatformTools
	case CapabilityNativeResume:
		state = c.NativeResume
	case CapabilityPermissionInteraction:
		state = c.PermissionInteraction
	case CapabilityCancel:
		state = c.Cancel
	case CapabilityTerminalEvidence:
		state = c.TerminalEvidence
	case CapabilitySteer:
		state = c.Steer
	}
	return state == SupportSupported
}

// Adapter is implemented by trusted host code, not dynamically loaded plugins.
// Capabilities must reject unsupported provider versions without constructing an
// executor or probing a CLI. Implementations must be safe for concurrent reads.
// Session creation/resume/cancel stay inside concrete executors; Execute remains
// the only scheduling entry point. Neither operation grants tool authority.
type Adapter interface {
	Capabilities(Provider) (AdapterCapabilities, error)
	NewExecutor(Provider, time.Duration) (Executor, error)
}

// AdapterSelection is trusted host configuration, not model-controlled identity.
// ID identifies the adapter, never Provider.ID, a caller, group or grant.
type AdapterSelection struct {
	ID       string
	Protocol Protocol
}

type AdapterRegistration struct {
	ID       string
	Protocol Protocol
	Adapter  Adapter
}

var ErrAdapterConfiguration = errors.New("适配器配置无效或所需能力未经确认")

// AdapterRegistry is immutable after construction. It never exposes adapters or
// stores execution sessions, credentials, grants or mutable capability catalogs.
type AdapterRegistry struct {
	entries map[string]AdapterRegistration
}

func NewAdapterRegistry(entries ...AdapterRegistration) (*AdapterRegistry, error) {
	r := &AdapterRegistry{entries: make(map[string]AdapterRegistration, len(entries))}
	for _, entry := range entries {
		if entry.ID == "" || strings.TrimSpace(entry.ID) != entry.ID || strings.ContainsRune(entry.ID, 0) ||
			!knownProtocol(entry.Protocol) || nilImplementation(entry.Adapter) {
			return nil, ErrAdapterConfiguration
		}
		if _, exists := r.entries[entry.ID]; exists {
			return nil, ErrAdapterConfiguration
		}
		r.entries[entry.ID] = entry
	}
	return r, nil
}

// DefaultAdapterRegistry enables only the existing tool-disabled Pi baseline
// and Codex read-only one-shot baseline. Native integrations require explicit
// future host registration; protocol constants do not enable implementations.
func DefaultAdapterRegistry() *AdapterRegistry {
	return &AdapterRegistry{entries: map[string]AdapterRegistration{
		PiOneShotAdapterID:    {ID: PiOneShotAdapterID, Protocol: OneShotProtocol, Adapter: oneShotAdapter{Pi}},
		CodexOneShotAdapterID: {ID: CodexOneShotAdapterID, Protocol: OneShotProtocol, Adapter: oneShotAdapter{Codex}},
	}}
}

func (r *AdapterRegistry) Capabilities(s AdapterSelection, p Provider) (AdapterCapabilities, error) {
	if r == nil || strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" || p.Version == "" ||
		p.Installation != Installed || !filepath.IsAbs(p.Path) || strings.ContainsRune(p.Path, 0) {
		return AdapterCapabilities{}, ErrAdapterConfiguration
	}
	entry, exists := r.entries[s.ID]
	if !exists || s.Protocol != entry.Protocol {
		return AdapterCapabilities{}, ErrAdapterConfiguration
	}
	c, err := entry.Adapter.Capabilities(p)
	if err != nil {
		return AdapterCapabilities{}, err
	}
	e := c.Evidence
	if e.Source != BuiltinContractEvidence || e.ProviderID != p.ID || e.Name != p.Name ||
		e.Path != p.Path || e.Version != p.Version || e.Protocol != s.Protocol {
		return AdapterCapabilities{}, ErrAdapterConfiguration
	}
	return c, nil
}

// NewExecutor selects only explicitly supported requirements, then constructs
// without launching the CLI. Construction must revalidate the executable path.
func (r *AdapterRegistry) NewExecutor(s AdapterSelection, p Provider, timeout time.Duration, required ...Capability) (Executor, error) {
	if timeout < 0 {
		return nil, ErrAdapterConfiguration
	}
	c, err := r.Capabilities(s, p)
	if err != nil {
		return nil, err
	}
	for _, capability := range required {
		if !c.Supports(capability) {
			return nil, ErrAdapterConfiguration
		}
	}
	e, err := r.entries[s.ID].Adapter.NewExecutor(p, timeout)
	if err != nil {
		return nil, err
	}
	if nilImplementation(e) {
		return nil, ErrAdapterConfiguration
	}
	return e, nil
}

func knownProtocol(p Protocol) bool {
	return p == OneShotProtocol || p == PiExtensionProtocol || p == CodexAppServerProtocol
}

// Reject typed nil implementations as well as nil interfaces before calling them.
func nilImplementation(implementation any) bool {
	if implementation == nil {
		return true
	}
	v := reflect.ValueOf(implementation)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

type oneShotAdapter struct{ name string }

func (a oneShotAdapter) Capabilities(p Provider) (AdapterCapabilities, error) {
	if p.Name != a.name {
		return AdapterCapabilities{}, errExecutorConfiguration
	}
	if err := ValidateExecutor(p, 0); err != nil {
		return AdapterCapabilities{}, err
	}
	return AdapterCapabilities{
		PlatformTools: SupportUnsupported, NativeResume: SupportUnsupported,
		PermissionInteraction: SupportUnsupported, Cancel: SupportUnsupported,
		TerminalEvidence: SupportSupported, Steer: SupportUnsupported,
		Evidence: CapabilityEvidence{Source: BuiltinContractEvidence, ProviderID: p.ID,
			Name: p.Name, Path: p.Path, Version: p.Version, Protocol: OneShotProtocol},
	}, nil
}

func (a oneShotAdapter) NewExecutor(p Provider, timeout time.Duration) (Executor, error) {
	if p.Name != a.name {
		return nil, errExecutorConfiguration
	}
	return NewExecutor(p, timeout)
}
