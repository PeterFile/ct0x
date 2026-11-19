package provider

import (
	"path/filepath"
	"slices"
	"time"
)

const (
	ReadInvestigationChannelTool  = "read_investigation_channel"
	ReadInvestigationArtifactTool = "read_investigation_artifact"
)

// A strict profile is not a subset or extension of ordinary role tools. Only
// the selected Pi entry point accepts it; the Codex and native-resume adapters
// continue validating their legacy profiles.
func piInvestigationProfile(names []string) ([]string, error) {
	var profile []string
	for _, read := range []string{ReadInvestigationChannelTool, ReadInvestigationArtifactTool} {
		if slices.Contains(names, read) {
			profile = append(profile, read)
		}
	}
	decide, submit := slices.Contains(names, "decide_dispatch"), slices.Contains(names, "submit_result")
	if len(profile) == 0 || decide == submit || len(names) != len(profile)+1 {
		return nil, errPiMemberToolConfiguration
	}
	if decide {
		return append(profile, "decide_dispatch"), nil
	}
	return append(profile, "submit_result"), nil
}

type piInvestigationAdapter struct {
	extension string
	config    NativeMemberToolConfig
	names     []string
}

// NewPiInvestigationAdapter selects the dedicated fresh tools-only profile.
// A zero config is for sampling only. The registry, not the digest, grants tools.
func NewPiInvestigationAdapter(extension string, config NativeMemberToolConfig, names []string) Adapter {
	return piInvestigationAdapter{extension, config, slices.Clone(names)}
}

func (a piInvestigationAdapter) Capabilities(selected Provider) (AdapterCapabilities, error) {
	if _, err := piInvestigationProfile(a.names); err != nil || !PiInvestigationBridgeSupported(a.extension) {
		return AdapterCapabilities{}, errPiMemberToolConfiguration
	}
	return NewPiMemberToolAdapter(a.extension, "", []string{"read_channel"}).Capabilities(selected)
}

func (a piInvestigationAdapter) NewExecutor(selected Provider, timeout time.Duration) (Executor, error) {
	if _, err := a.Capabilities(selected); err != nil {
		return nil, err
	}
	names, _ := piInvestigationProfile(a.names)
	path := filepath.Clean(a.config.Path)
	config, err := readPiMemberToolConfig(path, true)
	if err != nil || config.digest != a.config.SHA256 || !slices.Equal(config.investigationTools, names) {
		return nil, errPiMemberToolConfiguration
	}
	entry := qualifyPiInvestigationBridge(a.extension)
	native, err := newPiMemberToolExecutor(selected, entry, path, names, timeout, true)
	if err != nil {
		return nil, err
	}
	native.memberTools.configSHA256 = a.config.SHA256
	return piDigestBoundExecutor{piExecutor: native, configuredBridge: a.extension, investigationBridge: true}, nil
}

// NewPiInvestigationIntakeExecutor binds the fresh intake tool set to the
// qualified Pi bridge and issued config. It never selects a native session.
// The issued schema can offer discussion_only_excerpt without the investigation action.
func NewPiInvestigationIntakeExecutor(selected Provider, extension string, issued NativeMemberToolConfig, timeout time.Duration) (Executor, error) {
	bridge, path := qualifyPiInvestigationBridge(extension), filepath.Clean(issued.Path)
	if bridge == "" || !piMemberToolConfigMatches(path, issued.SHA256, false) {
		return nil, errPiMemberToolConfiguration
	}
	names := []string{"list_members", "read_channel", "send_message", "search_history", "read_history"}
	native, err := newPiMemberToolExecutor(selected, bridge, path, names, timeout, false)
	if err != nil {
		return nil, err
	}
	native.memberTools.configSHA256 = issued.SHA256
	return piDigestBoundExecutor{piExecutor: native, configuredBridge: extension, investigationBridge: true}, nil
}
