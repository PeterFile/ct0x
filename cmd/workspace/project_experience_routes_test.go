package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
)

const projectExperienceRoute = "/project-experiences"

type projectExperienceHTTP struct {
	base   string
	client *http.Client
}

func (h projectExperienceHTTP) request(t *testing.T, method, suffix string, input any, key string, status int, result any) []byte {
	t.Helper()
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		artifactCmdMust(t, err)
		body = bytes.NewReader(data)
	}
	r, err := http.NewRequest(method, h.base+suffix, body)
	artifactCmdMust(t, err)
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	response, err := h.client.Do(r)
	artifactCmdMust(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s err=%v", method, suffix, response.StatusCode, status, data, err)
	}
	if result != nil {
		artifactCmdMust(t, json.Unmarshal(data, result))
	}
	return data
}

func experienceHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func experienceSchemaInstalled(t *testing.T, dir string) bool {
	t.Helper()
	dsn := url.URL{Scheme: "file", Path: filepath.Join(dir, "workspace.db"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", dsn.String())
	artifactCmdMust(t, err)
	defer db.Close()
	var installed bool
	artifactCmdMust(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='project_experiences')`).Scan(&installed))
	return installed
}

func experienceHTTPMessage(t *testing.T, h projectExperienceHTTP, groupPath, topic, body string) domain.Message {
	t.Helper()
	var message domain.Message
	h.request(t, "POST", groupPath+"/messages", platform.MessageInput{Content: body, TopicID: topic, RecipientIDs: []string{}}, "", 201, &message)
	if message.ID == "" || message.SourceKind != "local_user_chat" || message.SourceID == "" {
		t.Fatalf("fixture needs actual local-user provenance: %+v", message)
	}
	return message
}

func experienceHTTPSelection(message domain.Message, version string) platform.SelectProjectExperienceInput {
	return platform.SelectProjectExperienceInput{TopicID: message.TopicID, Problem: "导出 BUG CSV-218", Decision: message.Content,
		Component: "CSV", ApplicableVersion: version, Conditions: "仅限导出组件", OriginalMessageID: message.ID, ConfirmationMessageID: message.ID}
}

func TestRunProjectExperienceSelection(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir}
	base, stop := startRunWithSearch(t, provider.Search{}, args...)
	h := projectExperienceHTTP{base: base, client: experienceHTTPClient(t)}
	var group, other domain.Group
	for _, g := range []*domain.Group{&group, &other} {
		h.request(t, "POST", "/api/v1/groups", platform.GroupInput{Name: "same display name"}, "", 201, g)
	}
	groupPath, otherPath := "/api/v1/groups/"+group.ID, "/api/v1/groups/"+other.ID
	entry := groupPath + projectExperienceRoute
	source := experienceHTTPMessage(t, h, groupPath, "export-bug", "必须兼容旧版 CSV")
	choice := experienceHTTPSelection(source, "v2.9")
	search := entry + "?q=" + url.QueryEscape("CSV-218") + "&version=v2.9"
	if got := h.request(t, "GET", search, nil, "", 200, nil); string(got) != "{\"items\":[],\"next_cursor\":\"\"}\n" {
		t.Fatalf("chat did not stay outside selected experience before the write: %s", got)
	}
	if !experienceSchemaInstalled(t, dir) {
		h.request(t, "POST", entry, choice, "confirm-export", 503, nil)
		var persisted domain.Message
		h.request(t, "GET", groupPath+"/messages/"+source.ID, nil, "", 200, &persisted)
		if !reflect.DeepEqual(persisted, source) {
			t.Fatal("legacy Group message behavior changed without the E3 migration")
		}
		t.Log("unregistered E3: empty lookup, unavailable write, unchanged original Group message API")
		return
	}

	var selected domain.ProjectExperience
	receipt := h.request(t, "POST", entry, choice, "confirm-export", 201, &selected)
	if selected.ID == "" || selected.Origin.ProjectID == "" || selected.Origin.ChannelID == "" || selected.Origin.WorkspaceID == "" ||
		selected.Origin.ProjectID == group.ID || selected.Origin.ChannelID == group.ID || selected.Origin.GroupID != group.ID || selected.Origin.TopicID != source.TopicID ||
		selected.OriginalMessageID != source.ID || selected.ConfirmationMessageID != source.ID || selected.AcceptedOutcome != nil ||
		selected.Decision != choice.Decision || selected.Component != choice.Component || selected.ApplicableVersion != choice.ApplicableVersion ||
		selected.Conditions != choice.Conditions || selected.ConfirmedBy != "user" || selected.ConfirmedAt.IsZero() || !selected.ConfirmedAt.Equal(selected.SelectedAt) {
		t.Fatalf("postmortem without a Task lost its D origin, passage or confirmation: %+v", selected)
	}
	if replay := h.request(t, "POST", entry, choice, "confirm-export", 201, nil); !bytes.Equal(replay, receipt) {
		t.Fatal("idempotent selection changed")
	}
	changed := choice
	changed.ApplicableVersion = "v3.0"
	h.request(t, "POST", entry, changed, "confirm-export", 409, nil)
	h.request(t, "POST", entry, map[string]any{"project_id": selected.Origin.ProjectID}, "", 400, nil)
	h.request(t, "GET", search+"&caller_id=user", nil, "", 400, nil)
	foreign := experienceHTTPMessage(t, h, otherPath, source.TopicID, source.Content)
	for _, id := range []string{foreign.ID, "missing"} {
		changed = choice
		changed.OriginalMessageID = id
		h.request(t, "POST", entry, changed, "", 404, nil)
	}
	changed = choice
	changed.Decision = "must also support ORC"
	h.request(t, "POST", entry, changed, "", 404, nil)

	// Select another problem, then put unrelated matching words in ordinary chat.
	// Only deliberate experience metadata participates in the literal lookup.
	alarm := experienceHTTPMessage(t, h, groupPath, "alerts", "告警要按规则去重")
	alarmChoice := experienceHTTPSelection(alarm, "v2.9")
	alarmChoice.Problem, alarmChoice.Component = "告警 ALARM-100%_", "alerting"
	var alarmSelected domain.ProjectExperience
	h.request(t, "POST", entry, alarmChoice, "confirm-alert", 201, &alarmSelected)
	h.request(t, "POST", otherPath+projectExperienceRoute, experienceHTTPSelection(foreign, "v2.9"), "confirm-export", 201, nil)
	var page platform.ProjectExperiencePage
	before := h.request(t, "GET", search, nil, "", 200, &page)
	if len(page.Items) != 1 || page.Items[0].ID != selected.ID || page.Items[0].Status != domain.ProjectExperienceActive {
		t.Fatalf("export results across selectable problems and groups: %s", before)
	}
	h.request(t, "GET", entry+"?q="+url.QueryEscape("100%_"), nil, "", 200, &page)
	if len(page.Items) != 1 || page.Items[0].ID != alarmSelected.ID {
		t.Fatalf("literal alarm query did not distinguish selected problems: %+v", page)
	}
	for i := range 200 {
		experienceHTTPMessage(t, h, groupPath, "planning", fmt.Sprintf("NOISE_%03d unrelated planning CSV-218 导出 BUG 100%%_", i))
	}
	if got := h.request(t, "GET", search, nil, "", 200, nil); !bytes.Equal(got, before) {
		t.Fatalf("ordinary unrelated bodies affected scoped experience lookup: %s", got)
	}
	artifactCmdMust(t, stop())
	h.base, stop = startRunWithSearch(t, provider.Search{}, args...)
	defer func() { artifactCmdMust(t, stop()) }()
	if got := h.request(t, "GET", search, nil, "", 200, nil); !bytes.Equal(got, before) {
		t.Fatalf("selection changed after reopen: %s", got)
	}
	if got := h.request(t, "POST", entry, choice, "confirm-export", 201, nil); !bytes.Equal(got, receipt) {
		t.Fatalf("creation receipt changed after reopen: %s", got)
	}
	var original domain.Message
	h.request(t, "GET", groupPath+"/messages/"+selected.OriginalMessageID, nil, "", 200, &original)
	if !reflect.DeepEqual(original, source) || strings.Contains(string(before), "NOISE_") {
		t.Fatal("original source or source isolation changed")
	}
	t.Log("local-user postmortem, literal Export/Alarm differentiation, same-name Group isolation, 0/200 unrelated bodies, original evidence and replay after reopen")
}
