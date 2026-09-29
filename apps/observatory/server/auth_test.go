package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteGuard(t *testing.T) {
	h := writeGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	cases := []struct {
		name, method, site, origin, ctype string
		want                              int
	}{
		{"read from anywhere", "GET", "cross-site", "", "", http.StatusNoContent},
		{"same-origin JSON write", "PUT", "same-origin", "https://observatory.ops.lab", "application/json", http.StatusNoContent},
		{"same-origin YAML apply", "POST", "same-origin", "https://observatory.ops.lab", "application/yaml", http.StatusNoContent},
		{"a sibling site's write", "POST", "same-site", "https://grafana.ops.lab", "application/json", http.StatusForbidden},
		{"a cross-site write", "POST", "cross-site", "https://evil.example", "application/json", http.StatusForbidden},
		{"an old browser, other origin", "POST", "", "https://evil.example", "application/json", http.StatusForbidden},
		{"a form post", "POST", "same-origin", "https://observatory.ops.lab", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"no content type", "PUT", "same-origin", "https://observatory.ops.lab", "", http.StatusUnsupportedMediaType},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "https://observatory.ops.lab/api/resource", strings.NewReader("{}"))
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	securityHeaders(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	for _, k := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy"} {
		if w.Header().Get(k) == "" {
			t.Errorf("missing %s", k)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("the UI must not be frameable")
	}
}
