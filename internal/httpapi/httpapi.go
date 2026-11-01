// Package httpapi joins phial's HTTP surfaces: /healthz (open), the OAuth
// discovery documents (open), /mcp and /api (bearer), and the web UI at /.
package httpapi

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/jaspermayone/phial/internal/auth"
	"github.com/jaspermayone/phial/internal/mcpserver"
	"github.com/jaspermayone/phial/internal/store"
	"github.com/jaspermayone/phial/internal/tenant"
	"github.com/jaspermayone/phial/internal/web"
)

// Options wires the server.
type Options struct {
	Manager *tenant.Manager
	Auth    *auth.Auth  // token and UI sessions
	OAuth   *auth.OAuth // nil unless mode oauth
	Mode    string      // token | oauth | none
	Primary string      // identity for token and none modes
	UILogin string      // token | oauth
}

// New builds the root handler.
func New(o Options) (http.Handler, error) {
	ui, err := web.New(web.Options{Manager: o.Manager, Auth: o.Auth, OAuth: o.OAuth, Primary: o.Primary, UILogin: o.UILogin})
	if err != nil {
		return nil, err
	}
	resolve := func(r *http.Request) (*tenant.View, error) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			return nil, errors.New("no identity")
		}
		return o.Manager.View(id.Subject)
	}
	// guard puts an identity on every API and MCP request.
	var guard func(http.Handler) http.Handler
	switch o.Mode {
	case "oauth":
		guard = o.OAuth.Bearer
	case "token":
		guard = func(next http.Handler) http.Handler {
			return o.Auth.Bearer(asPrimary(o.Primary, next))
		}
	default:
		guard = func(next http.Handler) http.Handler { return asPrimary(o.Primary, next) }
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": mcpserver.Version})
	})
	if o.OAuth != nil {
		mux.HandleFunc("GET "+auth.PathProtectedResource, o.OAuth.ProtectedResourceMetadata)
		mux.HandleFunc("GET "+auth.PathProtectedResource+"/", o.OAuth.ProtectedResourceMetadata)
		mux.HandleFunc("GET "+auth.PathAuthServer, o.OAuth.AuthServerMetadata)
	}
	mux.Handle("/mcp", guard(mcpserver.HTTPHandler(resolve, o.Mode != "none")))
	mux.Handle("/api/", guard(api(resolve)))
	mux.Handle("/", ui)
	if o.Mode != "none" {
		return mux, nil
	}
	return loopbackOnly(mux), nil
}

func asPrimary(primary string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{Subject: primary, ClientID: "token"})))
	})
}

// loopbackOnly refuses a Host that is not a loopback name. With auth off
// this stops a DNS-rebinding page, whose requests carry its own host name,
// from reaching the UI and its forms.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch strings.Trim(host, "[]") {
		case "127.0.0.1", "localhost", "::1":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "auth is off, so only loopback host names are served", http.StatusForbidden)
		}
	})
}

// ---- the client API ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, tenant.ErrNotAllowed):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrNotActive), errors.Is(err, tenant.ErrReadOnly):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// IngestRequest is what the client uploads.
type IngestRequest struct {
	Sessions []store.Session `json:"sessions"`
}

// VerifyRequest carries verify results from the client.
type VerifyRequest struct {
	Results []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Note  string `json:"note"`
	} `json:"results"`
}

func api(resolve func(*http.Request) (*tenant.View, error)) http.Handler {
	mux := http.NewServeMux()
	with := func(fn func(w http.ResponseWriter, r *http.Request, v *tenant.View)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			v, err := resolve(r)
			if err != nil {
				writeErr(w, err)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
			fn(w, r, v)
		}
	}
	mux.HandleFunc("GET /api/whoami", with(func(w http.ResponseWriter, r *http.Request, v *tenant.View) {
		var pools []string
		for _, p := range v.Pools {
			pools = append(pools, p.Pool)
		}
		st, _ := v.Stats(r.Context())
		total, pending, _ := v.Own.SessionStats(r.Context())
		writeJSON(w, 200, map[string]any{"identity": v.Identity, "pools": pools, "stats": st, "sessions": total, "pending_sessions": pending, "version": mcpserver.Version})
	}))
	mux.HandleFunc("POST /api/ingest", with(func(w http.ResponseWriter, r *http.Request, v *tenant.View) {
		var req IngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, errors.Join(store.ErrInvalid, err))
			return
		}
		for i := range req.Sessions {
			req.Sessions[i].UploadedBy = v.Identity
		}
		n, err := v.Own.PutSessions(r.Context(), req.Sessions)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]int{"received": len(req.Sessions), "new": n})
	}))
	mux.HandleFunc("GET /api/loci", with(func(w http.ResponseWriter, r *http.Request, v *tenant.View) {
		rs, err := v.Loci(r.Context(), r.URL.Query().Get("repo"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"phials": rs})
	}))
	mux.HandleFunc("POST /api/verify", with(func(w http.ResponseWriter, r *http.Request, v *tenant.View) {
		var req VerifyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, errors.Join(store.ErrInvalid, err))
			return
		}
		n := 0
		for _, res := range req.Results {
			if _, err := v.SetVerify(r.Context(), res.ID, res.State, res.Note); err == nil {
				n++
			}
		}
		writeJSON(w, 200, map[string]int{"updated": n})
	}))
	mux.HandleFunc("GET /api/recall", with(func(w http.ResponseWriter, r *http.Request, v *tenant.View) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		rs, err := v.Recall(r.Context(), store.Query{Text: q.Get("q"), Subject: q.Get("subject"), Kind: q.Get("kind"), PathPrefix: q.Get("path"), Scope: q.Get("scope"), Locus: q.Get("locus"), Status: q.Get("status"), Limit: limit})
		if err != nil {
			writeErr(w, err)
			return
		}
		v.RecordReceipts(r.Context(), "api", rs)
		writeJSON(w, 200, map[string]any{"phials": rs})
	}))
	mux.HandleFunc("POST /api/remember", with(func(w http.ResponseWriter, r *http.Request, v *tenant.View) {
		var in store.RememberInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, errors.Join(store.ErrInvalid, err))
			return
		}
		in.Actor = ""
		p, created, err := v.Remember(r.Context(), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"phial": p, "created": created})
	}))
	mux.HandleFunc("POST /api/retract", with(func(w http.ResponseWriter, r *http.Request, v *tenant.View) {
		var in struct{ ID, Reason string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, errors.Join(store.ErrInvalid, err))
			return
		}
		p, err := v.Retract(r.Context(), in.ID, in.Reason)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"phial": p})
	}))
	mux.HandleFunc("POST /api/supersede", with(func(w http.ResponseWriter, r *http.Request, v *tenant.View) {
		var in store.SupersedeInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, errors.Join(store.ErrInvalid, err))
			return
		}
		in.Actor = ""
		p, err := v.Supersede(r.Context(), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"phial": p})
	}))
	return mux
}
