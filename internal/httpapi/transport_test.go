package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"
)

func hasChinese(text string) bool {
	return strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) })
}

func mustServer(t *testing.T, address string, routes ...Route) *http.Server {
	t.Helper()
	server, err := NewServer(address, routes...)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func assertError(t *testing.T, status int, header http.Header, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", status, wantStatus, body)
	}
	var envelope map[string]map[string]string
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("error is not JSON: %s (%v)", body, err)
	}
	if len(envelope) != 1 || len(envelope["error"]) != 2 || envelope["error"]["code"] != wantCode || !hasChinese(envelope["error"]["message"]) {
		t.Fatalf("unexpected error contract: %s", body)
	}
	if !strings.HasPrefix(header.Get("Content-Type"), "application/json") || header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unexpected response headers: %v", header)
	}
}

func TestListenerAddress(t *testing.T) {
	for _, address := range []string{DefaultAddress, "127.0.0.2:7310", "127.0.0.1:80", "[::1]:7310", "[::1]:80", "[0:0:0:0:0:0:0:1]:7310"} {
		t.Run(address, func(t *testing.T) {
			server := mustServer(t, address)
			if server.Addr == "" || server.Handler == nil {
				t.Fatal("server must have an explicit address and handler")
			}
		})
	}
	for _, address := range []string{"", "localhost:7310", "attacker.example:7310", ":7310", "0.0.0.0:7310", "[::]:7310", "192.168.1.1:7310", "[2001:db8::1]:7310", "127.0.0.1", "127.0.0.1:0", "[::1]:0", "127.0.0.1:http", "127.0.0.1:-1", "127.0.0.1:65536", "[::1%lo0]:7310", "http://127.0.0.1:7310", "user@127.0.0.1:7310"} {
		t.Run("reject/"+address, func(t *testing.T) {
			if _, err := NewServer(address); err == nil {
				t.Fatalf("accepted unsafe or unresolved listener %q", address)
			} else if !hasChinese(err.Error()) {
				t.Fatalf("configuration error must be Chinese: %v", err)
			}
		})
	}
}

func TestRouteConflicts(t *testing.T) {
	route := Route{Method: http.MethodGet, Pattern: "/probe", Handler: func(http.ResponseWriter, *http.Request) {}}
	for _, routes := range [][]Route{{route, route}, {{Method: http.MethodGet, Pattern: "/healthz", Handler: route.Handler}}} {
		if _, err := NewServer(DefaultAddress, routes...); err == nil {
			t.Fatal("duplicate routes must not silently replace another handler")
		}
	}
}

func TestWriteJSONEncodingFailure(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSON(w, http.StatusOK, make(chan int))
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusInternalServerError, "internal_error")
}

func TestHostGuard(t *testing.T) {
	for _, tt := range []struct {
		address string
		valid   []string
		invalid []string
	}{
		{DefaultAddress, []string{DefaultAddress}, []string{"", "localhost:7310", "attacker.example:7310", "127.0.0.1", "127.0.0.1:80", "127.0.0.1:7311", "127.0.0.2:7310", "127.0.0.1:07310", "127.0.0.1:7310.", "user@127.0.0.1:7310"}},
		{"[::1]:7310", []string{"[::1]:7310"}, []string{"::1", "[::1]", "[::1]:7311", "[0:0:0:0:0:0:0:1]:7310", DefaultAddress}},
		{"127.0.0.1:80", []string{"127.0.0.1", "127.0.0.1:80"}, []string{"127.0.0.1:81", "localhost"}},
		{"[::1]:80", []string{"[::1]", "[::1]:80"}, []string{"::1", "[::1]:81"}},
	} {
		t.Run(tt.address, func(t *testing.T) {
			server := mustServer(t, tt.address)
			for _, host := range tt.valid {
				r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
				r.Host = host
				r.Header.Set("X-Forwarded-Host", "attacker.example")
				r.Header.Set("X-Forwarded-Proto", "https")
				r.Header.Set("Forwarded", "host=attacker.example;proto=https")
				w := httptest.NewRecorder()
				server.Handler.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("valid host %q rejected: %d %s", host, w.Code, w.Body)
				}
			}
			for _, host := range tt.invalid {
				for _, path := range []string{"/healthz", "/missing", "/api/v1/groups"} {
					r := httptest.NewRequest(http.MethodGet, path, nil)
					r.Host = host
					r.Header.Set("X-Forwarded-Host", tt.address)
					r.Header.Set("Forwarded", "host="+tt.address)
					w := httptest.NewRecorder()
					server.Handler.ServeHTTP(w, r)
					assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusForbidden, "invalid_host")
				}
			}
		})
	}
}

