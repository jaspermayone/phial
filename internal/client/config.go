// Package client is phial-client: it logs a machine in, uploads agent
// sessions, checks code anchors, and serves brief and verify to a local
// agent over MCP.
package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Config is ~/.config/phial/client.toml. It holds the server, the OAuth
// client and the tokens, so it is written with mode 0600.
type Config struct {
	Server string `toml:"server"` // https://phial.hogwarts.dev
	// Auth is "oauth" or "token".
	Auth string `toml:"auth"`
	// Token is a static bearer token (auth = token).
	Token string `toml:"token,omitempty"`
	// OAuth client and tokens (auth = oauth).
	Issuer       string    `toml:"issuer,omitempty"`
	ClientID     string    `toml:"client_id,omitempty"`
	ClientSecret string    `toml:"client_secret,omitempty"`
	Scopes       []string  `toml:"scopes,omitempty"`
	AccessToken  string    `toml:"access_token,omitempty"`
	RefreshToken string    `toml:"refresh_token,omitempty"`
	Expiry       time.Time `toml:"expiry,omitempty"`
	// Roots are directories whose repos are synced.
	Roots []string `toml:"roots,omitempty"`
	// Synced remembers how far each transcript was read: path -> bytes.
	Synced map[string]int64 `toml:"synced,omitempty"`
	// BrainSeen holds entire-brain memory ids already sent.
	BrainSeen map[string]bool `toml:"brain_seen,omitempty"`
}

// Path is the config file.
func Path() string {
	if p := os.Getenv("PHIAL_CLIENT_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "phial", "client.toml")
}

// Load reads the config. A missing file gives an empty config.
func Load() (*Config, error) {
	c := &Config{Synced: map[string]int64{}, BrainSeen: map[string]bool{}}
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	if c.Synced == nil {
		c.Synced = map[string]int64{}
	}
	if c.BrainSeen == nil {
		c.BrainSeen = map[string]bool{}
	}
	return c, nil
}

// Save writes the config with owner-only permissions.
func (c *Config) Save() error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoggedIn reports whether there is something to send.
func (c *Config) LoggedIn() bool {
	return c.Server != "" && (c.Token != "" || c.AccessToken != "" || c.RefreshToken != "")
}

// Env overrides for the OAuth client, for a provider without dynamic
// registration: the operator registers one client and hands it out.
func (c *Config) applyEnv() {
	if v := os.Getenv("PHIAL_CLIENT_ID"); v != "" {
		c.ClientID = v
	}
	if v := os.Getenv("PHIAL_CLIENT_SECRET"); v != "" {
		c.ClientSecret = v
	}
	if v := os.Getenv("PHIAL_CLIENT_SCOPES"); v != "" {
		c.Scopes = strings.Fields(strings.ReplaceAll(v, ",", " "))
	}
}
