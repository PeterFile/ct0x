package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
)

func TestRunProjectExperienceNativeOrdinary(t *testing.T) {
	fixture, dir := memberToolsFixture(t), t.TempDir()
	launcher, err := os.ReadFile(fixture.path)
	artifactCmdMust(t, err)
	launcher = bytes.Replace(launcher, []byte("^TestMemberToolsNativeHelper$"), []byte("^TestProjectExperienceNativeHelper$"), 1)
	artifactCmdMust(t, os.WriteFile(fixture.path, launcher, 0700))
	entry, err := filepath.Abs("../../extensions/pi-bridge/index.ts")
	artifactCmdMust(t, err)
	if !provider.PiProjectExperienceBridgeSupported(entry) {
		t.Fatal("checkout does not match the qualified project-experience bridge")
	}
	base, stop := startRunWithSearch(t, provider.Search{}, "--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", fixture.path,
		"--enable-execution", "--enable-member-tools", "--pi-member-tools-extension", entry, "--enable-pi-project-experience")
	selected := executionProvider(t, base)
	h := projectExperienceHTTP{base: base, client: experienceHTTPClient(t)}
	var group domain.Group
	h.request(t, "POST", "/api/v1/groups", platform.GroupInput{Name: "E3 synthetic group"}, "", 201, &group)
	groupPath := "/api/v1/groups/" + group.ID
	var member domain.Member
	h.request(t, "POST", groupPath+"/members", platform.MemberInput{Name: "Fresh member", Role: "Check sources", ProviderID: selected.ID, Model: "fixture-model"}, "", 201, &member)
	source := experienceHTTPMessage(t, h, groupPath, "export-bug", "必须兼容旧版 CSV")
	var choice domain.ProjectExperience
	if experienceSchemaInstalled(t, dir) {
		h.request(t, "POST", groupPath+projectExperienceRoute, experienceHTTPSelection(source, "v2.9"), "", 201, &choice)
		if choice.ID == "" {
			t.Fatal("installed profile did not persist the chosen experience")
		}
	}
	memberToolsWriteJSON(t, filepath.Join(fixture.root, "experience.json"), choice.ID)
	var trigger domain.Message
	h.request(t, "POST", groupPath+"/messages", platform.MessageInput{TopicID: source.TopicID, Content: "跟进那个导出 BUG", RecipientIDs: []string{member.ID}}, "", 201, &trigger)
	if len(trigger.Deliveries) != 1 {
		t.Fatal("fixture requires one ordinary targeted wake")
	}
	// The fixture includes a fresh SQLite database, scheduler, signed config,
	// child process and real HTTP. Allow for slow private disks without
	// changing any production deadline or hiding a terminal child failure.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	raw := memberToolsHistoryDB(t, dir)
	released := false
	for {
		var runID, candidate, reason string
		var state domain.TurnState
		err := raw.QueryRowContext(ctx, "SELECT id,state,candidate_terminal,reason FROM turns WHERE delivery_id=?", trigger.Deliveries[0].ID).
			Scan(&runID, &state, &candidate, &reason)
		if err == nil && candidate == string(provider.OutcomeSucceeded) && !released {
			// Let the synthetic child exit only after the runtime has drained
			// and recorded its last frame; slow fixture disks can otherwise hit
			// the production child pipe WaitDelay.
			artifactCmdMust(t, os.WriteFile(filepath.Join(fixture.root, "release"), nil, 0600))
			released = true
		}
		if state == domain.TurnStateSucceeded {
			break
		}
		if err == nil && state != domain.TurnStateStarting && state != domain.TurnStateRunning {
			output, _ := os.ReadFile(filepath.Join(dir, "runtime", runID, "stdout.jsonl"))
			t.Fatalf("offline project experience run ended in %s (%s): %s", state, reason, output)
		}
		var busy *sqlite.Error
		if err != nil && !errors.Is(err, sql.ErrNoRows) &&
			!(errors.As(err, &busy) && busy.Code()&0xff == sqlite3.SQLITE_BUSY) {
			t.Fatalf("cannot read the fixture turn: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ordinary fixture did not settle: last state %q", state)
		case <-time.After(75 * time.Millisecond):
		}
	}
	calls, err := os.ReadFile(filepath.Join(fixture.root, "calls"))
	artifactCmdMust(t, err)
	configs, err := os.ReadDir(filepath.Join(dir, "member-tools"))
	artifactCmdMust(t, err)
	if len(configs) != 0 || bytes.Count(calls, []byte("\n")) != 1 {
		t.Fatal("ordinary lookup retained its config or caused another wake")
	}
	artifactCmdMust(t, stop())
}