func TestHealthAndRouting(t *testing.T) {
	server := mustServer(t, DefaultAddress, Route{Method: http.MethodGet, Pattern: "/api/v1/probes/{id}", Handler: func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
	}})
	for _, tt := range []struct {
		method, path, code, allow string
		status                    int
	}{
		{http.MethodGet, "/healthz", "", "", http.StatusOK},
		{http.MethodPost, "/healthz", "method_not_allowed", "GET", http.StatusMethodNotAllowed},
		{http.MethodHead, "/healthz", "method_not_allowed", "GET", http.StatusMethodNotAllowed},
		{http.MethodOptions, "/healthz", "method_not_allowed", "GET", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/groups", "not_found", "", http.StatusNotFound},
		{http.MethodPost, "/missing", "not_found", "", http.StatusNotFound},
		{http.MethodGet, "/api/v1/probes/probe-1", "", "", http.StatusOK},
		{http.MethodDelete, "/api/v1/probes/probe-1", "method_not_allowed", "GET", http.StatusMethodNotAllowed},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "http://"+DefaultAddress+tt.path, nil)
			w := httptest.NewRecorder()
			server.Handler.ServeHTTP(w, r)
			if tt.code != "" {
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), tt.status, tt.code)
				if w.Header().Get("Allow") != tt.allow {
					t.Fatalf("Allow = %q, want %q", w.Header().Get("Allow"), tt.allow)
				}
				return
			}
			var got map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != tt.status || len(got) != 1 {
				t.Fatalf("unexpected response: %d %s (%v)", w.Code, w.Body, err)
			}
			if tt.path == "/healthz" && got["status"] != "ready" || tt.path != "/healthz" && got["id"] != "probe-1" {
				t.Fatalf("unexpected readiness or route response: %v", got)
			}
			if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("unexpected JSON/CORS headers: %v", w.Header())
			}
		})
	}
}

func mutationRoutes() []Route {
	routes := make([]Route, 0, 4)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		routes = append(routes, Route{Method: method, Pattern: "/api/v1/probes", Handler: func(w http.ResponseWriter, r *http.Request) {
			var input struct {
				Name    string `json:"name"`
				Options struct {
					Note string `json:"note"`
				} `json:"options"`
				Rows []struct {
					Value string `json:"value"`
				} `json:"rows"`
			}
			if DecodeJSON(w, r, &input) {
				WriteJSON(w, http.StatusCreated, input)
			}
		}})
	}
	return routes
}

