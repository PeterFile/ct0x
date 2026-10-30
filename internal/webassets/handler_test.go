package webassets_test

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/webassets"
)

const (
	indexBody = "<!doctype html><html><body>workspace</body></html>\n"
	assetBody = "export const ready = true;\n"
	assetPath = "/assets/app-Ab12Cd34.js"
	immutable = "public, max-age=31536000, immutable"
)

var buildTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func putFile(t *testing.T, dir, name, body string) {
	t.Helper()
	name = filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, buildTime, buildTime); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (*webassets.Handler, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dist")
	for name, body := range map[string]string{
		"index.html": indexBody, "assets/app-Ab12Cd34.js": assetBody,
		"assets/style-Z9_x-ABC1.css":      "body { color: black; }\n",
		"assets/nested/icon-abcDEF12.svg": "<svg></svg>\n",
		"assets/app-debug.js":             assetBody, "assets/app-1234567.js": assetBody,
		"assets/app-abcd!123.js": assetBody, "assets/report-final.txt": "report\n",
		"assets/plain.js": assetBody, "app-Ab12Cd34.js": assetBody,
		"plain.js": assetBody, "robots.txt": "User-agent: *\n", "marker": "build marker\n",
		"docs/guide.txt": "guide\n", ".env": "hidden secret\n", ".private/secret.txt": "hidden secret\n",
		"api/v1/existing": "not an API\n", "healthz/existing": "not health\n",
	} {
		putFile(t, dir, name, body)
	}
	h, err := webassets.Open(dir)
	if err != nil {
		t.Fatalf("Open valid build: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, dir
}

func request(h http.Handler, method, target, accept string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+httpapi.DefaultAddress+target, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func hasChinese(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) })
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string, head bool) {
	t.Helper()
	if w.Code != status || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("response = %d %v %q; want JSON %d", w.Code, w.Header(), w.Body.String(), status)
	}
	if head {
		if w.Body.Len() != 0 {
			t.Fatalf("HEAD error has body: %q", w.Body.String())
		}
		return
	}
	var envelope map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || len(envelope) != 1 || len(envelope["error"]) != 2 || envelope["error"]["code"] != code || !hasChinese(envelope["error"]["message"]) {
		t.Fatalf("invalid Chinese JSON error: %s (%v)", w.Body, err)
	}
}

func assertStaticHeaders(t *testing.T, w *httptest.ResponseRecorder, cache string) {
	t.Helper()
	if w.Header().Get("Cache-Control") != cache || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unexpected static headers: %v; want cache %q", w.Header(), cache)
	}
}

func TestOpenRejectsInvalidBuild(t *testing.T) {
	base := t.TempDir()
	putFile(t, base, "file", "not a directory")
	for _, dir := range []string{"empty", "directory-index"} {
		if err := os.MkdirAll(filepath.Join(base, dir, "index.html"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(base, "empty", "index.html")); err != nil {
		t.Fatal(err)
	}
	putFile(t, base, "outside.html", "outside secret")
	for _, name := range []string{"outside-index", "broken-index"} {
		if err := os.Mkdir(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
		target := "../outside.html"
		if name == "broken-index" {
			target = "missing.html"
		}
		if err := os.Symlink(target, filepath.Join(base, name, "index.html")); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"", filepath.Join(base, "absent"), filepath.Join(base, "file"), filepath.Join(base, "empty"), filepath.Join(base, "directory-index"), filepath.Join(base, "outside-index"), filepath.Join(base, "broken-index")} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			h, err := webassets.Open(dir)
			if h != nil {
				_ = h.Close()
			}
			if h != nil || err == nil || !hasChinese(err.Error()) || !strings.Contains(err.Error(), "npm run build") {
				t.Fatalf("invalid build accepted or missing startup guidance: handler=%v error=%v", h, err)
			}
		})
	}
}

func TestIndexAndDeepLinks(t *testing.T) {
	h, _ := fixture(t)
	for _, target := range []string{"/", "/index.html", "/groups/one", "/groups/one?tab=tasks", "/apiculture", "/healthzone"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := request(h, method, target, "text/html", nil)
			want := indexBody
			if method == http.MethodHead {
				want = ""
			}
			if w.Code != http.StatusOK || w.Body.String() != want || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("Content-Length") != strconv.Itoa(len(indexBody)) {
				t.Fatalf("%s %s: %d %v %q", method, target, w.Code, w.Header(), w.Body.String())
			}
			assertStaticHeaders(t, w, "no-cache")
		}
	}
	for _, target := range []string{"/", "/index.html"} {
		for _, accept := range []string{"", "application/json", "text/html;q=0"} {
			if w := request(h, http.MethodGet, target, accept, nil); w.Code != http.StatusOK || w.Body.String() != indexBody {
				t.Fatalf("index must not require HTML Accept: %s %q: %d %q", target, accept, w.Code, w.Body.String())
			}
		}
	}
}

