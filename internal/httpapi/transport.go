// Package httpapi provides a local-user HTTP transport, not a member tool transport.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"
)

const DefaultAddress = "127.0.0.1:7310"

// Route uses a path-only ServeMux pattern and one explicit method.
// Handlers must keep GET, HEAD, and OPTIONS free of side effects.
type Route struct {
	Method  string
	Pattern string
	Handler http.HandlerFunc
}

// NewServer does not bind or serve. Pass the actual bound listener address,
// including its assigned port when using an ephemeral listener. Port zero is
// rejected because it cannot identify a request authority. IPs are canonicalized;
// request authorities must match that spelling (port 80 may be omitted).
// The caller must Serve on that listener, keep it loopback-only, and own shutdown.
// These limits and cooperative context deadlines are for short JSON requests,
// not streaming endpoints. Routes and listener configuration are trusted code.
func NewServer(address string, routes ...Route) (*http.Server, error) {
	return NewServerWithConfig(Config{Address: address}, routes...)
}

// NewServerWithConfig adds one explicit development proxy origin. It does not
// enable CORS or change the Host guard; proxies must rewrite Host to the listener.
func NewServerWithConfig(config Config, routes ...Route) (*http.Server, error) {
	devOrigin, err := canonicalDevOrigin(config.DevOrigin)
	if err != nil {
		return nil, err
	}
	listener, err := netip.ParseAddrPort(config.Address)
	if err != nil || !listener.Addr().IsLoopback() || listener.Addr().Zone() != "" || listener.Port() == 0 {
		return nil, errors.New("监听地址必须是带有明确非零端口的回环 IP 地址")
	}
	authority := listener.String()
	defaultAuthority := authority
	if listener.Port() == 80 {
		defaultAuthority = strings.TrimSuffix(authority, ":80")
	}

	methods := make(map[string]map[string]http.HandlerFunc)
	routes = append([]Route{{Method: http.MethodGet, Pattern: "/healthz", Handler: func(w http.ResponseWriter, r *http.Request) {
		// Process readiness only, not complete application or dependency health.
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}}}, routes...)
	for _, route := range routes {
		if methods[route.Pattern] == nil {
			methods[route.Pattern] = make(map[string]http.HandlerFunc)
		}
		if _, exists := methods[route.Pattern][route.Method]; exists {
			return nil, errors.New("HTTP 路由与方法重复")
		}
		methods[route.Pattern][route.Method] = route.Handler
	}
	mux := http.NewServeMux()
	if methods["/"] == nil {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, http.StatusNotFound, "not_found", "接口不存在")
		})
	}
	for pattern, handlers := range methods {
		allowed := make([]string, 0, len(handlers))
		for method := range handlers {
			allowed = append(allowed, method)
		}
		sort.Strings(allowed)
		allow := strings.Join(allowed, ", ")
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if handler := handlers[r.Method]; handler != nil {
				handler.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Allow", allow)
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "此接口不支持该请求方法")
		})
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Forwarded headers never determine local authority or origin.
		if r.Host != authority && r.Host != defaultAuthority {
			WriteError(w, http.StatusForbidden, "invalid_host", "请求主机与本地监听地址不匹配")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && !sameOrigin(r, authority, defaultAuthority, devOrigin) {
			WriteError(w, http.StatusForbidden, "invalid_origin", "仅允许同源浏览器写入请求或本地客户端请求")
			return
		}
		if r.RequestURI == "*" {
			WriteError(w, http.StatusNotFound, "not_found", "接口不存在")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	return &http.Server{
		Addr: authority, Handler: handler,
		// Otherwise net/http answers OPTIONS * before our Host guard.
		DisableGeneralOptionsHandler: true,
		ReadHeaderTimeout:            5 * time.Second,
		ReadTimeout:                  10 * time.Second,
		WriteTimeout:                 15 * time.Second,
		IdleTimeout:                  60 * time.Second,
		MaxHeaderBytes:               16 << 10,
	}, nil
}

func sameOrigin(r *http.Request, authority, defaultAuthority, devOrigin string) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		// Only clients without browser Fetch Metadata may omit Origin. This is
		// a browser CSRF boundary, not authentication: local software can forge
		// these headers. Member tools need a separate run-bound capability.
		for key := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "sec-fetch-") {
				return false
			}
		}
		return true
	}
	development := devOrigin != "" && len(origins) == 1 && origins[0] == devOrigin
	sites := r.Header.Values("Sec-Fetch-Site")
	if len(sites) > 1 {
		return false
	}
	if len(sites) == 1 && sites[0] != "same-origin" && sites[0] != "none" && !(development && (sites[0] == "same-site" || sites[0] == "cross-site")) {
		return false
	}
	// Exact serialized origins reject URL decorations, null, and origin lists.
	return len(origins) == 1 && (development || origins[0] == "http://"+authority || origins[0] == "http://"+defaultAuthority)
}

func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"internal_error","message":"响应编码失败"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
