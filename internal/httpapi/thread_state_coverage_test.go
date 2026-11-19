package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func threadStateCoverageBody(expected, slot int, kind string, source domain.Message, coveredJSON string) string {
	body := stateUpdate(expected, slot, kind, "set", source.Content, source.ID)
	if coveredJSON != "" {
		return strings.TrimSuffix(body, "}") + `,"covered_through_message_id":` + coveredJSON + `}`
	}
	return body
}

func TestThreadStateCoverageCommand(t *testing.T) {
	a := openMessageAPI(t, "")
	if !threadStateInstalled(t, a) {
		t.Skip("requires the candidate migration overlay")
	}
	g, foreign := newGroup(t, a, "export"), newGroup(t, a, "foreign")
	const topic = "exact export"
	url := threadStateURL(g.ID, topic)
	source := threadStateMessage(t, a, g.ID, topic, "Keep old CSV support.")
	anchor := threadStateMessage(t, a, g.ID, topic, "First checkpoint.")
	later := threadStateMessage(t, a, g.ID, topic, "Later checkpoint.")
	wrongTopic := threadStateMessage(t, a, g.ID, "", "Do not cross topics.")
	wrongGroup := threadStateMessage(t, a, foreign.ID, topic, "Do not cross groups.")
	body := func(expected, slot int, coveredJSON string) string {
		return threadStateCoverageBody(expected, slot, "constraint", source, coveredJSON)
	}
	without := a.request("PUT", url, body(0, 0, ""), "old-caller")
	if got := decoded[domain.ThreadStateSnapshot](t, without, 200); got.CoveredThrough != nil {
		t.Fatalf("a confirmation source became coverage: %+v", got)
	}
	for _, id := range []string{wrongTopic.ID, wrongGroup.ID, "missing"} {
		w := a.request("PUT", url, body(1, 1, fmt.Sprintf("%q", id)), "mark")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	for _, invalid := range []string{`null`, `""`, `1`} {
		w := a.request("PUT", url, body(1, 1, invalid), "mark")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	mark := body(1, 1, fmt.Sprintf("%q", anchor.ID))
	first := a.request("PUT", url, mark, "mark")
	got := decoded[domain.ThreadStateSnapshot](t, first, 200)
	want := domain.ThreadStateCoverage{MessageID: anchor.ID}
	if err := messageSQL(t, a).QueryRow(`SELECT sequence FROM message_log WHERE message_id=?`, anchor.ID).Scan(&want.MessageLogPosition); err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || got.CoveredThrough == nil || *got.CoveredThrough != want {
		t.Fatalf("wrong explicit coverage: %+v", got)
	}
	w := a.request("PUT", url, body(2, 2, fmt.Sprintf("%q", source.ID)), "inherit")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "thread_state_conflict")
	inherited := a.request("PUT", url, body(2, 2, ""), "inherit")
	if got := decoded[domain.ThreadStateSnapshot](t, inherited, 200); got.Revision != 3 || got.CoveredThrough == nil || *got.CoveredThrough != want {
		t.Fatalf("omitting coverage did not preserve it: %+v", got)
	}
	if got := decoded[domain.ThreadStateSnapshot](t, a.request("PUT", url, body(3, 3, fmt.Sprintf("%q", later.ID)), "advance"), 200); got.CoveredThrough == nil || got.CoveredThrough.MessageID != later.ID {
		t.Fatalf("later anchor did not advance coverage: %+v", got)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = openMessageAPI(t, a.path)
	for _, tc := range []struct{ actual, original []byte }{
		{a.request("GET", url+"&revision=1", "").Body.Bytes(), without.Body.Bytes()},
		{a.request("GET", url+"&revision=2", "").Body.Bytes(), first.Body.Bytes()},
		{a.request("PUT", url, mark, "mark").Body.Bytes(), first.Body.Bytes()},
		{a.request("PUT", url, body(2, 2, ""), "inherit").Body.Bytes(), inherited.Body.Bytes()},
	} {
		if !bytes.Equal(tc.actual, tc.original) {
			t.Fatalf("historical coverage or replay changed: got %s, want %s", tc.actual, tc.original)
		}
	}
	for _, tc := range []struct{ key, body string }{
		{"mark", body(1, 1, "")}, {"inherit", body(2, 2, fmt.Sprintf("%q", anchor.ID))},
	} {
		w := a.request("PUT", url, tc.body, tc.key)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
	}
}

func TestThreadStateCoveragePreparation(t *testing.T) {
	a := openMessageAPI(t, "")
	if !threadStateInstalled(t, a) {
		t.Skip("requires the candidate migration overlay")
	}
	g, foreign := newGroup(t, a, "export"), newGroup(t, a, "foreign")
	const topic = "export"
	url := threadStateURL(g.ID, topic)
	old := threadStateMessage(t, a, g.ID, topic, "必须兼容旧版 CSV")
	pre := threadStateMessage(t, a, g.ID, topic, "PRE_COVERED_BODY")
	anchor := threadStateMessage(t, a, g.ID, topic, "COVERAGE_ANCHOR_BODY")
	body := threadStateCoverageBody(0, 0, "constraint", old, fmt.Sprintf("%q", anchor.ID))
	decoded[domain.ThreadStateSnapshot](t, a.request("PUT", url, body, "mark"), 200)
	first := threadStateMessage(t, a, g.ID, topic, "Only apply the CSV change to exports.")
	threadStateMessage(t, a, g.ID, "another topic", "PRIVATE_OTHER_TOPIC")
	threadStateMessage(t, a, foreign.ID, topic, "PRIVATE_OTHER_GROUP")
	second := threadStateMessage(t, a, g.ID, topic, "Include the new CSV header.")
	member := decoded[domain.Member](t, a.request("POST", threadStateBase+g.ID+"/members", `{"name":"new","role":"handoff","provider_id":"fixture","model":"fixture/model"}`), 201)
	trigger := threadStateMessage(t, a, g.ID, topic, "Continue the export work.", member.ID)
	future := threadStateMessage(t, a, g.ID, topic, "AFTER_TRIGGER_BODY")
	raw := messageSQL(t, a)
	if _, err := raw.Exec(`UPDATE messages SET created_at=CASE WHEN id=? THEN '2026-01-03T00:00:00.000000000Z'
	 WHEN id IN (?,?) THEN '2026-01-02T00:00:00.000000000Z' ELSE '2026-01-01T00:00:00.000000000Z' END
	 WHERE id IN (?,?,?,?,?,?)`, first.ID, second.ID, trigger.ID, first.ID, second.ID, trigger.ID, pre.ID, anchor.ID, future.ID); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(t.TempDir(), "never-run")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	p, err := platform.NewPreparer(a.db, platform.PrepareOptions{PrivateWorkspaceRoot: t.TempDir(),
		Providers: []provider.Provider{{ID: "fixture", Name: provider.Codex, Version: "0.162.1", Installation: provider.Installed, Path: cli}}})
	if err != nil {
		t.Fatal(err)
	}
	pending := store.PendingDelivery{GroupID: g.ID, Delivery: trigger.Deliveries[0]}
	got, err := p.Prepare(t.Context(), pending)
	if err != nil {
		t.Fatal(err)
	}
	var pack struct {
		State            domain.ThreadStateSnapshot `json:"thread_state"`
		Pending, History []contextpack.Message
		Summary          *contextpack.Summary
		Overflow         []contextpack.Source
	}
	prompt := got.Request.Prompt
	if err := json.Unmarshal([]byte(prompt), &pack); err != nil {
		t.Fatal(err)
	}
	if cov := pack.State.CoveredThrough; cov == nil || cov.MessageID != anchor.ID || len(pack.State.Records) != 1 ||
		pack.State.Records[0].Content != old.Content || pack.State.Records[0].Source.MessageID != old.ID ||
		pack.State.Records[0].Source.MessageLogPosition >= cov.MessageLogPosition {
		t.Fatalf("mandatory older constraint or separate coverage lost: %+v", pack.State)
	}
	if pack.Summary != nil || len(pack.Overflow) != 0 || len(pack.Pending) != 1 || pack.Pending[0].ID != trigger.ID || pack.Pending[0].Sequence != 3 ||
		len(pack.History) != 2 || pack.History[0].ID != first.ID || pack.History[0].Sequence != 1 || pack.History[1].ID != second.ID || pack.History[1].Sequence != 2 ||
		!strings.Contains(pack.History[0].Content, first.Content) || !strings.Contains(pack.History[1].Content, second.Content) {
		t.Fatalf("wrong sequence window or snapshot-local positions: %s", prompt)
	}
	for _, excluded := range []string{pre.Content, anchor.Content, future.Content, "PRIVATE_OTHER_"} {
		if strings.Contains(prompt, excluded) {
			t.Fatalf("out-of-window body in prompt: %s", excluded)
		}
	}

	// Keep the schema shape intact but corrupt this anchor's log binding.
	var guard string
	if err := raw.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='message_log_no_update'`).Scan(&guard); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TRIGGER message_log_no_update; UPDATE message_log SET topic_id='another topic' WHERE message_id=?; `+guard, anchor.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Prepare(t.Context(), pending); err == nil || got.Request.Prompt != "" || got.Executor != nil {
		t.Fatal("corrupt coverage must not produce a prompt")
	}
	w := a.request("GET", url, "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
}
