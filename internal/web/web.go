// Package web is phial's browser UI: browse subjects, search, read one
// phial with its history, and remember, confirm, supersede or retract.
//
// It is server-rendered HTML with no JavaScript. Every page needs a session
// from /login, and every form carries a CSRF token.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jaspermayone/phial/internal/auth"
	"github.com/jaspermayone/phial/internal/render"
	"github.com/jaspermayone/phial/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// UI serves the web pages.
type UI struct {
	st    *store.Store
	auth  *auth.Auth
	pages map[string]*template.Template
	mux   *http.ServeMux

	mu       sync.Mutex
	failures int // failed logins since the last success, for back-off
}

// New builds the UI.
func New(st *store.Store, a *auth.Auth) (*UI, error) {
	u := &UI{st: st, auth: a, pages: map[string]*template.Template{}, mux: http.NewServeMux()}
	funcs := template.FuncMap{
		"age":   func(t time.Time) string { return render.Age(st.Now(), t) },
		"date":  func(t time.Time) string { return t.UTC().Format("2006-01-02") },
		"score": func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) },
		"pct":   func(f float64) string { return strconv.Itoa(int(f*100+0.5)) + "%" },
		"join":  strings.Join,
		"q":     url.QueryEscape,
		"kinds": func() []string { return store.Kinds },
		"short": func(path string) string {
			if _, rest, ok := strings.Cut(path, "/"); ok {
				return rest
			}
			return path
		},
		// lvl is the filled width of a level bar w units wide; a non-zero
		// score always shows at least a sliver, a zero shows none.
		"lvl": func(score float64, w int) int {
			n := int(score*float64(w) + 0.5)
			if score > 0 && n < 2 {
				n = 2
			}
			return min(n, w)
		},
		"warn": func(state string) bool {
			return state == "stale" || state == "orphaned" || state == "moved"
		},
	}
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", n)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", n, err)
		}
		u.pages[name] = t
	}

	static, _ := fs.Sub(assets, "static")
	u.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	u.mux.HandleFunc("GET /login", u.loginPage)
	u.mux.HandleFunc("POST /login", u.login)
	u.mux.HandleFunc("POST /logout", u.session(u.logout))
	u.mux.HandleFunc("GET /{$}", u.session(u.home))
	u.mux.HandleFunc("GET /search", u.session(u.search))
	u.mux.HandleFunc("GET /low", u.session(u.low))
	u.mux.HandleFunc("GET /s/{path...}", u.session(u.subject))
	u.mux.HandleFunc("GET /p/{id}", u.session(u.phial))
	u.mux.HandleFunc("GET /remember", u.session(u.rememberPage))
	u.mux.HandleFunc("POST /remember", u.session(u.remember))
	u.mux.HandleFunc("POST /p/{id}/confirm", u.session(u.confirm))
	u.mux.HandleFunc("POST /p/{id}/retract", u.session(u.retract))
	u.mux.HandleFunc("POST /p/{id}/supersede", u.session(u.supersede))
	return u, nil
}

// ServeHTTP adds security headers and routes.
func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	u.mux.ServeHTTP(w, r)
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, s *auth.Session)

