package provider

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPiFreshTaskWorkExactPairAndBindingWithoutLaunch(t *testing.T) {
	root := t.TempDir()
	src, err := filepath.Abs("../../extensions/pi-bridge")
	if err != nil {
		t.Fatal(err)
	}
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"fresh-task-work.ts", "fresh-task-work.mjs", "index.ts", "bridge.mjs"} {
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(root, name), data, 0600)
	}
	entry, impl := filepath.Join(root, "fresh-task-work.ts"), filepath.Join(root, "fresh-task-work.mjs")
	path, config := filepath.Join(root, "never-launch"), filepath.Join(root, "config.json")
	write(path, nil, 0700) // Metadata only, no CLI or shell script.
	const body = `{"endpoint":"http://127.0.0.1:1/api/v1/member-tools","token":"synthetic-capability"}` + "\n"
	write(config, []byte(body), 0600)

	selected := Provider{ID: "metadata-only", Installation: Installed, Path: path, Name: Pi, Version: "1.4.0"}
	a := NewPiFreshTaskWorkAdapter(entry, 0)
	if !a.Supports(selected) || NewPiFreshTaskWorkAdapter(filepath.Join(root, "index.ts"), 0).Supports(selected) {
		t.Fatal("ordinary and typed entry points were not kept separate")
	}
	b := FreshTaskWorkBinding{ConfigPath: config, ConfigSHA256: sha256.Sum256([]byte(body)), RunID: "fixture-turn", Operations: PiFreshTaskWorkToolNames()}
	ctx := context.Background()
	for _, edit := range []func(*FreshTaskWorkBinding){
		func(v *FreshTaskWorkBinding) { v.ConfigSHA256 = [32]byte{} },
		func(v *FreshTaskWorkBinding) { v.RunID = "bad/run" },
		func(v *FreshTaskWorkBinding) { v.Operations = []string{"submit_result"} },
		func(v *FreshTaskWorkBinding) { v.Operations = append(slices.Clone(v.Operations), "report_work") },
	} {
		changed := b
		edit(&changed)
		if e, err := a.NewExecutor(ctx, selected, changed); e != nil || err == nil {
			t.Fatal("invalid binding selected an executor")
		}
	}
	e, err := a.NewExecutor(ctx, selected, b)
	if err != nil {
		t.Fatal(err)
	}
	fresh := e.(*piFreshTaskWorkExecutor)
	opts := fresh.pi.memberTools
	inherited := []string{"KEEP=yes", piMemberToolConfigEnv + "=stale", piMemberToolConfigSHAEnv + "=stale"}
	want := []string{"KEEP=yes", piMemberToolConfigEnv + "=" + config, fmt.Sprintf("%s=%x", piMemberToolConfigSHAEnv, b.ConfigSHA256)}
	if !slices.Equal(opts.environment(inherited), want) || opts.extensionPath != a.qualifiedBridge() || !slices.Equal(opts.toolNames, b.Operations) {
		t.Fatal("wrong native bootstrap or tool allowlist")
	}
	write(config, []byte(body+" "), 0600)
	if got := e.Execute(ctx, ExecuteRequest{RunID: b.RunID}, nil); got.Started || got.Outcome != OutcomeUnknown {
		t.Fatal("changed config was not stopped before launch")
	}
	write(config, []byte(body), 0600)
	if got := e.Execute(ctx, ExecuteRequest{RunID: b.RunID}, nil); got.Started || got.Outcome != OutcomeUnknown {
		t.Fatal("unknown launch could be called again")
	}
	ordinary, err := os.ReadFile(filepath.Join(root, "bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	write(impl, ordinary, 0600)
	if a.Supports(selected) {
		t.Fatal("mixed ordinary/typed bytes qualified")
	}
}
