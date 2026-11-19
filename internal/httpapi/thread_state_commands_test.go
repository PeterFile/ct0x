package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestThreadStateCommandIsolationAndReplay(t *testing.T) {
	a := openMessageAPI(t, "")
	ctx := context.Background()
	g := newGroup(t, a, "thread state")
	const topic = "bug"
	source := threadStateMessage(t, a, g.ID, topic, "Keep the old CSV format.")
	url := threadStateURL(g.ID, topic)
	body := stateUpdate(0, 0, "constraint", "set", source.Content, source.ID)
	if !threadStateInstalled(t, a) {
		assertThreadStateUnavailable(t, a, url, body)
		return
	}
	other := newGroup(t, a, "other group")
	f := threadStateMessage(t, a, other.ID, topic, "PRIVATE_FOREIGN_SOURCE")
	a2 := openMessageAPI(t, a.path) // Independent connections exercise SQLite serialization.
	put := func(a *groupAPI, expected int, text, sourceID, key string) *httptest.ResponseRecorder {
		return a.request("PUT", url, stateUpdate(expected, 0, "constraint", "set", text, sourceID), key)
	}
	crossOrigin := httptest.NewRequest("PUT", "http://"+DefaultAddress+url, strings.NewReader(body))
	crossOrigin.Header.Set("Origin", "http://other.invalid")
	crossOrigin.Header.Set("Content-Type", "application/json")
	crossOrigin.Header.Set("Idempotency-Key", "same")
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, crossOrigin)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "invalid_origin")
	decoded[map[string]any](t, a.request("PUT", url, body), 400)
	decoded[map[string]any](t, a.request("PUT", url, strings.TrimSuffix(body, "}")+`,"confirmed_by":"user"}`, "spoof"), 400)
	decoded[map[string]any](t, put(a, 0, f.Content, f.ID, "same"), 404)
	decoded[map[string]any](t, a.request("PUT", threadStateURL(g.ID, ""), body, "same"), 404)
	legacy, err := a.db.CreateMessage(ctx, store.MessageRequest{GroupID: g.ID, TopicID: topic, ActorID: "user", Content: "unverified user label"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded[map[string]any](t, put(a, 0, legacy.Content, legacy.ID, "same"), 404)
	decoded[map[string]any](t, put(a, 0, "not in the source", source.ID, "same"), 400)
	decoded[map[string]any](t, put(a, 0, strings.Repeat("旧", 342), source.ID, "same"), 400)

	race := func(first, second func() *httptest.ResponseRecorder) [2]*httptest.ResponseRecorder {
		start := make(chan struct{})
		result := [2]chan *httptest.ResponseRecorder{make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)}
		for i, fn := range []func() *httptest.ResponseRecorder{first, second} {
			go func() { <-start; result[i] <- fn() }()
		}
		close(start)
		return [2]*httptest.ResponseRecorder{<-result[0], <-result[1]}
	}
	both := race(func() *httptest.ResponseRecorder { return put(a, 0, source.Content, source.ID, "same") },
		func() *httptest.ResponseRecorder { return put(a2, 0, source.Content, source.ID, "same") })
	for _, w := range both {
		decoded[map[string]any](t, w, 200)
	}
	if !bytes.Equal(both[0].Body.Bytes(), both[1].Body.Bytes()) {
		t.Fatal("concurrent replay returned different immutable receipts")
	}
	decoded[map[string]any](t, put(a, 0, "old CSV", source.ID, "same"), 409)
	decoded[map[string]any](t, put(a, 1, source.Content, source.ID, "stale-source"), 409)

	next := threadStateMessage(t, a, g.ID, topic, "Keep the old and new CSV formats.")
	p := race(func() *httptest.ResponseRecorder { return put(a, 1, next.Content, next.ID, "first") },
		func() *httptest.ResponseRecorder { return put(a2, 1, next.Content, next.ID, "second") })
	if p[0].Code > p[1].Code {
		p[0], p[1] = p[1], p[0]
	}
	decoded[map[string]any](t, p[0], 200)
	assertError(t, p[1].Code, p[1].Header(), p[1].Body.Bytes(), 409, "thread_state_conflict")
	for _, connection := range []*store.Store{a.db, a2.db} {
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
	}
	a = openMessageAPI(t, a.path)
	for _, w := range []*httptest.ResponseRecorder{
		put(a, 0, source.Content, source.ID, "same"), a.request("GET", url+"&revision=1", ""),
	} {
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), both[0].Body.Bytes()) {
			t.Fatalf("read/replay lost the old revision: %d %s", w.Code, w.Body)
		}
	}
	for _, path := range []string{threadStateURL(g.ID, "") + "&revision=1", threadStateURL(other.ID, topic) + "&revision=1", url + "&revision=3"} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
		if strings.Contains(w.Body.String(), next.Content) || strings.Contains(w.Body.String(), f.Content) {
			t.Fatal("an inaccessible historical query revealed content")
		}
	}
	if w := a.request("GET", threadStateURL(other.ID, topic), ""); w.Code != 200 || strings.Contains(w.Body.String(), next.Content) {
		t.Fatalf("foreign thread returned an invalid state: %d %s", w.Code, w.Body)
	}
	raw := messageSQL(t, a)
	for _, statement := range []string{
		`UPDATE thread_state_revisions SET content='rewritten'`,
		`DELETE FROM thread_state_revisions`,
		`INSERT OR REPLACE INTO thread_state_revisions SELECT * FROM thread_state_revisions WHERE revision=1`,
	} {
		if _, err := raw.Exec(statement); err == nil {
			t.Fatalf("mutable state history: %s", statement)
		}
	}
	// Force the second write in the unit of work to fail; neither the new
	// revision nor its key should survive the transaction.
	if _, err := raw.Exec(`CREATE TRIGGER fail_state_key BEFORE INSERT ON mutation_idempotency
	 WHEN NEW.operation='set_thread_state' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	last := threadStateMessage(t, a, g.ID, topic, "Export safely.")
	lastUpdate := stateUpdate(2, 1, "goal", "set", last.Content, last.ID)
	decoded[map[string]any](t, a.request("PUT", url, lastUpdate, "atomic"), 500)
	if _, err := raw.Exec(`DROP TRIGGER fail_state_key`); err != nil {
		t.Fatal(err)
	}
	current := decoded[domain.ThreadStateSnapshot](t, a.request("GET", url, ""), 200)
	if current.Revision != 2 {
		t.Fatalf("failed receipt left a state revision: %+v", current)
	}
	current = decoded[domain.ThreadStateSnapshot](t, a.request("PUT", url, lastUpdate, "atomic"), 200)
	if current.Revision != 3 || len(current.Records) != 2 {
		t.Fatalf("failed write reserved its key or changed an unrelated slot: %+v", current)
	}
	if _, err := raw.Exec(`DROP TRIGGER thread_state_no_update`); err != nil {
		t.Fatal(err)
	}
	for _, w := range []*httptest.ResponseRecorder{a.request("GET", url, ""), a.request("PUT", url, lastUpdate, "atomic")} {
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
	}
}

func TestThreadStateConfirmationRequiresWholeMessage(t *testing.T) {
	a := openMessageAPI(t, "")
	installed := threadStateInstalled(t, a)
	g := newGroup(t, a, "whole message")
	const topic = "bug"
	url := threadStateURL(g.ID, topic)
	for slot, text := range []string{"不要删除数据", " 引用别人的建议：‘删除数据’；我不同意。 "} {
		m := threadStateMessage(t, a, g.ID, topic, text)
		whole := stateUpdate(slot, slot, "confirmed_decision", "set", text, m.ID)
		if !installed {
			assertThreadStateUnavailable(t, a, url, whole)
			return
		}
		key := fmt.Sprintf("decision-%d", slot)
		w := a.request("PUT", url, stateUpdate(slot, slot, "confirmed_decision", "set", "删除数据", m.ID), key)
		if w.Code != 400 {
			t.Errorf("fragment of %q was confirmed: %d %s", text, w.Code, w.Body)
			return
		}
		got := decoded[domain.ThreadStateSnapshot](t, a.request("PUT", url, whole, key), 200)
		if got.Revision != int64(slot+1) || got.Records[slot].Content != text {
			t.Fatalf("confirmation discarded original context: %+v", got)
		}
	}
}

func TestThreadStateActualTaskBinding(t *testing.T) {
	a := openMessageAPI(t, "")
	ctx := context.Background()
	g := newGroup(t, a, "home")
	source := threadStateMessage(t, a, g.ID, "export", "Keep old CSV syntax.")
	linked, err := a.db.RequestWork(ctx, g.ID, domain.RequestWorkRequest{TopicID: source.TopicID, MessageID: source.ID,
		Mode: domain.RoutingDecisionCreateNew, Reason: "fixture admission",
		NewTask: &domain.NewTaskRequest{Title: "linked", Acceptance: "valid CSV"}}, "admission")
	if err != nil {
		t.Fatal(err)
	}
	task := func(group, topic, id string, rev int) string {
		return threadStateURL(group, topic) + fmt.Sprintf("&task_id=%s&task_revision=%d", id, rev)
	}
	body := stateUpdate(0, 0, "constraint", "set", source.Content, source.ID)
	endpoint := task(g.ID, source.TopicID, linked.Task.ID, 1)
	if !threadStateInstalled(t, a) {
		assertThreadStateUnavailable(t, a, endpoint, body)
		return
	}
	other := newGroup(t, a, "foreign")
	unlinked, err := a.db.CreateTask(ctx, store.CreateTaskInput{GroupID: g.ID, ActorID: "user", Title: "unlinked", Acceptance: "valid CSV"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{task(g.ID, source.TopicID, unlinked.ID, 1), task(g.ID, "other", linked.Task.ID, 1), task(other.ID, source.TopicID, linked.Task.ID, 1)} {
		decoded[map[string]any](t, a.request("PUT", path, body, "task"), 404)
		decoded[map[string]any](t, a.request("GET", path, ""), 404)
	}
	stale := task(g.ID, source.TopicID, linked.Task.ID, 2)
	decoded[map[string]any](t, a.request("PUT", stale, body, "task"), 409)
	first := a.request("PUT", endpoint, body, "task")
	decoded[map[string]any](t, first, 200)
	w := a.request("GET", threadStateURL(g.ID, source.TopicID), "")
	if w.Code != 200 || strings.Contains(w.Body.String(), source.Content) {
		t.Fatalf("thread-only read discovered task-private state: %d %s", w.Code, w.Body)
	}
	raw := messageSQL(t, a)
	if _, err := raw.Exec(`UPDATE tasks SET revision=revision+1 WHERE group_id=? AND id=?`, g.ID, linked.Task.ID); err != nil {
		t.Fatal(err)
	}
	decoded[map[string]any](t, a.request("PUT", endpoint, body, "task"), 409)
	decoded[map[string]any](t, a.request("GET", endpoint, ""), 409)
	w = a.request("GET", endpoint+"&revision=1", "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("explicit historical read lost task-version evidence: %d %s", w.Code, w.Body)
	}
	current := a.request("GET", stale, "")
	if current.Code != 200 || strings.Contains(current.Body.String(), source.Content) {
		t.Fatalf("state cannot silently carry over to a different task version: %d %s", current.Code, current.Body)
	}
}
