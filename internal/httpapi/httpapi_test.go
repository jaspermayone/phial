package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jaspermayone/phial/internal/auth"
	"github.com/jaspermayone/phial/internal/rank"
	"github.com/jaspermayone/phial/internal/store"
	"github.com/jaspermayone/phial/internal/tenant"
)

const tok = "0123456789abcdef0123456789abcdef"

func server(t *testing.T, mode string) (*httptest.Server, *tenant.Manager) {
	t.Helper()
	m, err := tenant.New(t.TempDir(), rank.Default(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	a := auth.New(tok, false)
	if mode == "none" {
		a = auth.New("", false)
	}
	a.Primary = "jsp"
	h, err := New(Options{Manager: m, Auth: a, Mode: mode, Primary: "jsp", UILogin: "token"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, m
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestHealthzIsOpen(t *testing.T) {
	srv, _ := server(t, "token")
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
}

func TestMCPNeedsTokenAndWorksWithIt(t *testing.T) {
	srv, m := server(t, "token")
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}

	ctx := context.Background()
	v, _ := m.View("jsp")
	if _, _, err := v.Remember(ctx, store.RememberInput{Subject: "profile", Text: "Name: Jasper"}); err != nil {
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

func TestClientAPI(t *testing.T) {
	srv, m := server(t, "token")
	c := &http.Client{Transport: bearer{tok}}
	post := func(path string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		resp, err := c.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	status, out := post("/api/ingest", IngestRequest{Sessions: []store.Session{
		{ID: "claude-code:1", Source: "claude-code", Repo: "github.com/a/b", Turns: []store.Turn{{Role: "user", Text: "we deploy to alastor"}}},
	}})
	if status != 200 || out["new"].(float64) != 1 {
		t.Fatalf("ingest: %d %v", status, out)
	}
	status, out = post("/api/remember", store.RememberInput{Subject: "areas/b", Text: "main.go holds serve", Loci: []store.Locus{{Repo: "github.com/a/b", File: "main.go", Symbol: "serve"}}})
	if status != 200 {
		t.Fatalf("remember: %d %v", status, out)
	}
	id := out["phial"].(map[string]any)["id"].(string)
	resp, _ := c.Get(srv.URL + "/api/loci?repo=git@github.com:a/b.git")
	var loci struct {
		Phials []store.Result `json:"phials"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&loci)
	if len(loci.Phials) != 1 || loci.Phials[0].ID != id {
		t.Fatalf("loci = %+v", loci)
	}
	status, out = post("/api/verify", map[string]any{"results": []map[string]string{{"id": id, "state": "stale", "note": "serve is gone"}}})
	if status != 200 || out["updated"].(float64) != 1 {
		t.Fatalf("verify: %d %v", status, out)
	}
	v, _ := m.View("jsp")
	p, _ := v.Get(context.Background(), id)
	if p.VerifyState != "stale" {
		t.Fatalf("verify state = %q", p.VerifyState)
	}
	resp, _ = c.Get(srv.URL + "/api/whoami")
	var who map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&who)
	if who["identity"] != "jsp" || who["pending_sessions"].(float64) != 1 {
		t.Fatalf("whoami = %v", who)
	}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func TestUILoginAndActions(t *testing.T) {
	srv, m := server(t, "token")
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

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
	mt := csrfRe.FindSubmatch(body)
	if mt == nil {
		t.Fatal("no CSRF token on the home page")
	}
	csrf := string(mt[1])

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

	resp, _ = c.PostForm(srv.URL+"/p/"+id+"/supersede", url.Values{"csrf": {csrf}, "text": {"Uses Go and SQLite with FTS5"}, "why": {"fts added"}})
	if resp.StatusCode != 200 {
		t.Fatalf("supersede: %d", resp.StatusCode)
	}
	newID := strings.TrimPrefix(resp.Request.URL.Path, "/p/")
	page, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(page), "<ins>with FTS5</ins>") || !strings.Contains(string(page), "fts added") {
		t.Fatalf("change diff not shown on the phial page")
	}
	resp, _ = c.PostForm(srv.URL+"/p/"+newID+"/retract", url.Values{"csrf": {csrf}, "reason": {"testing"}})
	if resp.StatusCode != 200 {
		t.Fatalf("retract: %d", resp.StatusCode)
	}
	v, _ := m.View("jsp")
	p, err := v.Get(context.Background(), newID)
	if err != nil || p.Status != store.StatusRetracted || p.CreatedBy != "jsp" {
		t.Fatalf("after retract: %+v %v", p, err)
	}
	for _, path := range []string{"/", "/s/areas/phial?status=all", "/search?q=sqlite&status=all", "/low", "/activity", "/proposals", "/remember", "/p/" + id} {
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
}

func TestNoAuthServesLoopbackHostsOnly(t *testing.T) {
	m, err := tenant.New(t.TempDir(), rank.Default(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	a := auth.New("", false)
	a.Primary = "jsp"
	h, err := New(Options{Manager: m, Auth: a, Mode: "none", Primary: "jsp", UILogin: "token"})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]int{"127.0.0.1:7480": 200, "localhost:7480": 200, "[::1]:7480": 200, "rebind.evil.example:7480": 403} {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %s: %d, want %d", host, w.Code, want)
		}
	}
}