// A local test binary, not Pi or a provider: inspect the actual launcher
// manifest and call the running cmd's real guarded routes with its live bearer.
func TestProjectExperienceNativeHelper(t *testing.T) {
	root, runID := os.Getenv("MEMBER_TOOLS_FIXTURE_ROOT"), os.Getenv("MEMBER_TOOLS_FIXTURE_RUN")
	if root == "" {
		return
	}
	manifest, err := os.ReadFile(os.Getenv(memberToolsConfigEnv))
	artifactCmdMust(t, err)
	if fmt.Sprintf("%x", sha256.Sum256(manifest)) != os.Getenv("AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256") {
		t.Fatal("fresh launcher did not bind exact manifest bytes")
	}
	var config struct {
		memberToolsNativeConfig
		Tools []string `json:"project_experience_tools"`
	}
	artifactCmdMust(t, json.Unmarshal(manifest, &config))
	want := []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history", provider.LookupProjectExperiencesTool}
	argv, err := os.ReadFile(filepath.Join(root, "args-"+runID))
	artifactCmdMust(t, err)
	if !slices.Equal(config.Tools, want) || !strings.Contains(string(argv), "\n--tools\n"+strings.Join(want, ",")+"\n") {
		t.Fatal("ordinary launch did not expose precisely history plus lookup without material authority")
	}
	_, err = io.Copy(io.Discard, os.Stdin)
	artifactCmdMust(t, err)
	var experienceID string
	memberToolsReadJSON(t, filepath.Join(root, "experience.json"), &experienceID)
	page := memberToolsHistoryDecode[platform.ProjectExperiencePage](t, memberToolsHTTP(t, config.Endpoint, config.Token, provider.LookupProjectExperiencesTool,
		map[string]any{"query": "CSV-218", "limit": 1}, "", 200, "", 15*time.Second))
	if experienceID == "" && (page.Items == nil || len(page.Items) != 0) || experienceID != "" && (len(page.Items) != 1 || page.Items[0].ID != experienceID) || page.NextCursor != "" {
		t.Fatal("ordinary lookup did not consume the installed scoped experience or the unregistered empty page")
	}
	memberToolsHTTP(t, config.Endpoint, config.Token, provider.ReadArtifactTool, map[string]any{"artifact_id": "unapproved", "version_id": "unapproved"}, "", 403, "forbidden", 15*time.Second)
	cwd, err := os.Getwd()
	artifactCmdMust(t, err)
	var frames bytes.Buffer
	out := json.NewEncoder(&frames)
	for _, event := range []any{
		map[string]any{"type": "session", "version": 3, "id": runID, "cwd": cwd},
		map[string]any{"type": "agent_start"},
		map[string]any{"type": "message_end", "message": memberToolsAssistant([]any{map[string]any{"type": "text", "text": "离线读取验证完成"}}, "stop")},
		map[string]any{"type": "agent_settled", "aborted": false},
	} {
		artifactCmdMust(t, out.Encode(event))
	}
	_, err = os.Stdout.Write(frames.Bytes())
	artifactCmdMust(t, err)
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
			break
		} else if !os.IsNotExist(err) || time.Now().After(deadline) {
			t.Fatal("fixture did not receive its frame-drain acknowledgement")
		}
		time.Sleep(30 * time.Millisecond)
	}
	os.Exit(0)
}
