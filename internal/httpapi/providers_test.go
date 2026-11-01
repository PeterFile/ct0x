package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
)

const catalogSecret = "Bearer sk-fixture-catalog-must-not-escape"

func providerAPI(t *testing.T, search provider.Search) *groupAPI {
	t.Helper()
	a := openGroupAPI(t, "")
	catalog := platform.DiscoverProviders(context.Background(), search, provider.Runner{Timeout: time.Second, OutputLimit: 4096})
	routes := append(LocalUserRoutes(platform.NewLocalUser(a.db)), ProviderRoute(catalog))
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	return a
}

func providerFixture(t *testing.T, dir, name, version, account, calls string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	body := "#!/bin/sh\nprintf 'call\\n' >> '" + calls + "'\nprintf '%s' '" + catalogSecret + "' >&2\ncase \"$*\" in\n--version) printf '%s' '" + version + "';;\n'auth check --provider openai-codex --json --no-refresh') " + account + ";;\n*) exit 93;;\nesac\n"
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEmptyProviderCatalog(t *testing.T) {
	a := providerAPI(t, provider.Search{})
	w := a.request("GET", "/api/v1/providers", "")
	page := decoded[listResult[provider.Provider]](t, w, 200)
	if page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("empty provider response: %+v", page)
	}
	if w.Body.String() != "{\"items\":[],\"next_cursor\":\"\"}\n" {
		t.Fatalf("unexpected catalog envelope: %s", w.Body)
	}
}

