// Package importer brings memory from other tools into phial.
//
// lard keeps one markdown file per subject, with a front matter block and a
// body of bullets. Each top-level bullet becomes one phial with origin
// "imported". See docs/design.md section 9.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jaspermayone/phial/internal/store"
)

// LardSubject is one lard subject file.
type LardSubject struct {
	Path        string    // e.g. areas/homelab/netbox
	Description string    `json:"description"`
	Aliases     []string  `json:"aliases"`
	Repos       []string  `json:"repos"`
	Body        string    `json:"body"`
	Updated     time.Time `json:"updated"`
}

// Item is one bullet, ready to become a phial.
type Item struct {
	Text    string
	Tag     string // stated, observed, inferred, ... or ""
	Date    time.Time
	Heading string // the markdown heading above the bullet, if any
}

var (
	tagRe  = regexp.MustCompile(`^\[([a-zA-Z-]+)(?:\s+(\d{4}-\d{2}-\d{2}))?\]\s*`)
	dateRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})[:,]?\s`)
)

// ParseBody splits a lard body into items. A line that starts with "- " or
// "* " at column 0 starts an item; the indented or plain lines that follow
// belong to it. Headings set the path of the items below them.
func ParseBody(body string) []Item {
	var items []Item
	var cur *strings.Builder
	heading := ""
	flush := func() {
		if cur == nil {
			return
		}
		items = append(items, parseItem(cur.String(), heading))
		cur = nil
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#"):
			flush()
			heading = strings.TrimSpace(strings.TrimLeft(line, "#"))
		case strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* "):
			flush()
			cur = &strings.Builder{}
			cur.WriteString(strings.TrimSpace(line[2:]))
		case trimmed == "":
			// A blank line inside a bullet keeps the bullet open; the next
			// bullet or heading closes it.
		case cur != nil:
			cur.WriteByte(' ')
			cur.WriteString(trimmed)
		default:
			// Prose outside any bullet is its own item.
			flush()
			cur = &strings.Builder{}
			cur.WriteString(trimmed)
		}
	}
	flush()
	out := items[:0]
	for _, it := range items {
		if it.Text != "" {
			out = append(out, it)
		}
	}
	return out
}

func parseItem(s, heading string) Item {
	it := Item{Heading: heading}
	s = strings.TrimSpace(s)
	if m := tagRe.FindStringSubmatch(s); m != nil {
		it.Tag = strings.ToLower(m[1])
		if m[2] != "" {
			it.Date, _ = time.Parse("2006-01-02", m[2])
		}
		s = s[len(m[0]):]
	}
	if it.Date.IsZero() {
		if m := dateRe.FindStringSubmatch(s); m != nil {
			it.Date, _ = time.Parse("2006-01-02", m[1])
		}
	}
	it.Text = strings.Join(strings.Fields(s), " ")
	return it
}

// ParseFile reads one lard subject file (front matter plus body).
func ParseFile(path, content string) LardSubject {
	sub := LardSubject{Path: store.CleanSubject(path)}
	rest := content
	if strings.HasPrefix(content, "---\n") {
		if end := strings.Index(content[4:], "\n---"); end >= 0 {
			for _, line := range strings.Split(content[4:4+end], "\n") {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				v = strings.TrimSpace(v)
				switch strings.TrimSpace(k) {
				case "description":
					sub.Description = v
				case "aliases":
					sub.Aliases = listValue(v)
				case "repos":
					sub.Repos = listValue(v)
				case "updated":
					sub.Updated, _ = time.Parse(time.RFC3339, v)
				}
			}
			rest = strings.TrimPrefix(content[4+end+4:], "\n")
		}
	}
	sub.Body = strings.TrimSpace(rest)
	return sub
}

func listValue(v string) []string {
	v = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(v), "["), "]")
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.Trim(strings.TrimSpace(p), `"'`); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ReadDir loads every subject under a lard memory directory. It also
// accepts a directory that holds a "memory" folder, such as one tenant of
// a `lard backup` tree.
func ReadDir(dir string) ([]LardSubject, error) {
	if st, err := os.Stat(filepath.Join(dir, "memory")); err == nil && st.IsDir() {
		dir = filepath.Join(dir, "memory")
	}
	var subs []LardSubject
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != "profile.md" && !strings.HasPrefix(rel, "areas/") && !strings.HasPrefix(rel, "topics/") && !strings.HasPrefix(rel, "people/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		subs = append(subs, ParseFile(rel, string(b)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, fmt.Errorf("no lard subject files under %s", dir)
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].Path < subs[j].Path })
	return subs, nil
}

