package provider

import (
	"os"
	"slices"
	"testing"
)

func TestMemberWorkInputsRequireCompleteWorkerProfile(t *testing.T) {
	pi, extension, config := memberToolFiles(t, `: > "$0.invoked"; exit 99`)
	codex := pi
	codex.Name, codex.Version = Codex, "0.166.0"
	base := []string{"read_channel", "search_history", "read_history", "submit_result"}
	extra := []string{"read_work_inputs", "ack_work_input", "hand_back_work_input"}
	for _, names := range [][]string{append(slices.Clone(base), extra...), append(slices.Clone(base), extra[2])} {
		reversed := slices.Clone(names)
		slices.Reverse(reversed)
		if executor, err := NewPiMemberToolAdapter(extension, config, reversed).NewExecutor(pi, 0); err != nil || !slices.Equal(executor.(piExecutor).memberTools.toolNames, names) {
			t.Errorf("Pi rejected or changed an issued work-input profile: %T %v", executor, err)
		}
		if executor, err := NewCodexAppServerMemberToolAdapter(config, reversed).NewExecutor(codex, 0); err != nil || !slices.Equal(executor.(codexAppServerMemberToolExecutor).names, names) {
			t.Errorf("Codex rejected or changed an issued work-input profile: %T %v", executor, err)
		}
	}
	workspace := append(WorkspaceWorkerToolNames(), extra...)
	if executor, err := NewPiWorkspaceMemberToolAdapter(extension, config, extra...).NewExecutor(pi, 0); err != nil || !slices.Equal(executor.(piExecutor).memberTools.toolNames, workspace) {
		t.Errorf("Pi rejected or changed the file worker's work-input profile: %T %v", executor, err)
	}
	if _, err := NewCodexAppServerMemberToolAdapter(config, workspace).Capabilities(codex); err == nil {
		t.Error("Codex work inputs opened the file-worker profile")
	}
	for _, names := range [][]string{
		extra, {"read_channel", "read_work_inputs"}, {"read_channel", "submit_result", "read_work_inputs"},
		{"list_members", "read_channel", "search_history", "read_history", "decide_dispatch", "ack_work_input"},
		append(slices.Clone(base), "ack_work_input", "ack_work_input"),
		append(slices.Clone(base), "read_work_inputs", "read_artifact"),
		append(slices.Clone(base), "read_work_inputs", "load_skill"),
		append(slices.Clone(base), "read_work_inputs", "bash"),
	} {
		if _, err := NewPiMemberToolAdapter(extension, config, names).Capabilities(pi); err == nil {
			t.Errorf("Pi accepted partial or mixed scope: %v", names)
		}
		if _, err := NewCodexAppServerMemberToolAdapter(config, names).Capabilities(codex); err == nil {
			t.Errorf("Codex accepted partial or mixed scope: %v", names)
		}
	}
	if _, err := os.Stat(pi.Path + ".invoked"); !os.IsNotExist(err) {
		t.Fatal("constructing a work profile invoked the fixture executable")
	}
}
