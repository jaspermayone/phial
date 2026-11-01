// Package config reads the server config: a TOML file, then environment
// variables, which always win.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/jaspermayone/phial/internal/rank"
	"github.com/jaspermayone/phial/internal/tenant"
)

// Config is the server config.
type Config struct {
	// Addr is the listen address. The default is loopback only: put a
	// reverse proxy in front for remote access.
	Addr string `toml:"addr"`
	// DataDir holds every store: users/<id>/phial.db and pools/<name>/phial.db.
	DataDir string `toml:"data_dir"`
	// PublicURL is the external URL, e.g. https://phial.hogwarts.dev. When
	// it is https, UI cookies are marked Secure.
	PublicURL string `toml:"public_url"`
	// PrimaryUser is the identity that token sign-ins, mode none, and the
	// CLI act as.
	PrimaryUser string        `toml:"primary_user"`
	Auth        Auth          `toml:"auth"`
	Pools       []tenant.Pool `toml:"pools"`
	LLM         LLM           `toml:"llm"`
	Extract     Extract       `toml:"extract"`
	Ranking     rank.Config   `toml:"ranking"`
}

// Auth selects how callers prove who they are.
type Auth struct {
	// Mode is "token" (default), "oauth", or "none" (loopback only).
	Mode string `toml:"mode"`
	// Token is the shared bearer token for mode token, and the UI's
	// password when ui_login is token. Prefer TokenFile or PHIAL_TOKEN.
	Token     string `toml:"token"`
	TokenFile string `toml:"token_file"`
	// AllowedUsers are the identities (OAuth sub) that may use phial. Empty
	// means anyone the authorization server vouches for.
	AllowedUsers []string `toml:"allowed_users"`
	// OAuth settings, for mode oauth.
	Issuer            string   `toml:"issuer"`
	IntrospectionURL  string   `toml:"introspection_url"`
	ClientID          string   `toml:"client_id"`
	ClientSecret      string   `toml:"client_secret"`
	ClientSecretFile  string   `toml:"client_secret_file"`
	AcceptedAudiences []string `toml:"accepted_audiences"`
	RequiredScopes    []string `toml:"required_scopes"`
	// UILogin is "token" (default) or "oauth": how the browser UI signs in.
	// "oauth" needs <public_url>/auth/callback registered on the provider.
	UILogin string `toml:"ui_login"`
}

// LLM is the model extraction uses. No key disables extraction.
type LLM struct {
	// Provider is "anthropic" (the Claude API, default) or "openai" (any
	// OpenAI-compatible endpoint, such as OpenRouter).
	Provider   string `toml:"provider"`
	BaseURL    string `toml:"base_url"`
	Model      string `toml:"model"`
	APIKey     string `toml:"api_key"`
	APIKeyFile string `toml:"api_key_file"`
}

// Extract tunes the background worker.
type Extract struct {
	Interval Duration `toml:"interval"` // how often to look; default 1m
	Quiet    Duration `toml:"quiet"`    // how long a session must sit first; default 5m
}

// Duration reads Go durations from TOML.
type Duration time.Duration

// UnmarshalText reads a Go duration.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Path returns the config file path: PHIAL_CONFIG, else
// $XDG_CONFIG_HOME/phial/config.toml.
func Path() string {
	if p := os.Getenv("PHIAL_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "phial", "config.toml")
}

func xdg(env, fallback string) string {
	if d := os.Getenv(env); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, fallback)
}

// Load reads the config file (a missing file is fine), applies env
// overrides and defaults, and validates the result.
func Load() (*Config, error) {
	c, err := load(Path(), os.Getenv)
	if err != nil {
		return nil, err
	}
	return c, c.validate()
}

