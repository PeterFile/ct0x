package platform

import (
	"context"
	"slices"
	"testing"

	"agentworkspace/internal/provider"
)

func TestMemberToolRuntimeHistoryNamesAndCreationBinding(t *testing.T) {
	f := setupMemberToolRuntime(t)
	for _, names := range [][]string{{"search_history"}, {"read_history"}, {"read_history", "list_members", "search_history"}, historyProfileNames} {
		options := f.options
		options.Operations = slices.Clone(names)
		runtime, err := NewMemberToolRuntime(f.db, options)
		if err != nil {
			t.Errorf("exact supported history names rejected: %v", err)
			continue
		}
		options.Operations[0] = "create_task"
		if !slices.Equal(runtime.operations, names) {
			t.Error("runtime profile did not snapshot caller names")
		}
		factory := func(string, string, []string) (provider.Executor, error) {
			t.Error("Wrap must not invoke factory")
			return nil, nil
		}
		binding := f.binding
		binding.ProviderID, binding.Model = "", ""
		_, err = runtime.Wrap(binding, factory)
		if slices.Contains(names, "create_member") {
			if err == nil {
				t.Error("six profile relaxed create_member provider/model requirements")
			}
		} else if err != nil {
			t.Error("read-only history profile incorrectly requires creation selection")
		}
	}
	for _, names := range [][]string{{"search_history", "search_history"}, {"read_history", "read_history"},
		{"search_history "}, {" read_history"}, {"Search_history"}, {"READ_HISTORY"}, {"search_history", "create_task"}} {
		options := f.options
		options.Operations = names
		if runtime, err := NewMemberToolRuntime(f.db, options); runtime != nil || err != errMemberToolRuntimeConfig {
			t.Errorf("invalid exact-name policy accepted: %v", err)
		}
	}
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
	memberToolRuntimeEmpty(t, f.request.RuntimeDir)
}

func TestMemberToolRuntimeHistoryFactoryReceivesIndependentSnapshot(t *testing.T) {
	f := setupMemberToolRuntime(t)
	options := f.options
	options.Operations = slices.Clone(historyProfileNames)
	var err error
	f.runtime, err = NewMemberToolRuntime(f.db, options)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	executor := f.wrap(t, func(path, runID string, names []string) (provider.Executor, error) {
		called = true
		if runID != f.claim.TurnID || !slices.Equal(names, historyProfileNames) {
			t.Error("factory received wrong run/profile")
		}
		names[0] = "create_task"
		return memberToolRuntimeExecutorFunc(func(context.Context, provider.ExecuteRequest, func(provider.Event) error) provider.ExecutionReceipt {
			if !slices.Equal(f.runtime.operations, historyProfileNames) {
				t.Error("factory mutated runtime policy")
			}
			return provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}
		}), nil
	})
	executor.Execute(context.Background(), f.request, nil)
	if !called {
		t.Fatal("claimed history policy did not reach factory")
	}
	memberToolRuntimeEmpty(t, f.options.ConfigRoot)
}
