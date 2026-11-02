//go:build unix

package httpapi

import (
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
)

func assertArtifactHTTPContent(t *testing.T, w *httptest.ResponseRecorder, data, filename string) {
	t.Helper()
	if w.Code != 200 || w.Body.String() != data || w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unverified/active content response: status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	header := w.Header().Get("Content-Disposition")
	mediaType, params, err := mime.ParseMediaType(header)
	if err != nil || mediaType != "attachment" || params["filename"] != filename || len(params) != 1 || len(header) > 1024 {
		t.Fatalf("unsafe/unbounded attachment: %q (%v), want filename %q", header, err, filename)
	}
	if !utf8.ValidString(filename) || len(filename) > 255 || strings.ContainsAny(filename, "/\\") || strings.ContainsFunc(filename, unicode.IsControl) {
		t.Fatalf("unsafe decoded filename: %q", filename)
	}
}

func TestArtifactHTTPContentExactHistoricalVersionsAfterReopen(t *testing.T) {
	f := newArtifactHTTPFixture(t)
	name := "研究报告.html"
	base := f.publish(t, platform.PublishArtifactInput{Name: name}, "<html><script>alert(1)</script></html>\x00\n")
	update := platform.PublishArtifactInput{ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID}
	head := f.publish(t, update, "head\x00\xff")
	candidate := f.publish(t, update, "unapplied candidate\n")
	later := f.publish(t, platform.PublishArtifactInput{ArtifactID: base.ArtifactID, ExpectedVersionID: head.ID}, "later")
	if candidate.Applied || !later.Applied {
		t.Fatal("fixture must include a losing version and a newer head")
	}
	artifactHTTPMust(t, f.source.Remove("input"))
	check := func() {
		t.Helper()
		for _, tt := range []struct {
			version domain.ArtifactVersion
			data    string
		}{
			{base, "<html><script>alert(1)</script></html>\x00\n"}, {head, "head\x00\xff"},
			{candidate, "unapplied candidate\n"}, {later, "later"},
		} {
			w := f.request("GET", artifactHTTPVersionPath(tt.version)+"/content", "")
			assertArtifactHTTPContent(t, w, tt.data, name)
		}
		artifact := decoded[domain.Artifact](t, f.request("GET", artifactHTTPPath(f.group.ID)+"/"+base.ArtifactID, ""), 200)
		if artifact.CurrentVersionID != later.ID {
			t.Fatalf("downloads changed head: %+v", artifact)
		}
	}
	check()
	f.reopen(t)
	check()
}

func TestArtifactHTTPContentSafeBoundedFilenames(t *testing.T) {
	f := newArtifactHTTPFixture(t)
	for _, tt := range []struct {
		label, name string
		safe        bool
	}{
		{"ascii", "report.html", true}, {"unicode", "成果 报告🌏.html", true},
		{"quoted", `report"; injected=.html`, true}, {"ascii-budget", strings.Repeat("a", 255), true},
		{"unicode-budget", strings.Repeat("界", 85), true},
		{"crlf", "report\r\nX-Injected: yes", false}, {"nul", "report\x00.html", false},
		{"control", "report\u0085.html", false}, {"unix-path", "../private/report.html", false},
		{"windows-path", `C:\private\report.html`, false}, {"dot", ".", false}, {"dotdot", "..", false},
		{"invalid-utf8", "report\xff.html", false}, {"ascii-oversized", strings.Repeat("a", 256), false},
		{"unicode-oversized", strings.Repeat("界", 86), false},
	} {
		t.Run(tt.label, func(t *testing.T) {
			v := f.publish(t, platform.PublishArtifactInput{Name: tt.name}, "<script>not active</script>")
			filename := v.ID
			if tt.safe {
				filename = tt.name
			}
			w := f.request("GET", artifactHTTPVersionPath(v)+"/content", "")
			assertArtifactHTTPContent(t, w, "<script>not active</script>", filename)
			if w.Header().Get("X-Injected") != "" {
				t.Fatal("artifact name injected a header")
			}
		})
	}
}

