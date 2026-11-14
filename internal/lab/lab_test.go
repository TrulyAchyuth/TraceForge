package lab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendAuthenticatesAndNeverFollowsRedirects(t *testing.T) {
	var gotAuth string
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect was followed") }))
	defer leak.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/admin/fault" && r.Method == http.MethodDelete {
			http.Redirect(w, r, leak.URL, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()
	m := NewManager(nil, map[string]string{"payment": target.URL}, nil)
	m.SetToken("lab-secret")
	if _, err := m.send(context.Background(), http.MethodGet, "payment", nil); err != nil || gotAuth != "Bearer lab-secret" {
		t.Fatalf("auth header %q, err %v", gotAuth, err)
	}
	if _, err := m.send(context.Background(), http.MethodDelete, "payment", nil); err == nil {
		t.Fatal("a redirect must be reported as a failure")
	}
}

func TestParseTargetsRejectsBadInput(t *testing.T) {
	for _, bad := range []string{"payment", "=http://x", "payment=ftp://x", "payment=http://"} {
		if _, err := ParseTargets(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	got, err := ParseTargets("a=http://h:1/, b=https://x")
	if err != nil || got["a"] != "http://h:1" || got["b"] != "https://x" {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestScenariosStayInsideTheirBounds(t *testing.T) {
	for _, s := range Scenarios() {
		if s.DefaultDuration < MinDuration || s.DefaultDuration > MaxDuration {
			t.Errorf("%s default duration %d out of bounds", s.ID, s.DefaultDuration)
		}
	}
}
