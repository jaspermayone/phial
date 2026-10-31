package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaspermayone/phial/internal/rank"
)

const tok = "0123456789abcdef0123456789abcdef"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultsNeedAToken(t *testing.T) {
	_, err := load(filepath.Join(t.TempDir(), "missing.toml"), env(nil))
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want a token error", err)
	}
}

func TestEnvWinsAndDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(`
addr = "127.0.0.1:9000"
public_url = "http://x"
[auth]
token = "from-file-but-too-short"
[ranking.kinds.gotcha]
half_life = "90d"
floor = 0.25
`), 0o600)
	c, err := load(path, env(map[string]string{"PHIAL_TOKEN": tok, "PHIAL_PUBLIC_URL": "https://phial.hogwarts.dev", "XDG_DATA_HOME": dir}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:9000" || c.Auth.Token != tok || !c.SecureCookies() {
		t.Fatalf("config = %+v", c)
	}
	if c.DB != filepath.Join(dir, "phial", "phial.db") {
		t.Fatalf("db = %q", c.DB)
	}
	if g := c.Ranking.Kinds["gotcha"]; time.Duration(g.HalfLife) != 90*rank.Day || g.Floor != 0.25 {
		t.Fatalf("gotcha decay = %+v", g)
	}
	if c.Ranking.Kinds["fact"] != rank.Default().Kinds["fact"] {
		t.Fatal("defaults not merged")
	}
}

func TestTokenFile(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	os.WriteFile(tf, []byte(tok+"\n"), 0o600)
	c, err := load(filepath.Join(dir, "none.toml"), env(map[string]string{"PHIAL_TOKEN_FILE": tf}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.Token != tok {
		t.Fatalf("token = %q", c.Auth.Token)
	}
}

func TestNoneOnlyOnLoopback(t *testing.T) {
	dir := t.TempDir()
	if _, err := load(filepath.Join(dir, "x.toml"), env(map[string]string{"PHIAL_AUTH": "none"})); err != nil {
		t.Fatalf("none on default loopback addr: %v", err)
	}
	if _, err := load(filepath.Join(dir, "x.toml"), env(map[string]string{"PHIAL_AUTH": "none", "PHIAL_ADDR": ":7480"})); err == nil {
		t.Fatal("none on all interfaces accepted")
	}
}
