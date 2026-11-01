package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultScopes identify the user and keep the login alive: without openid
// Authentik puts no subject on the token, and without offline_access it
// issues no refresh token.
var DefaultScopes = []string{"openid", "profile", "offline_access"}

type endpoints struct {
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	Device        string `json:"device_authorization_endpoint"`
}

// discoverServer reads phial's protected-resource metadata to find the
// authorization server, then the server's own metadata.
func discoverServer(ctx context.Context, server string) (issuer string, eps *endpoints, err error) {
	prm, err := getJSON[struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}](ctx, strings.TrimRight(server, "/")+"/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		return "", nil, fmt.Errorf("the server does not publish OAuth metadata (is auth mode oauth?): %w", err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return "", nil, errors.New("the server names no authorization server")
	}
	issuer = strings.TrimRight(prm.AuthorizationServers[0], "/")
	for _, u := range []string{issuer + "/.well-known/openid-configuration", issuer + "/.well-known/oauth-authorization-server"} {
		e, err := getJSON[endpoints](ctx, u)
		if err == nil && e.Token != "" {
			return issuer, &e, nil
		}
	}
	return "", nil, fmt.Errorf("no metadata at %s", issuer)
}

func getJSON[T any](ctx context.Context, u string) (T, error) {
	var out T
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	return out, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
}

// LoginDevice runs the device authorization grant (RFC 8628): it prints a
// link and a code, and waits for the user to approve in any browser. The
// client id and secret come from the config or PHIAL_CLIENT_ID and
// PHIAL_CLIENT_SECRET, because Authentik has no usable dynamic registration.
func LoginDevice(ctx context.Context, c *Config) error {
	c.applyEnv()
	if c.ClientID == "" {
		return errors.New("no OAuth client: set PHIAL_CLIENT_ID (and PHIAL_CLIENT_SECRET) to the client the server's operator registered")
	}
	issuer, eps, err := discoverServer(ctx, c.Server)
	if err != nil {
		return err
	}
	if eps.Device == "" {
		return errors.New("the authorization server does not offer the device grant")
	}
	scopes := c.Scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	form := c.clientForm()
	form.Set("scope", strings.Join(scopes, " "))
	form.Set("resource", strings.TrimRight(c.Server, "/")+"/mcp")
	var da struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	if err := postForm(ctx, eps.Device, form, &da); err != nil {
		return fmt.Errorf("starting authorization: %w", err)
	}
	if da.DeviceCode == "" {
		return fmt.Errorf("starting authorization: %s %s", da.Error, da.ErrorDescription)
	}
	link := da.VerificationURIComplete
	if link == "" {
		link = da.VerificationURI
	}
	fmt.Println("Open this link in any browser to authorize:")
	fmt.Println()
	fmt.Println("  " + link)
	fmt.Println()
	if da.UserCode != "" {
		fmt.Printf("Your code: %s\n", da.UserCode)
	}
	fmt.Println("Waiting for you to authorize...")

	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(10 * time.Minute)
	if da.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return errors.New("this login expired; run login again")
		}
		form := c.clientForm()
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		form.Set("device_code", da.DeviceCode)
		var tok tokenResponse
		if err := postForm(ctx, eps.Token, form, &tok); err != nil {
			continue // a network blip should not abandon the login
		}
		switch tok.Error {
		case "":
			if tok.AccessToken == "" {
				return errors.New("the token response had no access token")
			}
			c.Auth, c.Issuer = "oauth", issuer
			c.Scopes = scopes
			c.setToken(tok)
			return c.Save()
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token":
			return errors.New("this login expired; run login again")
		case "access_denied":
			return errors.New("authorization was declined")
		default:
			return fmt.Errorf("authorization failed: %s %s", tok.Error, tok.ErrorDescription)
		}
	}
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (c *Config) clientForm() url.Values {
	f := url.Values{"client_id": {c.ClientID}}
	if c.ClientSecret != "" {
		f.Set("client_secret", c.ClientSecret)
	}
	return f
}

func (c *Config) setToken(t tokenResponse) {
	c.AccessToken = t.AccessToken
	if t.RefreshToken != "" {
		c.RefreshToken = t.RefreshToken
	}
	if t.ExpiresIn > 0 {
		c.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	} else {
		c.Expiry = time.Time{}
	}
}

func postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// bearer returns a live access token, refreshing it first when it is
// within a minute of expiry.
func (c *Config) bearer(ctx context.Context) (string, error) {
	if c.Auth == "token" || c.Token != "" {
		return c.Token, nil
	}
	if c.AccessToken != "" && (c.Expiry.IsZero() || time.Until(c.Expiry) > time.Minute) {
		return c.AccessToken, nil
	}
	if c.RefreshToken == "" {
		return "", errors.New("the login expired and there is no refresh token; run login again")
	}
	c.applyEnv()
	eps, err := getJSON[endpoints](ctx, c.Issuer+"/.well-known/openid-configuration")
	if err != nil {
		return "", err
	}
	form := c.clientForm()
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", c.RefreshToken)
	var tok tokenResponse
	if err := postForm(ctx, eps.Token, form, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("refresh failed: %s %s; run login again", tok.Error, tok.ErrorDescription)
	}
	c.setToken(tok)
	if err := c.Save(); err != nil {
		return "", err
	}
	return c.AccessToken, nil
}

// Logout forgets the tokens. The authorization server keeps its own record;
// revoke the session there if the machine is gone.
func (c *Config) Logout() error {
	c.AccessToken, c.RefreshToken, c.Token, c.Expiry = "", "", "", time.Time{}
	return c.Save()
}

// Hostname is the machine's name, for logs and service labels.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "this machine"
	}
	return h
}
