// Package web is phial's browser UI: browse subjects, search, read one
// phial with its history and changes, review proposals, and remember,
// confirm, supersede or retract.
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
	"github.com/jaspermayone/phial/internal/tenant"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Options wires the UI.
type Options struct {
	Manager *tenant.Manager
	Auth    *auth.Auth
	OAuth   *auth.OAuth // for ui_login = oauth
	Primary string      // identity for token sign-ins
	UILogin string      // token | oauth
}

// UI serves the web pages.
type UI struct {
	o     Options
	pages map[string]*template.Template
	mux   *http.ServeMux

	mu       sync.Mutex
	failures int // failed logins since the last success, for back-off
}

// New builds the UI.
func New(o Options) (*UI, error) {
	u := &UI{o: o, pages: map[string]*template.Template{}, mux: http.NewServeMux()}
	now := func() time.Time { return time.Now().UTC() }
	funcs := template.FuncMap{
		"age":   func(t time.Time) string { return render.Age(now(), t) },
		"date":  func(t time.Time) string { return t.UTC().Format("2006-01-02") },
		"when":  func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") },
		"score": func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) },
		"pct":   func(f float64) string { return strconv.Itoa(int(f*100+0.5)) + "%" },
		"join":  strings.Join,
		"q":     url.QueryEscape,
		"kinds": func() []string { return store.Kinds },
		"diff":  render.DiffOps,
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
	u.mux.HandleFunc("GET /login/oauth", u.loginOAuth)
	u.mux.HandleFunc("GET /auth/callback", u.callback)
	u.mux.HandleFunc("POST /logout", u.session(u.logout))
	u.mux.HandleFunc("GET /{$}", u.session(u.home))
	u.mux.HandleFunc("GET /search", u.session(u.search))
	u.mux.HandleFunc("GET /low", u.session(u.low))
	u.mux.HandleFunc("GET /activity", u.session(u.activity))
	u.mux.HandleFunc("GET /proposals", u.session(u.proposals))
	u.mux.HandleFunc("POST /proposals/{id}/{action}", u.session(u.resolve))
	u.mux.HandleFunc("GET /remember", u.session(u.rememberPage))
	u.mux.HandleFunc("POST /remember", u.session(u.remember))
	u.mux.HandleFunc("GET /s/{path...}", u.session(u.subject))
	u.mux.HandleFunc("GET /p/{id}", u.session(u.phial))
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

type sessionHandler func(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View)

