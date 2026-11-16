package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jaspermayone/phial/internal/store"
)

// Version is phial-client's version, set by main.
var Version = "dev"

// GranolaMCP is Granola's remote MCP server. It serves get_agent_brief, a
// profile of the user that Granola writes from their meetings and
// refreshes every morning.
const GranolaMCP = "https://mcp.granola.ai/mcp"

// GranolaSource is the session source for an uploaded brief. The server's
// extraction reads it as a third-party summary, not as the user's words.
const GranolaSource = "granola-brief"

// granolaEvery is the least time between two brief fetches. Granola
// refreshes the brief once a day, so a sync every 5 minutes fetches it a
// few times a day at most.
const granolaEvery = 6 * time.Hour

// Granola is the [granola] table of client.toml: the client that phial
// registered with Granola's authorization server, its tokens, and the last
// brief sent.
type Granola struct {
	Issuer       string    `toml:"issuer,omitempty"`
	ClientID     string    `toml:"client_id,omitempty"`
	AccessToken  string    `toml:"access_token,omitempty"`
	RefreshToken string    `toml:"refresh_token,omitempty"`
	Expiry       time.Time `toml:"expiry,omitempty"`
	// LastFetch is when the brief was last read; LastHash is the hash of
	// the brief last sent, so an unchanged brief is not sent again.
	LastFetch time.Time `toml:"last_fetch,omitempty"`
	LastHash  string    `toml:"last_hash,omitempty"`
}

// Connected reports whether there is a Granola login.
func (g *Granola) Connected() bool {
	return g != nil && (g.AccessToken != "" || g.RefreshToken != "")
}

var granolaScopes = []string{"openid", "profile", "email", "offline_access"}

// discoverGranola reads Granola's protected-resource metadata and its
// authorization server's metadata.
func discoverGranola(ctx context.Context) (issuer string, eps *endpoints, register string, err error) {
	u, _ := url.Parse(GranolaMCP)
	prm, err := getJSON[struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}](ctx, u.Scheme+"://"+u.Host+"/.well-known/oauth-protected-resource")
	if err != nil {
		return "", nil, "", fmt.Errorf("granola: %w", err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return "", nil, "", errors.New("granola names no authorization server")
	}
	issuer = strings.TrimRight(prm.AuthorizationServers[0], "/")
	meta, err := getJSON[struct {
		endpoints
		Registration string `json:"registration_endpoint"`
	}](ctx, issuer+"/.well-known/oauth-authorization-server")
	if err != nil {
		return "", nil, "", fmt.Errorf("granola: %w", err)
	}
	e := meta.endpoints
	return issuer, &e, meta.Registration, nil
}