func TestArtifactHTTPContentScopeBeforeBlobAccess(t *testing.T) {
	f := newArtifactHTTPFixture(t)
	base := f.publish(t, platform.PublishArtifactInput{Name: "first"}, "first")
	neighbor := f.publish(t, platform.PublishArtifactInput{Name: "neighbor"}, "neighbor")
	artifactHTTPMust(t, f.blobs.Close())
	for _, scope := range [][3]string{
		{f.other.ID, base.ArtifactID, base.ID}, {f.group.ID, neighbor.ArtifactID, base.ID},
		{f.group.ID, base.ArtifactID, neighbor.ID}, {f.group.ID, base.ArtifactID, "missing"},
		{f.group.ID, "missing", base.ID}, {"missing", base.ArtifactID, base.ID},
	} {
		target := artifactHTTPPath(scope[0]) + "/" + scope[1] + "/versions/" + scope[2] + "/content"
		w := f.request("GET", target, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	w := f.request("GET", artifactHTTPVersionPath(base)+"/content", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
}

func TestArtifactHTTPContentMissingOrCorruptBytesFailBeforeSuccess(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		label := "missing"
		if corrupt {
			label = "corrupt"
		}
		t.Run(label, func(t *testing.T) {
			f := newArtifactHTTPFixture(t)
			v := f.publish(t, platform.PublishArtifactInput{Name: "private report"}, "verified bytes")
			blobPath := filepath.Join("blobs", v.BlobHash)
			artifactHTTPMust(t, f.private.Remove(blobPath))
			if corrupt {
				artifactHTTPMust(t, f.private.WriteFile(blobPath, []byte("corrupt secret bytes"), 0600))
			}
			w := f.request("GET", artifactHTTPVersionPath(v)+"/content", "")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
			if w.Body.String() != "{\"error\":{\"code\":\"internal_error\",\"message\":\"内部服务错误\"}}\n" || w.Header().Get("Content-Disposition") != "" || w.Header().Get("Content-Length") != "" {
				t.Fatalf("byte-store failure leaked details or started a binary response: %v %s", w.Header(), w.Body)
			}
			got := decoded[domain.ArtifactVersion](t, f.request("GET", artifactHTTPVersionPath(v), ""), 200)
			if got != v {
				t.Fatalf("unavailable bytes changed registry metadata: %+v", got)
			}
		})
	}
}

func TestArtifactHTTPContentNeverUsesRegistrySizeForFraming(t *testing.T) {
	f := newArtifactHTTPFixture(t)
	data := strings.Repeat("actual bytes\x00", 1000)
	v := f.publish(t, platform.PublishArtifactInput{Name: "report"}, data)
	raw := messageSQL(t, f.groupAPI)
	// Simulate damaged registry size without replacing the real service or files.
	_, err := raw.Exec("DROP TRIGGER artifact_versions_no_update")
	artifactHTTPMust(t, err)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = mustServer(t, ts.Listener.Addr().String(), ArtifactRoutes(f.service)...)
	ts.Start()
	defer ts.Close()
	for _, size := range []int64{0, 1, 1 << 40} {
		_, err := raw.Exec("UPDATE artifact_versions SET size=? WHERE id=?", size, v.ID)
		artifactHTTPMust(t, err)
		metadata := decoded[domain.ArtifactVersion](t, f.request("GET", artifactHTTPVersionPath(v), ""), 200)
		if metadata.Size != size {
			t.Fatalf("damaged registry fixture not visible: %d, want %d", metadata.Size, size)
		}
		response, err := ts.Client().Get(ts.URL + artifactHTTPVersionPath(v) + "/content")
		artifactHTTPMust(t, err)
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		artifactHTTPMust(t, readErr)
		artifactHTTPMust(t, closeErr)
		if response.StatusCode != 200 || string(body) != data || response.ContentLength >= 0 && response.ContentLength != int64(len(data)) {
			t.Fatalf("registry size controlled framing: registry=%d status=%d length=%d body=%d", size, response.StatusCode, response.ContentLength, len(body))
		}
	}
}

func TestArtifactHTTPReadOnlyRoutesIgnoreBodyAuthority(t *testing.T) {
	f := newArtifactHTTPFixture(t)
	v := f.publish(t, platform.PublishArtifactInput{Name: "report"}, "authorized exact bytes")
	foreign := f.publish(t, platform.PublishArtifactInput{Name: "other"}, "other bytes")
	path := artifactHTTPPath(f.group.ID)
	body := `{"actor_id":"member","group_id":"foreign","artifact_id":"foreign","version_id":"foreign","blob_hash":"forged","size":1,"source_root":"/","path":"/etc/passwd","grants":["all"]}`
	for _, target := range []string{path, path + "/" + v.ArtifactID, path + "/" + v.ArtifactID + "/versions", artifactHTTPVersionPath(v), artifactHTTPVersionPath(v) + "/content"} {
		baseline := f.request("GET", target, "")
		got := f.request("GET", target, body)
		if baseline.Code != 200 || got.Code != 200 || !reflect.DeepEqual(got.Header(), baseline.Header()) || got.Body.String() != baseline.Body.String() {
			t.Fatalf("body changed read authority for %s: %d %s", target, got.Code, got.Body)
		}
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
			w := f.request(method, target, body)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
			if w.Header().Get("Allow") != "GET" {
				t.Fatalf("unexpected allowed methods: %v", w.Header())
			}
		}
	}
	for _, query := range []string{"limit=1", "before=cursor", "actor_id=forged", "hash=" + foreign.BlobHash, "version_id=" + foreign.ID, "path=/etc/passwd", "unknown=", "%ZZ"} {
		w := f.request("GET", artifactHTTPVersionPath(v)+"/content?"+query, body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, target := range []string{
		"/api/v1/artifacts/" + v.BlobHash, "/api/v1/blobs/" + v.BlobHash,
		path + "/" + v.BlobHash + "/content", path + "/" + v.ArtifactID + "/content",
		path + "/" + v.BlobHash + "/versions/" + v.BlobHash + "/content",
	} {
		w := f.request("GET", target, body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	r := httptest.NewRequest(http.MethodGet, "http://attacker.example"+artifactHTTPVersionPath(v)+"/content", nil)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "invalid_host")
	raw := messageSQL(t, f.groupAPI)
	for table, want := range map[string]int{"artifacts": 2, "artifact_versions": 2, "mutation_idempotency": 0} {
		var count int
		if err := raw.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("read-only HTTP changed %s: %d (%v), want %d", table, count, err, want)
		}
	}
}

type artifactFailingWriter struct {
	*httptest.ResponseRecorder
	short  bool
	writes int
}

func (w *artifactFailingWriter) Write(data []byte) (int, error) {
	w.writes++
	n, _ := w.ResponseRecorder.Write(data[:min(7, len(data))])
	if w.short {
		return n, nil
	}
	return n, io.ErrClosedPipe
}

func TestArtifactHTTPContentWriteFailureAbortsResponse(t *testing.T) {
	f := newArtifactHTTPFixture(t)
	data := strings.Repeat("verified content", 500)
	v := f.publish(t, platform.PublishArtifactInput{Name: "report"}, data)
	for _, short := range []bool{false, true} {
		w := &artifactFailingWriter{ResponseRecorder: httptest.NewRecorder(), short: short}
		r := httptest.NewRequest("GET", "http://"+DefaultAddress+artifactHTTPVersionPath(v)+"/content", nil)
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			f.handler.ServeHTTP(w, r)
		}()
		if recovered != http.ErrAbortHandler {
			t.Fatalf("write failure returned cleanly or panicked incorrectly: short=%t panic=%v", short, recovered)
		}
		if w.Code != 200 || w.writes != 1 || w.Body.String() != data[:7] || w.Header().Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("write failure appended JSON or changed binary response: status=%d writes=%d headers=%v body=%q", w.Code, w.writes, w.Header(), w.Body.String())
		}
	}
}
