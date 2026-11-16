package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func workspaceGrantAPI(t *testing.T, a *groupAPI) *platform.Preparer {
	t.Helper()
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := platform.NewPreparer(a.db, platform.PrepareOptions{PrivateWorkspaceRoot: private, PrivateDataRoot: filepath.Dir(a.path)})
	if err != nil {
		t.Fatal(err)
	}
	routes := append(LocalUserRoutes(platform.NewLocalUser(a.db)), WorkspaceGrantRoutes(p)...)
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	return p
}

func TestWorkspaceGrantHTTPExplicitScopeReplayAndReopen(t *testing.T) {
	a := openGroupAPI(t, "")
	workspaceGrantAPI(t, a)
	group := newGroup(t, a, "group")
	member := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+group.ID+"/members", `{"name":"reader","role":"read"}`), 201)
	path := "/api/v1/groups/" + group.ID + "/members/" + member.ID + "/workspace-grant"
	root := t.TempDir()
	body, _ := json.Marshal(map[string]string{"root": root, "scope": "read_only"})
	first := a.request("POST", path, string(body), "authorize")
	receipt := decoded[store.WorkspaceGrantReceipt](t, first, 201)
	if receipt.GroupID != group.ID || receipt.MemberID != member.ID || receipt.AuthorizedBy != "user" || receipt.NodeID != "local" || receipt.Scope != "read_only" || receipt.Root != root {
		t.Fatalf("invalid receipt: %+v", receipt)
	}
	if got := decoded[store.WorkspaceGrantReceipt](t, a.request("GET", path, ""), 200); got != receipt {
		t.Fatal("receipt read differs")
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = openGroupAPI(t, a.path)
	workspaceGrantAPI(t, a)
	replay := a.request("POST", path, string(body), "authorize")
	if replay.Code != 201 || replay.Body.String() != first.Body.String() {
		t.Fatalf("reopen lost identical replay: %d %s", replay.Code, replay.Body)
	}
	changed := a.request("POST", path, `{"root":"/different","scope":"read_only"}`, "authorize")
	assertError(t, changed.Code, changed.Header(), changed.Body.Bytes(), 409, "conflict")
	foreign := newGroup(t, a, "foreign")
	foreignPath := strings.Replace(path, group.ID, foreign.ID, 1)
	for _, method := range []string{"GET", "POST"} {
		w := a.request(method, foreignPath, string(body), "authorize")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	stored, err := a.db.GetMember(context.Background(), group.ID, member.ID)
	if err != nil || stored.Workspace != root {
		t.Fatal("authorization did not select the explicit directory")
	}
}

func TestWorkspaceGrantHTTPRejectsForgedAuthorityAndBrowserMutations(t *testing.T) {
	a := openGroupAPI(t, "")
	workspaceGrantAPI(t, a)
	group := newGroup(t, a, "group")
	member := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+group.ID+"/members", `{"name":"reader","role":"read"}`), 201)
	path := "/api/v1/groups/" + group.ID + "/members/" + member.ID + "/workspace-grant"
	body, _ := json.Marshal(map[string]string{"root": t.TempDir(), "scope": "read_only"})
	for _, field := range []string{"actor_id", "group_id", "member_id", "node_id", "authorized_by", "resource_key", "resolved_root", "id", "idempotency_key"} {
		w := a.request("POST", path, strings.TrimSuffix(string(body), "}")+`,"`+field+`":"forged"}`, "authorize")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
	}
	for _, input := range []string{`{"root":"/tmp"}`, `{"root":"/tmp","scope":"read_write"}`, `{"scope":"read_only"}`, `{"root":"/tmp","root":"/else","scope":"read_only"}`} {
		w := a.request("POST", path, input, "authorize")
		if w.Code != 400 {
			t.Fatalf("invalid authorization accepted: %d %s", w.Code, w.Body)
		}
	}
	for _, keys := range [][]string{nil, {""}, {"key", "key"}, {"has space"}} {
		w := a.request("POST", path, string(body), keys...)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	w := a.request("POST", path+"?actor_id=user", string(body), "authorize")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	r := httptest.NewRequest("POST", "http://"+DefaultAddress+path, strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "authorize")
	r.Header.Set("Origin", "http://foreign.invalid")
	w = httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusForbidden, "invalid_origin")
	w = a.request("GET", path, "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
}
