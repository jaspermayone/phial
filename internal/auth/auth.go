// Package auth checks callers. MCP and API clients send the shared token as
// a bearer token. The web UI trades the same token for a signed session
// cookie at /login, and every form carries a CSRF token.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CookieName is the UI session cookie.
const CookieName = "phial_session"

// SessionTTL is how long a UI login lasts.
const SessionTTL = 30 * 24 * time.Hour

// Auth holds the token and the keys derived from it. A zero Token means
// mode "none": every check passes.
type Auth struct {
	token   []byte
	key     []byte // signs session cookies
	Secure  bool   // set the Secure flag on cookies
	Now     func() time.Time
	enabled bool
	// Origin is the public origin ("https://phial.hogwarts.dev"). When it is
	// set, a POST's Origin header must equal it; this works behind a proxy
	// that rewrites Host. When it is empty, the Origin must match Host.
	Origin string
	// Primary is the identity a token sign-in or mode none acts as.
	Primary string
}

// New builds an Auth. An empty token disables auth (mode "none").
func New(token string, secure bool) *Auth {
	a := &Auth{token: []byte(token), Secure: secure, Now: time.Now, enabled: token != ""}
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("phial session key v1"))
	a.key = m.Sum(nil)
	return a
}

// Enabled reports whether a token is required.
func (a *Auth) Enabled() bool { return a.enabled }

// CheckToken compares a presented token in constant time.
func (a *Auth) CheckToken(t string) bool {
	if !a.enabled {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(t), a.token) == 1
}

// Bearer guards an API or MCP handler. A missing or wrong token gets 401
// with a WWW-Authenticate header, which tells MCP clients to send one.
func (a *Auth) Bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.enabled {
			h := r.Header.Get("Authorization")
			tok, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || !a.CheckToken(strings.TrimSpace(tok)) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="phial"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) sign(parts ...string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(strings.Join(parts, "\x00")))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Session is a valid UI login.
type Session struct {
	Subject string // who is signed in
	Expires time.Time
	// CSRF is the token every form in this session must send back.
	CSRF string
}

// StartSession sets the session cookie after a successful login for subject.
func (a *Auth) StartSession(w http.ResponseWriter, subject string) {
	exp := a.Now().Add(SessionTTL).Unix()
	e := strconv.FormatInt(exp, 10)
	sub := base64.RawURLEncoding.EncodeToString([]byte(subject))
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    e + "." + sub + "." + a.sign("session", e, sub),
		Path:     "/",
		Expires:  time.Unix(exp, 0),
		HttpOnly: true,
		Secure:   a.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// EndSession clears the session cookie.
func (a *Auth) EndSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.Secure, SameSite: http.SameSiteStrictMode,
	})
}

// SessionFrom returns the request's session, or nil if there is none or it
// is forged or expired. With auth disabled every request has a session.
func (a *Auth) SessionFrom(r *http.Request) *Session {
	if !a.enabled {
		return &Session{Subject: a.Primary, Expires: a.Now().Add(SessionTTL), CSRF: a.sign("csrf", "none")}
	}
	c, err := r.Cookie(CookieName)
	if err != nil {
		return nil
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 || !hmacEqual(parts[2], a.sign("session", parts[0], parts[1])) {
		return nil
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || a.Now().Unix() >= exp {
		return nil
	}
	sub, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	return &Session{Subject: string(sub), Expires: time.Unix(exp, 0), CSRF: a.sign("csrf", parts[0], parts[1])}
}

func hmacEqual(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// CheckCSRF validates a state-changing UI request: the form token must
// match the session, and an Origin header, when sent, must be this host.
func (a *Auth) CheckCSRF(r *http.Request, s *Session) bool {
	if !a.SameOrigin(r) {
		return false
	}
	return hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(s.CSRF))
}

// SameOrigin reports whether a request's Origin header, when sent, is this
// server. A missing Origin passes; the CSRF token still has to match.
func (a *Auth) SameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	if a.Origin != "" {
		return strings.EqualFold(strings.TrimSuffix(o, "/"), a.Origin)
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// OriginOf turns a public URL into its origin: scheme and host only.
func OriginOf(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// Fingerprint is a short, safe label for the token, for logs.
func (a *Auth) Fingerprint() string {
	if !a.enabled {
		return "none"
	}
	sum := sha256.Sum256(a.token)
	return hex.EncodeToString(sum[:4])
}
