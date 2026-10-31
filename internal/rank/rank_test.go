package rank

import (
	"math"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestFreshnessHalvesPerHalfLife(t *testing.T) {
	c := Default()
	if f := c.Freshness("area", "decision", 0); f != 1 {
		t.Fatalf("age 0: %v", f)
	}
	if f := c.Freshness("area", "decision", Year); !near(f, 0.5) {
		t.Fatalf("one half-life: %v", f)
	}
	// Two years would be 0.25, but the decision floor is 0.4.
	if f := c.Freshness("area", "decision", 2*Year); f != 0.4 {
		t.Fatalf("floor: %v", f)
	}
	if f := c.Freshness("profile", "fact", 50*Year); f != 1 {
		t.Fatalf("profile must not age: %v", f)
	}
	if f, g := c.Freshness("area", "unknown-kind", 180*Day), c.Freshness("area", "fact", 180*Day); f != g {
		t.Fatalf("unknown kind should use fact decay: %v vs %v", f, g)
	}
}

func TestTrust(t *testing.T) {
	c := Default()
	for state, want := range map[string]float64{"ok": 1, "stale": 0.3, "orphaned": 0.2, "made-up": 0.9} {
		if got := c.TrustFor(state); got != want {
			t.Errorf("TrustFor(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestNormalize(t *testing.T) {
	got := Normalize([]float64{-4, -2, -1})
	want := []float64{1, 0.5, 0.25}
	for i := range want {
		if !near(got[i], want[i]) {
			t.Fatalf("Normalize = %v, want %v", got, want)
		}
	}
	if got := Normalize([]float64{0, 0}); got[0] != 1 || got[1] != 1 {
		t.Fatalf("no query should give relevance 1: %v", got)
	}
}

func TestConfigFromTOMLMergesDefaults(t *testing.T) {
	var c Config
	err := toml.Unmarshal([]byte(`
[kinds.gotcha]
half_life = "90d"
floor = 0.2

[trust]
stale = 0.1
`), &c)
	if err != nil {
		t.Fatal(err)
	}
	c = c.Merge()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if g := c.Kinds["gotcha"]; time.Duration(g.HalfLife) != 90*Day || g.Floor != 0.2 {
		t.Fatalf("gotcha = %+v", g)
	}
	if c.Kinds["decision"] != Default().Kinds["decision"] {
		t.Fatal("unset kind did not take the default")
	}
	if c.Trust["stale"] != 0.1 || c.Trust["ok"] != 1 {
		t.Fatalf("trust = %v", c.Trust)
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	c.Trust["ok"] = 1.5
	if c.Validate() == nil {
		t.Fatal("trust above 1 accepted")
	}
	c = Default()
	c.Kinds["fact"] = Decay{HalfLife: Duration(Day), Floor: -1}
	if c.Validate() == nil {
		t.Fatal("negative floor accepted")
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"2y": 2 * Year, "180d": 180 * Day, "720h": 720 * time.Hour, "none": 0, "0": 0, "1.5d": 36 * time.Hour,
	} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseDuration("soon"); err == nil {
		t.Error("ParseDuration(soon) accepted")
	}
}
