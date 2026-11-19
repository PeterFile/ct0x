package provider

import (
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	LookupProjectExperiencesTool = "lookup_project_experiences"
	ReadArtifactTool             = "read_artifact"
)

// PiProjectExperienceToolNames projects exactly a fresh ordinary history
// profile (or its read-only subset) plus the new native reads. The caller
// supplies actual issued operations; this function attests no authority.
func PiProjectExperienceToolNames(operations []string) ([]string, error) {
	base := slices.DeleteFunc(slices.Clone(operations), func(name string) bool { return name == ReadArtifactTool })
	artifact := len(operations) - len(base)
	names, err := canonicalMemberToolProfile(base, false)
	if err != nil || artifact > 1 {
		return nil, errPiMemberToolConfiguration
	}
	switch strings.Join(names, ",") {
	case "search_history,read_history", "list_members,read_channel,send_message,search_history,read_history", piMemberToolNames + ",search_history,read_history":
	default:
		return nil, errPiMemberToolConfiguration
	}
	names = append(names, LookupProjectExperiencesTool)
	if artifact == 1 {
		names = append(names, ReadArtifactTool)
	}
	return names, nil
}

func validPiProjectExperienceToolNames(names []string) bool {
	i := slices.Index(names, LookupProjectExperiencesTool)
	if i < 0 {
		return false
	}
	operations := slices.Concat(names[:i], names[i+1:])
	want, err := PiProjectExperienceToolNames(operations)
	return err == nil && slices.Equal(want, names)
}

// NewPiProjectExperienceExecutor selects the pinned fresh Pi profile only.
// The exact argv list comes from the issued, digest-bound file, not model
// arguments or a legacy/native-resume tool allowlist.
func NewPiProjectExperienceExecutor(selected Provider, extension string, issued NativeMemberToolConfig, timeout time.Duration) (Executor, error) {
	bridge, path := qualifyPiProjectExperienceBridge(extension), filepath.Clean(issued.Path)
	config, err := readPiProjectExperienceConfig(path)
	if bridge == "" || err != nil || config.digest != issued.SHA256 {
		return nil, errPiMemberToolConfiguration
	}
	base := config.projectExperienceTools[:slices.Index(config.projectExperienceTools, LookupProjectExperiencesTool)]
	native, err := newPiMemberToolExecutor(selected, bridge, path, base, timeout, false)
	if err != nil {
		return nil, err
	}
	native.memberTools.toolNames = config.projectExperienceTools
	native.memberTools.configSHA256 = issued.SHA256
	native.memberTools.projectExperience = true
	return piDigestBoundExecutor{piExecutor: native, configuredBridge: extension}, nil
}
