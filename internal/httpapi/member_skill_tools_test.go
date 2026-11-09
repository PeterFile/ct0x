package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/skills"
)

const memberSkillHTTPDescription = "中文技能说明 Test Description"

func memberSkillHTTPRaw(name, license, body string) string {
	return "---\nname: " + name + "\ndescription: " + memberSkillHTTPDescription + "\nlicense: '" + license + "'\ncompatibility: local\nallowed-tools: Bash(*)\nmetadata: {private: PRIVATE-METADATA}\n---\n" + body
}

func memberSkillHTTPPut(t *testing.T, root, resource, data string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(resource))
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func memberSkillHTTPJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func memberSkillHTTPCatalog(t *testing.T, roots []skills.Root, limits skills.Limits) *platform.MemberSkillCatalog {
	t.Helper()
	catalog, err := platform.NewMemberSkillCatalog(roots, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := catalog.Close(); err != nil {
			t.Error(err)
		}
	})
	return catalog
}

type memberSkillToolHTTPFixture struct {
	*memberToolHTTPFixture
	root    string
	catalog *platform.MemberSkillCatalog
}

func newMemberSkillToolHTTP(t *testing.T, operations ...string) *memberSkillToolHTTPFixture {
	t.Helper()
	f := &memberSkillToolHTTPFixture{memberToolHTTPFixture: newMemberToolHTTP(t), root: t.TempDir()}
	memberSkillHTTPPut(t, f.root, "demo/SKILL.md", memberSkillHTTPRaw("demo", "MIT", "PRIVATE-BODY 正文🙂\n"))
	catalog := memberSkillHTTPCatalog(t, []skills.Root{{ID: "authorized", Path: f.root}}, skills.DefaultLimits())
	f.useCatalog(t, catalog, operations...)
	f.routes = append(f.routes, MemberSkillToolRoutes(f.tools)...)
	f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
	return f
}

func (f *memberSkillToolHTTPFixture) useCatalog(t *testing.T, catalog *platform.MemberSkillCatalog, operations ...string) {
	t.Helper()
	if len(operations) == 0 {
		operations = []string{"search_skills", "load_skill"}
	}
	f.tools.Revoke(f.token)
	token, err := f.tools.Issue(context.Background(), f.claim, platform.MemberToolGrants{Operations: operations, Skills: catalog})
	if err != nil {
		t.Fatal(err)
	}
	f.token, f.catalog = token, catalog
	t.Cleanup(func() { f.tools.Revoke(token) })
}

func (f *memberSkillToolHTTPFixture) requestToken(t *testing.T, token, operation, body string, keys ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := f.newRequest(operation, body, keys...)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
	if bytes.Contains(w.Body.Bytes(), []byte(token)) || w.Code >= 400 && strings.Contains(w.Body.String(), f.root) {
		t.Fatal("response exposed a capability or configured root")
	}
	return w
}

func (f *memberSkillToolHTTPFixture) request(t *testing.T, operation, body string, keys ...string) *httptest.ResponseRecorder {
	t.Helper()
	return f.requestToken(t, f.token, operation, body, keys...)
}

func (f *memberSkillToolHTTPFixture) state(t *testing.T) string {
	t.Helper()
	return memberHistoryToolState(t, f.memberToolHTTPFixture)
}

func (f *memberSkillToolHTTPFixture) identity(t *testing.T) platform.MemberSkillIdentity {
	t.Helper()
	page := decoded[platform.MemberSkillSearchResult](t, f.request(t, "search_skills", `{"query":"demo"}`), 200)
	if len(page.Items) != 1 {
		t.Fatalf("expected one real skill: %+v", page)
	}
	return page.Items[0].Identity
}

