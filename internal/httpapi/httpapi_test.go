package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jaspermayone/phial/internal/auth"
	"github.com/jaspermayone/phial/internal/rank"
	"github.com/jaspermayone/phial/internal/store"
)

const tok = "0123456789abcdef0123456789abcdef"

func server(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "phial.db"), rank.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h, err := New(st, auth.New(tok, false))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, st
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestHealthzIsOpen(t *testing.T) {
	srv, _ := server(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
}

func TestMCPNeedsTokenAndWorksWithIt(t *testing.T) {
	srv, st := server(t)
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}

	ctx := context.Background()
	if _, _, err := st.Remember(ctx, store.RememberInput{Subject: "profile", Text: "Name: Jasper"}); err != nil {
		t.Fatal(err)
	}
	tr := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{tok}}, MaxRetries: -1}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "recall", Arguments: map[string]any{"query": "jasper"}})
	if err != nil {
		t.Fatal(err)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "Name: Jasper") {
		t.Fatalf("recall over HTTP: %s", txt)
	}

	bad := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{"wrong"}}, MaxRetries: -1}
	if _, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, bad, nil); err == nil {
		t.Fatal("wrong token connected")
	}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func TestUILoginAndActions(t *testing.T) {
	srv, st := server(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	// Not signed in: pages redirect to /login, posts are refused.
	resp, err := c.Get(srv.URL + "/s/profile")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Request.URL.Path != "/login" || resp.Request.URL.Query().Get("next") != "/s/profile" {
		t.Fatalf("not redirected to login: %s", resp.Request.URL)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
	resp, _ = c.PostForm(srv.URL+"/remember", url.Values{"subject": {"profile"}, "text": {"x"}})
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous post: %d", resp.StatusCode)
	}

	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"token": {"wrong"}})
	if resp.StatusCode != 401 {
		t.Fatalf("wrong token login: %d", resp.StatusCode)
	}
	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"token": {tok}, "next": {"https://evil.example/"}})
	if resp.StatusCode != 200 || resp.Request.URL.Host != strings.TrimPrefix(srv.URL, "http://") || resp.Request.URL.Path != "/" {
		t.Fatalf("login: %d at %s", resp.StatusCode, resp.Request.URL)
	}
	body, _ := io.ReadAll(resp.Body)
	m := csrfRe.FindSubmatch(body)
	if m == nil {
		t.Fatal("no CSRF token on the home page")
	}
	csrf := string(m[1])

	// A post without the CSRF token is refused even with a session.
	resp, _ = c.PostForm(srv.URL+"/remember", url.Values{"subject": {"areas/phial"}, "text": {"Uses Go"}})
	if resp.StatusCode != 403 {
		t.Fatalf("post without CSRF: %d", resp.StatusCode)
	}
	resp, _ = c.PostForm(srv.URL+"/remember", url.Values{"csrf": {csrf}, "subject": {"areas/phial"}, "text": {"Uses Go <b>and</b> SQLite"}, "kind": {"decision"}, "paths": {"stack"}})
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Request.URL.Path, "/p/ph_") {
		t.Fatalf("remember: %d at %s", resp.StatusCode, resp.Request.URL)
	}
	page, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(page), "Uses Go &lt;b&gt;and&lt;/b&gt; SQLite") {
		t.Fatal("phial text was not escaped")
	}
	id := strings.TrimPrefix(resp.Request.URL.Path, "/p/")

	resp, _ = c.PostForm(srv.URL+"/p/"+id+"/supersede", url.Values{"csrf": {csrf}, "text": {"Uses Go and SQLite with FTS5"}})
	if resp.StatusCode != 200 {
		t.Fatalf("supersede: %d", resp.StatusCode)
	}
	newID := strings.TrimPrefix(resp.Request.URL.Path, "/p/")
	resp, _ = c.PostForm(srv.URL+"/p/"+newID+"/retract", url.Values{"csrf": {csrf}, "reason": {"testing"}})
	if resp.StatusCode != 200 {
		t.Fatalf("retract: %d", resp.StatusCode)
	}
	p, err := st.Get(context.Background(), newID)
	if err != nil || p.Status != store.StatusRetracted {
		t.Fatalf("after retract: %+v %v", p, err)
	}

	for _, path := range []string{"/", "/s/areas/phial?status=all", "/search?q=sqlite&status=all", "/low", "/p/" + id} {
		resp, err := c.Get(srv.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s: %v %d", path, err, resp.StatusCode)
		}
	}
	if resp, _ := c.Get(srv.URL + "/p/ph_000000000000000000000000"); resp.StatusCode != 404 {
		t.Fatalf("missing phial: %d", resp.StatusCode)
	}

	resp, _ = c.PostForm(srv.URL+"/logout", url.Values{"csrf": {csrf}})
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("logout landed on %s", resp.Request.URL)
	}
	resp, _ = c.Get(srv.URL + "/")
	if resp.Request.URL.Path != "/login" {
		t.Fatal("still signed in after logout")
	}
}