func TestFallbackRequiresExplicitHTMLAccept(t *testing.T) {
	h, _ := fixture(t)
	for _, accept := range []string{"", "*/*", "text/*", "application/json", "text/htmlish", "text/html;q=0", "text/html;q=0, */*;q=1", "text/html;q=invalid", "text/html;q=1.1", "text/html;q=-1", "text/html;q=NaN"} {
		w := request(h, http.MethodGet, "/missing", accept, nil)
		assertError(t, w, http.StatusNotFound, "not_found", false)
		assertStaticHeaders(t, w, "no-cache")
	}
	for _, accept := range []string{"text/html", "application/json, text/html; q=0.7", "TEXT/HTML; charset=utf-8"} {
		if w := request(h, http.MethodGet, "/missing", accept, nil); w.Code != http.StatusOK || w.Body.String() != indexBody {
			t.Fatalf("HTML deep link rejected for %q: %d %s", accept, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/missing", nil)
	r.Header.Add("Accept", "application/json")
	r.Header.Add("Accept", "text/html;q=0.5")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != indexBody {
		t.Fatalf("multiple Accept lines: %d %s", w.Code, w.Body)
	}
}

func TestMissingAndReservedPathsNeverFallback(t *testing.T) {
	h, dir := fixture(t)
	for _, target := range []string{"/missing.js", "/missing.json", "/assets", "/assets/", "/assets/missing", "/assets/missing-Ab12Cd34.js", "/assets%2Fmissing", "/missing/", "/docs", "/docs/", "/index.html/child", "/api", "/api/", "/api/v1/existing", "/api/v1/missing", "/api%2Fv1%2Fmissing", "/healthz", "/healthz/", "/healthz/existing", "/healthz/missing"} {
		t.Run(target, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := request(h, method, target, "text/html", nil)
				assertError(t, w, http.StatusNotFound, "not_found", method == http.MethodHead)
				assertStaticHeaders(t, w, "no-cache")
				if strings.Contains(w.Body.String(), dir) {
					t.Fatal("filesystem path disclosed")
				}
			}
		})
	}
}

func TestRejectsUnsafePaths(t *testing.T) {
	h, _ := fixture(t)
	for _, target := range []string{"/../index.html", "/a/../index.html", "/./index.html", "/%2e%2e/index.html", "/a/%2E%2E/index.html", "/%2e/index.html", "/.env", "/%2eenv", "/.private/secret.txt", "/docs/.secret", "/%5cindex.html", "/a%5cb", "//index.html", "/a//b", "/index.html/", "/%00"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := request(h, method, target, "text/html", nil)
			assertError(t, w, http.StatusNotFound, "not_found", method == http.MethodHead)
		}
	}
	for _, target := range []string{"", "relative", "index.html", `\index.html`} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.URL.Path = target
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assertError(t, w, http.StatusNotFound, "not_found", false)
	}
}

