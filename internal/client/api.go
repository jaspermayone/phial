package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jaspermayone/phial/internal/store"
)

// API talks to the phial server as the logged-in user.
type API struct {
	cfg  *Config
	http *http.Client
}

// NewAPI builds an API client.
func NewAPI(cfg *Config) *API {
	return &API{cfg: cfg, http: &http.Client{Timeout: 2 * time.Minute}}
}

func (a *API) do(ctx context.Context, method, path string, body any, out any) error {
	tok, err := a.cfg.bearer(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.cfg.Server, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("%s %s: %d: %.300s", method, path, resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Whoami is the server's view of this login.
type Whoami struct {
	Identity        string      `json:"identity"`
	Pools           []string    `json:"pools"`
	Stats           store.Stats `json:"stats"`
	Sessions        int         `json:"sessions"`
	PendingSessions int         `json:"pending_sessions"`
	Version         string      `json:"version"`
}

// Whoami asks the server who this login is.
func (a *API) Whoami(ctx context.Context) (*Whoami, error) {
	var w Whoami
	return &w, a.do(ctx, "GET", "/api/whoami", nil, &w)
}

// Ingest uploads sessions and returns how many were new.
func (a *API) Ingest(ctx context.Context, sessions []store.Session) (int, error) {
	var out struct {
		New int `json:"new"`
	}
	err := a.do(ctx, "POST", "/api/ingest", map[string]any{"sessions": sessions}, &out)
	return out.New, err
}

// Loci lists the phials with code anchors in repo.
func (a *API) Loci(ctx context.Context, repo string) ([]store.Result, error) {
	var out struct {
		Phials []store.Result `json:"phials"`
	}
	err := a.do(ctx, "GET", "/api/loci?repo="+url.QueryEscape(repo), nil, &out)
	return out.Phials, err
}

// VerifyResult is one checked anchor.
type VerifyResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Note  string `json:"note"`
}

// Verify reports anchor checks.
func (a *API) Verify(ctx context.Context, results []VerifyResult) (int, error) {
	var out struct {
		Updated int `json:"updated"`
	}
	err := a.do(ctx, "POST", "/api/verify", map[string]any{"results": results}, &out)
	return out.Updated, err
}

// Recall searches the server.
func (a *API) Recall(ctx context.Context, q store.Query) ([]store.Result, error) {
	v := url.Values{"q": {q.Text}, "subject": {q.Subject}, "kind": {q.Kind}, "path": {q.PathPrefix}, "scope": {q.Scope}, "locus": {q.Locus}, "status": {q.Status}}
	if q.Limit > 0 {
		v.Set("limit", fmt.Sprint(q.Limit))
	}
	var out struct {
		Phials []store.Result `json:"phials"`
	}
	err := a.do(ctx, "GET", "/api/recall?"+v.Encode(), nil, &out)
	return out.Phials, err
}

// Remember stores one phial.
func (a *API) Remember(ctx context.Context, in store.RememberInput) (*store.Phial, bool, error) {
	var out struct {
		Phial   *store.Phial `json:"phial"`
		Created bool         `json:"created"`
	}
	err := a.do(ctx, "POST", "/api/remember", in, &out)
	return out.Phial, out.Created, err
}
