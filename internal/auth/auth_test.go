package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const tok = "0123456789abcdef0123456789abcdef"

func TestBearer(t *testing.T) {
	a := New(tok, false)
	h := a.Bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for hdr, want := range map[string]int{"": 401, "Bearer wrong": 401, "Basic " + tok: 401, "Bearer " + tok: 204} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		if hdr != "" {
			r.Header.Set("Authorization", hdr)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Authorization %q: status %d, want %d", hdr, w.Code, want)
		}
		if w.Code == 401 && w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("401 without WWW-Authenticate")
		}
	}
}

func cookieFrom(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	return nil
}

func TestSessionRoundTripAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	a := New(tok, true)
	a.Now = func() time.Time { return now }
	w := httptest.NewRecorder()
	a.StartSession(w)
	c := cookieFrom(w)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", c)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	if a.SessionFrom(r) == nil {
		t.Fatal("valid session rejected")
	}

	forged := *c
	forged.Value = strings.Replace(c.Value, c.Value[:3], "999", 1)
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.AddCookie(&forged)
	if a.SessionFrom(r2) != nil {
		t.Fatal("forged session accepted")
	}

	other := New("ffffffffffffffffffffffffffffffff", true)
	if other.SessionFrom(r) != nil {
		t.Fatal("session from another token accepted")
	}

	now = now.Add(SessionTTL + time.Second)
	if a.SessionFrom(r) != nil {
		t.Fatal("expired session accepted")
	}
}

func TestCSRF(t *testing.T) {
	a := New(tok, false)
	w := httptest.NewRecorder()
	a.StartSession(w)
	get := httptest.NewRequest("GET", "/", nil)
	get.AddCookie(cookieFrom(w))
	s := a.SessionFrom(get)

	post := func(csrf, origin string) bool {
		r := httptest.NewRequest("POST", "http://phial.test/retract", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
		r.Host = "phial.test"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return a.CheckCSRF(r, s)
	}
	if !post(s.CSRF, "") || !post(s.CSRF, "http://phial.test") {
		t.Fatal("valid CSRF rejected")
	}
	if post("nope", "") || post(s.CSRF, "https://evil.example") {
		t.Fatal("bad CSRF accepted")
	}
}

func TestDisabled(t *testing.T) {
	a := New("", false)
	if a.Enabled() || !a.CheckToken("anything") || a.SessionFrom(httptest.NewRequest("GET", "/", nil)) == nil {
		t.Fatal("disabled auth should let every request in")
	}
}
