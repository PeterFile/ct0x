package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
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
	"agentworkspace/internal/store"
)

func TestRunCoordinationRoutes(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--listen", "127.0.0.1:0", "--data-dir", dir}
	base, stop := startRunWithSearch(t, provider.Search{}, args...)
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	requestJSON := func(method, path, body, key string, status int, result any, alters ...func(*http.Request)) []byte {
		t.Helper()
		request, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		for _, alter := range alters {
			if alter != nil {
				alter(request)
			}
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || !json.Valid(data) {
			t.Fatalf("%s %s: status=%d type=%q body=%s err=%v", method, path, response.StatusCode, response.Header.Get("Content-Type"), data, err)
		}
		if status >= 400 && !chinese(string(data)) {
			t.Fatalf("%s %s: error must be Chinese JSON: %s", method, path, data)
		}
		if result != nil {
			if err := json.Unmarshal(data, result); err != nil {
				t.Fatal(err)
			}
		}
		return data
	}

	var group, other domain.Group
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "工作授权接线"}, &group)
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "隔离群组"}, &other)
	groupPath, otherPath := "/api/v1/groups/"+group.ID, "/api/v1/groups/"+other.ID
	var member domain.Member
	executionJSON(t, "POST", base+groupPath+"/members", platform.MemberInput{Name: "观察成员", Role: "不接收公开消息投递"}, &member)
	const topic = " exact\ntopic "
	const content = "普通公开讨论不是工作授权"
	var message domain.Message
	executionJSON(t, "POST", base+groupPath+"/messages", platform.MessageInput{Content: content, RecipientIDs: []string{}, TopicID: topic}, &message)
	if message.ID == "" || message.GroupID != group.ID || message.TopicID != topic || message.SenderID != "user" || message.Content != content || message.Deliveries == nil || len(message.Deliveries) != 0 {
		t.Fatalf("public-only message changed or queued a delivery: %+v", message)
	}
	coordinationPath := groupPath + "/coordination"
	requestPath := coordinationPath + "/requests"
	topicQuery := "?topic_id=" + url.QueryEscape(topic)
	var tasks struct {
		Items      []domain.Task `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	var triggers struct {
		Items      []domain.Trigger `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	requestJSON("GET", groupPath+"/tasks", "", "", 200, &tasks)
	requestJSON("GET", coordinationPath+"/triggers"+topicQuery, "", "", 200, &triggers)
	if tasks.Items == nil || len(tasks.Items) != 0 || tasks.NextCursor != "" || triggers.Items == nil || len(triggers.Items) != 0 || triggers.NextCursor != "" {
		t.Fatalf("ordinary message created work: tasks=%+v triggers=%+v", tasks, triggers)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	sourceSequence := coordinationRunDurableCounts(t, dir, message, 0)
	t.Log("ordinary public message persisted without Task, admission, delivery, or Turn")

	input := domain.RequestWorkRequest{
		TopicID: topic, MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew, Reason: " 用户明确要求开展工作 ",
		NewTask: &domain.NewTaskRequest{Title: " 接线目标 ", Description: " 授权时的描述 ", Acceptance: " 通过本地 HTTP 查询授权 ", DependencyIDs: []string{}},
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	wrongTopic := input
	wrongTopic.TopicID += " "
	wrongTopicData, err := json.Marshal(wrongTopic)
	if err != nil {
		t.Fatal(err)
	}
	const key = "composition"
	base, stop = startRunWithSearch(t, provider.Search{}, args...)
	for _, test := range []struct {
		name, path, body, key string
		status                int
		code                  string
		alter                 func(*http.Request)
	}{
		{"malformed", requestPath, "{", key, 400, "invalid_json", nil},
		{"unknown_field", requestPath, strings.TrimSuffix(body, "}") + `,"actor_id":"forged"}`, key, 400, "invalid_json", nil},
		{"invalid_union", requestPath, strings.TrimSuffix(body, "}") + `,"existing_task":{"task_id":"foreign-task","expected_revision":1}}`, key, 400, "invalid_input", nil},
		{"missing_key", requestPath, body, "", 400, "invalid_input", nil},
		{"foreign_group", otherPath + "/coordination/requests", body, key, 404, "not_found", nil},
		{"foreign_topic", requestPath, string(wrongTopicData), key, 404, "not_found", nil},
		{"host", requestPath, body, key, 403, "invalid_host", func(r *http.Request) { r.Host = "evil.invalid" }},
		{"origin", requestPath, body, key, 403, "invalid_origin", func(r *http.Request) {
			r.Header.Set("Origin", "http://evil.invalid")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		}},
	} {
		var failure struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		requestJSON("POST", test.path, test.body, test.key, test.status, &failure, test.alter)
		if failure.Error.Code != test.code || !chinese(failure.Error.Message) {
			t.Fatalf("%s: unexpected error: %+v", test.name, failure)
		}
		t.Logf("rejected %s: %d %s", test.name, test.status, failure.Error.Code)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := coordinationRunDurableCounts(t, dir, message, 0); got != sourceSequence {
		t.Fatalf("rejections changed source sequence: %d != %d", got, sourceSequence)
	}

	base, stop = startRunWithSearch(t, provider.Search{}, args...)
	var receipt domain.AdmissionReceipt
	first := requestJSON("POST", requestPath, body, key, 201, &receipt, func(r *http.Request) {
		r.Header.Set("Origin", base)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	})
	decision, mandate := receipt.Decision, receipt.Mandate
	if decision.ID == "" || decision.TriggerID == "" || mandate.ID == "" || receipt.Task.ID == "" || decision.CreatedAt.IsZero() || mandate.CreatedAt.IsZero() {
		t.Fatalf("missing server-assigned admission identity: %+v", receipt)
	}
	wantReceipt := domain.AdmissionReceipt{
		Decision: domain.RoutingDecision{
			ID: decision.ID, TriggerID: decision.TriggerID, GroupID: group.ID, TopicID: topic,
			Kind: domain.RoutingDecisionCreateNew, ActorID: "user", PolicyRevision: domain.LocalUserExplicitPolicyRevision,
			Reason: input.Reason, TaskID: receipt.Task.ID, TaskRevisionAtAdmission: 1, MandateID: mandate.ID, CreatedAt: decision.CreatedAt,
		},
		Mandate: domain.Mandate{
			ID: mandate.ID, DecisionID: decision.ID, OriginTriggerID: decision.TriggerID, GroupID: group.ID, TopicID: topic,
			TaskID: receipt.Task.ID, TaskRevisionAtAdmission: 1, AuthorityKind: domain.MandateAuthorityLocalUserExplicit,
			AuthorizedBy: "user", Strength: domain.MandateStrengthExplicit, PolicyRevision: domain.LocalUserExplicitPolicyRevision,
			Scope:     domain.TaskScope{Objective: input.NewTask.Title, Description: input.NewTask.Description, Acceptance: input.NewTask.Acceptance, DependencyIDs: []string{}},
			CreatedAt: mandate.CreatedAt,
		},
		Task: domain.TaskRef{ID: receipt.Task.ID, GroupID: group.ID, RevisionAtAdmission: 1},
	}
	if !reflect.DeepEqual(receipt, wantReceipt) {
		t.Fatalf("admission linkage or immutable scope changed: got=%+v want=%+v", receipt, wantReceipt)
	}
	var wire struct {
		Task map[string]json.RawMessage `json:"task"`
	}
	if err := json.Unmarshal(first, &wire); err != nil || len(wire.Task) != 3 || wire.Task["id"] == nil || wire.Task["group_id"] == nil || wire.Task["revision_at_admission"] == nil {
		t.Fatalf("receipt must contain only historical TaskRef, not live Task: %s err=%v", first, err)
	}
	checkReadsAndReplay := func() {
		t.Helper()
		var trigger domain.Trigger
		requestJSON("GET", coordinationPath+"/triggers/"+decision.TriggerID+topicQuery, "", "", 200, &trigger)
		wantTrigger := domain.Trigger{ID: decision.TriggerID, SourceKind: domain.TriggerSourceLocalIM, GroupID: group.ID, TopicID: topic, MessageID: message.ID, MessageSequence: sourceSequence, CreatedAt: trigger.CreatedAt}
		requestJSON("GET", coordinationPath+"/triggers"+topicQuery, "", "", 200, &triggers)
		if trigger.CreatedAt.IsZero() || !reflect.DeepEqual(trigger, wantTrigger) || !reflect.DeepEqual(triggers.Items, []domain.Trigger{trigger}) || triggers.NextCursor != "" {
			t.Fatalf("exact source tuple or list changed: trigger=%+v page=%+v", trigger, triggers)
		}
		if got := requestJSON("GET", coordinationPath+"/decisions/"+decision.ID+topicQuery, "", "", 200, nil); !bytes.Equal(got, first) {
			t.Fatalf("historical decision changed: %s != %s", got, first)
		}
		var view domain.TaskCoordinationView
		requestJSON("GET", groupPath+"/tasks/"+receipt.Task.ID+"/coordination", "", "", 200, &view)
		wantCoordination := domain.TaskCoordination{TaskID: receipt.Task.ID, GroupID: group.ID, TopicID: topic, CurrentMandateID: mandate.ID, AuthorityGeneration: 1}
		if view.Coordination != wantCoordination || !reflect.DeepEqual(view.Admission, receipt) {
			t.Fatalf("current pointer does not select the exact admission: %+v", view)
		}
		if got := requestJSON("POST", requestPath, body, key, 201, nil); !bytes.Equal(got, first) {
			t.Fatalf("same-key 201 replay changed immutable receipt: %s != %s", got, first)
		}
		var task domain.Task
		requestJSON("GET", groupPath+"/tasks/"+receipt.Task.ID, "", "", 200, &task)
		requestJSON("GET", groupPath+"/tasks", "", "", 200, &tasks)
		if task.ID != receipt.Task.ID || task.GroupID != group.ID || task.Revision != 1 || task.Status != domain.TaskStatusOpen || task.Title != input.NewTask.Title || task.Description != input.NewTask.Description || task.Acceptance != input.NewTask.Acceptance || task.AssigneeID != "" || len(task.DependencyIDs) != 0 || task.ActiveAttemptID != "" || task.CurrentSubmissionID != "" || !reflect.DeepEqual(tasks.Items, []domain.Task{task}) || tasks.NextCursor != "" {
			t.Fatalf("Task routes lost admitted work or auto-dispatched it: task=%+v page=%+v", task, tasks)
		}
		var history store.HistoryPage
		var detail domain.Message
		requestJSON("GET", groupPath+"/history?q="+url.QueryEscape(content), "", "", 200, &history)
		requestJSON("GET", groupPath+"/history/"+message.ID, "", "", 200, &detail)
		if !reflect.DeepEqual(history.Items, []domain.Message{message}) || history.NextCursor != "" || !reflect.DeepEqual(detail, message) {
			t.Fatalf("History routes or public message changed: history=%+v detail=%+v", history, detail)
		}
	}
	checkIsolation := func() {
		t.Helper()
		notFound := requestJSON("GET", coordinationPath+"/decisions/missing"+topicQuery, "", "", 404, nil)
		for _, path := range []string{
			otherPath + "/coordination/triggers/" + decision.TriggerID + topicQuery,
			otherPath + "/coordination/decisions/" + decision.ID + topicQuery,
			otherPath + "/tasks/" + receipt.Task.ID + "/coordination",
			coordinationPath + "/triggers/" + decision.TriggerID + "?topic_id=" + url.QueryEscape(topic+" "),
			coordinationPath + "/decisions/" + decision.ID + "?topic_id=" + url.QueryEscape(topic+" "),
			coordinationPath + "/triggers/" + decision.TriggerID,
			coordinationPath + "/decisions/" + decision.ID + "?topic_id=",
		} {
			if got := requestJSON("GET", path, "", "", 404, nil); !bytes.Equal(got, notFound) {
				t.Fatalf("foreign scope disclosed identity: %s != %s", got, notFound)
			}
		}
		for _, path := range []string{
			coordinationPath + "/triggers",
			coordinationPath + "/triggers?topic_id=",
			coordinationPath + "/triggers?topic_id=" + url.QueryEscape(topic+" "),
			otherPath + "/coordination/triggers" + topicQuery,
		} {
			requestJSON("GET", path, "", "", 200, &triggers)
			if triggers.Items == nil || len(triggers.Items) != 0 || triggers.NextCursor != "" {
				t.Fatalf("scoped list disclosed a foreign topic/group: %s: %+v", path, triggers)
			}
		}
	}
	checkIsolation()
	checkReadsAndReplay()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := coordinationRunDurableCounts(t, dir, message, 1); got != sourceSequence {
		t.Fatalf("admission changed source sequence: %d != %d", got, sourceSequence)
	}
	t.Log("five coordination routes, exact source tuple, immutable 201 replay, Task/History, and scope isolation passed")

	base, stop = startRunWithSearch(t, provider.Search{}, args...)
	checkIsolation()
	checkReadsAndReplay()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := coordinationRunDurableCounts(t, dir, message, 1); got != sourceSequence {
		t.Fatalf("restart changed source sequence: %d != %d", got, sourceSequence)
	}
	t.Log("clean restart preserved receipt, scoped reads, and byte-identical same-key 201 replay; queue remained empty")
}

func coordinationRunDurableCounts(t *testing.T, dir string, message domain.Message, admissions int) int64 {
	t.Helper()
	dsn := url.URL{Scheme: "file", Path: filepath.Join(dir, "workspace.db"), RawQuery: "mode=ro"}
	raw, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	}()
	want := map[string]int{
		"groups": 2, "members": 1, "messages": 1, "message_log": 1,
		"tasks": admissions, "coordination_triggers": admissions, "routing_decisions": admissions,
		"mandates": admissions, "task_coordination": admissions, "mutation_idempotency": admissions,
		"conversations": 0, "deliveries": 0, "turns": 0, "resource_claims": 0,
		"task_dependencies": 0, "task_attempts": 0, "task_submissions": 0, "task_review_decisions": 0,
	}
	counts := make(map[string]int, len(want))
	for table := range want {
		var count int
		if err := raw.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("unexpected durable counts after stop: got=%v want=%v", counts, want)
	}
	var sequence int64
	if err := raw.QueryRow("SELECT sequence FROM message_log WHERE message_id=? AND group_id=? AND topic_id=?", message.ID, message.GroupID, message.TopicID).Scan(&sequence); err != nil || sequence < 1 {
		t.Fatalf("missing durable source tuple: sequence=%d err=%v", sequence, err)
	}
	t.Logf("durable counts after stop: %v; source_sequence=%d", counts, sequence)
	return sequence
}
