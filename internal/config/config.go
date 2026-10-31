// Package config reads the server config: a TOML file, then environment
// variables, which always win.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/jaspermayone/phial/internal/rank"
)

// Config is the server config.
type Config struct {
	// Addr is the listen address. The default is loopback only: put a
	// reverse proxy in front for remote access.
	Addr string `toml:"addr"`
	// DB is the SQLite file.
	DB string `toml:"db"`
	// PublicURL is the external URL, e.g. https://phial.hogwarts.dev. When
	// it is https, UI cookies are marked Secure.
	PublicURL string      `toml:"public_url"`
	Auth      Auth        `toml:"auth"`
	Ranking   rank.Config `toml:"ranking"`
}

// Auth selects how callers prove who they are.
type Auth struct {
	// Mode is "token" (default) or "none". "none" is for local testing on
	// a loopback address only.
	Mode string `toml:"mode"`
	// Token is the shared bearer token. Prefer TokenFile or PHIAL_TOKEN so
	// the secret is not in the config file.
	Token string `toml:"token"`
	// TokenFile is a file that holds the token, e.g. an agenix secret path.
	TokenFile string `toml:"token_file"`
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
	return load(Path(), os.Getenv)
}

func load(path string, getenv func(string) string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}

	for env, dst := range map[string]*string{
		"PHIAL_ADDR":       &c.Addr,
		"PHIAL_DB":         &c.DB,
		"PHIAL_PUBLIC_URL": &c.PublicURL,
		"PHIAL_AUTH":       &c.Auth.Mode,
		"PHIAL_TOKEN":      &c.Auth.Token,
		"PHIAL_TOKEN_FILE": &c.Auth.TokenFile,
	} {
		if v := getenv(env); v != "" {
			*dst = v
		}
	}

	if c.Addr == "" {
		c.Addr = "127.0.0.1:7480"
	}
	if c.DB == "" {
		dataHome := getenv("XDG_DATA_HOME")
		if dataHome == "" {
			dataHome = xdg("XDG_DATA_HOME", filepath.Join(".local", "share"))
		}
		c.DB = filepath.Join(dataHome, "phial", "phial.db")
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = "token"
	}
	if c.Auth.Token == "" && c.Auth.TokenFile != "" {
		tok, err := os.ReadFile(c.Auth.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("auth.token_file: %w", err)
		}
		c.Auth.Token = strings.TrimSpace(string(tok))
	}
	c.Ranking = c.Ranking.Merge()
	return c, c.validate()
}

func (c *Config) validate() error {
	switch c.Auth.Mode {
	case "token":
		if len(c.Auth.Token) < 32 {
			return errors.New("auth: token mode needs a token of at least 32 characters (PHIAL_TOKEN, PHIAL_TOKEN_FILE or auth.token); make one with `openssl rand -hex 32`")
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
		return fmt.Errorf("auth: unknown mode %q (token or none)", c.Auth.Mode)
	}
	return c.Ranking.Validate()
}

// SecureCookies reports whether UI cookies need the Secure flag.
func (c *Config) SecureCookies() bool { return strings.HasPrefix(c.PublicURL, "https://") }
