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
	ddl, err := os.ReadFile("../../internal/store/delivery_artifact_read_grant.sql")
	artifactCmdMust(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	artifactCmdMust(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(t.Context(), string(ddl))
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

// The production runner and Go Pi adapter are used with a fake CLI and an
// in-memory HTTP handler. No listener, scheduler, Pi or provider is started.
func TestStartupArtifactReadGrant(t *testing.T) {
	f := newArtifactGrantFixture(t)
	installed := false
	for _, mode := range []string{"v30 lookup only", "installed pin", "another delivery", "legacy with pin", "chat intake pin", "stale claim backlink"} {
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
