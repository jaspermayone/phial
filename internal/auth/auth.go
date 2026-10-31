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
	Expires time.Time
	// CSRF is the token every form in this session must send back.
	CSRF string
}

// StartSession sets the session cookie after a successful login.
func (a *Auth) StartSession(w http.ResponseWriter) {
	exp := a.Now().Add(SessionTTL).Unix()
	e := strconv.FormatInt(exp, 10)
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    e + "." + a.sign("session", e),
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
		return &Session{Expires: a.Now().Add(SessionTTL), CSRF: a.sign("csrf", "none")}
	}
	c, err := r.Cookie(CookieName)
	if err != nil {
		return nil
	}
	e, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign("session", e))) {
		return nil
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	if err != nil || a.Now().Unix() >= exp {
		return nil
	}
	return &Session{Expires: time.Unix(exp, 0), CSRF: a.sign("csrf", e)}
}

// CheckCSRF validates a state-changing UI request: the form token must
// match the session, and an Origin header, when sent, must be this host.
func (a *Auth) CheckCSRF(r *http.Request, s *Session) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(s.CSRF))
}

// Fingerprint is a short, safe label for the token, for logs.
func (a *Auth) Fingerprint() string {
	if !a.enabled {
		return "none"
	}
	sum := sha256.Sum256(a.token)
	return hex.EncodeToString(sum[:4])
}