func TestProviderCatalogFixturesAndSnapshotPaging(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	ready := `printf '%s' '{"status":"ready","provider":"openai-codex","authType":"oauth","secret":"` + catalogSecret + `"}'`
	absent := `printf '%s' '{"status":"not_ready","provider":"openai-codex","reason":"credentials_not_configured"}'; exit 1`
	first := providerFixture(t, filepath.Join(root, "one"), provider.Pi, "1.4.0", ready, calls)
	providerFixture(t, filepath.Join(root, "two"), provider.Pi, "9.9.9", "exit 93", calls)
	providerFixture(t, filepath.Join(root, "three"), provider.Pi, "1.4.0", absent, calls)
	providerFixture(t, filepath.Join(root, "four"), provider.Codex, "codex-cli 9.9.9", "exit 93", calls)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	search := provider.Search{Explicit: []provider.Location{{Name: provider.Pi, Path: alias}}, PathDirs: []string{filepath.Join(root, "one"), filepath.Join(root, "two"), filepath.Join(root, "three"), filepath.Join(root, "four")}, CommonDirs: []string{filepath.Join(root, "one")}}
	a := providerAPI(t, search)
	initial := a.request("GET", "/api/v1/providers", "")
	all := decoded[listResult[provider.Provider]](t, initial, 200)
	if len(all.Items) != 4 || all.NextCursor != "" {
		t.Fatalf("discovery did not preserve/deduplicate installations: %+v", all)
	}
	accounts := map[string]provider.Account{alias: provider.Authenticated, filepath.Join(root, "two", "pi"): provider.AccountUnknown, filepath.Join(root, "three", "pi"): provider.Unauthenticated, filepath.Join(root, "four", "codex"): provider.AccountUnknown}
	for i, p := range all.Items {
		if p.ID == "" || p.Version == "" || p.Account != accounts[p.Path] || p.Installation != provider.Installed || p.Connectivity != provider.Unchecked || p.Capabilities != (provider.Capabilities{}) || len(p.Evidence) != 3 {
			t.Fatalf("independent catalog states changed: %+v", p)
		}
		if i > 0 && all.Items[i-1].ID >= p.ID {
			t.Fatal("provider catalog must have stable ascending ID order")
		}
		if p.Path == filepath.Join(root, "two", "pi") && p.Evidence[2].Reason != "此版本尚未验证只读账户探针" {
			t.Fatal("account probe bypassed the verified-version gate")
		}
		for _, evidence := range p.Evidence {
			if evidence.At.IsZero() || evidence.Reason == "" {
				t.Fatal("missing sanitized probe evidence")
			}
		}
	}
	for _, fragment := range []string{catalogSecret, "authType", "Logged in using", "stdout", "stderr"} {
		if strings.Contains(initial.Body.String(), fragment) {
			t.Fatal("raw fixture output escaped into HTTP/evidence")
		}
	}
	firstPage := decoded[listResult[provider.Provider]](t, a.request("GET", "/api/v1/providers?limit=2", ""), 200)
	lastPage := decoded[listResult[provider.Provider]](t, a.request("GET", "/api/v1/providers?limit=2&before="+firstPage.NextCursor, ""), 200)
	if len(firstPage.Items) != 2 || firstPage.NextCursor == "" || firstPage.Items[0].ID != all.Items[0].ID || firstPage.Items[1].ID != all.Items[1].ID || len(lastPage.Items) != 2 || lastPage.NextCursor != "" || lastPage.Items[0].ID != all.Items[2].ID || lastPage.Items[1].ID != all.Items[3].ID {
		t.Fatal("ID paging skipped/duplicated rows or returned an extra exact-full final cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(firstPage.NextCursor)
	if err != nil || bytes.Contains(raw, []byte("created_at")) || !bytes.Contains(raw, []byte(`"scope":"providers"`)) {
		t.Fatal("catalog cursor must be opaque, ID-based and scoped")
	}
	for range 3 {
		if repeat := a.request("GET", "/api/v1/providers", ""); repeat.Code != 200 || !bytes.Equal(repeat.Body.Bytes(), initial.Body.Bytes()) {
			t.Fatal("GET changed the startup snapshot")
		}
	}
	body, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(body), "call\n") != 6 {
		t.Fatalf("discovery aliases, polling or pagination caused extra probes: %v", err)
	}
	for _, path := range []string{"/api/v1/groups?before=" + firstPage.NextCursor, "/api/v1/groups/missing/members?before=" + firstPage.NextCursor} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	newGroup(t, a, "one")
	newGroup(t, a, "two")
	groupPage := decoded[listResult[map[string]any]](t, a.request("GET", "/api/v1/groups?limit=1", ""), 200)
	w := a.request("GET", "/api/v1/providers?before="+groupPage.NextCursor, "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
}

func TestProviderQueryAndCursorValidation(t *testing.T) {
	a := providerAPI(t, provider.Search{})
	for _, query := range []string{"limit=0", "limit=-1", "limit=201", "limit=1.5", "limit=", "limit=1&limit=2", "limit=1&%6cimit=2", "limit=%ZZ", "limit=1;before=x", "extra=x", "before=", "before=a&before=b", "before=invalid", "before=" + strings.Repeat("x", 2049)} {
		w := a.request("GET", "/api/v1/providers?"+query, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	valid := `{"v":1,"scope":"providers","id":"pi_opaque-id"}`
	for _, raw := range []string{`null`, `[]`, `{}`, valid + `{}`, strings.Replace(valid, `"v":1`, `"v":2`, 1), strings.Replace(valid, `providers`, `groups`, 1), strings.Replace(valid, `pi_opaque-id`, ``, 1), strings.Replace(valid, `pi_opaque-id`, `has space`, 1), strings.Replace(valid, `"v":1`, `"v":1,"v":1`, 1), strings.TrimSuffix(valid, "}") + `,"created_at":"2026-10-30T12:00:00Z"}`} {
		w := a.request("GET", "/api/v1/providers?before="+base64.RawURLEncoding.EncodeToString([]byte(raw)), "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	decoded[listResult[provider.Provider]](t, a.request("GET", "/api/v1/providers?limit=200", ""), 200)
	w := a.request("POST", "/api/v1/providers", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
	w = a.request("POST", "/api/v1/providers/refresh", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
}