func TestSymlinkContainment(t *testing.T) {
	h, dir := fixture(t)
	outside := filepath.Join(filepath.Dir(dir), "outside")
	putFile(t, outside, "secret.txt", "outside secret")
	for name, target := range map[string]string{"alias.js": "plain.js", "linked": "docs", "outside-link": "../outside/secret.txt", "outside-dir": "../outside", "absolute-link": filepath.Join(outside, "secret.txt")} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for target, body := range map[string]string{"/alias.js": assetBody, "/linked/guide.txt": "guide\n"} {
		w := request(h, http.MethodGet, target, "application/json", nil)
		if w.Code != http.StatusOK || w.Body.String() != body {
			t.Fatalf("safe symlink %s: %d %q", target, w.Code, w.Body.String())
		}
	}
	for _, target := range []string{"/outside-link", "/outside-dir/secret.txt", "/outside-dir/missing", "/absolute-link", "/linked"} {
		w := request(h, http.MethodGet, target, "text/html", nil)
		assertError(t, w, http.StatusNotFound, "not_found", false)
		if strings.Contains(w.Body.String(), outside) || strings.Contains(w.Body.String(), "outside secret") {
			t.Fatal("outside file information disclosed")
		}
	}
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	putFile(t, dir, "entry.html", indexBody)
	if err := os.Symlink("entry.html", filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	linked, err := webassets.Open(dir)
	if err != nil {
		t.Fatalf("safe symlink index rejected: %v", err)
	}
	defer linked.Close()
	if w := request(linked, http.MethodGet, "/", "", nil); w.Code != http.StatusOK || w.Body.String() != indexBody {
		t.Fatalf("safe symlink index not served: %d %s", w.Code, w.Body)
	}
}

func TestMIMEHeadAndCache(t *testing.T) {
	h, _ := fixture(t)
	for _, tt := range []struct{ target, mediaType, cache string }{
		{assetPath, "javascript", immutable}, {"/assets/style-Z9_x-ABC1.css", "text/css", immutable},
		{"/assets/nested/icon-abcDEF12.svg", "image/svg+xml", immutable},
		{"/app-Ab12Cd34.js", "javascript", "no-cache"}, {"/plain.js", "javascript", "no-cache"},
		{"/assets/app-debug.js", "javascript", "no-cache"}, {"/assets/app-1234567.js", "javascript", "no-cache"},
		{"/assets/app-abcd!123.js", "javascript", "no-cache"}, {"/assets/report-final.txt", "text/plain", "no-cache"},
		{"/assets/plain.js", "javascript", "no-cache"}, {"/robots.txt", "text/plain", "no-cache"}, {"/marker", "text/plain", "no-cache"},
	} {
		get := request(h, http.MethodGet, tt.target, "application/json", nil)
		head := request(h, http.MethodHead, tt.target, "", nil)
		mediaType, _, err := mime.ParseMediaType(get.Header().Get("Content-Type"))
		if err != nil || !strings.HasSuffix(mediaType, tt.mediaType) || get.Code != http.StatusOK || get.Body.Len() == 0 || head.Code != get.Code || head.Body.Len() != 0 {
			t.Fatalf("MIME/HEAD %s: GET %d %v %q HEAD %d %v %q", tt.target, get.Code, get.Header(), get.Body.String(), head.Code, head.Header(), head.Body.String())
		}
		for _, header := range []string{"Content-Type", "Content-Length", "Last-Modified", "Cache-Control", "Accept-Ranges"} {
			if get.Header().Get(header) == "" || get.Header().Get(header) != head.Header().Get(header) {
				t.Fatalf("GET/HEAD %s disagree on %s: %v / %v", tt.target, header, get.Header(), head.Header())
			}
		}
		assertStaticHeaders(t, get, tt.cache)
		assertStaticHeaders(t, head, tt.cache)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _ := fixture(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace} {
		w := request(h, method, "/", "text/html", nil)
		assertError(t, w, http.StatusMethodNotAllowed, "method_not_allowed", false)
		if w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("Allow = %q", w.Header().Get("Allow"))
		}
		assertStaticHeaders(t, w, "no-cache")
	}
}

func TestServeContentRangesAndConditions(t *testing.T) {
	h, _ := fixture(t)
	modified := buildTime.Format(http.TimeFormat)
	older := buildTime.Add(-time.Hour).Format(http.TimeFormat)
	for _, tt := range []struct {
		name, method string
		headers      map[string]string
		status       int
		body, code   string
	}{
		{"range", "GET", map[string]string{"Range": "bytes=0-5"}, 206, assetBody[:6], ""},
		{"suffix", "GET", map[string]string{"Range": "bytes=-5"}, 206, assetBody[len(assetBody)-5:], ""},
		{"head range", "HEAD", map[string]string{"Range": "bytes=0-5"}, 206, "", ""},
		{"modified", "GET", map[string]string{"If-Modified-Since": modified}, 304, "", ""},
		{"head modified", "HEAD", map[string]string{"If-Modified-Since": modified}, 304, "", ""},
		{"old modified", "GET", map[string]string{"If-Modified-Since": older}, 200, assetBody, ""},
		{"none match", "GET", map[string]string{"If-None-Match": "*"}, 304, "", ""},
		{"if range", "GET", map[string]string{"Range": "bytes=0-5", "If-Range": modified}, 206, assetBody[:6], ""},
		{"stale range", "GET", map[string]string{"Range": "bytes=0-5", "If-Range": older}, 200, assetBody, ""},
		{"unsatisfied", "GET", map[string]string{"Range": "bytes=9999-"}, 416, "", "range_not_satisfiable"},
		{"head unsatisfied", "HEAD", map[string]string{"Range": "bytes=9999-"}, 416, "", "range_not_satisfiable"},
		{"invalid range", "GET", map[string]string{"Range": "bytes=invalid"}, 416, "", "range_not_satisfiable"},
		{"precondition", "GET", map[string]string{"If-Unmodified-Since": older}, 412, "", "precondition_failed"},
		{"head precondition", "HEAD", map[string]string{"If-Match": `"missing"`}, 412, "", "precondition_failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := request(h, tt.method, assetPath, "", tt.headers)
			if tt.code != "" {
				assertError(t, w, tt.status, tt.code, tt.method == http.MethodHead)
				assertStaticHeaders(t, w, "no-cache")
			} else {
				if w.Code != tt.status || w.Body.String() != tt.body {
					t.Fatalf("response = %d %q; want %d %q", w.Code, w.Body.String(), tt.status, tt.body)
				}
				assertStaticHeaders(t, w, immutable)
			}
			if tt.name == "range" || tt.name == "head range" || tt.name == "if range" {
				if w.Header().Get("Content-Range") != fmt.Sprintf("bytes 0-5/%d", len(assetBody)) {
					t.Fatalf("invalid range metadata: %v", w.Header())
				}
			}
			if strings.Contains(tt.name, "unsatisfied") && w.Header().Get("Content-Range") != fmt.Sprintf("bytes */%d", len(assetBody)) {
				t.Fatalf("missing unsatisfied range size: %v", w.Header())
			}
		})
	}
}