func TestMutationOrigin(t *testing.T) {
	for _, address := range []string{DefaultAddress, "[::1]:7310", "127.0.0.1:80", "[::1]:80"} {
		t.Run(address, func(t *testing.T) {
			server := mustServer(t, address, mutationRoutes()...)
			origin := "http://" + address
			for _, tt := range []struct {
				name    string
				origins []string
				sites   []string
				mode    string
				allowed bool
			}{
				{"local client", nil, nil, "", true},
				{"same origin", []string{origin}, nil, "", true},
				{"same origin fetch", []string{origin}, []string{"same-origin"}, "cors", true},
				{"user initiated", []string{origin}, []string{"none"}, "navigate", true},
				{"foreign", []string{"http://attacker.example"}, nil, "", false},
				{"dev origin not enabled", []string{"http://localhost:5173"}, nil, "", false},
				{"wrong scheme", []string{"https://" + address}, nil, "", false},
				{"null", []string{"null"}, nil, "", false},
				{"empty", []string{""}, nil, "", false},
				{"malformed", []string{"://"}, nil, "", false},
				{"userinfo", []string{"http://user@" + address}, nil, "", false},
				{"path", []string{origin + "/"}, nil, "", false},
				{"query", []string{origin + "?x=1"}, nil, "", false},
				{"fragment", []string{origin + "#fragment"}, nil, "", false},
				{"list", []string{origin + " http://attacker.example"}, nil, "", false},
				{"comma list", []string{origin + ", " + origin}, nil, "", false},
				{"repeated same", []string{origin, origin}, nil, "", false},
				{"repeated foreign", []string{origin, "http://attacker.example"}, nil, "", false},
				{"cross site", []string{origin}, []string{"cross-site"}, "", false},
				{"same site is not same origin", []string{origin}, []string{"same-site"}, "", false},
				{"bad site", []string{origin}, []string{"garbage"}, "", false},
				{"empty site", []string{origin}, []string{""}, "", false},
				{"multiple sites", []string{origin}, []string{"same-origin", "cross-site"}, "", false},
				{"joined sites", []string{origin}, []string{"same-origin, cross-site"}, "", false},
				{"no origin cross site", nil, []string{"cross-site"}, "", false},
				{"no origin same origin metadata", nil, []string{"same-origin"}, "", false},
				{"no origin navigation", nil, []string{"none"}, "navigate", false},
				{"no origin other browser metadata", nil, nil, "cors", false},
			} {
				for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
					t.Run(tt.name+"/"+method, func(t *testing.T) {
						r := httptest.NewRequest(method, "http://"+address+"/api/v1/probes", strings.NewReader(`{"name":"test"}`))
						r.Header.Set("Content-Type", "application/json")
						for _, value := range tt.origins {
							r.Header.Add("Origin", value)
						}
						for _, value := range tt.sites {
							r.Header.Add("Sec-Fetch-Site", value)
						}
						if tt.mode != "" {
							r.Header.Set("Sec-Fetch-Mode", tt.mode)
						}
						r.Header.Set("X-Forwarded-Host", "attacker.example")
						r.Header.Set("X-Forwarded-Proto", "https")
						w := httptest.NewRecorder()
						server.Handler.ServeHTTP(w, r)
						if !tt.allowed {
							assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusForbidden, "invalid_origin")
						} else if w.Code != http.StatusCreated {
							t.Fatalf("valid mutation rejected: %d %s", w.Code, w.Body)
						}
					})
				}
			}
		})
	}
}

func TestDefaultPortOrigins(t *testing.T) {
	for _, literal := range []string{"127.0.0.1", "[::1]"} {
		server := mustServer(t, literal+":80", mutationRoutes()...)
		for _, host := range []string{literal, literal + ":80"} {
			for _, origin := range []string{"http://" + literal, "http://" + literal + ":80"} {
				r := httptest.NewRequest(http.MethodPost, "/api/v1/probes", strings.NewReader(`{}`))
				r.Host = host
				r.Header.Set("Origin", origin)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				server.Handler.ServeHTTP(w, r)
				if w.Code != http.StatusCreated {
					t.Fatalf("equivalent default-port origin rejected: host=%s origin=%s: %d %s", host, origin, w.Code, w.Body)
				}
			}
		}
	}
}