// LoadAdmin is Load for the admin commands (import, extract, users,
// rank-report, purge). They open the stores directly and never check a
// caller, so the auth settings are not validated: an operator can run them
// as the service user without the OAuth secret or the UI token.
func LoadAdmin() (*Config, error) {
	c, err := load(Path(), os.Getenv)
	if err != nil {
		return nil, err
	}
	return c, c.Ranking.Validate()
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func load(path string, getenv func(string) string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		if err := fillDecayDefaults(b, &c.Ranking); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}

	for env, dst := range map[string]*string{
		"PHIAL_ADDR":                     &c.Addr,
		"PHIAL_DATA_DIR":                 &c.DataDir,
		"PHIAL_PUBLIC_URL":               &c.PublicURL,
		"PHIAL_PRIMARY_USER":             &c.PrimaryUser,
		"PHIAL_AUTH":                     &c.Auth.Mode,
		"PHIAL_TOKEN":                    &c.Auth.Token,
		"PHIAL_TOKEN_FILE":               &c.Auth.TokenFile,
		"PHIAL_OAUTH_ISSUER":             &c.Auth.Issuer,
		"PHIAL_OAUTH_INTROSPECTION_URL":  &c.Auth.IntrospectionURL,
		"PHIAL_OAUTH_CLIENT_ID":          &c.Auth.ClientID,
		"PHIAL_OAUTH_CLIENT_SECRET":      &c.Auth.ClientSecret,
		"PHIAL_OAUTH_CLIENT_SECRET_FILE": &c.Auth.ClientSecretFile,
		"PHIAL_UI_LOGIN":                 &c.Auth.UILogin,
		"PHIAL_LLM_PROVIDER":             &c.LLM.Provider,
		"PHIAL_LLM_BASE_URL":             &c.LLM.BaseURL,
		"PHIAL_LLM_MODEL":                &c.LLM.Model,
		"PHIAL_LLM_API_KEY":              &c.LLM.APIKey,
		"PHIAL_LLM_API_KEY_FILE":         &c.LLM.APIKeyFile,
	} {
		if v := getenv(env); v != "" {
			*dst = v
		}
	}
	for env, dst := range map[string]*[]string{
		"PHIAL_ALLOWED_USERS":      &c.Auth.AllowedUsers,
		"PHIAL_ACCEPTED_AUDIENCES": &c.Auth.AcceptedAudiences,
		"PHIAL_REQUIRED_SCOPES":    &c.Auth.RequiredScopes,
	} {
		if v := getenv(env); v != "" {
			*dst = splitList(v)
		}
	}

	if c.Addr == "" {
		c.Addr = "127.0.0.1:7480"
	}
	if c.DataDir == "" {
		dataHome := getenv("XDG_DATA_HOME")
		if dataHome == "" {
			dataHome = xdg("XDG_DATA_HOME", filepath.Join(".local", "share"))
		}
		c.DataDir = filepath.Join(dataHome, "phial")
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = "token"
	}
	if c.Auth.UILogin == "" {
		c.Auth.UILogin = "token"
	}
	if c.PrimaryUser == "" {
		c.PrimaryUser = "local"
	}
	if c.Auth.Token == "" && c.Auth.TokenFile != "" {
		if c.Auth.Token, err = readSecret(c.Auth.TokenFile); err != nil {
			return nil, fmt.Errorf("auth.token_file: %w", err)
		}
	}
	if c.Auth.ClientSecret == "" && c.Auth.ClientSecretFile != "" {
		if c.Auth.ClientSecret, err = readSecret(c.Auth.ClientSecretFile); err != nil {
			return nil, fmt.Errorf("auth.client_secret_file: %w", err)
		}
	}
	if c.LLM.APIKey == "" && c.LLM.APIKeyFile != "" {
		if c.LLM.APIKey, err = readSecret(c.LLM.APIKeyFile); err != nil {
			return nil, fmt.Errorf("llm.api_key_file: %w", err)
		}
	}
	if c.LLM.Provider == "" {
		c.LLM.Provider = "anthropic"
	}
	if c.LLM.Provider == "openai" && c.LLM.BaseURL == "" {
		c.LLM.BaseURL = "https://openrouter.ai/api/v1"
	}
	c.Ranking = c.Ranking.Merge()
	return c, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *Config) validate() error {
	switch c.Auth.Mode {
	case "token":
		if len(c.Auth.Token) < 32 {
			return errors.New("auth: token mode needs a token of at least 32 characters (PHIAL_TOKEN, PHIAL_TOKEN_FILE or auth.token); make one with `openssl rand -hex 32`")
		}
	case "oauth":
		if c.Auth.Issuer == "" {
			return errors.New("auth: oauth mode needs auth.issuer (PHIAL_OAUTH_ISSUER)")
		}
		if c.Auth.ClientID == "" {
			return errors.New("auth: oauth mode needs auth.client_id (PHIAL_OAUTH_CLIENT_ID) for introspection")
		}
		if c.PublicURL == "" {
			return errors.New("auth: oauth mode needs public_url")
		}
		if c.Auth.UILogin == "token" && len(c.Auth.Token) < 32 {
			return errors.New("auth: the UI signs in with a token (ui_login = token), so set a token of at least 32 characters, or set ui_login = oauth")
		}
	case "none":
		host := c.Addr
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		if host != "127.0.0.1" && host != "localhost" && host != "[::1]" {
			return fmt.Errorf("auth: mode none is only allowed on a loopback address, not %q", c.Addr)
		}
	default:
		return fmt.Errorf("auth: unknown mode %q (token, oauth or none)", c.Auth.Mode)
	}
	switch c.Auth.UILogin {
	case "token", "oauth":
	default:
		return fmt.Errorf("auth: ui_login must be token or oauth, not %q", c.Auth.UILogin)
	}
	if c.Auth.UILogin == "oauth" && c.Auth.Mode != "oauth" {
		return errors.New("auth: ui_login = oauth needs mode = oauth")
	}
	if c.LLM.APIKey != "" && c.LLM.Model == "" {
		return errors.New("llm: an api key is set but no model (llm.model / PHIAL_LLM_MODEL)")
	}
	if c.LLM.Provider != "anthropic" && c.LLM.Provider != "openai" {
		return fmt.Errorf("llm: provider must be anthropic or openai, not %q", c.LLM.Provider)
	}
	return c.Ranking.Validate()
}

// SecureCookies reports whether UI cookies need the Secure flag.
func (c *Config) SecureCookies() bool { return strings.HasPrefix(c.PublicURL, "https://") }

// fillDecayDefaults puts back the default half-life or floor for a kind
// whose table sets only the other one. Without it, a config that changes a
// half-life would silently set that kind's floor to 0.
func fillDecayDefaults(b []byte, rc *rank.Config) error {
	var raw struct {
		Ranking struct {
			Profile map[string]any            `toml:"profile"`
			Kinds   map[string]map[string]any `toml:"kinds"`
		} `toml:"ranking"`
	}
	if err := toml.Unmarshal(b, &raw); err != nil {
		return err
	}
	def := rank.Default()
	fill := func(set map[string]any, d *rank.Decay, dflt rank.Decay) {
		if _, ok := set["half_life"]; !ok {
			d.HalfLife = dflt.HalfLife
		}
		if _, ok := set["floor"]; !ok {
			d.Floor = dflt.Floor
		}
	}
	if raw.Ranking.Profile != nil {
		fill(raw.Ranking.Profile, &rc.Profile, def.Profile)
	}
	for k, set := range raw.Ranking.Kinds {
		dflt, ok := def.Kinds[k]
		if !ok {
			dflt = def.Kinds["fact"]
		}
		d := rc.Kinds[k]
		fill(set, &d, dflt)
		rc.Kinds[k] = d
	}
	return nil
}
