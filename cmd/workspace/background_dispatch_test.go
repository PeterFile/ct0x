package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const backgroundFixtureModel = "fixture-provider/discussion-model"

// Use the exact A1 schema on a disposable SQLite file without reserving a
// migration number. Once B registers it, the normal migrator is sufficient.
func installBackgroundPreviewFixture(t *testing.T, path string) {
	t.Helper()
	if err := openBackgroundPreviewTestDB(t, path).Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var installed bool
	if err := raw.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='group_background_bindings')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if installed {
		return
	}
	source, err := os.ReadFile("../../internal/store/group_background_dispatch_schema.go")
	if err != nil {
		t.Fatal(err)
	}
	_, definition, found := strings.Cut(string(source), "const groupBackgroundDispatchSchema = `")
	definition, _, closed := strings.Cut(definition, "`")
	if !found || !closed {
		t.Fatal("A1 schema definition missing")
	}
	if _, err := raw.Exec(definition); err != nil {
		t.Fatal(err)
	}
}

func openBackgroundPreviewTestDB(t *testing.T, path string) *store.Store {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func backgroundPreviewFixtureApproval(t *testing.T, cli string) map[string]any {
	t.Helper()
	selected := provider.Discover(provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: cli}}})
	if len(selected) != 1 {
		t.Fatal("fixture Pi not discovered")
	}
	return map[string]any{
		"provider_id": selected[0].ID, "model": backgroundFixtureModel, "max_active_members": 2,
		"approval_expires_at": time.Now().UTC().Add(time.Hour),
		"preset":              map[string]string{"id": "discussion", "name": "测试接待", "role": "讨论"},
	}
}

func writeBackgroundPreviewFixtureApproval(t *testing.T, path string, grant map[string]any) {
	t.Helper()
	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".next", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".next", path); err != nil {
		t.Fatal(err)
	}
}

func backgroundPreviewQualification(grant map[string]any, cli string, at time.Time) map[string]any {
	return map[string]any{"provider_id": grant["provider_id"], "model": grant["model"], "path": cli,
		"version": "1.4.0", "transport": provider.PiOneShotAdapterID, "observed_at": at.UTC()}
}

func postBackgroundPreview(t *testing.T, url string, input, result any) {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	// These fixtures run real SQLite commits on the shared HDD.
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		t.Fatalf("POST %s: %s: %s", url, response.Status, data)
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		t.Fatal(err)
	}
}

func awaitBackgroundPreviewTurn(t *testing.T, db *store.Store, message domain.Message) domain.Turn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var turn domain.Turn
	for {
		current, err := db.GetMessage(ctx, message.GroupID, message.ID)
		if err == nil && len(current.Deliveries) == 1 && current.Deliveries[0].TurnID != "" {
			turn, err = db.GetTurn(ctx, message.GroupID, current.Deliveries[0].TurnID)
			if err == nil && turn.State == domain.TurnStateSucceeded {
				return turn
			}
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("message %s did not finish: deliveries=%+v turn=%+v error=%v", message.ID, current.Deliveries, turn, err)
		}
	}
}