func TestRootHandleAndCloseLifecycle(t *testing.T) {
	h, dir := fixture(t)
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	putFile(t, dir, "index.html", "replacement directory")
	var readers sync.WaitGroup
	for range 24 {
		readers.Go(func() {
			w := request(h, http.MethodGet, "/", "", nil)
			if w.Code != http.StatusOK || w.Body.String() != indexBody {
				t.Errorf("root handle lost original directory: %d %q", w.Code, w.Body.String())
			}
		})
	}
	readers.Wait()
	if err := h.Close(); err != nil {
		t.Fatalf("Close after requests drained: %v", err)
	}
	assertError(t, request(h, http.MethodHead, "/", "", nil), http.StatusNotFound, "not_found", true)
}

func TestTransportRoutePrecedenceAndGuards(t *testing.T) {
	h, _ := fixture(t)
	server, err := httpapi.NewServerWithConfig(httpapi.Config{Address: httpapi.DefaultAddress},
		httpapi.Route{Method: http.MethodGet, Pattern: "/", Handler: h.ServeHTTP},
		httpapi.Route{Method: http.MethodHead, Pattern: "/", Handler: h.ServeHTTP},
		httpapi.Route{Method: http.MethodGet, Pattern: "/api/v1/probe", Handler: func(w http.ResponseWriter, _ *http.Request) {
			httpapi.WriteJSON(w, http.StatusOK, map[string]string{"source": "api"})
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for target, body := range map[string]string{"/": indexBody, "/groups/one": indexBody, "/healthz": "{\"status\":\"ready\"}\n", "/api/v1/probe": "{\"source\":\"api\"}\n"} {
		w := request(server.Handler, http.MethodGet, target, "text/html", nil)
		if w.Code != http.StatusOK || w.Body.String() != body {
			t.Fatalf("specific route precedence %s: %d %q", target, w.Code, w.Body.String())
		}
	}
	for _, target := range []string{"/api", "/api/v1/missing", "/healthz/missing", "/assets/missing-Ab12Cd34.js"} {
		assertError(t, request(server.Handler, http.MethodGet, target, "text/html", nil), http.StatusNotFound, "not_found", false)
	}
	assertError(t, request(server.Handler, http.MethodHead, "/missing.js", "text/html", nil), http.StatusNotFound, "not_found", true)
	for _, tt := range []struct{ method, host, origin, code string }{
		{"GET", "attacker.example:7310", "", "invalid_host"},
		{"POST", httpapi.DefaultAddress, "http://attacker.example", "invalid_origin"},
	} {
		r := httptest.NewRequest(tt.method, "/", nil)
		r.Host = tt.host
		if tt.origin != "" {
			r.Header.Set("Origin", tt.origin)
		}
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		assertError(t, w, http.StatusForbidden, tt.code, false)
	}
}