// session requires a login. GETs go to /login; POSTs also need CSRF.
func (u *UI) session(next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := u.auth.SessionFrom(r)
		if s == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if !u.auth.CheckCSRF(r, s) {
				http.Error(w, "bad or missing CSRF token; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		next(w, r, s)
	}
}

// page is the data every template gets.
type page struct {
	Title   string
	CSRF    string
	Query   string
	Flash   string
	Error   string
	AuthOn  bool
	Data    any
	Subject string // the subject in view, for the sidebar
	Nav     string // the nav item in view: overview, search, attention, remember
	Groups  []subjectGroup
	Attn    int
}

// attentionScore is the level under which an active phial needs a look.
const attentionScore = 0.5

// needsAttention reports whether a ranked phial should be confirmed,
// rewritten or retracted.
func needsAttention(r store.Result) bool {
	return r.Rank.Score < attentionScore || r.VerifyState == "stale" || r.VerifyState == "orphaned" || r.VerifyState == "moved"
}

func (u *UI) attention(ctx context.Context, limit int) ([]store.Result, int, error) {
	rs, err := u.st.Recall(ctx, store.Query{Lowest: true, Limit: 200})
	if err != nil {
		return nil, 0, err
	}
	var out []store.Result
	for _, r := range rs {
		if needsAttention(r) {
			out = append(out, r)
		}
	}
	n := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, n, nil
}

func (u *UI) groups(ctx context.Context) ([]subjectGroup, error) {
	subs, err := u.st.ListSubjects(ctx)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{"profile": "Profile", "area": "Areas", "topic": "Topics", "people": "People"}
	var groups []subjectGroup
	for _, sub := range subs {
		l := labels[sub.Kind]
		if len(groups) == 0 || groups[len(groups)-1].Kind != l {
			groups = append(groups, subjectGroup{Kind: l})
		}
		groups[len(groups)-1].Subjects = append(groups[len(groups)-1].Subjects, sub)
	}
	return groups, nil
}

func (u *UI) show(w http.ResponseWriter, r *http.Request, s *auth.Session, name string, p page) {
	if s != nil {
		p.CSRF = s.CSRF
	}
	p.AuthOn = u.auth.Enabled()
	if s != nil && name != "login" {
		if g, err := u.groups(r.Context()); err == nil {
			p.Groups = g
		}
		if _, n, err := u.attention(r.Context(), 0); err == nil {
			p.Attn = n
		}
	}
	if p.Flash == "" {
		p.Flash = r.URL.Query().Get("flash")
	}
	t, ok := u.pages[name]
	if !ok {
		http.Error(w, "no template "+name, 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := t.ExecuteTemplate(w, "layout.html", p); err != nil {
		slog.Error("web: render", "page", name, "error", err)
	}
}

func (u *UI) fail(w http.ResponseWriter, r *http.Request, s *auth.Session, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrNotActive):
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	u.show(w, r, s, "error", page{Title: "Error", Error: err.Error()})
}

func back(w http.ResponseWriter, r *http.Request, to, flash string) {
	http.Redirect(w, r, to+"?flash="+url.QueryEscape(flash), http.StatusSeeOther)
}

// ---- login ----

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if u.auth.SessionFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.show(w, r, nil, "login", page{Title: "Sign in", Data: safeNext(r.URL.Query().Get("next"))})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if !u.auth.SameOrigin(r) {
		http.Error(w, "cross-origin login refused", http.StatusForbidden)
		return
	}
	next := safeNext(r.PostFormValue("next"))
	if !u.auth.CheckToken(strings.TrimSpace(r.PostFormValue("token"))) {
		u.mu.Lock()
		u.failures++
		delay := time.Duration(min(u.failures, 10)) * 300 * time.Millisecond
		u.mu.Unlock()
		select { // slow down guessing without blocking shutdown forever
		case <-time.After(delay):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusUnauthorized)
		u.show(w, r, nil, "login", page{Title: "Sign in", Error: "That token is not right.", Data: next})
		return
	}
	u.mu.Lock()
	u.failures = 0
	u.mu.Unlock()
	u.auth.StartSession(w)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// safeNext only allows a local path, so /login cannot redirect off-site.
// Browsers drop tabs and newlines while they parse a URL and read "\" as
// "/", so "/\t/evil.example" would become "//evil.example": any control
// character or backslash is refused before the path checks.
func safeNext(n string) string {
	for _, r := range n {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "/"
		}
	}
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") {
		return "/"
	}
	u, err := url.Parse(n)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return n
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	u.auth.EndSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- pages ----

type subjectGroup struct {
	Kind     string
	Subjects []store.Subject
}

func (u *UI) home(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	ctx := r.Context()
	stats, err := u.st.Stats(ctx)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	_, n, err := u.attention(ctx, 0)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	// The lowest five, flagged where they need a look: an empty "needs
	// attention" card would leave a hole beside the recent list.
	lowest, err := u.st.Recall(ctx, store.Query{Lowest: true, Limit: 5})
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	recent, err := u.st.Recent(ctx, 5)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	subs, err := u.st.ListSubjects(ctx)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	u.show(w, r, s, "home", page{Title: "Overview", Nav: "overview", Data: map[string]any{
		"Stats": stats, "Lowest": lowest, "AttnTotal": n, "Recent": recent, "Subjects": subs,
	}})
}

func queryFrom(r *http.Request) store.Query {
	v := r.URL.Query()
	limit, _ := strconv.Atoi(v.Get("limit"))
	return store.Query{
		Text: v.Get("q"), Subject: v.Get("subject"), Kind: v.Get("kind"),
		PathPrefix: v.Get("path"), Scope: v.Get("scope"), Status: v.Get("status"), Limit: limit,
	}
}

func (u *UI) search(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	q := queryFrom(r)
	if q.Limit == 0 {
		q.Limit = 50
	}
	rs, err := u.st.Recall(r.Context(), q)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	u.show(w, r, s, "search", page{Title: "Search", Nav: "search", Query: q.Text, Data: map[string]any{"Q": q, "Results": rs}})
}

func (u *UI) low(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	rs, n, err := u.attention(r.Context(), 200)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	u.show(w, r, s, "low", page{Title: "Needs attention", Nav: "attention", Data: map[string]any{"Results": rs, "Total": n}})
}

func (u *UI) rememberPage(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	sub := store.CleanSubject(r.URL.Query().Get("subject"))
	u.show(w, r, s, "remember", page{Title: "Remember", Nav: "remember", Subject: sub, Data: sub})
}

func (u *UI) subject(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	ctx := r.Context()
	path := store.CleanSubject(r.PathValue("path"))
	sub, err := u.st.GetSubject(ctx, path)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	status := r.URL.Query().Get("status")
	rs, err := u.st.Recall(ctx, store.Query{Subject: path, Status: status, All: true})
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	own := rs[:0]
	var children []string
	seen := map[string]bool{}
	for _, res := range rs {
		if res.Subject == path {
			own = append(own, res)
		} else if !seen[res.Subject] {
			seen[res.Subject] = true
			children = append(children, res.Subject)
		}
	}
	remotes, _ := u.st.Remotes(ctx, path)
	u.show(w, r, s, "subject", page{Title: path, Subject: path, Data: map[string]any{
		"Subject": sub, "Results": own, "Children": children, "Remotes": remotes, "Status": status,
		"Outline": render.Outline(own, 2, 0),
	}})
}

func (u *UI) phial(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	ctx := r.Context()
	p, err := u.st.Get(ctx, r.PathValue("id"))
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	hist, err := u.st.History(ctx, p.ID)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	served, last, err := u.st.ReceiptCount(ctx, p.ID)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	u.show(w, r, s, "phial", page{Title: p.ID, Subject: p.Subject, Data: map[string]any{
		"P": p, "Rank": u.st.Score(p), "History": hist, "Served": served, "LastServed": last,
		"Global": p.Global(),
	}})
}

// ---- actions ----

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func lociFrom(s string) []store.Locus {
	var out []store.Locus
	for _, f := range splitList(s) {
		out = append(out, store.ParseLocus(f))
	}
	return out
}

func (u *UI) remember(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	p, created, err := u.st.Remember(r.Context(), store.RememberInput{
		Subject:     r.PostFormValue("subject"),
		Kind:        r.PostFormValue("kind"),
		Text:        r.PostFormValue("text"),
		Paths:       splitList(r.PostFormValue("paths")),
		Loci:        lociFrom(r.PostFormValue("loci")),
		Description: r.PostFormValue("description"),
		Provenance:  store.Provenance{Source: "web"},
	})
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	msg := "Remembered."
	if !created {
		msg = "Already known, so it was confirmed."
	}
	back(w, r, "/p/"+p.ID, msg)
}

func (u *UI) confirm(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	p, err := u.st.Confirm(r.Context(), r.PathValue("id"))
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	back(w, r, "/p/"+p.ID, "Confirmed.")
}

func (u *UI) retract(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	p, err := u.st.Retract(r.Context(), r.PathValue("id"), r.PostFormValue("reason"))
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	back(w, r, "/p/"+p.ID, "Retracted. It stays as history.")
}

func (u *UI) supersede(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	in := store.SupersedeInput{
		ID: r.PathValue("id"), Text: r.PostFormValue("text"), Kind: r.PostFormValue("kind"),
		Provenance: store.Provenance{Source: "web"},
	}
	if v := r.PostFormValue("paths"); strings.TrimSpace(v) != "" {
		in.Paths = splitList(v)
	}
	if v := r.PostFormValue("loci"); strings.TrimSpace(v) != "" {
		in.Loci = lociFrom(v)
	}
	p, err := u.st.Supersede(r.Context(), in)
	if err != nil {
		u.fail(w, r, s, err)
		return
	}
	back(w, r, "/p/"+p.ID, "Superseded. The old text is kept as history.")
}