func memberSkillHTTPBytes(t *testing.T, result platform.MemberSkillLoadResult) string {
	t.Helper()
	if result.Encoding == "utf8" {
		return result.Content
	}
	if result.Encoding != "base64" {
		t.Fatalf("unexpected encoding %q", result.Encoding)
	}
	body, err := base64.StdEncoding.DecodeString(result.Content)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestMemberSkillToolHTTPExplicitOptIn(t *testing.T) {
	f := newMemberToolHTTP(t)
	for _, history := range []bool{false, true} {
		if history {
			f.routes = append(f.routes, MemberHistoryToolRoutes(f.tools)...)
			f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
		}
		want := 4
		if history {
			want = 6
		}
		if len(f.routes) != want || len(MemberToolRoutes(f.tools)) != 4 || len(MemberHistoryToolRoutes(f.tools)) != 2 {
			t.Fatal("default four/history six routes changed")
		}
		for _, operation := range []string{"search_skills", "load_skill"} {
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, f.newRequest(operation, `{}`))
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
		}
	}
	routes := MemberSkillToolRoutes(f.tools)
	if len(routes) != 2 {
		t.Fatalf("skill opt-in has %d routes, want two", len(routes))
	}
	for i, operation := range []string{"search_skills", "load_skill"} {
		if routes[i].Method != http.MethodPost || routes[i].Pattern != "/api/v1/member-tools/"+operation {
			t.Fatalf("unexpected opt-in route: %+v", routes[i])
		}
	}
	f.routes = append(f.routes, routes...)
	f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
	before := memberHistoryToolState(t, f)
	for _, request := range []struct{ operation, body string }{{"search_skills", `{}`}, {"load_skill", `{"handle":"unknown"}`}} {
		w := f.request(t, request.operation, request.body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
	}
	if memberHistoryToolState(t, f) != before {
		t.Fatal("mounting routes granted authority or changed durable state")
	}
}

func TestMemberSkillToolHTTPRealNativeClosedLoop(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	binary := "\x00\xffA🙂Z"
	memberSkillHTTPPut(t, f.root, "demo/资料/二进制.bin", binary)
	before := f.state(t)
	ts := httptest.NewUnstartedServer(nil)
	if !strings.HasPrefix(ts.Listener.Addr().String(), "127.0.0.1:") || ts.Listener.Addr().String() == DefaultAddress {
		t.Fatal("fixture must use its own IPv4 loopback random port")
	}
	ts.Config = mustServer(t, ts.Listener.Addr().String(), f.routes...)
	ts.Start()
	t.Cleanup(ts.Close)
	client := ts.Client()
	client.Timeout = 5 * time.Second
	request := func(operation, body string) (int, http.Header, []byte) {
		t.Helper()
		r, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/member-tools/"+operation, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+f.token)
		// Read-only calls ignore malformed duplicate write keys.
		r.Header.Add("Idempotency-Key", "has space")
		r.Header.Add("Idempotency-Key", "duplicate")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		f.safeResponse(t, response.StatusCode, response.Header, data, true)
		return response.StatusCode, response.Header, data
	}
	status, _, data := request("search_skills", `{}`)
	var page platform.MemberSkillSearchResult
	if status != 200 || json.Unmarshal(data, &page) != nil || len(page.Items) != 1 || page.Items[0].Description != memberSkillHTTPDescription {
		t.Fatalf("real native search failed: %d %s", status, data)
	}
	native, err := f.tools.SearchSkills(context.Background(), f.token, platform.MemberSkillSearchInput{})
	if err != nil || string(data) != memberSkillHTTPJSON(t, native)+"\n" {
		t.Fatalf("HTTP changed the exact core search DTO: %v", err)
	}
	for _, secret := range []string{f.root, "PRIVATE", "allowed", "SourcePath", "Problems", "Bash"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("metadata disclosed %q", secret)
		}
	}
	status, _, data = request("load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": page.Items[0].Identity}))
	var loaded platform.MemberSkillLoadResult
	raw := memberSkillHTTPRaw("demo", "MIT", "PRIVATE-BODY 正文🙂\n")
	if status != 200 || json.Unmarshal(data, &loaded) != nil || loaded.Identity != page.Items[0].Identity || loaded.Handle == "" || loaded.Resource != "SKILL.md" || loaded.Content != raw || loaded.Encoding != "utf8" || loaded.Size != int64(len(raw)) || loaded.NextOffset != loaded.Size || !loaded.ContentComplete || loaded.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) || len(loaded.SnapshotVersion) != 64 {
		t.Fatalf("real native selection failed: %d %s", status, data)
	}
	status, _, data = request("load_skill", memberSkillHTTPJSON(t, map[string]any{"handle": loaded.Handle, "resource": "资料/二进制.bin", "offset": 1, "max_bytes": 1}))
	var fragment platform.MemberSkillLoadResult
	if status != 200 || json.Unmarshal(data, &fragment) != nil || fragment.Encoding != "base64" || memberSkillHTTPBytes(t, fragment) != "\xff" || fragment.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(binary))) || fragment.Offset != 1 || fragment.NextOffset != 2 || fragment.ContentComplete || fragment.SnapshotVersion != loaded.SnapshotVersion {
		t.Fatalf("real native resource read failed: %d %s", status, data)
	}
	identity := memberSkillHTTPJSON(t, page.Items[0].Identity)
	for _, extra := range []string{`,"caller":"forged"`, `,"source_path":"/private"`, `,"ROOT_ID":"forged"`, `,"name":null`} {
		body := `{"identity":` + strings.TrimSuffix(identity, "}") + extra + `}}`
		status, header, data := request("load_skill", body)
		assertError(t, status, header, data, 400, "invalid_json")
	}
	if f.state(t) != before {
		t.Fatal("real HTTP reads created key rows, receipts, acknowledgements or changed claims")
	}
	t.Logf("native closed loop on %s; nested identity rejected by the actual HTTP client", ts.Listener.Addr())
}