func TestBackgroundDispatchPreviewNormalStartup(t *testing.T) {
	ctx := context.Background()
	f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
	dbPath, approvalPath := filepath.Join(dir, "workspace.db"), filepath.Join(dir, "a1.json")
	installBackgroundPreviewFixture(t, dbPath)
	grant := backgroundPreviewFixtureApproval(t, f.path)
	writeBackgroundPreviewFixtureApproval(t, approvalPath, grant) // Approval is not readiness.
	ordinaryArgs := []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--enable-execution", "--pi-path", f.path}
	base, stop := startRun(t, append(ordinaryArgs, "--group-background-dispatch-preview", approvalPath)...)
	db := openBackgroundPreviewTestDB(t, dbPath)
	defer db.Close()
	var group domain.Group
	postBackgroundPreview(t, base+"/api/v1/groups", platform.GroupInput{Name: "讨论"}, &group)
	groupURL := base + "/api/v1/groups/" + group.ID
	var public, earlier, later domain.Message
	postBackgroundPreview(t, groupURL+"/messages", map[string]any{"content": "仅公开", "topic_id": "同话题", "recipient_ids": []string{}}, &public)
	postBackgroundPreview(t, groupURL+"/messages", map[string]string{"content": "先讨论", "topic_id": "同话题"}, &earlier)
	var explicit domain.Member
	postBackgroundPreview(t, groupURL+"/members", platform.MemberInput{
		Name: "手动成员", Role: "讨论", ProviderID: grant["provider_id"].(string), Model: "fixture-provider/other-model",
	}, &explicit)
	postBackgroundPreview(t, groupURL+"/messages", platform.MessageInput{
		Content: "后明确指定", TopicID: "同话题", RecipientIDs: []string{explicit.ID},
	}, &later)
	// A separate topic can actually claim while the same-topic source waits.
	var independent domain.Message
	postBackgroundPreview(t, groupURL+"/messages", platform.MessageInput{
		Content: "独立话题", TopicID: "另一个话题", RecipientIDs: []string{explicit.ID},
	}, &independent)
	awaitBackgroundPreviewTurn(t, db, independent)
	gotEarlier, err := db.GetMessage(ctx, group.ID, earlier.ID)
	if err != nil || earlier.SourceKind != "local_user_chat" || len(earlier.Deliveries) != 0 || len(gotEarlier.Deliveries) != 0 {
		t.Fatalf("approval + startup discovery alone created an A1 delivery: %+v, %v", gotEarlier, err)
	}
	gotLater, err := db.GetMessage(ctx, group.ID, later.ID)
	if err != nil || len(gotLater.Deliveries) != 1 || gotLater.Deliveries[0].TurnID != "" {
		t.Fatalf("later same-topic explicit delivery overtook A1: %+v, %v", gotLater, err)
	}
	grant["qualification"] = backgroundPreviewQualification(grant, f.path, time.Now())
	writeBackgroundPreviewFixtureApproval(t, approvalPath, grant)
	first, second := awaitBackgroundPreviewTurn(t, db, earlier), awaitBackgroundPreviewTurn(t, db, later)
	if !first.CreatedAt.Before(second.CreatedAt) || second.MemberID != explicit.ID || first.ReplyMessageID == "" {
		t.Fatalf("A1 did not share the existing ordered scheduler: first=%+v second=%+v", first, second)
	}
	kind, err := db.BackgroundDiscussionKind(ctx, group.ID, first.DeliveryID)
	if err != nil || kind != "preset" {
		t.Fatalf("want A1 preset for the approved pair, got %q, %v", kind, err)
	}
	member, err := db.GetMember(ctx, group.ID, first.MemberID)
	if err != nil || member.ProviderID != grant["provider_id"] || member.Model != backgroundFixtureModel || member.Name != "测试接待" || member.Workspace != "" {
		t.Fatalf("configured preset not respected: %+v, %v", member, err)
	}
	gotPublic, err := db.GetMessage(ctx, group.ID, public.ID)
	if err != nil || len(gotPublic.Deliveries) != 0 {
		t.Fatalf("public-only message woke a model: %+v, %v", gotPublic, err)
	}
	probes, err := os.ReadFile(filepath.Join(f.root, "probes"))
	if err != nil || string(probes) != "version\naccount\n" {
		t.Fatalf("A1 should not probe to discover readiness: %q, %v", probes, err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// Restart with the fixture evidence still on disk but no explicit opt-in.
	base, _ = startRun(t, ordinaryArgs...)
	groupURL = base + "/api/v1/groups/" + group.ID
	var parked, addressed domain.Message
	postBackgroundPreview(t, groupURL+"/messages", map[string]string{"content": "停用时保存请求", "topic_id": "同话题"}, &parked)
	postBackgroundPreview(t, groupURL+"/messages", platform.MessageInput{
		Content: "停用时仍可定向", TopicID: "同话题", RecipientIDs: []string{member.ID},
	}, &addressed)
	awaitBackgroundPreviewTurn(t, db, addressed)
	parked, err = db.GetMessage(ctx, group.ID, parked.ID)
	if err != nil || len(parked.Deliveries) != 0 || parked.SourceKind != "local_user_chat" {
		t.Fatalf("disabled A1 unexpectedly routed a bound group's request: %+v, %v", parked, err)
	}
}

func TestBackgroundDispatchPreviewOldDBAndExplicitGrant(t *testing.T) {
	f, dir := newExecutionFixture(t, "1.4.0"), t.TempDir()
	dbPath, approvalPath := filepath.Join(dir, "workspace.db"), filepath.Join(dir, "a1.json")
	db := openBackgroundPreviewTestDB(t, dbPath) // Intentionally no fixture migration.
	unregistered := errors.Is(db.EnableGroupBackgroundDispatch(context.Background()), store.ErrInvalid)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	grant := backgroundPreviewFixtureApproval(t, f.path)
	writeBackgroundPreviewFixtureApproval(t, approvalPath, grant)
	args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir, "--pi-path", f.path}
	flag := []string{"--group-background-dispatch-preview", approvalPath}
	if _, err := parseConfig(append(args, flag...), io.Discard); err == nil {
		t.Fatal("an A1 grant must not implicitly enable execution")
	}
	if unregistered {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var output bytes.Buffer
		err := runWithSearch(ctx, append(append(args, flag...), "--enable-execution"), &output, provider.Search{})
		if !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "未安装群组后台调度迁移") || output.Len() != 0 {
			t.Fatalf("explicit opt-in without installed schema should fail before admission: %v, %q", err, &output)
		}
	}
	base, _ := startRun(t, args...)
	var group domain.Group
	postBackgroundPreview(t, base+"/api/v1/groups", platform.GroupInput{Name: "旧配置讨论"}, &group)
	groupURL := base + "/api/v1/groups/" + group.ID
	var member domain.Member
	postBackgroundPreview(t, groupURL+"/members", platform.MemberInput{Name: "旧成员", Role: "讨论", ProviderID: grant["provider_id"].(string), Model: backgroundFixtureModel}, &member)
	var omitted domain.Message
	postBackgroundPreview(t, groupURL+"/messages", map[string]string{"content": "已有默认路由"}, &omitted)
	if len(omitted.Deliveries) != 1 || omitted.Deliveries[0].RecipientID != member.ID || omitted.Deliveries[0].TurnID != "" {
		t.Fatalf("absent A1 changed old DB startup or legacy routing: %+v", omitted)
	}
}
