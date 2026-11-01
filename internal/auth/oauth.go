package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OAuth makes phial an OAuth 2.1 protected resource: access tokens are
// checked at the authorization server's introspection endpoint (RFC 7662),
// and phial publishes protected-resource metadata (RFC 9728) so an MCP
// client can find the authorization server from a bare 401.
//
// A token must name phial as its audience. Without that check, a token the
// user got for any other app could read their memory. Providers that ignore
// the RFC 8707 resource indicator (Authentik) set aud to the client id, so
// AcceptedAudiences names that client.
type OAuth struct {
	// Issuer is the authorization server's base URL.
	Issuer string
	// PublicURL is phial's own URL; it is the resource identifier.
	PublicURL string
	// IntrospectionURL is the RFC 7662 endpoint. Empty means discovery.
	IntrospectionURL string
	// ClientID and ClientSecret authenticate phial to the introspection
	// endpoint, and the UI's sign-in to the token endpoint.
	ClientID     string
	ClientSecret string
	// AcceptedAudiences are audiences accepted besides PublicURL.
	AcceptedAudiences []string
	// RequiredScopes must all be on the token.
	RequiredScopes []string
	// Allowed reports whether a subject may use phial. nil means anyone.
	Allowed func(subject string) bool

	HTTP *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
	disco *discovery
}

// Identity is who a request turned out to be.
type Identity struct {
	Subject  string
	ClientID string
	Scopes   []string
}

type identityKey struct{}

// IdentityFrom returns the caller set by Bearer or the UI session.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// WithIdentity attaches a caller to a context.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// Well-known paths.
const (
	PathProtectedResource = "/.well-known/oauth-protected-resource"
	PathAuthServer        = "/.well-known/oauth-authorization-server"
)

type cacheEntry struct {
	res     introspection
	expires time.Time
}

type introspection struct {
	Active   bool     `json:"active"`
	Subject  string   `json:"sub"`
	Username string   `json:"preferred_username"`
	ClientID string   `json:"client_id"`
	Scope    string   `json:"scope"`
	Audience audience `json:"aud"`
	Exp      int64    `json:"exp"`
}

// audience reads RFC 7662's aud, a string or an array.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if one != "" {
			*a = audience{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a audience) has(want string) bool {
	want = normalize(want)
	for _, v := range a {
		if normalize(v) == want {
			return true
		}
	}
	return false
}

func normalize(s string) string { return strings.TrimRight(strings.ToLower(strings.TrimSpace(s)), "/") }

func (o *OAuth) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

type discovery struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	Introspection string `json:"introspection_endpoint"`
	UserInfo      string `json:"userinfo_endpoint"`
	Device        string `json:"device_authorization_endpoint"`
}

// Discover reads the authorization server's OpenID or OAuth metadata.
func (o *OAuth) Discover(ctx context.Context) (*discovery, error) {
	o.mu.Lock()
	if o.disco != nil {
		d := o.disco
		o.mu.Unlock()
		return d, nil
	}
	o.mu.Unlock()
	base := strings.TrimRight(o.Issuer, "/")
	var last error
	for _, u := range []string{base + "/.well-known/openid-configuration", base + PathAuthServer} {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := o.client().Do(req)
		if err != nil {
			last = err
			continue
		}
		var d discovery
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || d.Token == "" {
			last = fmt.Errorf("discovery at %s: status %d", u, resp.StatusCode)
			continue
		}
		o.mu.Lock()
		o.disco = &d
		o.mu.Unlock()
		return &d, nil
	}
	return nil, fmt.Errorf("oauth: no metadata at %s: %w", base, last)
}

// Bearer guards /mcp and the API with OAuth access tokens.
func (o *OAuth) Bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if tok == "" || tok == r.Header.Get("Authorization") {
			o.challenge(w, r, http.StatusUnauthorized, "invalid_request", "missing access token")
			return
		}
		id, status, code, desc := o.authorize(r.Context(), tok, r)
		if status != 0 {
			o.challenge(w, r, status, code, desc)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}

func (o *OAuth) resourceID(r *http.Request) string {
	base := strings.TrimRight(o.PublicURL, "/")
	if base == "" {
		base = requestBase(r)
	}
	if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
		return base + "/mcp"
	}
	return base
}

