package provider

import (
	"slices"
	"strings"
)

// The text adapter never accepts directory tools, even with work inputs.
func memberToolProfile(names []string) ([]string, error) {
	return canonicalMemberToolProfile(names, false)
}

func canonicalMemberToolProfile(names []string, allowWorkspace bool) ([]string, error) {
	canonical := strings.Split(piMemberToolNames+",search_history,read_history", ",")
	all, worker := false, false
	switch {
	case allowWorkspace && slices.Contains(names, "workspace_write"):
		canonical, all, worker = WorkspaceWorkerToolNames(), true, true
	case slices.Contains(names, "decide_dispatch"):
		canonical, all = []string{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch"}, true
	case slices.Contains(names, "submit_result"):
		canonical, all, worker = []string{"read_channel", "search_history", "read_history", "submit_result"}, true, true
	}
	if worker {
		for _, name := range []string{"read_work_inputs", "ack_work_input", "hand_back_work_input"} {
			if slices.Contains(names, name) {
				canonical = append(canonical, name)
			}
		}
	}
	if len(names) == 0 || all && len(names) != len(canonical) {
		return nil, errPiMemberToolConfiguration
	}
	for i, name := range names {
		if !slices.Contains(canonical, name) || slices.Contains(names[:i], name) {
			return nil, errPiMemberToolConfiguration
		}
	}
	selected := make([]string, 0, len(names))
	for _, name := range canonical {
		if slices.Contains(names, name) {
			selected = append(selected, name)
		}
	}
	return selected, nil
}