func TestMemberSkillToolHTTPPaginationPreservesCoreDTO(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	wantNames := []string{"demo"}
	for i := range 52 {
		name := fmt.Sprintf("entry-%03d", i)
		wantNames = append(wantNames, name)
		memberSkillHTTPPut(t, f.root, name+"/SKILL.md", memberSkillHTTPRaw(name, "MIT", "PRIVATE-BODY"))
	}
	memberSkillHTTPPut(t, f.root, "invalid/SKILL.md", "PRIVATE-INVALID")
	before := f.state(t)
	first := f.request(t, "search_skills", `{}`)
	page := decoded[platform.MemberSkillSearchResult](t, first, 200)
	memberToolHTTPKeys(t, first, "items", "next_before")
	if len(page.Items) != 20 || page.NextBefore == "" {
		t.Fatalf("omitted query/limit did not preserve listing default 20: %+v", page)
	}
	for _, body := range []string{`{"query":""}`, `{"before":""}`, `{"query":"","limit":20,"before":""}`} {
		if w := f.request(t, "search_skills", body, "has space", "duplicate"); !bytes.Equal(first.Body.Bytes(), w.Body.Bytes()) {
			t.Fatal("empty optional fields or write-key headers changed the read")
		}
	}
	native, err := f.tools.SearchSkills(context.Background(), f.token, platform.MemberSkillSearchInput{})
	if err != nil || first.Body.String() != memberSkillHTTPJSON(t, native)+"\n" {
		t.Fatalf("HTTP wrapped the native DTO or cursor: %v", err)
	}
	for _, limit := range []int{1, 50} {
		result := decoded[platform.MemberSkillSearchResult](t, f.request(t, "search_skills", fmt.Sprintf(`{"query":"中文","limit":%d}`, limit)), 200)
		if len(result.Items) != limit || result.NextBefore == "" {
			t.Fatalf("explicit boundary limit %d changed: %+v", limit, result)
		}
	}
	var names []string
	for {
		for _, item := range page.Items {
			names = append(names, item.Name)
			if item.Identity.RootID != "authorized" || item.Identity.Name != item.Name || len(item.Identity.SkillSHA256) != 64 || item.Description != memberSkillHTTPDescription || item.License != "MIT" || item.Compatibility != "local" {
				t.Fatalf("search projection changed: %+v", item)
			}
		}
		if page.NextBefore == "" {
			break
		}
		page = decoded[platform.MemberSkillSearchResult](t, f.request(t, "search_skills", memberSkillHTTPJSON(t, map[string]any{"before": page.NextBefore})), 200)
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("opaque pagination skipped, duplicated or reordered names: %v", names)
	}
	for _, body := range []string{`{"query":"absent"}`, `{"query":" 中文 "}`} {
		if w := f.request(t, "search_skills", body); w.Code != 200 || w.Body.String() != "{\"items\":[],\"next_before\":\"\"}\n" {
			t.Fatalf("empty projection or core query semantics changed: %d %s", w.Code, w.Body)
		}
	}
	caseFold := decoded[platform.MemberSkillSearchResult](t, f.request(t, "search_skills", `{"query":"TEST description","limit":1}`), 200)
	if len(caseFold.Items) != 1 {
		t.Fatal("core case-insensitive query was changed")
	}
	bad := f.request(t, "search_skills", memberSkillHTTPJSON(t, map[string]string{"query": "different", "before": native.NextBefore}))
	assertError(t, bad.Code, bad.Header(), bad.Body.Bytes(), 400, "invalid_input")
	if f.state(t) != before {
		t.Fatal("metadata pagination changed durable state")
	}
}

