//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

const artifactGrantBody = "跟进那个导出 BUG"
const artifactGrantTopic = "export-bug"

func artifactGrantSend(t *testing.T, h http.Handler, path string, input any, key, bearer string, want int) []byte {
	t.Helper()
	body, err := json.Marshal(input)
	artifactCmdMust(t, err)
	return artifactGrantSendRaw(t, h, path, body, key, bearer, want)
}

func artifactGrantSendRaw(t *testing.T, h http.Handler, path string, body []byte, key, bearer string, want int) []byte {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://"+httpapi.DefaultAddress+path, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("POST %s: status=%d want=%d body=%s", path, w.Code, want, w.Body)
	}
	return w.Body.Bytes()
}

type artifactGrantFixture struct {
	dir, group, other  string
	db                 *store.Store
	raw                *sql.DB
	local              *platform.LocalUser
	owner              io.Closer
	pin, head, foreign domain.ArtifactVersionRef
}

func newArtifactGrantFixture(t *testing.T) *artifactGrantFixture {
	t.Helper()
	f := &artifactGrantFixture{dir: t.TempDir()}
	path := filepath.Join(f.dir, "workspace.db")
	var err error
	f.db, err = store.Open(path)
	artifactCmdMust(t, err)
	t.Cleanup(func() { artifactCmdMust(t, f.db.Close()) })
	f.raw, err = sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	artifactCmdMust(t, err)
	t.Cleanup(func() { artifactCmdMust(t, f.raw.Close()) })
	_, f.owner, err = artifactReadRoutes(f.dir, f.db)
	artifactCmdMust(t, err)
	t.Cleanup(func() { artifactCmdMust(t, f.owner.Close()) })
	service, err := platform.NewArtifacts(f.db, f.owner.(*artifactfs.Store))
	artifactCmdMust(t, err)
	source, err := os.OpenRoot(t.TempDir())
	artifactCmdMust(t, err)
	defer source.Close()
	f.local = platform.NewLocalUser(f.db)
	for _, id := range []*string{&f.group, &f.other} {
		g, err := f.local.CreateGroup(t.Context(), platform.GroupInput{Name: "Export"}, "")
		artifactCmdMust(t, err)
		*id = g.ID
	}
	publish := func(group string, in platform.PublishArtifactInput, data string) domain.ArtifactVersionRef {
		artifactCmdMust(t, source.WriteFile("file", []byte(data), 0600))
		version, err := service.PublishArtifact(t.Context(), group, in, source, "file", "")
		artifactCmdMust(t, err)
		return domain.ArtifactVersionRef{ArtifactID: version.ArtifactID, VersionID: version.ID}
	}
	f.pin = publish(f.group, platform.PublishArtifactInput{Name: "historical"}, "fixed historic bytes")
	f.head = publish(f.group, platform.PublishArtifactInput{ArtifactID: f.pin.ArtifactID, ExpectedVersionID: f.pin.VersionID}, "different head")
	f.foreign = publish(f.other, platform.PublishArtifactInput{Name: "foreign"}, "private foreign bytes")
	return f
}

func installArtifactReadGrantFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	// Private fixture only. Do not assign a public schema_migrations version.
	installArtifactReadGrantDDL(t, db, artifactReadGrantDDL(t))
}

func artifactReadGrantDDL(t *testing.T) string {
	t.Helper()
	ddl, err := os.ReadFile("../../internal/store/delivery_artifact_read_grant.sql")
	artifactCmdMust(t, err)
	return string(ddl)
}

func installArtifactReadGrantDDL(t *testing.T, db *sql.DB, ddl string) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	artifactCmdMust(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(t.Context(), ddl)
	artifactCmdMust(t, err)
	artifactCmdMust(t, tx.Commit())
}

func (f *artifactGrantFixture) reopen(t *testing.T) {
	t.Helper()
	artifactCmdMust(t, f.db.Close())
	db, err := store.Open(filepath.Join(f.dir, "workspace.db"))
	artifactCmdMust(t, err)
	f.db, f.local = db, platform.NewLocalUser(db)
}