// registerGranola registers phial-client as a public client with Granola's
// authorization server (RFC 7591). Granola requires a redirect URI even
// for the device grant; the loopback one is never opened.
func registerGranola(ctx context.Context, endpoint string) (string, error) {
	body := map[string]any{
		"client_name":                "phial-client",
		"redirect_uris":              []string{"http://127.0.0.1:8453/callback"},
		"grant_types":                []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token", "authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(b)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ClientID         string `json:"client_id"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("registering with granola: %w", err)
	}
	if out.ClientID == "" {
		return "", fmt.Errorf("registering with granola: %s %s", out.Error, out.ErrorDescription)
	}
	return out.ClientID, nil
}

// LoginGranola connects this machine to Granola: it registers a client the
// first time, then runs the device grant. After it, every sync sends the
// agent brief to phial when it changes.
func LoginGranola(ctx context.Context, c *Config) error {
	issuer, eps, register, err := discoverGranola(ctx)
	if err != nil {
		return err
	}
	if c.Granola == nil {
		c.Granola = &Granola{}
	}
	g := c.Granola
	if g.ClientID == "" || g.Issuer != issuer {
		if register == "" {
			return errors.New("granola offers no client registration")
		}
		id, err := registerGranola(ctx, register)
		if err != nil {
			return err
		}
		g.ClientID, g.Issuer = id, issuer
		if err := c.Save(); err != nil {
			return err
		}
	}
	tok, err := deviceFlow(ctx, eps, url.Values{"client_id": {g.ClientID}}, granolaScopes, GranolaMCP)
	if err != nil {
		return err
	}
	g.setToken(tok)
	g.LastFetch, g.LastHash = time.Time{}, ""
	return c.Save()
}

func (g *Granola) setToken(t tokenResponse) {
	g.AccessToken = t.AccessToken
	if t.RefreshToken != "" {
		g.RefreshToken = t.RefreshToken
	}
	if t.ExpiresIn > 0 {
		g.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	} else {
		g.Expiry = time.Time{}
	}
}

// granolaBearer returns a live Granola access token, refreshing it when it is
// within a minute of expiry.
func (c *Config) granolaBearer(ctx context.Context) (string, error) {
	g := c.Granola
	if !g.Connected() {
		return "", errors.New("granola is not connected; run: phial-client granola login")
	}
	if g.AccessToken != "" && (g.Expiry.IsZero() || time.Until(g.Expiry) > time.Minute) {
		return g.AccessToken, nil
	}
	if g.RefreshToken == "" {
		return "", errors.New("the granola login expired; run: phial-client granola login")
	}
	eps, err := getJSON[endpoints](ctx, g.Issuer+"/.well-known/oauth-authorization-server")
	if err != nil {
		return "", err
	}
	form := url.Values{"client_id": {g.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {g.RefreshToken}}
	var tok tokenResponse
	if err := postForm(ctx, eps.Token, form, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("granola refresh failed: %s %s; run: phial-client granola login", tok.Error, tok.ErrorDescription)
	}
	g.setToken(tok)
	if err := c.Save(); err != nil {
		return "", err
	}
	return g.AccessToken, nil
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// FetchGranolaBrief reads the whole agent brief from Granola's MCP server.
func FetchGranolaBrief(ctx context.Context, c *Config) (string, error) {
	tok, err := c.granolaBearer(ctx)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             GranolaMCP,
		HTTPClient:           &http.Client{Transport: bearerTransport{token: tok, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: "phial-client", Version: Version}, nil)
	cs, err := cl.Connect(ctx, transport, nil)
	if err != nil {
		return "", fmt.Errorf("granola: %w", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_agent_brief",
		Arguments: map[string]any{
			"fields": []string{"all"},
			"intent": "Keeping a personal memory store current with the user's work context.",
		},
	})
	if err != nil {
		return "", fmt.Errorf("granola get_agent_brief: %w", err)
	}
	var b strings.Builder
	for _, ct := range res.Content {
		if t, ok := ct.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
			b.WriteString("\n")
		}
	}
	text := strings.TrimSpace(b.String())
	if res.IsError {
		return "", fmt.Errorf("granola get_agent_brief: %.300s", text)
	}
	if text == "" {
		return "", errors.New("granola returned an empty brief")
	}
	return text, nil
}

// GranolaSession wraps a brief as a session for the server's extraction.
// Its ID is the brief's hash, so the server stores each version once.
func GranolaSession(brief string, now time.Time) store.Session {
	sum := sha256.Sum256([]byte(brief))
	h := hex.EncodeToString(sum[:])
	return store.Session{
		ID:        GranolaSource + ":" + h[:20],
		Source:    GranolaSource,
		Turns:     []store.Turn{{Index: 0, Role: "user", Text: brief}},
		StartedAt: now,
		EndedAt:   now,
	}
}

// SyncGranola sends the agent brief to phial when Granola is connected, at
// most once every granolaEvery, and only when the brief changed. force
// skips the time check.
func SyncGranola(ctx context.Context, c *Config, api *API, force bool) (sent bool, err error) {
	g := c.Granola
	if !g.Connected() {
		return false, nil
	}
	if !force && time.Since(g.LastFetch) < granolaEvery {
		return false, nil
	}
	brief, err := FetchGranolaBrief(ctx, c)
	if err != nil {
		return false, err
	}
	g.LastFetch = time.Now()
	se := GranolaSession(brief, g.LastFetch)
	if se.ID == g.LastHash {
		return false, c.Save()
	}
	if _, err := api.Ingest(ctx, []store.Session{se}); err != nil {
		return false, err
	}
	g.LastHash = se.ID
	return true, c.Save()
}
