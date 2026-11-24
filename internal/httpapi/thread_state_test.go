package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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

const threadStateBase = "/api/v1/groups/"

func threadStateURL(group, topic string) string {
	return threadStateBase + group + "/thread-state?topic_id=" + url.QueryEscape(topic)
}

func threadStateInstalled(t *testing.T, a *groupAPI) bool {
	t.Helper()
	var installed bool
	if err := messageSQL(t, a).QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='thread_state_revisions')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	return installed
}

func threadStateMessage(t *testing.T, a *groupAPI, group, topic, text string, recipients ...string) domain.Message {
	t.Helper()
	if recipients == nil {
		recipients = []string{}
	}
	body, err := json.Marshal(platform.MessageInput{TopicID: topic, Content: text, RecipientIDs: recipients})
	if err != nil {
		t.Fatal(err)
	}
	return decoded[domain.Message](t, a.request("POST", threadStateBase+group+"/messages", string(body)), 201)
}

func stateUpdate(expected, slot int, kind, action, text, message string) string {
	body, _ := json.Marshal(map[string]any{"expected_revision": expected, "slot": slot,
		"kind": kind, "action": action, "content": text, "source_message_id": message})
	return string(body)
}

func assertThreadStateUnavailable(t *testing.T, a *groupAPI, endpoint, update string) {
	t.Helper()
	w := a.request("PUT", endpoint, update, "without-thread-state")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 503, "thread_state_unavailable")
	w = a.request("GET", endpoint, "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 503, "thread_state_unavailable")
	var clean bool
	if err := messageSQL(t, a).QueryRow(`SELECT NOT EXISTS(SELECT 1 FROM sqlite_schema WHERE name IN ('thread_state_revisions','thread_state_heads'))
	 AND NOT EXISTS(SELECT 1 FROM mutation_idempotency WHERE operation='set_thread_state')`).Scan(&clean); err != nil || !clean {
		t.Fatalf("unavailable state command persisted state or a receipt: clean=%v, %v", clean, err)
	}
	t.Log("unregistered E2: PUT and GET reject without creating state or receipts")
}

func TestThreadStateUnavailableOnV30(t *testing.T) {
	a := openMessageAPI(t, "")
	g := newGroup(t, a, "legacy")
	m := threadStateMessage(t, a, g.ID, "bug", "legacy message")
	endpoint := threadStateURL(g.ID, m.TopicID)
	if threadStateInstalled(t, a) {
		got := decoded[domain.ThreadStateSnapshot](t, a.request("GET", endpoint, ""), 200)
		if got.Scope != (domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: g.ID, TopicID: m.TopicID}}) ||
			got.Revision != 0 || got.Records == nil || len(got.Records) != 0 || got.CoveredThrough != nil {
			t.Fatalf("installed E2 must return an empty snapshot for the named Thread: %+v", got)
		}
		t.Log("installed E2: existing named Thread has an empty revision-zero snapshot")
	} else {
		assertThreadStateUnavailable(t, a, endpoint, stateUpdate(0, 0, "goal", "set", m.Content, m.ID))
	}
	if got := decoded[domain.Message](t, a.request("GET", threadStateBase+g.ID+"/messages/"+m.ID, ""), 200); got.ID != m.ID || got.TopicID != m.TopicID || got.Content != m.Content {
		t.Fatalf("legacy message changed: %+v", got)
	}
}