func TestMemberSkillToolHTTPFrozenLoadRawByteOffsets(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	raw := memberSkillHTTPRaw("demo", "MIT", strings.Repeat("正文🙂<>&\x00\n", 1800))
	binary := "\x00\xffA🙂Z"
	memberSkillHTTPPut(t, f.root, "demo/SKILL.md", raw)
	memberSkillHTTPPut(t, f.root, "demo/资料/data.bin", binary)
	id := f.identity(t)
	before := f.state(t)
	firstResponse := f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": id}), "has space", "duplicate")
	first := decoded[platform.MemberSkillLoadResult](t, firstResponse, 200)
	memberToolHTTPKeys(t, firstResponse, "identity", "handle", "snapshot_version", "resource", "sha256", "size", "offset", "next_offset", "content_complete", "encoding", "content")
	if first.Identity != id || first.Offset != 0 || first.NextOffset != 8192 || first.Size != int64(len(raw)) || first.ContentComplete || first.Resource != "SKILL.md" || memberSkillHTTPBytes(t, first) != raw[:8192] || first.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) {
		t.Fatalf("default 8192 raw bytes or full-file hash changed: %+v", first)
	}
	for _, fields := range []map[string]any{{"handle": first.Handle}, {"identity": id, "resource": "", "offset": 0}} {
		if same := f.request(t, "load_skill", memberSkillHTTPJSON(t, fields)); !bytes.Equal(same.Body.Bytes(), firstResponse.Body.Bytes()) {
			t.Fatal("empty resource or cached identity did not reuse the exact snapshot")
		}
	}
	native, err := f.tools.LoadSkill(context.Background(), f.token, platform.MemberSkillLoadInput{Handle: first.Handle})
	if err != nil || firstResponse.Body.String() != memberSkillHTTPJSON(t, native)+"\n" {
		t.Fatalf("HTTP changed the exact load DTO: %v", err)
	}
	for _, offset := range []int64{0, 1, 3, 4, int64(len(binary))} {
		part := decoded[platform.MemberSkillLoadResult](t, f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"handle": first.Handle, "resource": "资料/data.bin", "offset": offset, "max_bytes": 1})), 200)
		end := min(offset+1, int64(len(binary)))
		if memberSkillHTTPBytes(t, part) != binary[offset:end] || part.Offset != offset || part.NextOffset != end || part.ContentComplete != (end == int64(len(binary))) || part.Size != int64(len(binary)) || part.SnapshotVersion != first.SnapshotVersion || part.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(binary))) || (offset == 1 || offset == 3 || offset == 4) && part.Encoding != "base64" {
			t.Fatalf("binary fragment lost byte offsets/hash/encoding: %+v", part)
		}
	}
	var rebuilt strings.Builder
	for offset := int64(0); offset < int64(len(raw)); {
		part := decoded[platform.MemberSkillLoadResult](t, f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"handle": first.Handle, "offset": offset, "max_bytes": 16384})), 200)
		end := min(offset+16384, int64(len(raw)))
		if memberSkillHTTPBytes(t, part) != raw[offset:end] || part.NextOffset != end || part.ContentComplete != (end == int64(len(raw))) {
			t.Fatal("maximum raw fragment was truncated or mislabeled")
		}
		rebuilt.WriteString(memberSkillHTTPBytes(t, part))
		offset = end
	}
	if rebuilt.String() != raw {
		t.Fatal("HTTP fragments lost Chinese, binary NUL or original whitespace")
	}
	memberSkillHTTPPut(t, f.root, "demo/SKILL.md", memberSkillHTTPRaw("demo", "MIT", "changed source"))
	memberSkillHTTPPut(t, f.root, "demo/资料/data.bin", "changed bytes")
	newID := f.identity(t)
	if newID == id {
		t.Fatal("listing did not observe the changed source")
	}
	changed := f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": newID}))
	assertError(t, changed.Code, changed.Header(), changed.Body.Bytes(), 409, "skill_changed")
	if changed.Body.String() != "{\"error\":{\"code\":\"skill_changed\",\"message\":\"技能版本不匹配\"}}\n" {
		t.Fatal("version error was not fixed safe Chinese")
	}
	if err := os.RemoveAll(filepath.Join(f.root, "demo")); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []map[string]any{{"identity": id}, {"handle": first.Handle}} {
		w := f.request(t, "load_skill", memberSkillHTTPJSON(t, selector))
		if !bytes.Equal(w.Body.Bytes(), firstResponse.Body.Bytes()) {
			t.Fatal("source change/deletion invalidated frozen content, version or handle")
		}
	}
	part := decoded[platform.MemberSkillLoadResult](t, f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"handle": first.Handle, "resource": "资料/data.bin"})), 200)
	if memberSkillHTTPBytes(t, part) != binary || part.SnapshotVersion != first.SnapshotVersion {
		t.Fatal("resource source deletion changed captured bytes")
	}
	if f.state(t) != before {
		t.Fatal("snapshot reads changed durable claims, acknowledgements or write keys")
	}
}

func TestMemberSkillToolHTTPStaleIdentityBeforeSelection(t *testing.T) {
	f := newMemberSkillToolHTTP(t)
	id := f.identity(t)
	before := f.state(t)
	// Before any snapshot is cached, the core validates canonical hash spelling.
	bad := id
	bad.SkillSHA256 = strings.ToUpper(bad.SkillSHA256)
	w := f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": bad}))
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	memberSkillHTTPPut(t, f.root, "demo/SKILL.md", memberSkillHTTPRaw("demo", "MIT", "changed before selection"))
	w = f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": id}))
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "skill_changed")
	newID := f.identity(t)
	loaded := decoded[platform.MemberSkillLoadResult](t, f.request(t, "load_skill", memberSkillHTTPJSON(t, map[string]any{"identity": newID})), 200)
	if loaded.Identity != newID || !strings.Contains(loaded.Content, "changed before selection") {
		t.Fatal("failed stale selection prevented a valid first snapshot")
	}
	if f.state(t) != before {
		t.Fatal("stale selection created durable effects")
	}
}
