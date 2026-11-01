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
	if c.DataDir != filepath.Join(dir, "phial") {
		t.Fatalf("data_dir = %q", c.DataDir)
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

func TestPartialDecayKeepsTheOtherDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("[ranking.kinds.fact]\nhalf_life = \"90d\"\n[ranking.kinds.decision]\nfloor = 0.1\n"), 0o600)
	c, err := load(path, env(map[string]string{"PHIAL_TOKEN": tok, "XDG_DATA_HOME": dir}))
	if err != nil {
		t.Fatal(err)
	}
	if f := c.Ranking.Kinds["fact"]; time.Duration(f.HalfLife) != 90*rank.Day || f.Floor != rank.Default().Kinds["fact"].Floor {
		t.Fatalf("fact = %+v, want 90d with the default floor", f)
	}
	if d := c.Ranking.Kinds["decision"]; d.Floor != 0.1 || d.HalfLife != rank.Default().Kinds["decision"].HalfLife {
		t.Fatalf("decision = %+v, want floor 0.1 with the default half-life", d)
	}
}

func TestOAuthModeNeedsIssuerAndClient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.toml")
	env1 := env(map[string]string{"PHIAL_AUTH": "oauth", "PHIAL_PUBLIC_URL": "https://p.example", "PHIAL_TOKEN": tok})
	if _, err := load(path, env1); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("missing issuer: %v", err)
	}
	os.WriteFile(path, []byte(`
public_url = "https://p.example"
[auth]
mode = "oauth"
issuer = "https://a.example/application/o/phial/"
client_id = "phial"
accepted_audiences = ["phial"]
allowed_users = ["jsp"]
ui_login = "oauth"
[[pools]]
name = "crane"
members = ["jsp", "owen"]
subjects = ["areas/crane"]
`), 0o600)
	c, err := load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.UILogin != "oauth" || len(c.Pools) != 1 || c.Pools[0].Members[1] != "owen" || c.Auth.AllowedUsers[0] != "jsp" {
		t.Fatalf("config = %+v", c)
	}
}
