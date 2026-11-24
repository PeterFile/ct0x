package httpapi

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

const projectsTestPath = "/api/v1/projects"

type projectRouteTestDTO struct {
	ID          string                       `json:"id"`
	WorkspaceID string                       `json:"workspace_id"`
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	CreatedAt   time.Time                    `json:"created_at"`
	Directory   projectDirectoryRouteTestDTO `json:"directory"`
}

type projectDirectoryRouteTestDTO struct {
	Path   string `json:"path"`
	NodeID string `json:"node_id"`
}

type channelRouteTestDTO struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	GroupID     string    `json:"group_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

func TestProjectDirectorySelectionAndReplay(t *testing.T) {
	a := openGroupAPI(t, "")
	empty := decoded[projectRouteTestDTO](t, a.request("POST", projectsTestPath, `{"name":"直接讨论"}`), http.StatusCreated)
	if empty.ID == "" || empty.WorkspaceID == "" || empty.Name != "直接讨论" || empty.CreatedAt.IsZero() || empty.Directory != (projectDirectoryRouteTestDTO{}) {
		t.Fatalf("invalid directoryless project: %+v", empty)
	}
	emptyChannels := projectsTestPath + "/" + url.PathEscape(empty.ID) + "/channels"
	if page := decoded[listResult[channelRouteTestDTO]](t, a.request("GET", emptyChannels, ""), http.StatusOK); page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("new project must have no channels: %+v", page)
	}

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", ".", a.path} {
		body, _ := json.Marshal(map[string]string{"name": "目录项目", "directory": invalid})
		w := a.request("POST", projectsTestPath, string(body), "select-folder")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_input")
	}
	body, _ := json.Marshal(map[string]string{"name": "目录项目", "description": "使用已有文件夹", "directory": dir})
	first := a.request("POST", projectsTestPath, string(body), "select-folder")
	project := decoded[projectRouteTestDTO](t, first, http.StatusCreated)
	if project.ID == "" || project.ID == empty.ID || project.WorkspaceID != empty.WorkspaceID || project.Name != "目录项目" || project.Description != "使用已有文件夹" || project.CreatedAt.IsZero() {
		t.Fatalf("invalid project with directory: %+v", project)
	}
	if project.Directory != (projectDirectoryRouteTestDTO{Path: dir, NodeID: "local"}) {
		t.Fatalf("wrong local directory selection: %+v", project.Directory)
	}
	w := a.request("POST", projectsTestPath, `{"name":"不同内容"}`, "select-folder")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusConflict, "conflict")

	// Remove also verifies creation left the selected directory empty.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = openGroupAPI(t, a.path)
	replay := a.request("POST", projectsTestPath, string(body), "select-folder")
	if replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay after reopen and directory removal must return the original project: %d %s", replay.Code, replay.Body)
	}
	w = a.request("POST", projectsTestPath, string(body), "new-selection")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_input")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("selection must not recreate a missing directory: %v", err)
	}

	detail := projectsTestPath + "/" + url.PathEscape(project.ID)
	if got := decoded[projectRouteTestDTO](t, a.request("GET", detail, ""), http.StatusOK); got != project {
		t.Fatalf("project detail differs from creation: %+v", got)
	}
	page := decoded[listResult[projectRouteTestDTO]](t, a.request("GET", projectsTestPath, ""), http.StatusOK)
	if len(page.Items) != 2 || page.NextCursor != "" || !slices.Contains(page.Items, project) || !slices.Contains(page.Items, empty) {
		t.Fatalf("project list must contain only the two accepted creations: %+v", page)
	}
}

func TestProjectChannelScopesAndPaging(t *testing.T) {
	a := openGroupAPI(t, "")
	dir, _ := json.Marshal(t.TempDir())
	left := decoded[projectRouteTestDTO](t, a.request("POST", projectsTestPath, `{"name":"甲项目","directory":`+string(dir)+`}`), http.StatusCreated)
	right := decoded[projectRouteTestDTO](t, a.request("POST", projectsTestPath, `{"name":"乙项目","directory":`+string(dir)+`}`), http.StatusCreated)
	if left.ID == "" || right.ID == "" || left.ID == right.ID || left.WorkspaceID == "" || right.WorkspaceID != left.WorkspaceID {
		t.Fatalf("the same directory must allow distinct projects in one workspace: %+v %+v", left, right)
	}
	leftPath := projectsTestPath + "/" + url.PathEscape(left.ID) + "/channels"
	rightPath := projectsTestPath + "/" + url.PathEscape(right.ID) + "/channels"

	const alarm = `{"name":"告警","description":"提醒"}`
	first := a.request("POST", leftPath, alarm, "alarm-key")
	leftAlarm := decoded[channelRouteTestDTO](t, first, http.StatusCreated)
	rightAlarm := decoded[channelRouteTestDTO](t, a.request("POST", rightPath, alarm, "alarm-key"), http.StatusCreated)
	w := a.request("POST", leftPath, `{"name":"无效","actor_id":"wrong-member"}`, "discussion-key")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_json")
	w = a.request("POST", leftPath, `{"name":" "}`, "discussion-key")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_input")
	discussion := decoded[channelRouteTestDTO](t, a.request("POST", leftPath, `{"name":"讨论"}`, "discussion-key"), http.StatusCreated)

	replay := a.request("POST", leftPath, alarm, "alarm-key")
	if replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() {
		t.Fatalf("channel replay changed: %d %s", replay.Code, replay.Body)
	}
	w = a.request("POST", leftPath, `{"name":"告警","description":"变更"}`, "alarm-key")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusConflict, "conflict")
	missingPath := projectsTestPath + "/missing-project/channels"
	w = a.request("POST", missingPath, alarm, "alarm-key")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusNotFound, "not_found")

	seenChannels, seenGroups := map[string]bool{}, map[string]bool{}
	for _, tc := range []struct {
		channel                            channelRouteTestDTO
		path, projectID, name, description string
	}{
		{leftAlarm, leftPath, left.ID, "告警", "提醒"},
		{rightAlarm, rightPath, right.ID, "告警", "提醒"},
		{discussion, leftPath, left.ID, "讨论", ""},
	} {
		c := tc.channel
		if c.ID == "" || c.GroupID == "" || c.GroupID == c.ID || c.ProjectID != tc.projectID || c.Name != tc.name || c.Description != tc.description || c.CreatedAt.IsZero() || seenChannels[c.ID] || seenGroups[c.GroupID] {
			t.Fatalf("invalid or reused channel identity: %+v (want project %q, name %q)", c, tc.projectID, tc.name)
		}
		seenChannels[c.ID], seenGroups[c.GroupID] = true, true
		if got := decoded[channelRouteTestDTO](t, a.request("GET", tc.path+"/"+url.PathEscape(c.ID), ""), http.StatusOK); got != c {
			t.Fatalf("channel detail differs from creation: %+v", got)
		}
		groupPath := "/api/v1/groups/" + url.PathEscape(c.GroupID)
		if group := decoded[domain.Group](t, a.request("GET", groupPath, ""), http.StatusOK); group.ID != c.GroupID {
			t.Fatalf("legacy group route returned a different group: %+v", group)
		}
		identity := decoded[map[string]string](t, a.request("GET", groupPath+"/identity", ""), http.StatusOK)
		want := map[string]string{"workspace_id": left.WorkspaceID, "project_id": tc.projectID, "channel_id": c.ID, "group_id": c.GroupID}
		if !maps.Equal(identity, want) {
			t.Fatalf("group identity = %v, want %v", identity, want)
		}
	}
	for _, path := range []string{
		leftPath + "/" + url.PathEscape(rightAlarm.ID),
		rightPath + "/" + url.PathEscape(leftAlarm.ID),
		missingPath + "/" + url.PathEscape(leftAlarm.ID),
	} {
		w = a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusNotFound, "not_found")
	}
	w = a.request("GET", leftPath+"/"+url.PathEscape(leftAlarm.ID)+"?actor_id=wrong-member", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_input")

	newer, older := discussion, leftAlarm
	if leftAlarm.CreatedAt.After(discussion.CreatedAt) || (leftAlarm.CreatedAt.Equal(discussion.CreatedAt) && leftAlarm.ID > discussion.ID) {
		newer, older = leftAlarm, discussion
	}
	page := decoded[listResult[channelRouteTestDTO]](t, a.request("GET", leftPath+"?limit=1", ""), http.StatusOK)
	if len(page.Items) != 1 || page.Items[0] != newer || page.NextCursor == "" {
		t.Fatalf("wrong first channel page: %+v", page)
	}
	next := "?limit=1&before=" + url.QueryEscape(page.NextCursor)
	tail := decoded[listResult[channelRouteTestDTO]](t, a.request("GET", leftPath+next, ""), http.StatusOK)
	if len(tail.Items) != 1 || tail.Items[0] != older || tail.NextCursor != "" {
		t.Fatalf("wrong final channel page: %+v", tail)
	}
	other := decoded[listResult[channelRouteTestDTO]](t, a.request("GET", rightPath, ""), http.StatusOK)
	if len(other.Items) != 1 || other.Items[0] != rightAlarm || other.NextCursor != "" {
		t.Fatalf("channel list crossed project scope: %+v", other)
	}
	w = a.request("GET", rightPath+next, "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_input")

	projects := decoded[listResult[projectRouteTestDTO]](t, a.request("GET", projectsTestPath, ""), http.StatusOK)
	groups := decoded[listResult[domain.Group]](t, a.request("GET", "/api/v1/groups", ""), http.StatusOK)
	if len(projects.Items) != 2 || len(groups.Items) != 3 {
		t.Fatalf("channel creation or failed retries left extra projects or groups: %+v %+v", projects, groups)
	}
}

func TestProjectLegacyGroupIdentity(t *testing.T) {
	a := openGroupAPI(t, "")
	const body = `{"name":"原有群组","description":"保留旧接口"}`
	first := a.request("POST", "/api/v1/groups", body, "legacy-key")
	group := decoded[domain.Group](t, first, http.StatusCreated)
	path := "/api/v1/groups/" + url.PathEscape(group.ID)
	identity := decoded[map[string]string](t, a.request("GET", path+"/identity", ""), http.StatusOK)
	projectID, channelID := identity["project_id"], identity["channel_id"]
	if len(identity) != 4 || identity["group_id"] != group.ID || identity["workspace_id"] == "" || projectID == "" || channelID == "" || projectID == group.ID || channelID == group.ID || channelID == projectID {
		t.Fatalf("invalid identity for legacy group %q: %v", group.ID, identity)
	}
	channelPath := projectsTestPath + "/" + url.PathEscape(projectID) + "/channels/" + url.PathEscape(channelID)
	if c := decoded[channelRouteTestDTO](t, a.request("GET", channelPath, ""), http.StatusOK); c.ID != channelID || c.ProjectID != projectID || c.GroupID != group.ID {
		t.Fatalf("legacy group mapped to a different channel: %+v", c)
	}
	w := a.request("GET", path+"/identity?actor_id=wrong-member", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_input")

	detail := a.request("GET", path, "")
	if got := decoded[domain.Group](t, detail, http.StatusOK); got != group || len(decoded[map[string]json.RawMessage](t, detail, http.StatusOK)) != 4 {
		t.Fatalf("legacy group DTO changed: %s", detail.Body)
	}
	replay := a.request("POST", "/api/v1/groups", body, "legacy-key")
	if replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() {
		t.Fatalf("legacy group replay changed: %d %s", replay.Code, replay.Body)
	}
	if got := decoded[map[string]string](t, a.request("GET", path+"/identity", ""), http.StatusOK); !maps.Equal(got, identity) {
		t.Fatalf("legacy replay changed group identity: %v", got)
	}
}
