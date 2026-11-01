package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type groupAPI struct {
	db      *store.Store
	path    string
	handler http.Handler
}

func openGroupAPI(t *testing.T, path string) *groupAPI {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "workspace.db")
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &groupAPI{db, path, mustServer(t, DefaultAddress, LocalUserRoutes(platform.NewLocalUser(db))...).Handler}
}

func (a *groupAPI) request(method, path, body string, keys ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+DefaultAddress+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for _, key := range keys {
		r.Header.Add("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	return w
}

func decoded[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	var value T
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

type listResult[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func newGroup(t *testing.T, a *groupAPI, name string) domain.Group {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "description": "说明"})
	return decoded[domain.Group](t, a.request(http.MethodPost, "/api/v1/groups", string(body)), 201)
}

func TestGroupMemberRoutes(t *testing.T) {
	a := openGroupAPI(t, "")
	groups := decoded[listResult[domain.Group]](t, a.request("GET", "/api/v1/groups", ""), 200)
	if groups.Items == nil || len(groups.Items) != 0 || groups.NextCursor != "" {
		t.Fatalf("empty list contract: %+v", groups)
	}
	group, other := newGroup(t, a, " 群组 "), newGroup(t, a, "另一个群组")
	if group.Name != " 群组 " || group.Description != "说明" || group.ID == "" || group.CreatedAt.IsZero() || group.CreatedAt.Location() != time.UTC {
		t.Fatalf("invalid group: %+v", group)
	}
	if got := decoded[domain.Group](t, a.request("GET", "/api/v1/groups/"+group.ID, ""), 200); got != group {
		t.Fatalf("detail differs: %+v", got)
	}
	path := "/api/v1/groups/" + group.ID + "/members"
	empty := decoded[listResult[domain.Member]](t, a.request("GET", path, ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty members: %+v", empty)
	}
	member := decoded[domain.Member](t, a.request("POST", path, `{"name":" 成员 ","role":"职责","provider_id":"unverified","model":"preference","workspace":"/not-a-grant"}`), 201)
	if member.ID == "" || member.GroupID != group.ID || member.CreatorID != "user" || member.OwnerID != "user" || member.Status != domain.MemberStatusActive || member.Name != " 成员 " || member.Role != "职责" || member.ProviderID != "unverified" || member.Model != "preference" || member.Workspace != "/not-a-grant" || member.CreatedAt.IsZero() || member.CreatedAt.Location() != time.UTC {
		t.Fatalf("invalid member: %+v", member)
	}
	if got := decoded[domain.Member](t, a.request("GET", path+"/"+member.ID, ""), 200); got != member {
		t.Fatalf("member detail differs: %+v", got)
	}
	isolated := decoded[listResult[domain.Member]](t, a.request("GET", "/api/v1/groups/"+other.ID+"/members", ""), 200)
	if len(isolated.Items) != 0 {
		t.Fatalf("cross-group list leaked: %+v", isolated)
	}
	for _, path := range []string{"/api/v1/groups/missing", "/api/v1/groups/missing/members", path + "/missing", "/api/v1/groups/" + other.ID + "/members/" + member.ID} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	w := a.request("POST", "/api/v1/groups/missing/members", `{"name":"n","role":"r"}`)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	for _, tt := range []struct {
		method, path string
		status       int
		code         string
	}{
		{"DELETE", "/api/v1/groups", 405, "method_not_allowed"}, {"POST", "/api/v1/groups/" + group.ID, 405, "method_not_allowed"},
		{"PATCH", path + "/" + member.ID, 405, "method_not_allowed"}, {"GET", "/api/v1/groups/" + group.ID + "/messages", 404, "not_found"},
		{"GET", "/api/v1/providers", 404, "not_found"},
	} {
		w := a.request(tt.method, tt.path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), tt.status, tt.code)
	}
}

func TestMutationDTOsAndKeys(t *testing.T) {
	a := openGroupAPI(t, "")
	group := newGroup(t, a, "group")
	for _, path := range []string{"/api/v1/groups", "/api/v1/groups/" + group.ID + "/members"} {
		body := `{"name":"valid"}`
		if strings.HasSuffix(path, "/members") {
			body = `{"name":"valid","role":"role"}`
		}
		for _, field := range []string{"actor_id", "creator_id", "owner_id", "group_id", "status", "idempotency_key"} {
			spoof := strings.TrimSuffix(body, "}") + `,"` + field + `":"attacker"}`
			w := a.request("POST", path, spoof)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
		}
		for _, keys := range [][]string{{""}, {" "}, {"has space"}, {"a\tb"}, {"中文"}, {"a,b"}, {strings.Repeat("x", 129)}, {"key", "key"}, {"key", "other"}} {
			w := a.request("POST", path, body, keys...)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		}
		for _, key := range []string{"Key/one:1", strings.Repeat("x", 128)} {
			w := a.request("POST", path, body, key)
			if w.Code != 201 {
				t.Fatalf("valid key rejected: %d %s", w.Code, w.Body)
			}
		}
		w := a.request("POST", path+"?name=override", body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, tt := range []struct{ path, body string }{
		{"/api/v1/groups", `{"name":" "}`}, {"/api/v1/groups/" + group.ID + "/members", `{"name":"n","role":" "}`},
	} {
		w := a.request("POST", tt.path, tt.body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
}

func TestDurableHTTPIdempotency(t *testing.T) {
	a := openGroupAPI(t, "")
	groupBody := `{"name":" exact ","description":"payload"}`
	first := a.request("POST", "/api/v1/groups", groupBody, "shared-key")
	group := decoded[domain.Group](t, first, 201)
	memberPath := "/api/v1/groups/" + group.ID + "/members"
	memberBody := `{"name":"member","role":"role"}`
	member := a.request("POST", memberPath, memberBody, "shared-key")
	decoded[domain.Member](t, member, 201)
	for _, target := range []struct {
		path, body string
		first      *httptest.ResponseRecorder
	}{{"/api/v1/groups", groupBody, first}, {memberPath, memberBody, member}} {
		retry := a.request("POST", target.path, target.body, "shared-key")
		if retry.Code != 201 || !bytes.Equal(target.first.Body.Bytes(), retry.Body.Bytes()) {
			t.Fatalf("replay must be the identical 201 object: %d %s", retry.Code, retry.Body)
		}
	}
	conflict := a.request("POST", "/api/v1/groups", `{"name":"exact","description":"payload"}`, "shared-key")
	assertError(t, conflict.Code, conflict.Header(), conflict.Body.Bytes(), 409, "conflict")
	other := newGroup(t, a, "other")
	otherMember := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+other.ID+"/members", memberBody, "shared-key"), 201)
	if otherMember.GroupID != other.ID || otherMember.ID == decoded[domain.Member](t, member, 201).ID {
		t.Fatal("member idempotency key crossed group scope")
	}
	changedMember := a.request("POST", memberPath, `{"name":"member","role":"changed"}`, "shared-key")
	assertError(t, changedMember.Code, changedMember.Header(), changedMember.Body.Bytes(), 409, "conflict")
	caseDistinct := decoded[domain.Group](t, a.request("POST", "/api/v1/groups", groupBody, "Shared-key"), 201)
	if caseDistinct.ID == group.ID {
		t.Fatal("key bytes were changed")
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openGroupAPI(t, a.path)
	for _, target := range []struct {
		path, body string
		first      *httptest.ResponseRecorder
	}{{"/api/v1/groups", groupBody, first}, {memberPath, memberBody, member}} {
		got := reopened.request("POST", target.path, target.body, "shared-key")
		if got.Code != 201 || !bytes.Equal(got.Body.Bytes(), target.first.Body.Bytes()) {
			t.Fatalf("reopen lost replay: %d %s", got.Code, got.Body)
		}
	}
}

func TestConcurrentHTTPIdempotency(t *testing.T) {
	a := openGroupAPI(t, "")
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = mustServer(t, ts.Listener.Addr().String(), LocalUserRoutes(platform.NewLocalUser(a.db))...)
	ts.Start()
	defer ts.Close()
	client := ts.Client()
	client.Timeout = 5 * time.Second
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			r, _ := http.NewRequest("POST", ts.URL+"/api/v1/groups", strings.NewReader(`{"name":"concurrent"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "concurrent-key")
			response, err := client.Do(r)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != 201 {
				t.Errorf("concurrent response: %d %s %v", response.StatusCode, body, err)
				return
			}
			results <- string(body)
		})
	}
	wg.Wait()
	close(results)
	first, count := "", 0
	for body := range results {
		if first != "" && first != body {
			t.Fatal("concurrent retries returned different objects")
		}
		first = body
		count++
	}
	page, err := a.db.ListGroups(context.Background(), store.ListOptions{})
	if count != 8 || err != nil || len(page.Items) != 1 {
		t.Fatalf("concurrent submissions duplicated rows: %d %+v %v", count, page, err)
	}
}

func TestScopedPagination(t *testing.T) {
	a := openGroupAPI(t, "")
	var expected []domain.Group
	for range 4 {
		expected = append(expected, newGroup(t, a, "page"))
	}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].CreatedAt.Equal(expected[j].CreatedAt) {
			return expected[i].ID > expected[j].ID
		}
		return expected[i].CreatedAt.After(expected[j].CreatedAt)
	})
	first := decoded[listResult[domain.Group]](t, a.request("GET", "/api/v1/groups?limit=2", ""), 200)
	if len(first.Items) != 2 || first.NextCursor == "" || first.Items[0] != expected[0] || first.Items[1] != expected[1] {
		t.Fatalf("wrong first page: %+v", first)
	}
	raw, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil || !bytes.Contains(raw, []byte(expected[1].CreatedAt.Format(time.RFC3339Nano))) {
		t.Fatalf("cursor lost nanosecond position: %s %v", raw, err)
	}
	last := decoded[listResult[domain.Group]](t, a.request("GET", "/api/v1/groups?limit=2&before="+url.QueryEscape(first.NextCursor), ""), 200)
	if len(last.Items) != 2 || last.NextCursor != "" || last.Items[0] != expected[2] || last.Items[1] != expected[3] {
		t.Fatalf("exact-full final page duplicated or skipped: %+v", last)
	}
	memberPath := "/api/v1/groups/" + expected[0].ID + "/members"
	for range 3 {
		decoded[domain.Member](t, a.request("POST", memberPath, `{"name":"n","role":"r"}`), 201)
	}
	members := decoded[listResult[domain.Member]](t, a.request("GET", memberPath+"?limit=2", ""), 200)
	tail := decoded[listResult[domain.Member]](t, a.request("GET", memberPath+"?limit=2&before="+members.NextCursor, ""), 200)
	if len(members.Items) != 2 || members.NextCursor == "" || len(tail.Items) != 1 || tail.NextCursor != "" || tail.Items[0].ID == members.Items[0].ID || tail.Items[0].ID == members.Items[1].ID {
		t.Fatalf("member pagination wrong: %+v %+v", members, tail)
	}
	for _, path := range []string{
		memberPath + "?before=" + first.NextCursor,
		"/api/v1/groups?before=" + members.NextCursor,
		"/api/v1/groups/" + expected[1].ID + "/members?before=" + members.NextCursor,
	} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for range 47 {
		if _, err := a.db.CreateGroup(context.Background(), store.CreateGroupInput{Name: "bounded"}); err != nil {
			t.Fatal(err)
		}
	}
	bounded := decoded[listResult[domain.Group]](t, a.request("GET", "/api/v1/groups", ""), 200)
	if len(bounded.Items) != 50 || bounded.NextCursor == "" {
		t.Fatalf("default list is not SQL-bounded to 50: %+v", bounded)
	}
}

func TestQueryCursorValidationAndErrors(t *testing.T) {
	a := openGroupAPI(t, "")
	for _, query := range []string{"limit=0", "limit=-1", "limit=201", "limit=1.5", "limit=x", "limit=", "limit=1&limit=2", "limit=1&%6cimit=2", "after=x", "extra=x", "limit=%ZZ", "limit=1;before=x", "before=", "before=a&before=b", "before=" + strings.Repeat("x", 2049)} {
		w := a.request("GET", "/api/v1/groups?"+query, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	valid := `{"v":1,"scope":"groups","group_id":"","created_at":"2026-10-30T12:00:00.123456789Z","id":"opaque-id"}`
	for _, raw := range []string{
		`null`, `[]`, `{}`, valid + ` {}`, strings.Replace(valid, `"v":1`, `"v":2`, 1),
		strings.Replace(valid, `"groups"`, `"members"`, 1), strings.Replace(valid, `"group_id":""`, `"group_id":"other"`, 1),
		strings.Replace(valid, `2026-10-30T12:00:00.123456789Z`, `not-a-date`, 1), strings.Replace(valid, `opaque-id`, ``, 1),
		strings.Replace(valid, `opaque-id`, `has space`, 1), strings.Replace(valid, `"v":1`, `"v":1,"v":1`, 1),
		strings.TrimSuffix(valid, "}") + `,"extra":1}`,
	} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(raw))
		w := a.request("GET", "/api/v1/groups?before="+encoded, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, errContext := range []struct {
		err    error
		status int
		code   string
	}{{context.Canceled, 408, "request_canceled"}, {context.DeadlineExceeded, 504, "deadline_exceeded"}} {
		var ctx context.Context
		var cancel context.CancelFunc
		if errContext.err == context.Canceled {
			ctx, cancel = context.WithCancel(context.Background())
		} else {
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}
		cancel()
		r := httptest.NewRequest("GET", "http://"+DefaultAddress+"/api/v1/groups", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		a.handler.ServeHTTP(w, r)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), errContext.status, errContext.code)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	w := a.request("GET", "/api/v1/groups?before=invalid", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	w = a.request("GET", "/api/v1/groups", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
	if strings.Contains(w.Body.String(), "sql") || strings.Contains(w.Body.String(), a.path) || strings.Contains(w.Body.String(), "closed") {
		t.Fatalf("internal storage error leaked: %s", w.Body)
	}
}