func TestStrictJSON(t *testing.T) {
	server := mustServer(t, DefaultAddress, mutationRoutes()...)
	for _, tt := range []struct {
		name, body string
		types      []string
		status     int
		code       string
	}{
		{"object", `{"name":"中文"}`, []string{"application/json"}, 201, ""},
		{"empty object", `{}`, []string{"application/json"}, 201, ""},
		{"whitespace", " \n{\"name\":\"test\"}\t\r\n", []string{"application/json"}, 201, ""},
		{"utf8 charset", `{}`, []string{`application/json; charset="UTF-8"`}, 201, ""},
		{"optional parameter", `{}`, []string{`application/json; profile="local"`}, 201, ""},
		{"missing type", `{}`, nil, 415, "unsupported_media_type"},
		{"wrong type", `{}`, []string{"text/plain"}, 415, "unsupported_media_type"},
		{"json suffix not accepted", `{}`, []string{"application/problem+json"}, 415, "unsupported_media_type"},
		{"wrong charset", `{}`, []string{"application/json; charset=iso-8859-1"}, 415, "unsupported_media_type"},
		{"malformed type", `{}`, []string{"application/json; charset"}, 415, "unsupported_media_type"},
		{"conflicting parameters", `{}`, []string{"application/json; charset=utf-8; charset=ascii"}, 415, "unsupported_media_type"},
		{"repeated type", `{}`, []string{"application/json", "application/json"}, 415, "unsupported_media_type"},
		{"conflicting types", `{}`, []string{"application/json", "text/plain"}, 415, "unsupported_media_type"},
		{"joined types", `{}`, []string{"application/json, application/json"}, 415, "unsupported_media_type"},
		{"empty", ``, []string{"application/json"}, 400, "invalid_json"},
		{"blank", " \n\t", []string{"application/json"}, 400, "invalid_json"},
		{"null", `null`, []string{"application/json"}, 400, "invalid_json"},
		{"array", `[]`, []string{"application/json"}, 400, "invalid_json"},
		{"string", `"name"`, []string{"application/json"}, 400, "invalid_json"},
		{"number", `1`, []string{"application/json"}, 400, "invalid_json"},
		{"boolean", `true`, []string{"application/json"}, 400, "invalid_json"},
		{"unknown field", `{"extra":1}`, []string{"application/json"}, 400, "invalid_json"},
		{"caller identity", `{"name":"test","sender_id":"admin"}`, []string{"application/json"}, 400, "invalid_json"},
		{"nested unknown", `{"options":{"extra":1}}`, []string{"application/json"}, 400, "invalid_json"},
		{"wrong field type", `{"name":1}`, []string{"application/json"}, 400, "invalid_json"},
		{"duplicate field", `{"name":"first","name":"second"}`, []string{"application/json"}, 400, "invalid_json"},
		{"case alias duplicate", `{"name":"first","Name":"second"}`, []string{"application/json"}, 400, "invalid_json"},
		{"escaped duplicate", `{"name":"first","na\u006de":"second"}`, []string{"application/json"}, 400, "invalid_json"},
		{"nested duplicate", `{"options":{"note":"first","note":"second"}}`, []string{"application/json"}, 400, "invalid_json"},
		{"array duplicate", `{"rows":[{"value":"first","value":"second"}]}`, []string{"application/json"}, 400, "invalid_json"},
		{"distinct array objects", `{"rows":[{"value":"first"},{"value":"second"}]}`, []string{"application/json"}, 201, ""},
		{"second object", `{} {}`, []string{"application/json"}, 400, "invalid_json"},
		{"trailing null", `{} null`, []string{"application/json"}, 400, "invalid_json"},
		{"trailing junk", `{} junk`, []string{"application/json"}, 400, "invalid_json"},
		{"incomplete", `{"name":`, []string{"application/json"}, 400, "invalid_json"},
		{"invalid utf8", "{\"name\":\"\xff\"}", []string{"application/json"}, 400, "invalid_json"},
		{"unicode outside JSON", "\u00a0{}", []string{"application/json"}, 400, "invalid_json"},
		{"at limit", `{"name":"` + strings.Repeat("x", MaxJSONBodyBytes-11) + `"}`, []string{"application/json"}, 201, ""},
		{"over limit", `{"name":"` + strings.Repeat("x", MaxJSONBodyBytes-10) + `"}`, []string{"application/json"}, 413, "body_too_large"},
		{"trailing whitespace over limit", `{}` + strings.Repeat(" ", MaxJSONBodyBytes), []string{"application/json"}, 413, "body_too_large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://"+DefaultAddress+"/api/v1/probes?name=query&name=duplicate", strings.NewReader(tt.body))
			for _, value := range tt.types {
				r.Header.Add("Content-Type", value)
			}
			w := httptest.NewRecorder()
			server.Handler.ServeHTTP(w, r)
			if tt.code != "" {
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), tt.status, tt.code)
			} else if w.Code != tt.status {
				t.Fatalf("valid JSON rejected: %d %s", w.Code, w.Body)
			} else if tt.name == "object" && !strings.Contains(w.Body.String(), `"name":"中文"`) {
				t.Fatalf("query parameters must not override or merge into the body: %s", w.Body)
			}
		})
	}
}

