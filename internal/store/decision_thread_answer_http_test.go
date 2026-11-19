package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func threadDecisionHTTP(t *testing.T, db *store.Store) http.Handler {
	t.Helper()
	server, err := httpapi.NewServer(httpapi.DefaultAddress, httpapi.LocalUserRoutes(platform.NewLocalUser(db))...)
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler
}

func threadDecisionRequest(h http.Handler, method, path string, body []byte, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+httpapi.DefaultAddress+path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestThreadDecisionAnswerHTTPAliasesAndHistoricalRead(t *testing.T) {
	ctx := context.Background()
	db, path, q, otherGroup := store.ThreadDecisionHTTPFixture(t)
	alias, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer alias.Close()
	handlers := [2]http.Handler{threadDecisionHTTP(t, db), threadDecisionHTTP(t, alias)}
	global, group := "/api/v1/me/decisions/"+q.ID+"/answer", "/api/v1/groups/"+q.GroupID+"/decisions/"+q.ID+"/answer"
	const inbox = "/api/v1/me/pending-decisions"
	assertInbox := func(want bool) {
		t.Helper()
		w := threadDecisionRequest(handlers[0], http.MethodGet, inbox, nil, "")
		var page struct{ Items []domain.DecisionRequest }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Items == nil ||
			want && (len(page.Items) != 1 || page.Items[0].ID != q.ID || page.Items[0].ThreadRef != q.ThreadRef) || !want && len(page.Items) != 0 {
			t.Fatalf("inbox (want original request=%t): %d %s", want, w.Code, w.Body)
		}
	}
	body := func(thread domain.ThreadRef, text string) []byte {
		t.Helper()
		b, err := json.Marshal(struct {
			domain.ThreadRef
			domain.DecisionAnswerInput
		}{thread, domain.DecisionAnswerInput{Version: q.Version, Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	const text = "只看工作日"
	answer := body(q.ThreadRef, text)
	assertInbox(true)
	foreign := domain.ThreadRef{GroupID: otherGroup, TopicID: q.TopicID}
	wrongTopic := domain.ThreadRef{GroupID: q.GroupID, TopicID: q.TopicID + " "}
	for _, bad := range []struct {
		path string
		body []byte
		want int
	}{
		{global, body(foreign, text), http.StatusConflict},
		{group, body(foreign, text), http.StatusNotFound},
		{"/api/v1/groups/" + otherGroup + "/decisions/" + q.ID + "/answer", body(foreign, text), http.StatusConflict},
		{global, body(wrongTopic, text), http.StatusConflict},
		{group, body(wrongTopic, text), http.StatusConflict},
	} {
		if w := threadDecisionRequest(handlers[0], http.MethodPost, bad.path, bad.body, "foreign"); w.Code != bad.want {
			t.Fatalf("wrong Thread %s: %d %s", bad.path, w.Code, w.Body)
		}
	}
	assertInbox(true)
	var replies [2]*httptest.ResponseRecorder
	var wg sync.WaitGroup
	for i, route := range []string{global, group} {
		wg.Go(func() {
			replies[i] = threadDecisionRequest(handlers[i], http.MethodPost, route, answer, []string{"global", "group"}[i])
		})
	}
	wg.Wait()
	var saved domain.DecisionAnswerReceipt
	if replies[0].Code != http.StatusOK || replies[1].Code != http.StatusOK || !bytes.Equal(replies[0].Body.Bytes(), replies[1].Body.Bytes()) ||
		json.Unmarshal(replies[0].Body.Bytes(), &saved) != nil || saved.ID == "" || saved.ThreadRef != q.ThreadRef || saved.TaskID != "" || saved.Text != text {
		t.Fatalf("aliases returned different or invalid receipts: %d %s / %d %s", replies[0].Code, replies[0].Body, replies[1].Code, replies[1].Body)
	}
	assertInbox(false)
	if w := threadDecisionRequest(handlers[0], http.MethodPost, group, body(q.ThreadRef, "包括周末"), "different"); w.Code != http.StatusConflict {
		t.Fatalf("second answer replaced the first: %d %s", w.Code, w.Body)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := alias.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h := threadDecisionHTTP(t, reopened)
	if w := threadDecisionRequest(h, http.MethodPost, group, answer, "global"); w.Code != http.StatusOK || !bytes.Equal(replies[0].Body.Bytes(), w.Body.Bytes()) {
		t.Fatalf("reopen did not retain transport dedup: %d %s", w.Code, w.Body)
	}
	page, err := reopened.ListPendingDeliveries(ctx, store.PendingOptions{Lane: store.PendingThreadDecisions, Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor != nil || page.Items[0].Delivery.ID != saved.WakeDeliveryID {
		t.Fatalf("aliases/restart did not share one wake: %+v %v", page, err)
	}
	if _, err := platform.NewLocalUser(reopened).CreateMessage(ctx, q.GroupID, platform.MessageInput{
		TopicID: q.TopicID, Content: "新的同话题消息使旧答复无法再次启动。", RecipientIDs: []string{},
	}, "supersede"); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{global, group} {
		if w := threadDecisionRequest(h, http.MethodPost, route, answer, "global"); w.Code != http.StatusConflict {
			t.Fatalf("stale source revived through alias %s: %d %s", route, w.Code, w.Body)
		}
	}
	read := func(thread domain.ThreadRef, version int64) *httptest.ResponseRecorder {
		query := url.Values{"group_id": {thread.GroupID}, "topic_id": {thread.TopicID}, "version": {strconv.FormatInt(version, 10)}}
		return threadDecisionRequest(h, http.MethodGet, global+"?"+query.Encode(), nil, "")
	}
	w := read(q.ThreadRef, q.Version)
	var view domain.DecisionAnswerView
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &view) != nil ||
		view.Receipt != saved || view.Handling != nil {
		t.Fatalf("lost historical saved-only receipt: %d %s", w.Code, w.Body)
	}
	for _, w := range []*httptest.ResponseRecorder{read(foreign, q.Version), read(wrongTopic, q.Version), read(q.ThreadRef, q.Version+1)} {
		if w.Code != http.StatusNotFound {
			t.Fatalf("historical receipt escaped its scope: %d %s", w.Code, w.Body)
		}
	}
}