func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = strings.TrimSpace(strings.Split(v, ",")[0])
	}
	return scheme + "://" + host
}

func (o *OAuth) challenge(w http.ResponseWriter, r *http.Request, status int, code, desc string) {
	params := []string{`realm="phial"`, fmt.Sprintf(`error=%q`, code), fmt.Sprintf(`error_description=%q`, desc)}
	base := strings.TrimRight(o.PublicURL, "/")
	if base == "" {
		base = requestBase(r)
	}
	suffix := ""
	if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
		suffix = "/mcp"
	}
	params = append(params, fmt.Sprintf(`resource_metadata=%q`, base+PathProtectedResource+suffix))
	w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(params, ", "))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// authorize checks a token's liveness, audience, user and scopes.
func (o *OAuth) authorize(ctx context.Context, tok string, r *http.Request) (Identity, int, string, string) {
	res, ok := o.introspectCached(ctx, tok)
	if !ok || !res.Active {
		return Identity{}, http.StatusUnauthorized, "invalid_token", "token is not active"
	}
	want := o.resourceID(r)
	if !res.Audience.has(want) && !o.acceptedAudience(res.Audience) {
		slog.Warn("auth: token audience mismatch", "aud", []string(res.Audience), "want", want, "client", res.ClientID)
		return Identity{}, http.StatusForbidden, "invalid_token", "token was not issued for this resource"
	}
	subject := res.Subject
	if subject == "" {
		return Identity{}, http.StatusForbidden, "invalid_token", "token has no subject; the openid scope is needed"
	}
	if o.Allowed != nil && !o.Allowed(subject) {
		slog.Warn("auth: user not allowed", "sub", subject, "username", res.Username)
		return Identity{}, http.StatusForbidden, "invalid_token", "user is not permitted"
	}
	scopes := strings.Fields(res.Scope)
	for _, s := range o.RequiredScopes {
		found := false
		for _, have := range scopes {
			if have == s {
				found = true
			}
		}
		if !found {
			return Identity{}, http.StatusForbidden, "insufficient_scope", "missing scope " + s
		}
	}
	return Identity{Subject: subject, ClientID: res.ClientID, Scopes: scopes}, 0, "", ""
}

func (o *OAuth) acceptedAudience(a audience) bool {
	for _, acc := range o.AcceptedAudiences {
		if a.has(acc) {
			return true
		}
	}
	return false
}

func (o *OAuth) introspectCached(ctx context.Context, tok string) (introspection, bool) {
	sum := sha256.Sum256([]byte(tok))
	key := hex.EncodeToString(sum[:])
	o.mu.Lock()
	if o.cache == nil {
		o.cache = map[string]cacheEntry{}
	}
	if e, ok := o.cache[key]; ok && time.Now().Before(e.expires) {
		o.mu.Unlock()
		return e.res, true
	}
	o.mu.Unlock()
	res, ok := o.introspect(ctx, tok)
	if !ok {
		return introspection{}, false
	}
	ttl := 5 * time.Minute
	if !res.Active {
		ttl = 30 * time.Second
	} else if res.Exp > 0 {
		if d := time.Until(time.Unix(res.Exp, 0)); d < ttl {
			ttl = d
		}
	}
	if ttl > 0 {
		o.mu.Lock()
		o.cache[key] = cacheEntry{res: res, expires: time.Now().Add(ttl)}
		o.mu.Unlock()
	}
	return res, true
}

