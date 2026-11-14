package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
