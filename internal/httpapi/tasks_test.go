package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func openTaskAPI(t *testing.T, path string) *groupAPI {
	t.Helper()
	a := openGroupAPI(t, path)
	local := platform.NewLocalUser(a.db)
	routes := append(LocalUserRoutes(local), TaskRoutes(local)...)
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	return a
}

func TestTaskHTTPCreateGetListAndReplay(t *testing.T) {
	a := openTaskAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	path, otherPath := "/api/v1/groups/"+g.ID+"/tasks", "/api/v1/groups/"+other.ID+"/tasks"
	empty := decoded[listResult[domain.Task]](t, a.request("GET", path, ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty task list: %+v", empty)
	}
	member := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+g.ID+"/members", `{"name":"member","role":"role"}`), 201)
	plainBody := `{"title":"dependency","acceptance":"criteria"}`
	dependency := decoded[domain.Task](t, a.request("POST", path, plainBody), 201)
	if dependency.DependencyIDs == nil || len(dependency.DependencyIDs) != 0 {
		t.Fatalf("empty dependencies must be an array: %+v", dependency)
	}
	body := fmt.Sprintf(`{"title":" exact title ","description":" exact description ","acceptance":" criteria ","assignee_id":%q,"dependency_ids":[%q]}`, member.ID, dependency.ID)
	first := a.request("POST", path, body, "task-key")
	task := decoded[domain.Task](t, first, 201)
	if task.ID == "" || task.GroupID != g.ID || task.Title != " exact title " || task.Description != " exact description " || task.Acceptance != " criteria " ||
		task.AssigneeID != member.ID || !reflect.DeepEqual(task.DependencyIDs, []string{dependency.ID}) || task.Status != domain.TaskStatusOpen || task.Revision != 1 ||
		task.ActiveAttemptID != "" || task.CurrentSubmissionID != "" || task.CreatedAt.IsZero() || task.CreatedAt.Location() != time.UTC {
		t.Fatalf("created task contract: %+v", task)
	}
	if got := decoded[domain.Task](t, a.request("GET", path+"/"+task.ID, ""), 200); !reflect.DeepEqual(got, task) {
		t.Fatalf("task detail changed: %+v", got)
	}
	retry := a.request("POST", path, body, "task-key")
	if retry.Code != 201 || !bytes.Equal(first.Body.Bytes(), retry.Body.Bytes()) {
		t.Fatalf("task replay changed: %d %s", retry.Code, retry.Body)
	}
	w := a.request("POST", path, strings.Replace(body, " exact title ", "changed", 1), "task-key")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
	foreign := decoded[domain.Task](t, a.request("POST", otherPath, plainBody, "task-key"), 201)
	if foreign.ID == task.ID || foreign.GroupID != other.ID {
		t.Fatalf("task idempotency crossed groups: %+v", foreign)
	}
	page := decoded[listResult[domain.Task]](t, a.request("GET", path, ""), 200)
	isolated := decoded[listResult[domain.Task]](t, a.request("GET", otherPath, ""), 200)
	if !reflect.DeepEqual(page.Items, []domain.Task{task, dependency}) || page.NextCursor != "" || !reflect.DeepEqual(isolated.Items, []domain.Task{foreign}) {
		t.Fatalf("task list/detail or group isolation differs: %+v %+v", page, isolated)
	}
	for _, target := range []string{otherPath + "/" + task.ID, path + "/" + foreign.ID, path + "/missing", "/api/v1/groups/missing/tasks", "/api/v1/groups/missing/tasks/" + task.ID} {
		w := a.request("GET", target, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	w = a.request("POST", "/api/v1/groups/missing/tasks", plainBody)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	for _, tt := range []struct{ method, target string }{{"DELETE", path}, {"PATCH", path + "/" + task.ID}, {"POST", path + "/" + task.ID}} {
		w := a.request(tt.method, tt.target, body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTaskAPI(t, a.path)
	retry = reopened.request("POST", path, body, "task-key")
	if retry.Code != 201 || !bytes.Equal(first.Body.Bytes(), retry.Body.Bytes()) {
		t.Fatalf("task HTTP replay did not survive reopen: %d %s", retry.Code, retry.Body)
	}
}

func TestTaskHTTPRejectsInvalidInputWithoutWrites(t *testing.T) {
	a := openTaskAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	path := "/api/v1/groups/" + g.ID + "/tasks"
	valid := `{"title":"task","acceptance":"criteria"}`
	dependency := decoded[domain.Task](t, a.request("POST", path, valid), 201)
	foreignTask := decoded[domain.Task](t, a.request("POST", "/api/v1/groups/"+other.ID+"/tasks", valid), 201)
	foreignMember := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+other.ID+"/members", `{"name":"foreign","role":"role"}`), 201)
	raw := messageSQL(t, a)
	assertNoWrites := func(t *testing.T) {
		t.Helper()
		for table, want := range map[string]int{"tasks": 2, "task_dependencies": 0, "mutation_idempotency": 0} {
			var count int
			if err := raw.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
				t.Fatalf("rejected request wrote %s: %d, %v; want %d", table, count, err, want)
			}
		}
	}
	for i, body := range []string{
		`null`, `[]`, `{`, valid + ` {}`, `{"title":42,"acceptance":"criteria"}`,
		`{"title":"task","acceptance":{}}`, `{"title":"task","acceptance":"criteria","dependency_ids":"task"}`,
		`{"title":"task","acceptance":"criteria","dependency_ids":[42]}`,
		`{"title":"task","title":"again","acceptance":"criteria"}`,
		`{"title":"task","Title":"again","acceptance":"criteria"}`,
		`{"title":"task","acceptance":"criteria","dependency_ids":[],"dependency_ids":[]}`,
	} {
		t.Run(fmt.Sprintf("json-%d", i), func(t *testing.T) {
			w := a.request("POST", path, body, "rejected")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			assertNoWrites(t)
		})
	}
	for _, field := range []struct{ name, value string }{
		{"extra", `true`}, {"actor_id", `"forged"`}, {"ActorID", `"forged"`}, {"group_id", fmt.Sprintf("%q", other.ID)},
		{"status", `"accepted"`}, {"revision", `99`}, {"id", `"forged"`}, {"idempotency_key", `"forged"`},
		{"active_attempt_id", `"forged"`}, {"current_submission_id", `"forged"`}, {"created_at", `"2026-01-01T00:00:00Z"`},
	} {
		t.Run(field.name, func(t *testing.T) {
			body := strings.TrimSuffix(valid, "}") + `,"` + field.name + `":` + field.value + `}`
			w := a.request("POST", path, body, "rejected")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			assertNoWrites(t)
		})
	}
	for _, body := range []string{
		`{}`, `{"title":" \t","acceptance":"criteria"}`, `{"title":"task"}`, `{"title":"task","acceptance":" \t"}`,
		`{"title":"task","acceptance":"criteria","assignee_id":" "}`, `{"title":"task","acceptance":"criteria","dependency_ids":[" "]}`,
	} {
		w := a.request("POST", path, body, "rejected")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		assertNoWrites(t)
	}
	for _, keys := range [][]string{{""}, {"same", "same"}, {"a,b"}, {"has space"}, {strings.Repeat("x", 129)}} {
		w := a.request("POST", path, valid, keys...)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		assertNoWrites(t)
	}
	for _, body := range []string{
		fmt.Sprintf(`{"title":"task","acceptance":"criteria","assignee_id":%q}`, foreignMember.ID),
		`{"title":"task","acceptance":"criteria","assignee_id":"missing"}`,
		fmt.Sprintf(`{"title":"task","acceptance":"criteria","dependency_ids":[%q,%q]}`, dependency.ID, foreignTask.ID),
		`{"title":"task","acceptance":"criteria","dependency_ids":["missing"]}`,
	} {
		w := a.request("POST", path, body, "rejected")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
		assertNoWrites(t)
	}
	for _, method := range []string{"GET", "POST"} {
		for _, query := range []string{"limit=1", "before=cursor", "actor_id=forged", "unknown="} {
			target := path
			if method == "GET" {
				target += "/" + dependency.ID
			}
			w := a.request(method, target+"?"+query, valid, "rejected")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
			assertNoWrites(t)
		}
	}
	decoded[domain.Task](t, a.request("POST", path, valid, "rejected"), 201)
}