func (o *OAuth) introspect(ctx context.Context, tok string) (introspection, bool) {
	endpoint := o.IntrospectionURL
	if endpoint == "" {
		d, err := o.Discover(ctx)
		if err != nil || d.Introspection == "" {
			slog.Warn("auth: no introspection endpoint", "error", err)
			return introspection{}, false
		}
		endpoint = d.Introspection
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	form := url.Values{"token": {tok}}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return introspection{}, false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if o.ClientID != "" {
		req.SetBasicAuth(o.ClientID, o.ClientSecret)
	}
	resp, err := o.client().Do(req)
	if err != nil {
		slog.Warn("auth: introspection failed", "error", err)
		return introspection{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		slog.Warn("auth: introspection rejected", "status", resp.StatusCode)
		return introspection{}, false
	}
	var body introspection
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return introspection{}, false
	}
	return body, true
}

// ProtectedResourceMetadata serves the RFC 9728 document. The resource's own
// path follows the well-known segment, so /mcp is described at
// /.well-known/oauth-protected-resource/mcp.
func (o *OAuth) ProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimRight(o.PublicURL, "/")
	if base == "" {
		base = requestBase(r)
	}
	doc := map[string]any{
		"resource":                 base + strings.TrimPrefix(r.URL.Path, PathProtectedResource),
		"authorization_servers":    []string{strings.TrimRight(o.Issuer, "/")},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "phial",
		"resource_documentation":   "https://github.com/jaspermayone/phial",
	}
	if len(o.RequiredScopes) > 0 {
		doc["scopes_supported"] = o.RequiredScopes
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = json.NewEncoder(w).Encode(doc)
}

// AuthServerMetadata sends older MCP clients to the authorization server's
// own metadata. A redirect keeps the issuer check intact.
func (o *OAuth) AuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, strings.TrimRight(o.Issuer, "/")+PathAuthServer, http.StatusFound)
}

// ---- UI sign-in with the authorization code flow and PKCE ----

const loginCookie = "phial_login"

// loginState is what the browser carries between the redirect out and the
// callback: the PKCE verifier and the CSRF state, signed.
func (a *Auth) setLoginState(w http.ResponseWriter, state, verifier, next string) {
	v := state + "." + verifier + "." + base64.RawURLEncoding.EncodeToString([]byte(next))
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: v + "." + a.sign("login", v), Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: a.Secure, SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) loginState(r *http.Request) (state, verifier, next string, ok bool) {
	c, err := r.Cookie(loginCookie)
	if err != nil {
		return "", "", "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 4 {
		return "", "", "", false
	}
	v := strings.Join(parts[:3], ".")
	if !hmacEqual(parts[3], a.sign("login", v)) {
		return "", "", "", false
	}
	n, _ := base64.RawURLEncoding.DecodeString(parts[2])
	return parts[0], parts[1], string(n), true
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// StartLogin sends the browser to the authorization server. callback is
// phial's absolute redirect URI; it must be registered on the provider.
func (a *Auth) StartLogin(w http.ResponseWriter, r *http.Request, o *OAuth, callback, next string) error {
	d, err := o.Discover(r.Context())
	if err != nil {
		return err
	}
	if d.Authorization == "" {
		return errors.New("the authorization server has no authorization endpoint")
	}
	state, verifier := randomString(16), randomString(32)
	sum := sha256.Sum256([]byte(verifier))
	a.setLoginState(w, state, verifier, next)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {o.ClientID},
		"redirect_uri":          {callback},
		"scope":                 {"openid profile"},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, d.Authorization+"?"+q.Encode(), http.StatusSeeOther)
	return nil
}

// FinishLogin handles the callback: it checks the state, trades the code for
// a token, introspects it for the subject, and returns the subject and the
// page to go to next.
func (a *Auth) FinishLogin(w http.ResponseWriter, r *http.Request, o *OAuth, callback string) (subject, next string, err error) {
	state, verifier, next, ok := a.loginState(r)
	if !ok || state == "" || r.URL.Query().Get("state") != state {
		return "", "", errors.New("the sign-in state does not match; start again")
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.Secure})
	if e := r.URL.Query().Get("error"); e != "" {
		return "", "", fmt.Errorf("sign-in refused: %s %s", e, r.URL.Query().Get("error_description"))
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		return "", "", errors.New("no code in the callback")
	}
	d, err := o.Discover(r.Context())
	if err != nil {
		return "", "", err
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback},
		"client_id": {o.ClientID}, "code_verifier": {verifier},
	}
	if o.ClientSecret != "" {
		form.Set("client_secret", o.ClientSecret)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", d.Token, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Desc        string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", "", fmt.Errorf("token exchange failed: %s %s (status %d)", tok.Error, tok.Desc, resp.StatusCode)
	}
	res, ok := o.introspectCached(r.Context(), tok.AccessToken)
	if !ok || !res.Active || res.Subject == "" {
		return "", "", errors.New("the new token could not be checked")
	}
	if o.Allowed != nil && !o.Allowed(res.Subject) {
		return "", "", errors.New("this user is not allowed on this server")
	}
	if next == "" {
		next = "/"
	}
	return res.Subject, next, nil
}