// session requires a login and resolves the user's view. GETs go to
// /login; POSTs also need CSRF.
func (u *UI) session(next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := u.o.Auth.SessionFrom(r)
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
			if !u.o.Auth.CheckCSRF(r, s) {
				http.Error(w, "bad or missing CSRF token; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		v, err := u.o.Manager.View(s.Subject)
		if err != nil {
			u.o.Auth.EndSession(w)
			u.fail(w, r, nil, nil, err)
			return
		}
		next(w, r, s, v)
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
	OAuthUI bool
	Who     string
	Data    any
	Subject string // the subject in view, for the sidebar
	Nav     string // the nav item in view
	Groups  []subjectGroup
	Attn    int
	Pending int
}

type subjectGroup struct {
	Kind     string
	Subjects []store.Subject
}

// attentionScore is the level under which an active phial needs a look.
const attentionScore = 0.5

func needsAttention(r store.Result) bool {
	return r.Rank.Score < attentionScore || r.VerifyState == "stale" || r.VerifyState == "orphaned" || r.VerifyState == "moved"
}

func attention(ctx context.Context, v *tenant.View, limit int) ([]store.Result, int, error) {
	rs, err := v.Recall(ctx, store.Query{Lowest: true, All: true})
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
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, n, nil
}

func groups(ctx context.Context, v *tenant.View) ([]subjectGroup, error) {
	subs, err := v.ListSubjects(ctx)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{"profile": "Profile", "area": "Areas", "topic": "Topics", "people": "People"}
	var out []subjectGroup
	for _, sub := range subs {
		l := labels[sub.Kind]
		if len(out) == 0 || out[len(out)-1].Kind != l {
			out = append(out, subjectGroup{Kind: l})
		}
		out[len(out)-1].Subjects = append(out[len(out)-1].Subjects, sub)
	}
	return out, nil
}

func (u *UI) show(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View, name string, p page) {
	if s != nil {
		p.CSRF = s.CSRF
		p.Who = s.Subject
	}
	p.AuthOn = u.o.Auth.Enabled()
	p.OAuthUI = u.o.UILogin == "oauth" && u.o.OAuth != nil
	if v != nil {
		if g, err := groups(r.Context(), v); err == nil {
			p.Groups = g
		}
		if _, n, err := attention(r.Context(), v, 0); err == nil {
			p.Attn = n
		}
		if ps, err := v.Proposals(r.Context(), "", 500); err == nil {
			p.Pending = len(ps)
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

func (u *UI) fail(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, tenant.ErrNotAllowed):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrNotActive), errors.Is(err, tenant.ErrReadOnly):
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	u.show(w, r, s, v, "error", page{Title: "Error", Error: err.Error()})
}

func back(w http.ResponseWriter, r *http.Request, to, flash string) {
	http.Redirect(w, r, to+"?flash="+url.QueryEscape(flash), http.StatusSeeOther)
}

// ---- login ----

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if u.o.Auth.SessionFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	if u.o.UILogin == "oauth" && u.o.OAuth != nil {
		http.Redirect(w, r, "/login/oauth?next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	u.show(w, r, nil, nil, "login", page{Title: "Sign in", Data: next})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if !u.o.Auth.SameOrigin(r) {
		http.Error(w, "cross-origin login refused", http.StatusForbidden)
		return
	}
	next := safeNext(r.PostFormValue("next"))
	if u.o.UILogin != "token" || !u.o.Auth.CheckToken(strings.TrimSpace(r.PostFormValue("token"))) {
		u.mu.Lock()
		u.failures++
		delay := time.Duration(min(u.failures, 10)) * 300 * time.Millisecond
		u.mu.Unlock()
		select { // slow down guessing without blocking shutdown forever
		case <-time.After(delay):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusUnauthorized)
		u.show(w, r, nil, nil, "login", page{Title: "Sign in", Error: "That token is not right.", Data: next})
		return
	}
	u.mu.Lock()
	u.failures = 0
	u.mu.Unlock()
	u.o.Auth.StartSession(w, u.o.Primary)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (u *UI) callbackURL() string {
	return strings.TrimRight(u.o.OAuth.PublicURL, "/") + "/auth/callback"
}

func (u *UI) loginOAuth(w http.ResponseWriter, r *http.Request) {
	if u.o.OAuth == nil || u.o.UILogin != "oauth" {
		http.NotFound(w, r)
		return
	}
	if err := u.o.Auth.StartLogin(w, r, u.o.OAuth, u.callbackURL(), safeNext(r.URL.Query().Get("next"))); err != nil {
		u.fail(w, r, nil, nil, err)
	}
}

func (u *UI) callback(w http.ResponseWriter, r *http.Request) {
	if u.o.OAuth == nil || u.o.UILogin != "oauth" {
		http.NotFound(w, r)
		return
	}
	subject, next, err := u.o.Auth.FinishLogin(w, r, u.o.OAuth, u.callbackURL())
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		u.show(w, r, nil, nil, "error", page{Title: "Sign-in failed", Error: err.Error()})
		return
	}
	if !u.o.Manager.Allowed(subject) {
		w.WriteHeader(http.StatusForbidden)
		u.show(w, r, nil, nil, "error", page{Title: "Not allowed", Error: "This user is not allowed on this server."})
		return
	}
	u.o.Auth.StartSession(w, subject)
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
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

func (u *UI) logout(w http.ResponseWriter, r *http.Request, _ *auth.Session, _ *tenant.View) {
	u.o.Auth.EndSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- pages ----

func (u *UI) home(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	ctx := r.Context()
	stats, err := v.Stats(ctx)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	_, n, err := attention(ctx, v, 0)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	lowest, err := v.Recall(ctx, store.Query{Lowest: true, Limit: 5})
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	recent, err := v.Recent(ctx, 5)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	subs, err := v.ListSubjects(ctx)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	total, pending, _ := v.Own.SessionStats(ctx)
	u.show(w, r, s, v, "home", page{Title: "Overview", Nav: "overview", Data: map[string]any{
		"Stats": stats, "Lowest": lowest, "AttnTotal": n, "Recent": recent, "Subjects": subs,
		"Sessions": total, "PendingSessions": pending,
	}})
}

func queryFrom(r *http.Request) store.Query {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	return store.Query{
		Text: q.Get("q"), Subject: q.Get("subject"), Kind: q.Get("kind"),
		PathPrefix: q.Get("path"), Scope: q.Get("scope"), Status: q.Get("status"), Limit: limit,
	}
}

func (u *UI) search(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	q := queryFrom(r)
	if q.Limit == 0 {
		q.Limit = 50
	}
	rs, err := v.Recall(r.Context(), q)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	u.show(w, r, s, v, "search", page{Title: "Search", Nav: "search", Query: q.Text, Data: map[string]any{"Q": q, "Results": rs}})
}

func (u *UI) low(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	rs, n, err := attention(r.Context(), v, 200)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	u.show(w, r, s, v, "low", page{Title: "Needs attention", Nav: "attention", Data: map[string]any{"Results": rs, "Total": n}})
}

func (u *UI) activity(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	q := r.URL.Query()
	es, err := v.Ledger(r.Context(), "", q.Get("subject"), q.Get("actor"), 100)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	u.show(w, r, s, v, "activity", page{Title: "Activity", Nav: "activity", Subject: q.Get("subject"), Data: map[string]any{
		"Entries": es, "Subject": q.Get("subject"), "Actor": q.Get("actor"),
	}})
}

func (u *UI) proposals(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	state := r.URL.Query().Get("state")
	ps, err := v.Proposals(r.Context(), state, 200)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	if state == "" {
		state = "pending"
	}
	u.show(w, r, s, v, "proposals", page{Title: "Proposals", Nav: "proposals", Data: map[string]any{"Proposals": ps, "State": state}})
}

func (u *UI) resolve(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	id := r.PathValue("id")
	switch r.PathValue("action") {
	case "apply":
		p, err := v.Resolve(r.Context(), id, true)
		if err != nil {
			u.fail(w, r, s, v, err)
			return
		}
		back(w, r, "/p/"+p.ID, "Applied the proposal.")
	case "reject":
		if _, err := v.Resolve(r.Context(), id, false); err != nil {
			u.fail(w, r, s, v, err)
			return
		}
		back(w, r, "/proposals", "Rejected the proposal.")
	default:
		http.NotFound(w, r)
	}
}

func (u *UI) rememberPage(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	sub := store.CleanSubject(r.URL.Query().Get("subject"))
	u.show(w, r, s, v, "remember", page{Title: "Remember", Nav: "remember", Subject: sub, Data: sub})
}

func (u *UI) subject(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	ctx := r.Context()
	path := store.CleanSubject(r.PathValue("path"))
	sub, err := v.GetSubject(ctx, path)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	status := r.URL.Query().Get("status")
	rs, err := v.Recall(ctx, store.Query{Subject: path, Status: status, All: true})
	if err != nil {
		u.fail(w, r, s, v, err)
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
	remotes, _ := v.Remotes(ctx, path)
	src, writeErr := v.For(path)
	u.show(w, r, s, v, "subject", page{Title: path, Subject: path, Data: map[string]any{
		"Subject": sub, "Results": own, "Children": children, "Remotes": remotes, "Status": status,
		"Outline": render.Outline(own, 2, 0), "Pool": src.Pool, "ReadOnly": writeErr != nil,
	}})
}

func (u *UI) phial(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	ctx := r.Context()
	p, err := v.Get(ctx, r.PathValue("id"))
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	hist, err := v.History(ctx, p.ID)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	served, _, err := v.ReceiptCount(ctx, p.ID)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	changes, _ := v.Ledger(ctx, p.ID, "", "", 50)
	src, _, _ := v.Where(ctx, p.ID)
	u.show(w, r, s, v, "phial", page{Title: p.ID, Subject: p.Subject, Data: map[string]any{
		"P": p, "Rank": src.Store.Score(p), "History": hist, "Served": served, "Changes": changes,
		"Global": p.Global(), "ReadOnly": !src.Write,
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

func (u *UI) remember(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	p, created, err := v.Remember(r.Context(), store.RememberInput{
		Subject:     r.PostFormValue("subject"),
		Kind:        r.PostFormValue("kind"),
		Text:        r.PostFormValue("text"),
		Paths:       splitList(r.PostFormValue("paths")),
		Loci:        lociFrom(r.PostFormValue("loci")),
		Description: r.PostFormValue("description"),
		Provenance:  store.Provenance{Source: "web"},
	})
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	msg := "Remembered."
	if !created {
		msg = "Already known, so it was confirmed."
	}
	back(w, r, "/p/"+p.ID, msg)
}

func (u *UI) confirm(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	p, err := v.Confirm(r.Context(), r.PathValue("id"))
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	back(w, r, "/p/"+p.ID, "Confirmed.")
}

func (u *UI) retract(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	p, err := v.Retract(r.Context(), r.PathValue("id"), r.PostFormValue("reason"))
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	back(w, r, "/p/"+p.ID, "Retracted. It stays as history.")
}

func (u *UI) supersede(w http.ResponseWriter, r *http.Request, s *auth.Session, v *tenant.View) {
	in := store.SupersedeInput{
		ID: r.PathValue("id"), Text: r.PostFormValue("text"), Kind: r.PostFormValue("kind"),
		Provenance: store.Provenance{Source: "web"}, Note: r.PostFormValue("why"),
	}
	if val := r.PostFormValue("paths"); strings.TrimSpace(val) != "" {
		in.Paths = splitList(val)
	}
	if val := r.PostFormValue("loci"); strings.TrimSpace(val) != "" {
		in.Loci = lociFrom(val)
	}
	p, err := v.Supersede(r.Context(), in)
	if err != nil {
		u.fail(w, r, s, v, err)
		return
	}
	back(w, r, "/p/"+p.ID, "Superseded. The old text is kept as history.")
}