func TestTaskHTTPPaginationBoundsAndCursorScope(t *testing.T) {
	a := openTaskAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	path := "/api/v1/groups/" + g.ID + "/tasks"
	want := make([]domain.Task, 0, 52)
	for range 52 {
		task, err := a.db.CreateTask(context.Background(), store.CreateTaskInput{GroupID: g.ID, ActorID: "user", Title: "task", Acceptance: "criteria"})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, task)
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	defaults := decoded[listResult[domain.Task]](t, a.request("GET", path, ""), 200)
	maximum := decoded[listResult[domain.Task]](t, a.request("GET", path+"?limit=200", ""), 200)
	minimum := decoded[listResult[domain.Task]](t, a.request("GET", path+"?limit=1", ""), 200)
	if !reflect.DeepEqual(defaults.Items, want[:50]) || defaults.NextCursor == "" || !reflect.DeepEqual(maximum.Items, want) || maximum.NextCursor != "" ||
		!reflect.DeepEqual(minimum.Items, want[:1]) || minimum.NextCursor == "" {
		t.Fatalf("task limit bounds: default=%d max=%d min=%d", len(defaults.Items), len(maximum.Items), len(minimum.Items))
	}
	first := decoded[listResult[domain.Task]](t, a.request("GET", path+"?limit=26", ""), 200)
	if !reflect.DeepEqual(first.Items, want[:26]) || first.NextCursor == "" {
		t.Fatalf("task first page: %+v", first)
	}
	raw, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	var cursor pageCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Version != 1 || cursor.Scope != "tasks" || cursor.GroupID != g.ID ||
		!cursor.CreatedAt.Equal(want[25].CreatedAt) || cursor.ID != want[25].ID {
		t.Fatalf("task cursor scope or position: %s, %v", raw, err)
	}
	decoded[domain.Task](t, a.request("POST", path, `{"title":"later insert","acceptance":"criteria"}`), 201)
	tail := decoded[listResult[domain.Task]](t, a.request("GET", path+"?limit=26&before="+first.NextCursor, ""), 200)
	if !reflect.DeepEqual(tail.Items, want[26:]) || tail.NextCursor != "" {
		t.Fatalf("exact-full task tail skipped, duplicated, or included a later insert: %+v", tail)
	}
	last := want[len(want)-1]
	cursor.CreatedAt, cursor.ID = last.CreatedAt, last.ID
	raw, err = json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	end := base64.RawURLEncoding.EncodeToString(raw)
	empty := decoded[listResult[domain.Task]](t, a.request("GET", path+"?before="+end, ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty task tail must contain an array: %+v", empty)
	}
	groups := decoded[listResult[domain.Group]](t, a.request("GET", "/api/v1/groups?limit=1", ""), 200)
	if groups.NextCursor == "" {
		t.Fatal("missing cross-scope cursor fixture")
	}
	for _, target := range []string{
		"/api/v1/groups/" + other.ID + "/tasks?before=" + first.NextCursor,
		path + "?before=" + groups.NextCursor,
		"/api/v1/groups?before=" + first.NextCursor,
		"/api/v1/groups/" + g.ID + "/members?before=" + first.NextCursor,
	} {
		w := a.request("GET", target, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, query := range []string{
		"limit=0", "limit=-1", "limit=201", "limit=x", "limit=", "limit=1&limit=2", "limit=1&%6cimit=2",
		"before=", "before=invalid", "before=" + first.NextCursor + "&before=" + first.NextCursor,
		"after=x", "after=", "unknown=x", "group_id=" + other.ID, "status=open", "limit=%ZZ",
	} {
		w := a.request("GET", path+"?"+query, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
}

func TestTaskHTTPRoutesRemainOptIn(t *testing.T) {
	a := openGroupAPI(t, "")
	g := newGroup(t, a, "group")
	w := a.request("GET", "/api/v1/groups/"+g.ID+"/tasks", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
}
