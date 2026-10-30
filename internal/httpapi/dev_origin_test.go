package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDevOriginConfiguration(t *testing.T) {
	for _, origin := range []string{"", "http://127.0.0.1:5173", "http://127.0.0.2:5173", "http://[::1]:5173", "http://127.0.0.1:80"} {
		if _, err := NewServerWithConfig(Config{Address: DefaultAddress, DevOrigin: origin}); err != nil {
			t.Errorf("valid dev origin %q: %v", origin, err)
		}
	}
	for _, origin := range []string{"*", "null", "http://localhost:5173", "http://attacker.example:5173", "http://0.0.0.0:5173", "http://[::]:5173", "http://192.168.1.1:5173", "https://127.0.0.1:5173", "HTTP://127.0.0.1:5173", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user@127.0.0.1:5173", "http://127.0.0.1:5173/", "http://127.0.0.1:5173?x=1", "http://127.0.0.1:5173#x", "http://[::1%lo0]:5173", "http://127.0.0.1:5173 http://127.0.0.1:5174", "http://127.0.0.1:5173,http://127.0.0.1:5174", " http://127.0.0.1:5173"} {
		if _, err := NewServerWithConfig(Config{Address: DefaultAddress, DevOrigin: origin}); err == nil || !hasChinese(err.Error()) {
			t.Errorf("invalid dev origin %q: %v", origin, err)
		}
	}
	if _, err := NewServerWithConfig(Config{Address: "127.0.0.1:0", DevOrigin: "http://127.0.0.1:5173"}); err == nil {
		t.Fatal("dev configuration must not relax the actual listener authority invariant")
	}
}

func TestDevOriginProxyRequests(t *testing.T) {
	for _, configured := range []string{"", "http://127.0.0.1:5173", "http://[::1]:5173"} {
		t.Run(configured, func(t *testing.T) {
			ts := httptest.NewUnstartedServer(nil)
			var err error
			ts.Config, err = NewServerWithConfig(Config{Address: ts.Listener.Addr().String(), DevOrigin: configured}, mutationRoutes()...)
			if err != nil {
				ts.Listener.Close()
				t.Fatal(err)
			}
			ts.Start()
			t.Cleanup(ts.Close)
			client := ts.Client()
			for _, tt := range []struct {
				name, origin, site, host string
				multiple                 bool
				allowed                  bool
			}{
				{"same origin", ts.URL, "same-origin", "", false, true},
				{"proxy same site", configured, "same-site", "", false, configured != ""},
				{"proxy cross site", configured, "cross-site", "", false, configured != ""},
				{"proxy without fetch site", configured, "", "", false, configured != ""},
				{"default dev origin", "http://127.0.0.1:5173", "same-site", "", false, configured == "http://127.0.0.1:5173"},
				{"wrong port", "http://127.0.0.1:5174", "same-site", "", false, false},
				{"lookalike", "http://127.0.0.1.attacker.example:5173", "same-site", "", false, false},
				{"different loopback", "http://127.0.0.2:5173", "same-site", "", false, false},
				{"null", "null", "cross-site", "", false, false},
				{"decorated dev origin", configured + "/", "same-site", "", false, false},
				{"duplicate dev origin", configured, "same-site", "", true, false},
				{"unknown fetch site", configured, "bogus", "", false, false},
				{"joined fetch site", configured, "same-site, cross-site", "", false, false},
				{"real origin cannot borrow dev metadata", ts.URL, "cross-site", "", false, false},
				{"missing origin cannot borrow dev metadata", "", "cross-site", "", false, false},
				{"proxy must rewrite host", configured, "same-site", "127.0.0.1:5173", false, false},
			} {
				t.Run(tt.name, func(t *testing.T) {
					r, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/probes", strings.NewReader(`{"name":"proxy"}`))
					if err != nil {
						t.Fatal(err)
					}
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Origin", tt.origin)
					if tt.multiple {
						r.Header.Add("Origin", tt.origin)
					}
					if tt.site != "" {
						r.Header.Set("Sec-Fetch-Site", tt.site)
					}
					r.Host = tt.host
					r.Header.Set("X-Forwarded-Host", "127.0.0.1:5173")
					r.Header.Set("X-Forwarded-Proto", "https")
					response, err := client.Do(r)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					body, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					if tt.allowed {
						if response.StatusCode != http.StatusCreated {
							t.Fatalf("opt-in proxy rejected: %d %s", response.StatusCode, body)
						}
					} else {
						code := "invalid_origin"
						if tt.host != "" {
							code = "invalid_host"
						}
						assertError(t, response.StatusCode, response.Header, body, http.StatusForbidden, code)
					}
					if response.Header.Get("Access-Control-Allow-Origin") != "" {
						t.Fatal("proxy opt-in must not enable CORS")
					}
				})
			}
		})
	}
}
