// Package webassets serves an externally built SPA from a trusted local directory.
// The build tree must contain only approved public files and must not be writable
// by sandboxed members. It must not permit malicious file-type swaps between the
// metadata precheck and open. Root confines path and symlink resolution, not
// mounts, hard links, or special devices; this handler is not a filesystem sandbox.
// Mount it behind the transport's Host/Origin guards, preserving specific API
// and health routes ahead of the GET/HEAD catchall.
package webassets

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"agentworkspace/internal/httpapi"
)

var viteAsset = regexp.MustCompile(`^assets/(?:[^/]+/)*[^/]+-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$`)

// Handler holds an open build-directory handle and supports concurrent requests.
// It does not own the HTTP server or the build output.
type Handler struct {
	root *os.Root
}

// Open validates the build directory and its readable, regular index.html.
// Relative symlinks resolving within the build directory are supported.
func Open(dir string) (*Handler, error) {
	if dir == "" {
		return nil, errors.New("网页构建目录不能为空，请先运行 npm run build")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.New("无法打开网页构建目录，请先运行 npm run build")
	}
	h := &Handler{root: root}
	index, _, err := h.openRegular("index.html")
	if err != nil {
		_ = root.Close()
		return nil, errors.New("网页构建目录缺少可读取的普通文件 index.html，请先运行 npm run build")
	}
	_ = index.Close()
	return h, nil
}

// Close releases the directory handle. The caller must first stop accepting
// requests and drain all in-flight requests, then close the handler.
func (h *Handler) Close() error { return h.root.Close() }

// ServeHTTP serves only GET and HEAD; rejected paths never receive SPA fallback.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w = headResponse{w}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpapi.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "网页资源仅支持 GET 和 HEAD 请求")
		return
	}
	name, valid := requestName(r.URL.Path)
	if !valid || within(name, "api") || within(name, "healthz") {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "网页资源不存在")
		return
	}
	file, info, err := h.openRegular(name)
	if errors.Is(err, fs.ErrNotExist) && !within(name, "assets") && path.Ext(name) == "" && acceptsHTML(r) {
		name = "index.html"
		file, info, err = h.openRegular(name)
	}
	if err != nil {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "网页资源不存在")
		return
	}
	defer file.Close()
	if viteAsset.MatchString(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(&contentResponse{ResponseWriter: w}, r, name, info.ModTime(), file)
}

func (h *Handler) openRegular(name string) (*os.File, fs.FileInfo, error) {
	// Reject special files before Open: a FIFO can block before file.Stat runs.
	info, err := h.root.Stat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fs.ErrInvalid
	}
	file, err := h.root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err = file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fs.ErrInvalid
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func requestName(p string) (string, bool) {
	if p == "/" {
		return "index.html", true
	}
	// URL.Path is already decoded by net/http. Never clean away unsafe segments
	// or unescape a second time: the filesystem must see the validated path.
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return "", false
	}
	name := p[1:]
	if !fs.ValidPath(name) {
		return "", false
	}
	for _, segment := range strings.Split(name, "/") {
		if strings.HasPrefix(segment, ".") {
			return "", false
		}
	}
	return name, true
}

func within(name, prefix string) bool {
	return name == prefix || strings.HasPrefix(name, prefix+"/")
}

func acceptsHTML(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept") {
		for _, entry := range strings.Split(value, ",") {
			mediaType, params, err := mime.ParseMediaType(entry)
			if err != nil || mediaType != "text/html" {
				continue
			}
			if quality, ok := params["q"]; ok {
				q, err := strconv.ParseFloat(quality, 64)
				if err != nil || !(q > 0 && q <= 1) {
					continue
				}
			}
			return true
		}
	}
	return false
}

type headResponse struct{ http.ResponseWriter }

func (w headResponse) Write(p []byte) (int, error) { return len(p), nil }

// contentResponse keeps ServeContent's status and range metadata, but replaces
// its English error bodies without buffering successful file transfers.
type contentResponse struct {
	http.ResponseWriter
	failed bool
}

func (w *contentResponse) WriteHeader(status int) {
	if status < http.StatusBadRequest {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.failed = true
	for _, header := range []string{"Content-Length", "Content-Encoding", "ETag", "Last-Modified"} {
		w.Header().Del(header)
	}
	w.Header().Set("Cache-Control", "no-cache")
	code, message := "internal_error", "读取网页资源失败"
	switch status {
	case http.StatusRequestedRangeNotSatisfiable:
		code, message = "range_not_satisfiable", "请求范围无效或超出资源大小"
	case http.StatusPreconditionFailed:
		code, message = "precondition_failed", "请求条件与资源当前状态不匹配"
	}
	httpapi.WriteError(w.ResponseWriter, status, code, message)
}

func (w *contentResponse) Write(p []byte) (int, error) {
	if w.failed {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}