func TestServerTimeoutsAndContext(t *testing.T) {
	var captured context.Context
	server := mustServer(t, DefaultAddress, Route{Method: http.MethodGet, Pattern: "/context", Handler: func(w http.ResponseWriter, r *http.Request) {
		captured = r.Context()
		deadline, ok := captured.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 15*time.Second {
			t.Errorf("short requests need a bounded context deadline: %v %v", deadline, ok)
		}
		WriteJSON(w, http.StatusOK, struct{}{})
	}})
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout < server.ReadHeaderTimeout || server.WriteTimeout <= server.ReadTimeout || server.IdleTimeout <= 0 || server.MaxHeaderBytes <= 0 || server.MaxHeaderBytes > 64<<10 {
		t.Errorf("server limits are missing or inconsistent: %+v", server)
	}
	r := httptest.NewRequest(http.MethodGet, "http://"+DefaultAddress+"/context", nil)
	server.Handler.ServeHTTP(httptest.NewRecorder(), r)
	if captured == nil || captured.Err() != context.Canceled {
		t.Fatal("request context must be cancelled after handler returns")
	}
}

func TestRealHTTPStack(t *testing.T) {
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = mustServer(t, ts.Listener.Addr().String(), mutationRoutes()...)
	ts.Start()
	t.Cleanup(ts.Close)
	client := ts.Client()
	client.Timeout = 5 * time.Second

	for _, tt := range []struct {
		name, method, path, host, origin, code string
		status                                 int
	}{
		{"health", http.MethodGet, "/healthz", "", "", "", 200},
		{"host defense", http.MethodGet, "/healthz", "attacker.example", "", "invalid_host", 403},
		{"wrong port", http.MethodGet, "/healthz", "127.0.0.1:1", "", "invalid_host", 403},
		{"local mutation", http.MethodPost, "/api/v1/probes", "", "", "", 201},
		{"same origin", http.MethodPost, "/api/v1/probes", "", ts.URL, "", 201},
		{"foreign origin", http.MethodPost, "/api/v1/probes", "", "http://attacker.example", "invalid_origin", 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := http.NewRequest(tt.method, ts.URL+tt.path, strings.NewReader(`{"name":"real"}`))
			if err != nil {
				t.Fatal(err)
			}
			r.Host = tt.host
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Forwarded-Host", ts.Listener.Addr().String())
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if tt.code != "" {
				assertError(t, response.StatusCode, response.Header, body, tt.status, tt.code)
			} else if response.StatusCode != tt.status {
				t.Fatalf("unexpected real HTTP response: %d %s", response.StatusCode, body)
			}
		})
	}

	for _, tt := range []struct {
		name, request, code string
		status              int
	}{
		{"options star host guard", "OPTIONS * HTTP/1.1\r\nHost: attacker.example\r\n", "invalid_host", 403},
		{"options star not fake success", "OPTIONS * HTTP/1.1\r\nHost: " + ts.Listener.Addr().String() + "\r\n", "not_found", 404},
		{"absolute URI uses effective host", "GET http://attacker.example/healthz HTTP/1.1\r\nHost: " + ts.Listener.Addr().String() + "\r\n", "invalid_host", 403},
		{"duplicate origin", "POST /api/v1/probes HTTP/1.1\r\nHost: " + ts.Listener.Addr().String() + "\r\nOrigin: " + ts.URL + "\r\nOrigin: " + ts.URL + "\r\nContent-Length: 2\r\nContent-Type: application/json\r\n", "invalid_origin", 403},
		{"duplicate content type", "POST /api/v1/probes HTTP/1.1\r\nHost: " + ts.Listener.Addr().String() + "\r\nContent-Type: application/json\r\nContent-Type: application/json\r\nContent-Length: 2\r\n", "unsupported_media_type", 415},
	} {
		t.Run(tt.name, func(t *testing.T) {
			connection, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprint(connection, tt.request+"Connection: close\r\n\r\n{}"); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(connection), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			assertError(t, response.StatusCode, response.Header, body, tt.status, tt.code)
		})
	}

	t.Run("chunked over limit", func(t *testing.T) {
		r, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/probes", strings.NewReader(`{}`+strings.Repeat(" ", MaxJSONBodyBytes)))
		if err != nil {
			t.Fatal(err)
		}
		r.ContentLength = -1
		r.Header.Set("Content-Type", "application/json")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		assertError(t, response.StatusCode, response.Header, body, http.StatusRequestEntityTooLarge, "body_too_large")
	})
}
