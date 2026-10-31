// Package rank scores phials for read surfaces.
//
// phial never deletes a phial, so ranking does the work that deletion does in
// other tools: an old or stale phial ranks lower, but it can always come back.
//
//	score = relevance × freshness × trust
//
// See docs/design.md section 7.1.
package rank

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Decay says how fast a phial loses freshness. A zero HalfLife means the
// phial never ages.
type Decay struct {
	HalfLife Duration `toml:"half_life"`
	Floor    float64  `toml:"floor"`
}

// Config holds every ranking value, so they can be tuned without a
// migration. Kinds is keyed by phial kind; Trust by verify_state.
type Config struct {
	Profile Decay              `toml:"profile"`
	Kinds   map[string]Decay   `toml:"kinds"`
	Trust   map[string]float64 `toml:"trust"`
}

// Default returns the values in docs/design.md section 7.1.
func Default() Config {
	return Config{
		Profile: Decay{HalfLife: 0, Floor: 1},
		Kinds: map[string]Decay{
			"invariant":  {HalfLife: Duration(2 * Year), Floor: 0.5},
			"convention": {HalfLife: Duration(2 * Year), Floor: 0.5},
			"preference": {HalfLife: Duration(2 * Year), Floor: 0.5},
			"decision":   {HalfLife: Duration(Year), Floor: 0.4},
			"fact":       {HalfLife: Duration(180 * Day), Floor: 0.3},
			"gotcha":     {HalfLife: Duration(180 * Day), Floor: 0.3},
		},
		Trust: map[string]float64{
			"ok":           1.0,
			"unverifiable": 0.9,
			"moved":        0.7,
			"stale":        0.3,
			"orphaned":     0.2,
		},
	}
}

// Merge fills every value that c leaves unset from Default, so a config
// file only has to name the values it changes.
func (c Config) Merge() Config {
	d := Default()
	if c.Profile == (Decay{}) {
		c.Profile = d.Profile
	}
	if c.Kinds == nil {
		c.Kinds = map[string]Decay{}
	}
	for k, v := range d.Kinds {
		if _, ok := c.Kinds[k]; !ok {
			c.Kinds[k] = v
		}
	}
	if c.Trust == nil {
		c.Trust = map[string]float64{}
	}
	for k, v := range d.Trust {
		if _, ok := c.Trust[k]; !ok {
			c.Trust[k] = v
		}
	}
	return c
}

// Validate rejects values that would break the score: floors and trust
// outside 0..1, and negative half-lives.
func (c Config) Validate() error {
	check := func(name string, d Decay) error {
		if d.HalfLife < 0 {
			return fmt.Errorf("ranking: %s: half_life must not be negative", name)
		}
		if d.Floor < 0 || d.Floor > 1 {
			return fmt.Errorf("ranking: %s: floor must be in 0..1, got %v", name, d.Floor)
		}
		return nil
	}
	if err := check("profile", c.Profile); err != nil {
		return err
	}
	for k, d := range c.Kinds {
		if err := check("kinds."+k, d); err != nil {
			return err
		}
	}
	for k, t := range c.Trust {
		if t < 0 || t > 1 {
			return fmt.Errorf("ranking: trust.%s must be in 0..1, got %v", k, t)
		}
	}
	return nil
}

// Freshness returns a value in floor..1 for a phial of the given kind whose
// age (time since it was last confirmed or updated) is age. Phials in the
// profile subject use the Profile decay whatever their kind.
func (c Config) Freshness(subjectKind, kind string, age time.Duration) float64 {
	d := c.Profile
	if subjectKind != "profile" {
		var ok bool
		d, ok = c.Kinds[kind]
		if !ok {
			d = c.Kinds["fact"]
		}
	}
	if d.HalfLife == 0 || age <= 0 {
		return 1
	}
	f := math.Pow(0.5, float64(age)/float64(d.HalfLife))
	return math.Max(d.Floor, f)
}

// TrustFor returns the trust factor for a verify_state. An unknown state is
// treated as unverifiable.
func (c Config) TrustFor(state string) float64 {
	if t, ok := c.Trust[state]; ok {
		return t
	}
	return c.Trust["unverifiable"]
}

// Parts is one phial's score with the values that made it, for `explain`.
type Parts struct {
	Relevance float64 `json:"relevance"`
	Freshness float64 `json:"freshness"`
	Trust     float64 `json:"trust"`
	Score     float64 `json:"score"`
}

// Score combines the three parts.
func Score(relevance, freshness, trust float64) Parts {
	return Parts{
		Relevance: relevance,
		Freshness: freshness,
		Trust:     trust,
		Score:     relevance * freshness * trust,
	}
}

// Normalize turns SQLite FTS5 bm25() values into relevance in 0..1. bm25()
// is negative and lower is better, so the best match gets 1 and the others
// get their share of it.
func Normalize(bm25 []float64) []float64 {
	out := make([]float64, len(bm25))
	best := 0.0
	for _, b := range bm25 {
		if b < best {
			best = b
		}
	}
	for i, b := range bm25 {
		if best == 0 {
			out[i] = 1
			continue
		}
		out[i] = b / best
	}
	return out
}

// Day and Year are the units config files use for half-lives.
const (
	Day  = 24 * time.Hour
	Year = 365 * Day
)

// Duration is a time.Duration that also reads "180d" and "2y" from TOML.
type Duration time.Duration

// UnmarshalText reads Go durations ("720h") and the day and year forms.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalText writes the shortest exact form.
func (d Duration) MarshalText() ([]byte, error) {
	v := time.Duration(d)
	switch {
	case v == 0:
		return []byte("0"), nil
	case v%Year == 0:
		return []byte(strconv.FormatInt(int64(v/Year), 10) + "y"), nil
	case v%Day == 0:
		return []byte(strconv.FormatInt(int64(v/Day), 10) + "d"), nil
	}
	return []byte(v.String()), nil
}

// ParseDuration reads "0", "none", Go durations, "<n>d" and "<n>y".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", "0", "none", "never":
		return 0, nil
	}
	for suffix, unit := range map[string]time.Duration{"d": Day, "y": Year} {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			f, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return 0, fmt.Errorf("duration %q: %w", s, err)
			}
			return time.Duration(f * float64(unit)), nil
		}
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q: %w", s, err)
	}
	return v, nil
}