// Fetch loads every subject from a running lard server over HTTP:
// GET /memory for the listing, then GET /memory/{path}?format=json.
func Fetch(ctx context.Context, base, token string, client *http.Client) ([]LardSubject, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base = strings.TrimSuffix(base, "/")
	get := func(path string, v any) error {
		req, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
		if err != nil {
			return err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
		}
		return json.NewDecoder(resp.Body).Decode(v)
	}
	var listing []struct {
		Path string `json:"path"`
	}
	if err := get("/memory", &listing); err != nil {
		return nil, err
	}
	var subs []LardSubject
	for _, l := range listing {
		var sub LardSubject
		p := store.CleanSubject(l.Path)
		if err := get("/memory/"+escapePath(p)+"?format=json", &sub); err != nil {
			return nil, err
		}
		sub.Path = p
		subs = append(subs, sub)
	}
	return subs, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// SubjectReport counts what happened to one subject.
type SubjectReport struct {
	Subject  string `json:"subject"`
	Items    int    `json:"items"`
	Created  int    `json:"created"`
	Existing int    `json:"existing"`
	Error    string `json:"error,omitempty"`
}

// Report is the result of an import.
type Report struct {
	DryRun   bool            `json:"dry_run"`
	Subjects []SubjectReport `json:"subjects"`
	Items    int             `json:"items"`
	Created  int             `json:"created"`
	Existing int             `json:"existing"`
	Failed   int             `json:"failed_subjects"`
}

var pathSlug = regexp.MustCompile(`[^a-z0-9]+`)

// headingPath turns a heading into a one-segment phial path.
func headingPath(h string) string {
	p := strings.Trim(pathSlug.ReplaceAllString(strings.ToLower(h), "-"), "-")
	if len(p) > 40 {
		p = strings.TrimRight(p[:40], "-")
	}
	return p
}

// Import writes subjects into the store. With dryRun it only counts, and
// st may be nil when there is no database yet. A subject that fails is
// reported and the others still import. Running it twice does not make
// copies, and a purged phial is never imported again.
func Import(ctx context.Context, st *store.Store, subs []LardSubject, dryRun bool) (*Report, error) {
	rep := &Report{DryRun: dryRun}
	for _, sub := range subs {
		sr := SubjectReport{Subject: sub.Path}
		items := ParseBody(sub.Body)
		sr.Items = len(items)
		if _, err := store.SubjectKind(sub.Path); err != nil {
			sr.Error = err.Error()
			rep.add(sr)
			continue
		}
		if dryRun {
			if st != nil {
				if err := countExisting(ctx, st, sub.Path, items, &sr); err != nil {
					sr.Error = err.Error()
				}
			}
			rep.add(sr)
			continue
		}
		if err := importSubject(ctx, st, sub, items, &sr); err != nil {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			sr.Error = err.Error()
		}
		rep.add(sr)
	}
	return rep, nil
}

func (r *Report) add(sr SubjectReport) {
	r.Subjects = append(r.Subjects, sr)
	r.Items += sr.Items
	r.Created += sr.Created
	r.Existing += sr.Existing
	if sr.Error != "" {
		r.Failed++
	}
}

func importSubject(ctx context.Context, st *store.Store, sub LardSubject, items []Item, sr *SubjectReport) error {
	if _, err := st.EnsureSubject(ctx, sub.Path, sub.Description, sub.Aliases); err != nil {
		return err
	}
	if kind, _ := store.SubjectKind(sub.Path); kind == "area" {
		for _, r := range sub.Repos {
			if err := st.LinkRemote(ctx, sub.Path, r); err != nil {
				return err
			}
		}
	}
	for _, it := range items {
		skip, err := known(ctx, st, sub.Path, it.Text)
		if err != nil {
			return err
		}
		if skip {
			sr.Existing++
			continue
		}
		created := it.Date
		if created.IsZero() {
			created = sub.Updated
		}
		var paths []string
		if hp := headingPath(it.Heading); hp != "" {
			paths = []string{hp}
		}
		source := "lard:" + sub.Path
		if it.Tag != "" {
			source += " [" + it.Tag + "]"
		}
		if _, _, err := st.Remember(ctx, store.RememberInput{
			Subject:    sub.Path,
			Kind:       "fact",
			Text:       it.Text,
			Paths:      paths,
			Origin:     "imported",
			Provenance: store.Provenance{Source: source},
			CreatedAt:  created,
		}); err != nil {
			return fmt.Errorf("item %q: %w", truncate(it.Text, 60), err)
		}
		sr.Created++
	}
	return nil
}

// known reports whether an item is already in the store in any status, or
// was purged. A purged phial must not come back from a re-import.
func known(ctx context.Context, st *store.Store, subject, text string) (bool, error) {
	if _, err := st.FindByText(ctx, subject, text); err == nil {
		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	return st.WasPurged(ctx, subject, text)
}

// countExisting fills a dry run's report with what an apply would skip.
func countExisting(ctx context.Context, st *store.Store, subject string, items []Item, sr *SubjectReport) error {
	for _, it := range items {
		skip, err := known(ctx, st, subject, it.Text)
		if err != nil {
			return err
		}
		if skip {
			sr.Existing++
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
