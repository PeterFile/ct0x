package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func openCoordinationAPI(t *testing.T, path string) *groupAPI {
	t.Helper()
	a := openGroupAPI(t, path)
	local := platform.NewLocalUser(a.db)
	routes := append(LocalUserRoutes(local), TaskRoutes(local)...)
	routes = append(routes, CoordinationRoutes(platform.NewCoordinations(coordination.New(a.db)))...)
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	return a
}

func coordinationHTTPSource(t *testing.T, a *groupAPI, groupID, topic string) domain.RequestWorkRequest {
	t.Helper()
	m, err := a.db.CreateMessage(context.Background(), store.MessageRequest{GroupID: groupID, TopicID: topic, ActorID: "user", Content: "出处不是授权命令"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return domain.RequestWorkRequest{TopicID: topic, MessageID: m.ID, Mode: domain.RoutingDecisionCreateNew, Reason: " 用户明确开展此工作 ", NewTask: &domain.NewTaskRequest{Title: " 目标 ", Description: " 描述 ", Acceptance: " 验收 "}}
}

func coordinationHTTPBody(t *testing.T, request domain.RequestWorkRequest) string {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func coordinationHTTPBase(groupID string) string {
	return "/api/v1/groups/" + groupID + "/coordination"
}

func coordinationHTTPGetPaths(receipt domain.AdmissionReceipt) []string {
	base := coordinationHTTPBase(receipt.Task.GroupID)
	topic := "?topic_id=" + url.QueryEscape(receipt.Decision.TopicID)
	return []string{
		base + "/triggers/" + receipt.Decision.TriggerID + topic,
		base + "/triggers" + topic,
		base + "/decisions/" + receipt.Decision.ID + topic,
		"/api/v1/groups/" + receipt.Task.GroupID + "/tasks/" + receipt.Task.ID + "/coordination",
	}
}

func coordinationHTTPRows(t *testing.T, raw *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := raw.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := [][]any{}
	for rows.Next() {
		values, targets := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func coordinationHTTPTableRows(t *testing.T, raw *sql.DB, table string) [][]any {
	t.Helper()
	columns := coordinationHTTPRows(t, raw, "SELECT name FROM pragma_table_xinfo('"+strings.ReplaceAll(table, "'", "''")+"') WHERE hidden<>1 ORDER BY cid")
	var order []string
	for _, column := range columns {
		quoted := `"` + strings.ReplaceAll(column[0].(string), `"`, `""`) + `"`
		// Types distinguish equal numeric values; BINARY avoids collation-dependent ties.
		order = append(order, "typeof("+quoted+")", quoted+" COLLATE BINARY")
	}
	return coordinationHTTPRows(t, raw, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`" ORDER BY `+strings.Join(order, ","))
}

func coordinationHTTPSnapshot(t *testing.T, raw *sql.DB) map[string][][]any {
	t.Helper()
	names := coordinationHTTPRows(t, raw, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	result := make(map[string][][]any, len(names))
	for _, name := range names {
		table := name[0].(string)
		result[table] = coordinationHTTPTableRows(t, raw, table)
	}
	return result
}

func coordinationHTTPUnchanged(t *testing.T, raw *sql.DB, before map[string][][]any) {
	t.Helper()
	after := coordinationHTTPSnapshot(t, raw)
	for table, want := range before {
		if !reflect.DeepEqual(after[table], want) {
			t.Errorf("unexpected change to %s", table)
		}
	}
}

func TestCoordinationHTTPCreateLinkAndExactHistoricalReplay(t *testing.T) {
	for _, mode := range []domain.RoutingDecisionKind{domain.RoutingDecisionCreateNew, domain.RoutingDecisionLinkExisting} {
		for _, outcome := range []domain.ReviewOutcome{domain.ReviewOutcomeAccepted, domain.ReviewOutcomeRejected} {
			t.Run(string(mode)+"/"+string(outcome), func(t *testing.T) {
				a := openCoordinationAPI(t, "")
				g := newGroup(t, a, "group")
				request := coordinationHTTPSource(t, a, g.ID, " exact\ntopic ")
				base := coordinationHTTPBase(g.ID)
				if mode == domain.RoutingDecisionLinkExisting {
					task := decoded[domain.Task](t, a.request("POST", "/api/v1/groups/"+g.ID+"/tasks", `{"title":"authoritative title","description":"saved description","acceptance":"saved criteria"}`), 201)
					request.Mode, request.NewTask = mode, nil
					request.ExistingTask = &domain.ExistingTaskRequest{TaskID: task.ID, ExpectedRevision: task.Revision}
				}
				body := coordinationHTTPBody(t, request)
				first := a.request("POST", base+"/requests", body, "history")
				receipt := decoded[domain.AdmissionReceipt](t, first, 201)
				if receipt.Decision.Kind != mode || receipt.Decision.ActorID != "user" || receipt.Decision.Reason != request.Reason || receipt.Mandate.AuthorizedBy != "user" || receipt.Mandate.Scope.DependencyIDs == nil {
					t.Fatalf("incorrect admission contract: %+v", receipt)
				}
				if mode == domain.RoutingDecisionLinkExisting && (receipt.Task.ID != request.ExistingTask.TaskID || receipt.Mandate.Scope.Objective != "authoritative title" || receipt.Mandate.Scope.Description != "saved description") {
					t.Fatalf("link replaced authoritative task: %+v", receipt)
				}
				raw := messageSQL(t, a)
				paths := coordinationHTTPGetPaths(receipt)
				check := func(a *groupAPI) {
					t.Helper()
					before := coordinationHTTPSnapshot(t, raw)
					get := a.request("GET", paths[2], "")
					if get.Code != 200 || !bytes.Equal(first.Body.Bytes(), get.Body.Bytes()) {
						t.Fatalf("decision GET substituted live task state: %d %s", get.Code, get.Body)
					}
					view := decoded[domain.TaskCoordinationView](t, a.request("GET", paths[3], ""), 200)
					if !reflect.DeepEqual(view.Admission, receipt) || view.Coordination.CurrentMandateID != receipt.Mandate.ID || view.Coordination.AuthorityGeneration != 1 {
						t.Fatalf("task coordination mixed snapshots: %+v", view)
					}
					trigger := decoded[domain.Trigger](t, a.request("GET", paths[0], ""), 200)
					page := decoded[listResult[domain.Trigger]](t, a.request("GET", paths[1], ""), 200)
					if trigger.MessageID != request.MessageID || !reflect.DeepEqual(page.Items, []domain.Trigger{trigger}) || page.NextCursor != "" {
						t.Fatalf("trigger detail/list: %+v %+v", trigger, page)
					}
					retry := a.request("POST", base+"/requests", body, "history")
					if retry.Code != 201 || !bytes.Equal(first.Body.Bytes(), retry.Body.Bytes()) {
						t.Fatalf("historical 201 replay changed: %d %s", retry.Code, retry.Body)
					}
					coordinationHTTPUnchanged(t, raw, before)
				}
				check(a)
				task := decoded[domain.Task](t, a.request("GET", "/api/v1/groups/"+g.ID+"/tasks/"+receipt.Task.ID, ""), 200)
				attempt, err := a.db.ActivateTaskAttempt(context.Background(), store.ActivateTaskAttemptInput{GroupID: g.ID, TaskID: task.ID, ActorID: "user", ExpectedRevision: task.Revision})
				if err != nil {
					t.Fatal(err)
				}
				check(a)
				submission, err := a.db.SubmitTask(context.Background(), store.SubmitTaskInput{GroupID: g.ID, TaskID: task.ID, AttemptID: attempt.ID, ActorID: "user", ExpectedRevision: task.Revision, Summary: "result"})
				if err != nil {
					t.Fatal(err)
				}
				current := decoded[domain.Task](t, a.request("GET", "/api/v1/groups/"+g.ID+"/tasks/"+task.ID, ""), 200)
				if current.Status != domain.TaskStatusSubmitted || current.CurrentSubmissionID != submission.ID || reflect.DeepEqual(current, task) {
					t.Fatalf("independent live task did not advance: %+v", current)
				}
				check(a)
				if _, err := a.db.ReviewTask(context.Background(), store.ReviewTaskInput{GroupID: g.ID, TaskID: task.ID, SubmissionID: submission.ID, ExpectedRevision: task.Revision, ActorID: "user", Outcome: outcome, Reason: "review"}); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec("UPDATE tasks SET title='live title',description='live description',acceptance='live criteria' WHERE id=?", task.ID); err != nil {
					t.Fatal(err)
				}
				current = decoded[domain.Task](t, a.request("GET", "/api/v1/groups/"+g.ID+"/tasks/"+task.ID, ""), 200)
				wantStatus, wantRevision := domain.TaskStatusAccepted, int64(1)
				if outcome == domain.ReviewOutcomeRejected {
					wantStatus, wantRevision = domain.TaskStatusOpen, 2
				}
				if current.Status != wantStatus || current.Revision != wantRevision || current.Description != "live description" || receipt.Mandate.Scope.Description == current.Description {
					t.Fatalf("live/historical separation: task=%+v receipt=%+v", current, receipt)
				}
				check(a)
				if err := a.db.Close(); err != nil {
					t.Fatal(err)
				}
				check(openCoordinationAPI(t, a.path))
			})
		}
	}
}

func TestCoordinationHTTPScopedReadsAndMainTopic(t *testing.T) {
	a := openCoordinationAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	raw := messageSQL(t, a)
	base := coordinationHTTPBase(g.ID)
	for i, topic := range []string{"", " ", " \n\t ", "nul\x00topic"} {
		request := coordinationHTTPSource(t, a, g.ID, topic)
		r := decoded[domain.AdmissionReceipt](t, a.request("POST", base+"/requests", coordinationHTTPBody(t, request), fmt.Sprintf("topic-%d", i)), 201)
		paths := coordinationHTTPGetPaths(r)
		before := coordinationHTTPSnapshot(t, raw)
		for _, path := range paths {
			w := a.request("GET", path, "")
			if w.Code != 200 {
				t.Fatalf("exact opaque topic failed: %s: %d %s", path, w.Code, w.Body)
			}
		}
		for _, path := range []string{
			strings.Replace(paths[0], "groups/"+g.ID, "groups/"+other.ID, 1),
			strings.Replace(paths[2], "groups/"+g.ID, "groups/"+other.ID, 1),
			strings.Replace(paths[3], "groups/"+g.ID, "groups/"+other.ID, 1),
			base + "/decisions/" + r.Decision.ID + "?topic_id=" + url.QueryEscape(topic+" "),
			base + "/triggers/" + r.Decision.TriggerID + "?topic_id=" + url.QueryEscape(topic+" "),
			base + "/decisions/" + r.Mandate.ID + "?topic_id=" + url.QueryEscape(topic),
			base + "/triggers/" + request.MessageID + "?topic_id=" + url.QueryEscape(topic),
		} {
			w := a.request("GET", path, "")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
		}
		for _, suffix := range []string{"", "?topic_id="} {
			status := 404
			if topic == "" {
				status = 200
			}
			for _, path := range []string{base + "/decisions/" + r.Decision.ID, base + "/triggers/" + r.Decision.TriggerID} {
				if w := a.request("GET", path+suffix, ""); w.Code != status {
					t.Fatalf("omitted/empty topic became wildcard for %q: %d %s", topic, w.Code, w.Body)
				}
			}
			page := decoded[listResult[domain.Trigger]](t, a.request("GET", base+"/triggers"+suffix, ""), 200)
			if len(page.Items) != 1 || page.Items[0].TopicID != "" {
				t.Fatalf("main topic list became wildcard: %+v", page)
			}
		}
		coordinationHTTPUnchanged(t, raw, before)
	}
	manual := decoded[domain.Task](t, a.request("POST", "/api/v1/groups/"+g.ID+"/tasks", `{"title":"manual","acceptance":"criteria"}`), 201)
	before := coordinationHTTPSnapshot(t, raw)
	for _, path := range []string{
		"/api/v1/groups/" + g.ID + "/tasks/" + manual.ID + "/coordination",
		"/api/v1/groups/" + g.ID + "/tasks/missing/coordination",
		base + "/decisions/missing", base + "/triggers/missing", "/api/v1/groups/missing/coordination/triggers",
	} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	coordinationHTTPUnchanged(t, raw, before)
}

func TestCoordinationHTTPRoutesOptInAndRequestURLUnchanged(t *testing.T) {
	a := openGroupAPI(t, "")
	g := newGroup(t, a, "group")
	base := coordinationHTTPBase(g.ID)
	for _, path := range []string{base + "/requests", base + "/triggers", base + "/triggers/x", base + "/decisions/x", "/api/v1/groups/" + g.ID + "/tasks/x/coordination"} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	routes := CoordinationRoutes(platform.NewCoordinations(coordination.New(a.db)))
	if len(routes) != 5 {
		t.Fatalf("unexpected route count: %d", len(routes))
	}
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	request := coordinationHTTPSource(t, a, g.ID, "")
	decoded[domain.AdmissionReceipt](t, a.request("POST", base+"/requests", coordinationHTTPBody(t, request), "url"), 201)
	for _, query := range []string{"topic_id=&limit=1", "limit=01&topic_id=%20%0A%20", "topic_id=&extra=x"} {
		r := httptest.NewRequest(http.MethodGet, "http://"+DefaultAddress+base+"/triggers?"+query, nil)
		originalURL, originalQuery := r.URL, r.URL.RawQuery
		w := httptest.NewRecorder()
		a.handler.ServeHTTP(w, r)
		if r.URL != originalURL || r.URL.RawQuery != originalQuery {
			t.Fatalf("route mutated caller URL: %q -> %q", originalQuery, r.URL.RawQuery)
		}
	}
}
