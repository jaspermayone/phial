// Package httpapi joins phial's HTTP surfaces: /healthz (open), /mcp
// (bearer token) and the web UI (session cookie) at /.
package httpapi

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/jaspermayone/phial/internal/auth"
	"github.com/jaspermayone/phial/internal/mcpserver"
	"github.com/jaspermayone/phial/internal/store"
	"github.com/jaspermayone/phial/internal/web"
)

// New builds the root handler.
func New(st *store.Store, a *auth.Auth) (http.Handler, error) {
	ui, err := web.New(st, a)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := st.Stats(r.Context()); err != nil {
			http.Error(w, `{"status":"store error"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": mcpserver.Version})
	})
	mux.Handle("/mcp", a.Bearer(mcpserver.HTTPHandler(st, a.Enabled())))
	mux.Handle("/", ui)
	if a.Enabled() {
		return mux, nil
	}
	return loopbackOnly(mux), nil
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
