// Package render turns phials into the plain text that agents read.
//
// Every line carries the phial ID, so an agent can pass it back to
// supersede, retract or confirm, plus the labels from docs/design.md 7.1:
// (global), the age, and any verify problem.
package render

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jaspermayone/phial/internal/store"
)

// Age writes how long ago t was, in the largest whole unit.
func Age(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	day := 24 * time.Hour
	switch {
	case d < day:
		return "today"
	case d < 2*day:
		return "1 day ago"
	case d < 31*day:
		return fmt.Sprintf("%d days ago", int(d/day))
	case d < 365*day:
		n := int(d / (30 * day))
		if n <= 1 {
			return "1 month ago"
		}
		return fmt.Sprintf("%d months ago", n)
	default:
		n := int(d / (365 * day))
		if n == 1 {
			return "1 year ago"
		}
		return fmt.Sprintf("%d years ago", n)
	}
}

// Options change what a line shows.
type Options struct {
	// InProject marks global phials with (global). Set it when the agent
	// is working in a project, so a general rule is not taken as a rule of
	// this repo.
	InProject bool
	// Explain adds the score parts.
	Explain bool
}

// Line writes one phial as a markdown bullet.
func Line(p *store.Phial, parts *store.Result, now time.Time, o Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- [%s] (%s) %s", p.ID, p.Kind, p.Text)
	var tags []string
	if len(p.Paths) > 0 {
		tags = append(tags, "paths: "+strings.Join(p.Paths, ", "))
	}
	for _, l := range p.Loci {
		tags = append(tags, "locus: "+l.String())
	}
	if p.Status != store.StatusActive {
		switch p.Status {
		case store.StatusSuperseded:
			tags = append(tags, "SUPERSEDED by "+p.SupersededBy)
		case store.StatusRetracted:
			tags = append(tags, "RETRACTED: "+p.RetractReason)
		}
	}
	tags = append(tags, "confirmed "+Age(now, p.LastConfirmed()))
	if o.InProject && p.Global() {
		tags = append(tags, "(global) "+p.Subject)
	} else {
		tags = append(tags, p.Subject)
	}
	switch p.VerifyState {
	case "moved", "stale", "orphaned":
		note := p.VerifyNote
		if note == "" {
			note = "see verify"
		}
		tags = append(tags, fmt.Sprintf("(%s: %s)", p.VerifyState, note))
	}
	if o.Explain && parts != nil {
		r := parts.Rank
		tags = append(tags, fmt.Sprintf("score %.3f = relevance %.3f × freshness %.3f × trust %.2f", r.Score, r.Relevance, r.Freshness, r.Trust))
	}
	b.WriteString("  ·  ")
	b.WriteString(strings.Join(tags, "  ·  "))
	return b.String()
}

// Results writes a recall answer.
func Results(rs []store.Result, now time.Time, o Options) string {
	if len(rs) == 0 {
		return "No phials match. Try fewer or different words, or drop a filter."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d phial(s), best first:\n\n", len(rs))
	for i := range rs {
		b.WriteString(Line(rs[i].Phial, &rs[i], now, o))
		b.WriteByte('\n')
	}
	return b.String()
}

// Subject writes one subject as markdown, grouped by the first segment of
// each phial's first path. Phials with no path go first.
func Subject(sub *store.Subject, rs []store.Result, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", sub.Path)
	if sub.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", sub.Description)
	}
	if len(sub.Aliases) > 0 {
		fmt.Fprintf(&b, "Aliases: %s\n\n", strings.Join(sub.Aliases, ", "))
	}
	if len(rs) == 0 {
		b.WriteString("No active phials.\n")
		return b.String()
	}
	groups := map[string][]int{}
	var order []string
	for i := range rs {
		g := ""
		if len(rs[i].Paths) > 0 {
			g, _, _ = strings.Cut(rs[i].Paths[0], ".")
		}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], i)
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i] == "" || order[j] == "" {
			return order[i] == ""
		}
		return order[i] < order[j]
	})
	for _, g := range order {
		if g != "" {
			fmt.Fprintf(&b, "## %s\n\n", g)
		}
		for _, i := range groups[g] {
			b.WriteString(Line(rs[i].Phial, &rs[i], now, Options{InProject: true}))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Listing writes the subject index.
func Listing(subs []store.Subject) string {
	var b strings.Builder
	for _, s := range subs {
		fmt.Fprintf(&b, "- %s (%d)", s.Path, s.Active)
		if s.Description != "" {
			fmt.Fprintf(&b, ": %s", s.Description)
		}
		if len(s.Aliases) > 0 {
			fmt.Fprintf(&b, " [aka %s]", strings.Join(s.Aliases, ", "))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Fold writes the top n results and one line for the rest.
func Fold(rs []store.Result, total, n int, now time.Time, o Options, more string) string {
	var b strings.Builder
	if n > len(rs) {
		n = len(rs)
	}
	for i := 0; i < n; i++ {
		b.WriteString(Line(rs[i].Phial, &rs[i], now, o))
		b.WriteByte('\n')
	}
	if rest := total - n; rest > 0 {
		fmt.Fprintf(&b, "- +%d older phials, %s\n", rest, more)
	}
	return b.String()
}

// Node is one level of an outline.
type Node struct {
	Name     string
	Count    int
	Children []*Node
	Top      []store.Result
}

// Outline groups ranked phials by path. Depth 1 lists the first path
// segments, depth 2 the full paths, depth 3 adds the top `leaves` phials of
// each path. Phials with no path are counted under "(no path)".
func Outline(rs []store.Result, depth, leaves int) []*Node {
	if depth < 1 {
		depth = 1
	}
	if depth > 3 {
		depth = 3
	}
	top := map[string]*Node{}
	var order []*Node
	get := func(m map[string]*Node, list *[]*Node, name string) *Node {
		if n, ok := m[name]; ok {
			return n
		}
		n := &Node{Name: name}
		m[name] = n
		*list = append(*list, n)
		return n
	}
	sub := map[*Node]map[string]*Node{}
	for i := range rs {
		paths := rs[i].Paths
		if len(paths) == 0 {
			paths = []string{"(no path)"}
		}
		for _, p := range paths {
			first, _, _ := strings.Cut(p, ".")
			n := get(top, &order, first)
			n.Count++
			if depth >= 2 {
				if sub[n] == nil {
					sub[n] = map[string]*Node{}
				}
				c := get(sub[n], &n.Children, p)
				c.Count++
				if depth >= 3 && len(c.Top) < leaves {
					c.Top = append(c.Top, rs[i])
				}
			}
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Name < order[j].Name })
	for _, n := range order {
		sort.Slice(n.Children, func(i, j int) bool { return n.Children[i].Name < n.Children[j].Name })
	}
	return order
}

// OutlineText writes an outline.
func OutlineText(title string, nodes []*Node, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	if len(nodes) == 0 {
		b.WriteString("No active phials.\n")
	}
	for _, n := range nodes {
		fmt.Fprintf(&b, "- %s (%d)\n", n.Name, n.Count)
		for _, c := range n.Children {
			fmt.Fprintf(&b, "  - %s (%d)\n", c.Name, c.Count)
			for i := range c.Top {
				fmt.Fprintf(&b, "    %s\n", Line(c.Top[i].Phial, &c.Top[i], now, Options{InProject: true}))
			}
		}
	}
	return b.String()
}
