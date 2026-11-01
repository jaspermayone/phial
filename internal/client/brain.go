package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jaspermayone/phial/internal/store"
)

// brainRepos lists the repos under the roots that have an entire-brain, by
// asking `entire brain path` for each git repo found up to repoDepth levels
// below a root. Hidden directories and node_modules are not entered, and the
// walk does not go below a repo.
func brainRepos(roots []string) []string {
	if _, err := exec.LookPath("entire"); err != nil {
		return nil
	}
	var out []string
	for _, r := range roots {
		for _, dir := range findRepos(expandHome(r), repoDepth) {
			cmd := exec.Command("entire", "brain", "path", dir)
			cmd.Env = append(os.Environ(), "ENTIRE_BRAIN_NO_EGRESS=1")
			p, err := cmd.Output()
			if err != nil {
				continue
			}
			if _, err := os.Stat(strings.TrimSpace(string(p))); err == nil {
				out = append(out, dir)
			}
		}
	}
	return out
}

// repoDepth is how many directory levels below a root findRepos looks:
// ~/dev/<repo> and ~/dev/<group>/<repo> and one more.
const repoDepth = 3

// findRepos returns the git repos at most depth levels below dir, in
// lexical order. A directory with a .git entry is a repo and is not entered.
func findRepos(dir string, depth int) []string {
	if depth <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules" {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Lstat(filepath.Join(sub, ".git")); err == nil {
			out = append(out, sub)
			continue
		}
		out = append(out, findRepos(sub, depth-1)...)
	}
	return out
}

// SyncBrain sends entire-brain memories to phial, one way. Each memory
// becomes a phial in the repo's area with origin imported and the brain's
// id as its source, so a re-run sends nothing twice.
func SyncBrain(ctx context.Context, cfg *Config, api *API, log func(string, ...any)) (int, error) {
	repos := brainRepos(cfg.Roots)
	sent := 0
	for _, dir := range repos {
		tmp, err := os.MkdirTemp("", "phial-brain-")
		if err != nil {
			return sent, err
		}
		cmd := exec.CommandContext(ctx, "entire", "brain", "memories", "files", "--dir", tmp)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "ENTIRE_BRAIN_NO_EGRESS=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(tmp)
			log("entire-brain export in %s: %v: %.200s", dir, err, out)
			continue
		}
		remote := gitRemote(dir)
		subject := "areas/" + slug(filepath.Base(dir))
		n, err := importBrainDir(ctx, cfg, api, tmp, subject, remote)
		os.RemoveAll(tmp)
		sent += n
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// BrainMemory is one exported entire-brain memory file.
type BrainMemory struct {
	ID     string
	Kind   string
	Paths  []string
	Status string
	Text   string
	Commit string
}

// ParseBrainFile reads one of `entire brain memories files` markdown files:
// a front matter block, then the text.
func ParseBrainFile(content string) (BrainMemory, bool) {
	var m BrainMemory
	if !strings.HasPrefix(content, "---\n") {
		return m, false
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return m, false
	}
	for _, ln := range strings.Split(content[4:4+end], "\n") {
		k, v, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "id":
			m.ID = v
		case "kind":
			m.Kind = v
		case "paths":
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					m.Paths = append(m.Paths, p)
				}
			}
		case "status":
			m.Status = v
		case "anchor":
			if c, ok := strings.CutPrefix(v, "commit "); ok {
				m.Commit = strings.TrimSpace(c)
			}
		}
	}
	m.Text = strings.TrimSpace(content[4+end+4:])
	return m, m.ID != "" && m.Text != ""
}

func importBrainDir(ctx context.Context, cfg *Config, api *API, dir, subject, remote string) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		m, ok := ParseBrainFile(string(b))
		if !ok || m.Status != "active" || cfg.BrainSeen[m.ID] {
			return nil
		}
		kind := m.Kind
		switch kind {
		case "decision", "invariant", "gotcha", "preference", "convention":
		default:
			kind = "fact"
		}
		var paths []string
		for _, bp := range m.Paths {
			if sp := brainPath(bp); sp != "" {
				paths = append(paths, sp)
			}
		}
		_, _, err = api.Remember(ctx, store.RememberInput{
			Subject: subject, Kind: kind, Text: m.Text, Paths: paths, Origin: "imported",
			Provenance: store.Provenance{Repo: remote, Commit: m.Commit, Source: "entire-brain:" + m.ID},
		})
		if err != nil {
			return fmt.Errorf("%s: %w", m.ID, err)
		}
		cfg.BrainSeen[m.ID] = true
		n++
		return nil
	})
	return n, err
}

// brainPath turns a brain taxonomy path into a phial path: the category
// and subcategory, so "constraints.retry.policy" becomes "retry.policy".
func brainPath(p string) string {
	parts := strings.Split(strings.ToLower(p), ".")
	if len(parts) >= 3 {
		parts = parts[1:]
	}
	return strings.Join(parts, ".")
}
