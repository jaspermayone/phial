// Package httpapi joins phial's HTTP surfaces: /healthz (open), /mcp
// (bearer token) and the web UI (session cookie) at /.
package httpapi

import (
	"encoding/json"
	"net/http"

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
	mux.Handle("/mcp", a.Bearer(mcpserver.HTTPHandler(st)))
	mux.Handle("/", ui)
	return mux, nil
}
