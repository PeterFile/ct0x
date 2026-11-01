//go:build darwin || linux

package webassets_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentworkspace/internal/webassets"
)

// A failing implementation must be unblocked and joined before test cleanup
// closes the handler or removes the FIFO. O_RDWR|O_NONBLOCK works on both targets.
func assertNoFIFOBlock(t *testing.T, fifo string, operation func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		operation()
	}()
	select {
	case <-done:
		return
	case <-time.After(500 * time.Millisecond):
	}
	peer, err := syscall.Open(fifo, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open nonblocking FIFO cleanup peer: %v", err)
	}
	defer syscall.Close(peer)
	select {
	case <-done:
		t.Fatal("operation blocked on FIFO for 500ms; cleanup peer released and joined it")
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not finish after opening FIFO cleanup peer")
	}
}

func TestOpenRejectsFIFORootPromptly(t *testing.T) {
	base := t.TempDir()
	fifo := filepath.Join(base, "root-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "root-link")
	if err := os.Symlink("root-fifo", link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{fifo, link} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var h *webassets.Handler
			var err error
			assertNoFIFOBlock(t, fifo, func() {
				h, err = webassets.Open(dir)
				if h != nil {
					_ = h.Close()
				}
			})
			if h != nil || err == nil || !hasChinese(err.Error()) || !strings.Contains(err.Error(), "npm run build") {
				t.Fatalf("FIFO root must produce Chinese build guidance: handler=%v error=%v", h, err)
			}
		})
	}
}

func TestOpenRejectsFIFOIndexPromptly(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "index.html")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	var h *webassets.Handler
	var err error
	assertNoFIFOBlock(t, fifo, func() {
		h, err = webassets.Open(dir)
		if h != nil {
			_ = h.Close()
		}
	})
	if h != nil || err == nil || !hasChinese(err.Error()) || !strings.Contains(err.Error(), "npm run build") {
		t.Fatalf("FIFO index must produce Chinese build guidance: handler=%v error=%v", h, err)
	}
}

func TestServeHTTPRejectsFIFOPromptly(t *testing.T) {
	h, dir := fixture(t)
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			r := httptest.NewRequest(method, "/pipe", nil)
			r.Header.Set("Accept", "text/html")
			w := httptest.NewRecorder()
			assertNoFIFOBlock(t, fifo, func() { h.ServeHTTP(w, r) })
			assertError(t, w, http.StatusNotFound, "not_found", method == http.MethodHead)
			assertStaticHeaders(t, w, "no-cache")
		})
	}
}
