package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The first introspection call drops its connection, as a proxy does with a
// pooled connection it already closed. The second call must answer.
func TestIntrospectRetriesADroppedConnection(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			return
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("token") != "tok" {
			t.Errorf("second call lost the form: %v %q", err, r.PostForm.Get("token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true,"sub":"jsp"}`))
	}))
	defer srv.Close()

	o := &OAuth{IntrospectionURL: srv.URL, ClientID: "lard", ClientSecret: "s", HTTP: srv.Client()}
	res, ok := o.introspect(context.Background(), "tok")
	if !ok || !res.Active {
		t.Fatalf("introspect = %+v, %v; want an active result", res, ok)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

// A server that drops every connection gives up after two tries.
func TestIntrospectGivesUpAfterTwoTries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()

	o := &OAuth{IntrospectionURL: srv.URL, HTTP: srv.Client()}
	if _, ok := o.introspect(context.Background(), "tok"); ok {
		t.Fatal("introspect succeeded against a server that drops every connection")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

func TestAuthServerMetadataURL(t *testing.T) {
	for _, c := range []struct{ issuer, want string }{
		{"https://a.hogwarts.dev/application/o/lard/", "https://a.hogwarts.dev/.well-known/oauth-authorization-server/application/o/lard/"},
		{"https://a.example/tenant", "https://a.example/.well-known/oauth-authorization-server/tenant"},
		{"https://a.example/", "https://a.example/.well-known/oauth-authorization-server"},
		{"https://a.example", "https://a.example/.well-known/oauth-authorization-server"},
	} {
		if got := authServerMetadataURL(c.issuer); got != c.want {
			t.Errorf("authServerMetadataURL(%q) = %q, want %q", c.issuer, got, c.want)
		}
	}
}

func TestAuthServerMetadataRedirect(t *testing.T) {
	o := &OAuth{Issuer: "https://a.hogwarts.dev/application/o/lard/"}
	w := httptest.NewRecorder()
	o.AuthServerMetadata(w, httptest.NewRequest("GET", PathAuthServer, nil))
	want := "https://a.hogwarts.dev/.well-known/oauth-authorization-server/application/o/lard/"
	if w.Code != http.StatusFound || w.Header().Get("Location") != want {
		t.Fatalf("redirect = %d %q, want 302 %q", w.Code, w.Header().Get("Location"), want)
	}
}
