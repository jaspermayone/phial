package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jaspermayone/phial/internal/store"
)

// VerifyRepo checks every phial anchored in the repo at dir and reports
// the results to the server. A file that exists and holds the symbol is
// ok; a symbol that is gone is stale; a file that moved (same base name
// elsewhere in the repo) is moved; a missing file is stale.
//
// When entire-graph is installed its definition lookup is tried first,
// because it knows about renames and languages; the grep is the fallback.
func VerifyRepo(ctx context.Context, api *API, dir string, log func(string, ...any)) (checked, changed int, err error) {
	remote := gitRemote(dir)
	if remote == "" {
		return 0, 0, fmt.Errorf("%s has no git remote", dir)
	}
	phials, err := api.Loci(ctx, remote)
	if err != nil {
		return 0, 0, err
	}
	var results []VerifyResult
	for _, p := range phials {
		state, note := "ok", ""
		for _, l := range p.Loci {
			if l.File == "" && l.Symbol == "" {
				continue
			}
			s, n := checkLocus(ctx, dir, l)
			if worse(s, state) {
				state, note = s, n
			}
		}
		checked++
		if state != p.VerifyState {
			changed++
		}
		results = append(results, VerifyResult{ID: p.ID, State: state, Note: note})
	}
	if len(results) > 0 {
		if _, err := api.Verify(ctx, results); err != nil {
			return checked, changed, err
		}
	}
	return checked, changed, nil
}

var order = map[string]int{"ok": 0, "unverifiable": 1, "moved": 2, "stale": 3, "orphaned": 4}

func worse(a, b string) bool { return order[a] > order[b] }

func checkLocus(ctx context.Context, dir string, l store.Locus) (string, string) {
	if l.File != "" {
		path := filepath.Join(dir, filepath.FromSlash(l.File))
		if _, err := os.Stat(path); err != nil {
			if found := findMoved(dir, l.File); found != "" {
				return "moved", l.File + " is now " + found
			}
			return "stale", l.File + " does not exist"
		}
		if l.Symbol == "" {
			return "ok", ""
		}
		if fileHasSymbol(path, l.Symbol) {
			return "ok", ""
		}
		if graphHasSymbol(ctx, dir, l.Symbol) {
			return "moved", l.Symbol + " is no longer in " + l.File
		}
		return "stale", l.Symbol + " not found in " + l.File
	}
	if graphHasSymbol(ctx, dir, l.Symbol) || grepRepo(dir, l.Symbol) {
		return "ok", ""
	}
	return "stale", l.Symbol + " not found in the repo"
}

func fileHasSymbol(path, symbol string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(symbol) + `($|[^A-Za-z0-9_])`)
	return re.Match(b)
}

func grepRepo(dir, symbol string) bool {
	out, err := git(dir, "grep", "-l", "-w", "--", symbol)
	return err == nil && out != ""
}

func findMoved(dir, file string) string {
	out, err := git(dir, "ls-files", "--", "*/"+filepath.Base(file), filepath.Base(file))
	if err != nil || out == "" {
		return ""
	}
	return strings.Split(out, "\n")[0]
}

// graphHasSymbol asks entire-graph, when it is installed, whether a symbol
// is defined anywhere in the repo.
func graphHasSymbol(ctx context.Context, dir, symbol string) bool {
	if _, err := exec.LookPath("entire"); err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, "entire", "graph", "def", symbol, "--repo", dir, "--json")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), `"name"`) && strings.Contains(string(out), symbol)
}

// Brief gathers context for a task: the server's recall plus, when
// entire-graph is installed, its code map for the same words.
func Brief(ctx context.Context, api *API, dir, task string) (string, error) {
	var b strings.Builder
	q := store.Query{Text: task, Limit: 15}
	if remote := gitRemote(dir); remote != "" {
		// Area phials for this repo first, then everything else.
		if rs, err := api.Recall(ctx, store.Query{Text: task, Limit: 10, Scope: "area"}); err == nil && len(rs) > 0 {
			b.WriteString("# Phials about this project\n\n")
			for _, r := range rs {
				fmt.Fprintf(&b, "- [%s] (%s) %s\n", r.ID, r.Kind, r.Text)
			}
			b.WriteString("\n")
		}
		q.Scope = "global"
	}
	rs, err := api.Recall(ctx, q)
	if err != nil {
		return "", err
	}
	if len(rs) > 0 {
		b.WriteString("# Phials that apply everywhere\n\n")
		for _, r := range rs {
			fmt.Fprintf(&b, "- [%s] (%s) %s · %s\n", r.ID, r.Kind, r.Text, r.Subject)
		}
		b.WriteString("\n")
	}
	if _, err := exec.LookPath("entire"); err == nil {
		cmd := exec.CommandContext(ctx, "entire", "graph", "query", task, "--repo", dir)
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			b.WriteString("# Code map (entire-graph)\n\n")
			s := string(out)
			if len(s) > 12000 {
				s = s[:12000] + "\n…[truncated]"
			}
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "Nothing in memory matches this task.", nil
	}
	return b.String(), nil
}
