package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthAndStatic(t *testing.T) {
	s := New(Options{Token: "secret", WebDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	h := s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for auth, want := range map[string]int{"": 401, "Bearer wrong": 401, "Bearer secret": 204} {
		r := httptest.NewRequest("GET", "/api/v1/x", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("auth %q: got %d want %d", auth, w.Code, want)
		}
	}
	r := httptest.NewRequest("POST", "/api/v1/x", nil)
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Origin", "http://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("cross-origin POST: got %d", w.Code)
	}
	r.Header.Set("Origin", "http://"+r.Host)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Errorf("same-origin POST: got %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.static().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 {
		t.Errorf("static without a build: got %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.static().ServeHTTP(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 405 {
		t.Errorf("static POST: got %d", w.Code)
	}
}

func TestStaticDoesNotEscapeWebDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>ok</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	_ = os.WriteFile(secret, []byte("TOPSECRET"), 0o600)
	defer os.Remove(secret)
	s := New(Options{WebDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, p := range []string{"/../secret.txt", "/%2e%2e/secret.txt", "/assets/../../secret.txt"} {
		w := httptest.NewRecorder()
		s.static().ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if strings.Contains(w.Body.String(), "TOPSECRET") {
			t.Errorf("path traversal via %q leaked a file outside the web dir", p)
		}
	}
	w := httptest.NewRecorder()
	s.static().ServeHTTP(w, httptest.NewRequest("GET", "/some/client/route", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("unknown paths fall back to the app shell, got %d", w.Code)
	}
}

func TestSecurityHeadersAndTokenEdgeCases(t *testing.T) {
	s := New(Options{Token: "secret", WebDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	h := s.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Errorf("healthz must stay public for probes, got %d", w.Code)
	}
	for _, k := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if w.Header().Get(k) == "" {
			t.Errorf("missing security header %s", k)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "script-src *") {
		t.Errorf("CSP is too permissive: %s", csp)
	}
	for name, auth := range map[string]string{"empty bearer": "Bearer ", "token without scheme": "secret", "lowercase scheme": "bearer secret", "prefix of token": "Bearer secre"} {
		r := httptest.NewRequest("GET", "/api/v1/overview", nil)
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("%s: got %d, want 401", name, w.Code)
		}
	}
	// every state-changing and admin route must sit behind the token
	for _, route := range [][2]string{{"POST", "/api/v1/lab/experiments"}, {"POST", "/api/v1/lab/reset"}, {"PUT", "/api/v1/settings/detection"},
		{"POST", "/api/v1/memory/seed"}, {"DELETE", "/api/v1/memory/seed"}, {"PATCH", "/api/v1/incidents/1"}, {"POST", "/api/v1/incidents/1/replays"}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(route[0], route[1], nil))
		if w.Code != 401 {
			t.Errorf("%s %s without a token: got %d, want 401", route[0], route[1], w.Code)
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/does-not-exist", nil))
	if w.Code != 401 {
		t.Errorf("unknown API paths must not reveal themselves without a token, got %d", w.Code)
	}
}