// Simulate already-lost evidence while leaving the full schema present for
// the checks under test. Only the private fixture's listed guard is bypassed.
func (f *artifactGrantFixture) loseEvidence(t *testing.T, guard, change string, args ...any) {
	t.Helper()
	tx, err := f.raw.BeginTx(t.Context(), nil)
	artifactCmdMust(t, err)
	defer tx.Rollback()
	var ddl string
	artifactCmdMust(t, tx.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=?`, guard).Scan(&ddl))
	_, err = tx.Exec("DROP TRIGGER " + guard)
	artifactCmdMust(t, err)
	_, err = tx.Exec(change, args...)
	artifactCmdMust(t, err)
	_, err = tx.Exec(ddl)
	artifactCmdMust(t, err)
	artifactCmdMust(t, tx.Commit())
}

func (f *artifactGrantFixture) member(t *testing.T, group, providerID string) string {
	t.Helper()
	m, err := f.local.CreateMember(t.Context(), group, platform.MemberInput{Name: "reader", Role: "review", ProviderID: providerID, Model: "fixture-model"}, "")
	artifactCmdMust(t, err)
	return m.ID
}

func artifactGrantMessage(recipient string, pin domain.ArtifactVersionRef) map[string]any {
	return map[string]any{"content": artifactGrantBody, "topic_id": artifactGrantTopic, "recipient_ids": []string{recipient}, "artifact_read_grant": pin}
}

func TestLocalMessageArtifactReadGrant(t *testing.T) {
	f := newArtifactGrantFixture(t)
	member, peer, foreign := f.member(t, f.group, ""), f.member(t, f.group, ""), f.member(t, f.other, "")
	server, err := httpapi.NewServer(httpapi.DefaultAddress, httpapi.MessageRoutes(f.local)...)
	artifactCmdMust(t, err)
	path := "/api/v1/groups/" + f.group + "/messages"
	post := func(input any, key string, want int) []byte {
		return artifactGrantSend(t, server.Handler, path, input, key, "", want)
	}
	plain := platform.MessageInput{Content: artifactGrantBody, TopicID: artifactGrantTopic, RecipientIDs: []string{member}}
	// A failed pre-registration request must not reserve the key or persist a wake.
	post(artifactGrantMessage(member, f.pin), "v30", http.StatusServiceUnavailable)
	legacy := post(plain, "v30", http.StatusCreated)
	installArtifactReadGrantFixture(t, f.raw)
	if replay := post(plain, "v30", http.StatusCreated); !bytes.Equal(legacy, replay) {
		t.Fatal("legacy idempotency payload changed with the schema")
	}
	post(artifactGrantMessage(member, f.pin), "v30", http.StatusConflict)
	for _, routing := range []any{nil, []string{}, []string{member, peer}, []string{member, member}} {
		in := artifactGrantMessage(member, f.pin)
		if routing != nil {
			in["recipient_ids"] = routing
		} else {
			delete(in, "recipient_ids")
		}
		post(in, "exact", http.StatusBadRequest)
	}
	post(artifactGrantMessage(foreign, f.pin), "exact", http.StatusNotFound)
	post(artifactGrantMessage(member, f.foreign), "exact", http.StatusNotFound)
	artifactGrantSend(t, server.Handler, "/api/v1/groups/"+f.other+"/messages", artifactGrantMessage(foreign, f.pin), "exact", "", http.StatusNotFound)
	post(artifactGrantMessage(member, domain.ArtifactVersionRef{ArtifactID: f.pin.ArtifactID, VersionID: f.foreign.VersionID}), "exact", http.StatusNotFound)
	post(artifactGrantMessage(member, domain.ArtifactVersionRef{ArtifactID: f.pin.ArtifactID}), "exact", http.StatusBadRequest)
	for _, bad := range []any{nil, []domain.ArtifactVersionRef{f.pin},
		map[string]string{"artifact_id": f.pin.ArtifactID, "version_id": f.pin.VersionID, "actor_id": "user"},
		json.RawMessage(`{"artifact_id":"\ud800","version_id":"exact"}`)} {
		in := artifactGrantMessage(member, f.pin)
		in["artifact_read_grant"] = bad
		post(in, "exact", http.StatusBadRequest)
	}
	// An active member or bare sender_id=user is not the trusted local-user command seam.
	msg := store.MessageRequest{GroupID: f.group, ActorID: peer, Content: artifactGrantBody, RecipientIDs: []string{member}, ArtifactReadGrant: &f.pin}
	if _, err := f.db.CreateLocalUserMessage(t.Context(), msg, msg.RecipientIDs); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("member impersonated LocalUser: %v", err)
	}
	msg.ActorID = "user"
	if _, err := f.db.CreateMessage(t.Context(), msg, msg.RecipientIDs); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("grant accepted outside LocalUser: %v", err)
	}
	msg.ArtifactReadGrant = nil
	untrusted, err := f.db.CreateMessage(t.Context(), msg, msg.RecipientIDs)
	artifactCmdMust(t, err)
	const inject = `INSERT INTO delivery_artifact_read_grants
	 SELECT id,message_id,group_id,topic_id,conversation_id,recipient_id,?,? FROM deliveries WHERE id=?`
	_, err = f.raw.Exec(inject, f.pin.ArtifactID, f.pin.VersionID, untrusted.Deliveries[0].ID)
	if err == nil {
		t.Fatal("bare sender label was accepted as a grant source")
	}

	ok := post(artifactGrantMessage(member, f.pin), "exact", http.StatusCreated)
	if retry := post(artifactGrantMessage(member, f.pin), "exact", http.StatusCreated); !bytes.Equal(ok, retry) {
		t.Fatal("same grant retry changed receipt")
	}
	post(artifactGrantMessage(member, f.head), "exact", http.StatusConflict)
	post(artifactGrantMessage(member, f.foreign), "exact", http.StatusConflict)
	post(artifactGrantMessage(peer, f.pin), "exact", http.StatusConflict)
	post(plain, "exact", http.StatusConflict)
	var message domain.Message
	artifactCmdMust(t, json.Unmarshal(ok, &message))
	if message.Content != artifactGrantBody || message.SenderID != "user" || message.GroupID != f.group ||
		message.TopicID != artifactGrantTopic || message.SourceKind != "local_user_chat" || len(message.Deliveries) != 1 {
		t.Fatalf("structured pin replaced ordinary message semantics: %+v", message)
	}
	delivery := message.Deliveries[0]
	var count int
	artifactCmdMust(t, f.raw.QueryRow(`SELECT count(*) FROM delivery_artifact_read_grants
	 WHERE (delivery_id,message_id,group_id,topic_id,conversation_id,recipient_id,artifact_id,version_id)=(?,?,?,?,?,?,?,?)`,
		delivery.ID, message.ID, f.group, artifactGrantTopic, delivery.ConversationID, member, f.pin.ArtifactID, f.pin.VersionID).Scan(&count))
	if count != 1 || delivery.RecipientID != member || delivery.State != domain.DeliveryStateQueued {
		t.Fatal("grant was not normalized to its concrete queued delivery")
	}
	for table, want := range map[string]int{"messages": 3, "deliveries": 3, "message_command_sources": 2, "delivery_artifact_read_grants": 1} {
		artifactCmdMust(t, f.raw.QueryRow("SELECT count(*) FROM "+table).Scan(&count))
		if count != want {
			t.Fatalf("retry or denied request changed %s: count=%d want=%d", table, count, want)
		}
	}
	for _, sql := range []string{
		"UPDATE delivery_artifact_read_grants SET version_id=?", "DELETE FROM delivery_artifact_read_grants WHERE version_id<>?",
		`INSERT OR REPLACE INTO delivery_artifact_read_grants SELECT delivery_id,message_id,group_id,topic_id,conversation_id,recipient_id,artifact_id,? FROM delivery_artifact_read_grants`,
	} {
		if _, err := f.raw.Exec(sql, f.head.VersionID); err == nil {
			t.Fatalf("pin is mutable: %s", sql)
		}
	}
	// Even an independently injected row on a real local command cannot turn
	// default routing (or a tuple mentioned in the body) into explicit approval.
	var implicit domain.Message
	body := fmt.Sprintf("%s（%s/%s）", artifactGrantBody, f.pin.ArtifactID, f.pin.VersionID)
	artifactCmdMust(t, json.Unmarshal(post(platform.MessageInput{Content: body, TopicID: artifactGrantTopic}, "", 201), &implicit))
	if len(implicit.Deliveries) != 1 {
		t.Fatal("expected one default delivery")
	}
	_, err = f.raw.Exec(inject, f.pin.ArtifactID, f.pin.VersionID, implicit.Deliveries[0].ID)
	artifactCmdMust(t, err)
	if pin, err := f.db.QueuedDeliveryArtifactReadGrant(t.Context(), f.group, implicit.Deliveries[0], implicit.TopicID); pin != (domain.ArtifactVersionRef{}) || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("default routing became exact approval: %v", err)
	}
}

func TestLocalMessageArtifactReadGrantContent(t *testing.T) {
	f := newArtifactGrantFixture(t)
	installArtifactReadGrantFixture(t, f.raw)
	member := f.member(t, f.group, "")
	server, err := httpapi.NewServer(httpapi.DefaultAddress, httpapi.MessageRoutes(f.local)...)
	artifactCmdMust(t, err)
	path := "/api/v1/groups/" + f.group + "/messages"
	template, err := json.Marshal(artifactGrantMessage(member, f.pin))
	artifactCmdMust(t, err)
	post := func(content string, want int) []byte {
		t.Helper()
		raw := bytes.Replace(template, []byte(`"`+artifactGrantBody+`"`), []byte(content), 1)
		return artifactGrantSendRaw(t, server.Handler, path, raw, "scalar", "", want)
	}
	const invalidSurrogate = `" before \ud800 after "`
	const invalidUTF8 = "\" before \xff after \""
	for _, bad := range []string{invalidSurrogate, `"\udc00"`, `"\ud800\u0041"`, invalidUTF8} {
		if body := post(bad, http.StatusBadRequest); !bytes.Contains(body, []byte(`"code":"invalid_json"`)) {
			t.Fatalf("expected invalid JSON, got %s", body)
		}
	}
	// The trusted Go entry point also rejects invalid bytes before reserving a key.
	in := platform.MessageInput{Content: "invalid\xff", TopicID: artifactGrantTopic, RecipientIDs: []string{member}, ArtifactReadGrant: &f.pin}
	if _, err := f.local.CreateMessage(t.Context(), f.group, in, "scalar"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("explicit grant accepted invalid in-process content: %v", err)
	}
	for _, table := range []string{"messages", "deliveries", "message_command_sources", "delivery_artifact_read_grants", "mutation_idempotency"} {
		var n int
		artifactCmdMust(t, f.raw.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n != 0 {
			t.Fatalf("invalid grant body left %d rows in %s", n, table)
		}
	}

	// A scalar replacement character is not an invalid surrogate. A proper
	// pair and a literal backslash-u also survive byte-for-byte, even above
	// the unrelated member-tool frame limit.
	pad := strings.Repeat("x", 64<<10)
	want := ` before � after 😀 \ud800 ` + pad
	first := post(`" before � after 😀 \\ud800 `+pad+`"`, http.StatusCreated)
	var m domain.Message
	artifactCmdMust(t, json.Unmarshal(first, &m))
	if m.Content != want {
		t.Fatal("valid explicit grant content was altered")
	}
	if retry := post(`" before \uFFFD after \ud83d\ude00 \\ud800 `+pad+`"`, http.StatusCreated); !bytes.Equal(retry, first) {
		t.Fatal("equivalent scalar content did not replay the original command")
	}
	post(`" before \ud800 after 😀 \\ud800 `+pad+`"`, http.StatusBadRequest)

	// Do not tighten the historical no-grant surrogate decoder; raw invalid
	// UTF-8 was already invalid in that path, too.
	plain := func(content string, want int) []byte {
		return artifactGrantSendRaw(t, server.Handler, path, []byte(`{"content":`+content+`}`), "ordinary", "", want)
	}
	plain(invalidUTF8, http.StatusBadRequest)
	ordinary := plain(invalidSurrogate, http.StatusCreated)
	artifactCmdMust(t, json.Unmarshal(ordinary, &m))
	if m.Content != " before � after " || !bytes.Equal(ordinary, plain(`" before � after "`, http.StatusCreated)) {
		t.Fatal("ordinary message decoding or replay changed")
	}
}

func TestDeliveryArtifactReadGrantSchema(t *testing.T) {
	for _, fault := range []struct{ name, sql string }{
		{"absent", ""},
		{"stub table", `CREATE TABLE delivery_artifact_read_grants(delivery_id TEXT)`},
		{"missing unique message key", ""},
		{"missing grant table with remaining triggers", `DROP TABLE delivery_artifact_read_grants`},
		{"missing grant insert guard", `DROP TRIGGER delivery_artifact_read_grants_insert`},
		{"missing grant immutability guard", `DROP TRIGGER delivery_artifact_read_grants_update`},
		{"missing delivery identity guard", `DROP TRIGGER delivery_artifact_read_grant_deliveries_identity`},
		{"missing delivery insert guard", `DROP TRIGGER delivery_artifact_read_grant_deliveries_insert`},
		{"missing message identity guard", `DROP TRIGGER messages_identity`},
		{"missing command source guard", `DROP TRIGGER message_command_sources_update`},
		{"missing version immutability guard", `DROP TRIGGER artifact_versions_no_update`},
		{"missing delivery FK key", `DROP INDEX deliveries_turn_identity`},
	} {
		t.Run(fault.name, func(t *testing.T) {
			f := newArtifactGrantFixture(t)
			member := f.member(t, f.group, "")
			plain := platform.MessageInput{Content: "explicit_artifact_read_grant/v1", TopicID: artifactGrantTopic, RecipientIDs: []string{member}}
			old, err := f.local.CreateMessage(t.Context(), f.group, plain, "ordinary")
			artifactCmdMust(t, err)
			want := store.ErrArtifactReadGrantIncomplete
			switch fault.name {
			case "absent":
				want = store.ErrArtifactReadGrantUnavailable
			case "stub table":
				_, err = f.raw.Exec(fault.sql)
				artifactCmdMust(t, err)
			case "missing unique message key":
				ddl := strings.Replace(artifactReadGrantDDL(t), "message_id TEXT NOT NULL UNIQUE", "message_id TEXT NOT NULL", 1)
				installArtifactReadGrantDDL(t, f.raw, ddl)
			default:
				installArtifactReadGrantFixture(t, f.raw)
				_, err = f.raw.Exec(fault.sql)
				artifactCmdMust(t, err)
			}
			approved := plain
			approved.ArtifactReadGrant = &f.pin
			if got, err := f.local.CreateMessage(t.Context(), f.group, approved, "exact"); !errors.Is(err, want) || got.ID != "" {
				t.Fatalf("grant send on %s: %+v, %v (want %v)", fault.name, got, err, want)
			}
			pin, err := f.db.QueuedDeliveryArtifactReadGrant(t.Context(), f.group, old.Deliveries[0], old.TopicID)
			if fault.name == "absent" {
				want = nil // The mode string was only ordinary content, not evidence.
			}
			if !errors.Is(err, want) || pin != (domain.ArtifactVersionRef{}) {
				t.Fatalf("queued lookup on %s: %+v, %v (want %v)", fault.name, pin, err, want)
			}
			for _, table := range []string{"messages", "deliveries", "message_command_sources", "mutation_idempotency"} {
				var n int
				artifactCmdMust(t, f.raw.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
				if n != 1 {
					t.Fatalf("denied grant changed %s: rows=%d", table, n)
				}
			}
		})
	}
}

func TestDeliveryArtifactReadGrantEvidenceAfterReopen(t *testing.T) {
	for _, mode := range []string{"complete", "missing row", "changed pin", "missing fragment"} {
		t.Run(mode, func(t *testing.T) {
			f := newArtifactGrantFixture(t)
			installArtifactReadGrantFixture(t, f.raw)
			member := f.member(t, f.group, "")
			ctx := t.Context()
			in := platform.MessageInput{Content: artifactGrantBody, TopicID: artifactGrantTopic, RecipientIDs: []string{member}, ArtifactReadGrant: &f.pin}
			message, err := f.local.CreateMessage(ctx, f.group, in, "exact")
			artifactCmdMust(t, err)
			d := message.Deliveries[0]
			switch mode {
			case "missing row":
				f.loseEvidence(t, "delivery_artifact_read_grants_delete", "DELETE FROM delivery_artifact_read_grants WHERE delivery_id=?", d.ID)
			case "changed pin":
				f.loseEvidence(t, "delivery_artifact_read_grants_update", "UPDATE delivery_artifact_read_grants SET version_id=? WHERE delivery_id=?", f.head.VersionID, d.ID)
			case "missing fragment":
				// Use the immutable provenance of even a keyless send to detect
				// known grant evidence after every grant object disappears.
				message, err = f.local.CreateMessage(ctx, f.group, in, "")
				artifactCmdMust(t, err)
				d = message.Deliveries[0]
				_, err = f.raw.Exec(`DROP TRIGGER delivery_artifact_read_grant_deliveries_insert;
				 DROP TRIGGER delivery_artifact_read_grant_deliveries_identity;
				 DROP TRIGGER delivery_artifact_read_grant_deliveries_delete;
				 DROP TABLE delivery_artifact_read_grants;`)
				artifactCmdMust(t, err)
			}
			f.reopen(t)
			pin, prepErr := f.db.QueuedDeliveryArtifactReadGrant(ctx, f.group, d, message.TopicID)
			replay, err := f.local.CreateMessage(ctx, f.group, in, "exact")
			if mode == "complete" {
				if err != nil || replay.ID != message.ID || prepErr != nil || pin != f.pin {
					t.Fatalf("complete grant changed across reopen: replay=%v preparation=%v, pin=%+v", err, prepErr, pin)
				}
				return
			}
			if !errors.Is(err, store.ErrArtifactReadGrantIncomplete) || replay.ID != "" {
				t.Fatalf("replayed without the same durable grant: %+v, %v", replay, err)
			}
			want := store.ErrArtifactReadGrantIncomplete
			if mode == "changed pin" {
				want = store.ErrMemberToolDenied
			}
			if !errors.Is(prepErr, want) {
				t.Fatalf("invalid evidence treated as absent: %v, want %v", prepErr, want)
			}
			if pin != (domain.ArtifactVersionRef{}) {
				t.Fatalf("incomplete approval produced pin %+v", pin)
			}
		})
	}
}

func TestDeliveryArtifactReadGrantIdentity(t *testing.T) {
	f := newArtifactGrantFixture(t)
	installArtifactReadGrantFixture(t, f.raw)
	member, peer := f.member(t, f.group, ""), f.member(t, f.group, "")
	ctx := t.Context()
	in := platform.MessageInput{Content: artifactGrantBody, TopicID: artifactGrantTopic, RecipientIDs: []string{member}, ArtifactReadGrant: &f.pin}
	approved, err := f.local.CreateMessage(ctx, f.group, in, "exact")
	artifactCmdMust(t, err)
	d := approved.Deliveries[0]
	other, err := f.local.CreateMessage(ctx, f.group, platform.MessageInput{Content: "other", TopicID: artifactGrantTopic, RecipientIDs: []string{peer}}, "")
	artifactCmdMust(t, err)
	neighbor := other.Deliveries[0]
	for _, attack := range []struct {
		query string
		args  []any
	}{
		{`UPDATE deliveries SET message_id=? WHERE id=?`, []any{other.ID, d.ID}},
		{`UPDATE deliveries SET conversation_id=?,recipient_id=? WHERE id=?`, []any{neighbor.ConversationID, peer, d.ID}},
		{`UPDATE deliveries SET message_id=? WHERE id=?`, []any{approved.ID, neighbor.ID}},
		{`INSERT INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
		 SELECT 'second',?,conversation_id,recipient_id,group_id,topic_id,created_at FROM deliveries WHERE id=?`, []any{approved.ID, neighbor.ID}},
		{`INSERT OR REPLACE INTO deliveries(id,message_id,conversation_id,recipient_id,group_id,topic_id,created_at)
		 SELECT id,?,conversation_id,recipient_id,group_id,topic_id,created_at FROM deliveries WHERE id=?`, []any{other.ID, d.ID}},
		{`UPDATE OR REPLACE deliveries SET rowid=(SELECT rowid FROM deliveries WHERE id=?) WHERE id=?`, []any{d.ID, neighbor.ID}},
	} {
		if _, err := f.raw.Exec(attack.query, attack.args...); err == nil || !strings.Contains(err.Error(), "成果读取许可") {
			t.Fatalf("grant delivery guard did not reject %s: %v", attack.query, err)
		}
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: d.ID, ResourceKeys: []string{"grant-fixture"}, MaxActive: 1})
	artifactCmdMust(t, err)
	claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	artifactCmdMust(t, f.db.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "fixture"}))
	replay, err := f.local.CreateMessage(ctx, f.group, in, "exact")
	artifactCmdMust(t, err)
	if replay.ID != approved.ID || len(replay.Deliveries) != 1 || replay.Deliveries[0].ID != d.ID ||
		replay.Deliveries[0].State != domain.DeliveryStateAccepted || replay.Deliveries[0].TurnID != turn.ID {
		t.Fatalf("identity guard or replay interfered with accepted state: %+v", replay)
	}
	var n int
	artifactCmdMust(t, f.raw.QueryRow(`SELECT count(*) FROM resource_claims WHERE turn_id=? AND resource_key='grant-fixture'`, turn.ID).Scan(&n))
	if n != 1 {
		t.Fatal("claim did not retain the usual delivery resource")
	}
	settled, err := f.db.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded,
		Started: true, Accepted: true, OutputComplete: true, DirectChildReaped: true, NativeSessionID: "fixture"})
	artifactCmdMust(t, err)
	artifactCmdMust(t, f.raw.QueryRow(`SELECT count(*) FROM resource_claims WHERE turn_id=?`, turn.ID).Scan(&n))
	if n != 0 || settled.State != domain.TurnStateSucceeded {
		t.Fatalf("settlement did not release the usual delivery resource: %s, claims=%d", settled.State, n)
	}
}

// The production runner and Go Pi adapter are used with a fake CLI and an
// in-memory HTTP handler. No listener, scheduler, Pi or provider is started.
func TestStartupArtifactReadGrant(t *testing.T) {
	f := newArtifactGrantFixture(t)
	installed := false
	for _, mode := range []string{"v30 lookup only", "installed pin", "another delivery", "legacy with pin", "chat intake pin", "wrong provenance before prepare", "stale claim backlink"} {
		t.Run(mode, func(t *testing.T) {
			if !installed && mode != "v30 lookup only" {
				installArtifactReadGrantFixture(t, f.raw)
				installed = true
			}
			cli := newExecutionFixture(t, "1.4.0")
			fresh, approved := mode != "legacy with pin", mode != "v30 lookup only" && mode != "another delivery"
			intake := mode == "chat intake pin"
			launcher, err := os.ReadFile(cli.path)
			artifactCmdMust(t, err)
			launcher = bytes.Replace(launcher, []byte(`while [ -f "$root/hold" ]`), []byte(`printf '%s\n%s\n' "$AGENT_WORKSPACE_MEMBER_TOOL_CONFIG" "${AGENT_WORKSPACE_MEMBER_TOOL_CONFIG_SHA256:-}" > "$root/issued.tmp"
mv "$root/issued.tmp" "$root/issued"
while [ -f "$root/hold" ]`), 1)
			artifactCmdMust(t, os.WriteFile(cli.path, launcher, 0700))
			catalog := platform.DiscoverProviders(t.Context(), provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: cli.path}}}, provider.Runner{})
			items, _, err := catalog.List(10, "")
			artifactCmdMust(t, err)
			if len(items) != 1 {
				t.Fatal("expected exactly one synthetic Pi")
			}
			member := f.member(t, f.group, items[0].ID)
			args := memberToolsArgs(t, f.dir, cli.path)
			if fresh {
				args = append(args, "--enable-pi-project-experience")
			}
			if intake {
				args = append(args, "--enable-coordination-roles")
			}
			cfg, err := parseConfig(args, io.Discard)
			artifactCmdMust(t, err)
			workspace, err := executionArtifactOptions(cfg, f.owner)
			artifactCmdMust(t, err)
			_, runtime, preparer, err := executionRunner(cfg, f.db, coordination.New(f.db), catalog, "http://"+httpapi.DefaultAddress+"/api/v1/member-tools", io.Discard, workspace...)
			artifactCmdMust(t, err)
			defer runtime.Close()
			routes := append(httpapi.MessageRoutes(f.local), httpapi.MemberProjectExperienceToolRoutes(runtime.Tools())...)
			routes = append(routes, httpapi.MemberToolRoutes(runtime.Tools())...)
			server, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
			artifactCmdMust(t, err)
			if mode == "installed pin" {
				if _, err := os.Stat(filepath.Join(f.dir, "workspace-snapshots")); !os.IsNotExist(err) {
					t.Fatal("ordinary startup provisioned workspace staging")
				}
			}
			path := "/api/v1/groups/" + f.group + "/messages"
			var input any = platform.MessageInput{Content: artifactGrantBody, TopicID: artifactGrantTopic, RecipientIDs: []string{member}}
			if approved {
				input = artifactGrantMessage(member, f.pin)
			}
			if mode == "another delivery" {
				artifactGrantSend(t, server.Handler, path, artifactGrantMessage(member, f.pin), "", "", http.StatusCreated)
			}
			var message domain.Message
			artifactCmdMust(t, json.Unmarshal(artifactGrantSend(t, server.Handler, path, input, "", "", http.StatusCreated), &message))
			d := message.Deliveries[0]
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if mode == "wrong provenance before prepare" {
				f.loseEvidence(t, "message_command_sources_update", `UPDATE message_command_sources SET kind='local_user_policy' WHERE message_id=?`, message.ID)
				if _, err := f.db.QueuedDeliveryArtifactReadGrant(ctx, f.group, d, message.TopicID); !errors.Is(err, store.ErrArtifactReadGrantIncomplete) {
					t.Fatalf("missing local-user proof: %v", err)
				}
				if _, err := preparer.Prepare(ctx, store.PendingDelivery{GroupID: f.group, Delivery: d}); err == nil {
					t.Fatal("preparation silently downgraded an approved delivery with missing local-user proof")
				}
				return
			}
			prepared, err := preparer.Prepare(ctx, store.PendingDelivery{GroupID: f.group, Delivery: d})
			artifactCmdMust(t, err)
			turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group, DeliveryID: d.ID, ResourceKeys: prepared.ResourceKeys, MaxActive: 1})
			artifactCmdMust(t, err)
			claim := store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
			request := prepared.Request
			request.RunID, request.RuntimeDir = turn.ID, filepath.Join(f.dir, "runtime")
			if strings.Contains(request.Prompt, f.pin.VersionID) != (fresh && approved) || !strings.Contains(request.Prompt, artifactGrantBody) {
				t.Fatal("prompt lost the original body or leaked a pin from another delivery/profile")
			}
			if mode == "stale claim backlink" {
				_, err := f.raw.Exec("UPDATE deliveries SET turn_id='wrong' WHERE id=?", d.ID)
				artifactCmdMust(t, err)
				got := prepared.Executor.Execute(ctx, request, nil)
				if got.Outcome != provider.OutcomeUnknown || got.Started {
					t.Fatalf("startup did not re-prove the claimed delivery: %+v", got)
				}
				return
			}
			hold := filepath.Join(cli.root, "hold")
			artifactCmdMust(t, os.WriteFile(hold, nil, 0600))
			done := make(chan provider.ExecutionReceipt, 1)
			go func() {
				done <- prepared.Executor.Execute(ctx, request, func(event provider.Event) error { return f.db.RecordTurnEvent(ctx, claim, event) })
				close(done)
			}()
			defer func() { _ = os.Remove(hold); cancel(); <-done }()
			var issued []byte
			for len(issued) == 0 {
				issued, err = os.ReadFile(filepath.Join(cli.root, "issued"))
				if err == nil || !os.IsNotExist(err) {
					artifactCmdMust(t, err)
					break
				}
				select {
				case got := <-done:
					t.Fatalf("synthetic Pi did not receive a config: %+v", got)
				case <-ctx.Done():
					t.Fatal("synthetic Pi did not report issued config")
				case <-time.After(30 * time.Millisecond):
				}
			}
			configPath, digest, _ := strings.Cut(string(issued), "\n")
			data, err := os.ReadFile(configPath)
			artifactCmdMust(t, err)
			var manifest struct {
				Token string   `json:"token"`
				Tools []string `json:"project_experience_tools"`
			}
			artifactCmdMust(t, json.Unmarshal(data, &manifest))
			want := []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"}
			if intake {
				want = slices.Delete(want, 3, 4)
			}
			if fresh {
				want = append(want, provider.LookupProjectExperiencesTool)
				if approved {
					want = append(want, provider.ReadArtifactTool)
				}
				if !slices.Equal(manifest.Tools, want) || strings.TrimSpace(digest) != fmt.Sprintf("%x", sha256.Sum256(data)) {
					t.Fatalf("unexpected issued projection: %v want %v", manifest.Tools, want)
				}
			} else if manifest.Tools != nil || strings.TrimSpace(digest) != "" {
				t.Fatal("legacy startup changed profile")
			}
			argv, err := os.ReadFile(filepath.Join(cli.root, "args"))
			artifactCmdMust(t, err)
			if !strings.Contains(string(argv), "\n--tools\n"+strings.Join(want, ",")+"\n") || manifest.Token == "" || manifest.Token == claim.Token {
				t.Fatal("startup did not advertise only the issued profile with a new bearer")
			}
			tool := func(name string, input any, want int) []byte {
				return artifactGrantSend(t, server.Handler, "/api/v1/member-tools/"+name, input, "", manifest.Token, want)
			}
			if fresh && approved {
				var read platform.MemberArtifactReadResult
				// Read before any lookup. The previous immutable version is not the head.
				artifactCmdMust(t, json.Unmarshal(tool(provider.ReadArtifactTool, f.pin, 200), &read))
				if read.ArtifactID != f.pin.ArtifactID || read.VersionID != f.pin.VersionID || read.CurrentVersionID != f.head.VersionID || read.Content != "fixed historic bytes" {
					t.Fatalf("native exact read issued the wrong bytes or tuple: %+v", read)
				}
				tool(provider.ReadArtifactTool, f.head, 403)
				tool(provider.ReadArtifactTool, f.foreign, 403)
				if mode == "installed pin" {
					// The live bearer and Turn claim alone cannot authorize bytes
					// after the delivery backlink or persisted approval is lost.
					_, err := f.raw.Exec("UPDATE deliveries SET turn_id='wrong' WHERE id=?", d.ID)
					artifactCmdMust(t, err)
					tool(provider.ReadArtifactTool, f.pin, 403)
					_, err = f.raw.Exec("UPDATE deliveries SET turn_id=? WHERE id=?", claim.TurnID, d.ID)
					artifactCmdMust(t, err)
					tool(provider.ReadArtifactTool, f.pin, 200)
					f.loseEvidence(t, "delivery_artifact_read_grants_delete", "DELETE FROM delivery_artifact_read_grants WHERE delivery_id=?", d.ID)
					tool(provider.ReadArtifactTool, f.pin, 403)
				}
			} else {
				tool(provider.ReadArtifactTool, f.pin, 403)
			}
			if fresh {
				tool(provider.LookupProjectExperiencesTool, map[string]string{"query": "CSV-218"}, 200)
			}
			invalid := artifactGrantSend(t, server.Handler, "/api/v1/member-tools/send_message",
				map[string]any{"content": artifactGrantBody, "artifact_read_grant": f.pin}, "no-model-grant", manifest.Token, 400)
			if !bytes.Contains(invalid, []byte(`"code":"invalid_json"`)) {
				t.Fatalf("member send did not reject the unknown DTO field: %s", invalid)
			}
			artifactCmdMust(t, os.Remove(hold))
			receipt := <-done
			if receipt.Outcome != provider.OutcomeSucceeded || receipt.NativeRef != nil {
				t.Fatalf("synthetic one-shot Pi failed: %+v", receipt)
			}
			tool(provider.ReadArtifactTool, f.pin, 403)
			_, err = f.db.ApplyTurnReceipt(ctx, claim, receipt)
			artifactCmdMust(t, err)
		})
	}
}