// Exercise the normal local HTTP commands and the real prompt builder. The
// executable sentinel is never launched, and the new member has no prior Turn.
func TestThreadStateHandoffPrompt(t *testing.T) {
	a := openMessageAPI(t, "")
	g := newGroup(t, a, "home")
	const topic = " export+bug "
	const constraint = "必须兼容旧版 CSV"
	const oldPlan = "切回旧导出器"
	const retirement = "v2.9 生效，停用旧导出器方案"
	endpoint := threadStateURL(g.ID, topic)
	post := func(expected, slot int, kind, action string, m domain.Message) {
		t.Helper()
		decoded[map[string]any](t, a.request("PUT", endpoint, stateUpdate(expected, slot, kind, action, m.Content, m.ID), fmt.Sprintf("change-%d", expected)), 200)
	}
	oldMember := decoded[domain.Member](t, a.request("POST", threadStateBase+g.ID+"/members", `{"name":"first","role":"earlier owner"}`), 201)
	old := threadStateMessage(t, a, g.ID, topic, constraint, oldMember.ID)
	if !threadStateInstalled(t, a) {
		assertThreadStateUnavailable(t, a, endpoint, stateUpdate(0, 0, "constraint", "set", old.Content, old.ID))
		return
	}
	foreign := newGroup(t, a, "foreign")
	post(0, 0, "constraint", "set", old)
	plan := threadStateMessage(t, a, g.ID, topic, oldPlan)
	post(1, 1, "plan", "set", plan)
	for i := range 61 {
		threadStateMessage(t, a, g.ID, topic, fmt.Sprintf("same-topic-noise-%02d", i))
	}
	stop := threadStateMessage(t, a, g.ID, topic, retirement)
	post(2, 1, "plan", "invalidate", stop)
	decision := threadStateMessage(t, a, g.ID, topic, "保留 v2.9 修复")
	post(3, 2, "confirmed_decision", "set", decision)
	newPlan := threadStateMessage(t, a, g.ID, topic, "继续使用 v2.9 导出器")
	w := a.request("PUT", endpoint, stateUpdate(4, 1, "plan", "set", newPlan.Content, newPlan.ID), "change-4")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "thread_state_conflict")
	post(4, 3, "plan", "set", newPlan)
	for _, tc := range []struct{ group, topic, secret string }{
		{g.ID, "export+bug", "PRIVATE_OTHER_THREAD"}, {foreign.ID, topic, "PRIVATE_OTHER_GROUP"},
	} {
		m := threadStateMessage(t, a, tc.group, tc.topic, tc.secret)
		decoded[map[string]any](t, a.request("PUT", threadStateURL(tc.group, tc.topic), stateUpdate(0, 0, "goal", "set", m.Content, m.ID), "private"), 200)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = openMessageAPI(t, a.path)
	member := decoded[domain.Member](t, a.request("POST", threadStateBase+g.ID+"/members",
		`{"name":"new","role":"new handoff","provider_id":"fixture","model":"fixture/model"}`), 201)
	trigger := threadStateMessage(t, a, g.ID, topic, "跟进导出 BUG", member.ID)
	if trigger.Deliveries[0].ConversationID == old.Deliveries[0].ConversationID {
		t.Fatal("fixture reused the old conversation")
	}
	root := t.TempDir()
	private, cli := filepath.Join(root, "private"), filepath.Join(root, "never-run")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	const budget = 15 << 10
	prepare := func(maxBytes int) (string, error) {
		p, err := platform.NewPreparer(a.db, platform.PrepareOptions{PrivateWorkspaceRoot: private, MaxContextBytes: maxBytes,
			Providers: []provider.Provider{{ID: "fixture", Name: provider.Codex, Path: cli, Version: "0.162.1", Installation: provider.Installed}}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.Prepare(context.Background(), store.PendingDelivery{GroupID: g.ID, Delivery: trigger.Deliveries[0]})
		return got.Request.Prompt, err
	}
	prompt, err := prepare(budget)
	if err != nil {
		t.Fatal(err)
	}
	var pack struct {
		ThreadState                domain.ThreadStateSnapshot `json:"thread_state"`
		Pending, History, Overflow []contextpack.Source
		Summary                    *contextpack.Summary
	}
	if err := json.Unmarshal([]byte(prompt), &pack); err != nil {
		t.Fatal(err)
	}
	if len(prompt) > budget || strings.Contains(prompt, "PRIVATE_OTHER_") || pack.Summary != nil || len(pack.Pending) != 1 ||
		pack.Pending[0].ID != trigger.ID || pack.Pending[0].Sequence != 51 || len(pack.History)+len(pack.Overflow) != 50 || len(pack.Overflow) == 0 {
		t.Fatalf("incorrect scoped or bounded prompt: %s", prompt)
	}
	for _, messages := range [][]contextpack.Source{pack.History, pack.Overflow} {
		for _, m := range messages {
			if m.ID == old.ID || m.ID == plan.ID {
				t.Fatalf("old source was within candidate range: %s", m.ID)
			}
		}
	}
	st := pack.ThreadState
	if st.Scope != (domain.ThreadStateScope{ThreadRef: domain.ThreadRef{GroupID: g.ID, TopicID: topic}}) || st.Revision != 5 || len(st.Records) != 4 {
		t.Fatalf("missing current durable state: %+v", st)
	}
	orig, disabled, replacement := st.Records[0], st.Records[1], st.Records[3]
	if !replacement.Active || replacement.Content != newPlan.Content || replacement.Source.SourceID != newPlan.SourceID {
		t.Fatalf("replacement plan missing from handoff: %+v", replacement)
	}
	if !orig.Active || orig.Revision != 1 || orig.Content != constraint || orig.Source.ConfirmedBy != "user" ||
		orig.Source.SourceKind != "local_user_chat" || orig.Source.ConfirmedAt.IsZero() || orig.Source.MessageID != old.ID || orig.Source.SourceID != old.SourceID ||
		disabled.Active || disabled.Revision != 3 || disabled.Content != oldPlan || disabled.Source.MessageID != plan.ID || disabled.Source.SourceID != plan.SourceID ||
		disabled.InvalidatedBy == nil || disabled.InvalidatedBy.Reason != retirement || disabled.InvalidatedBy.Source.MessageID != stop.ID || disabled.InvalidatedBy.Source.SourceID != stop.SourceID {
		t.Fatalf("incorrect live constraint or plan lineage: %+v", st)
	}
	raw := messageSQL(t, a)
	for id, got := range map[string]int64{old.ID: orig.Source.MessageLogPosition, plan.ID: disabled.Source.MessageLogPosition, stop.ID: disabled.InvalidatedBy.Source.MessageLogPosition} {
		var durable int64
		if err := raw.QueryRow(`SELECT sequence FROM message_log WHERE message_id=?`, id).Scan(&durable); err != nil || durable != got {
			t.Fatalf("wrong durable source position for %s: %d, want %d (%v)", id, got, durable, err)
		}
	}
	if prompt, err := prepare(1024); err == nil || prompt != "" {
		t.Fatal("mandatory state/trigger must not be silently truncated")
	}
}
