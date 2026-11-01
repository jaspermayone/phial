package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jaspermayone/phial/internal/store"
)

// ClaudeProjectsDir is where Claude Code keeps transcripts.
func ClaudeProjectsDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// Transcript is one Claude Code session file.
type Transcript struct {
	Path    string
	Session store.Session
	Size    int64
}

// line is the subset of a Claude Code JSONL record phial reads.
type line struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"sessionId"`
	CWD         string          `json:"cwd"`
	Timestamp   string          `json:"timestamp"`
	IsMeta      bool            `json:"isMeta"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseTranscript reads a Claude Code transcript into a session. Only the
// user's own prompts and the assistant's text are kept: tool results,
// images, thinking, meta lines and subagent side chains are dropped. A
// file with no user prompt gives nil.
func ParseTranscript(path string) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, _ := f.Stat()
	se := store.Session{Source: "claude-code"}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	idx := 0
	var first, last time.Time
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		if l.Type != "user" && l.Type != "assistant" {
			continue
		}
		if l.IsMeta || l.IsSidechain {
			continue
		}
		if se.ID == "" && l.SessionID != "" {
			se.ID = "claude-code:" + l.SessionID
		}
		if se.Path == "" && l.CWD != "" {
			se.Path = l.CWD
		}
		if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
			if first.IsZero() {
				first = t
			}
			last = t
		}
		var m message
		if err := json.Unmarshal(l.Message, &m); err != nil {
			continue
		}
		text := contentText(m.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if l.Type == "user" && (strings.HasPrefix(text, "<command-name>") || strings.HasPrefix(text, "<local-command")) {
			continue // slash-command plumbing, not the user's words
		}
		se.Turns = append(se.Turns, store.Turn{Index: idx, Role: m.Role, Text: text})
		idx++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	users := 0
	for _, t := range se.Turns {
		if t.Role == "user" {
			users++
		}
	}
	if se.ID == "" || users == 0 {
		return nil, nil
	}
	se.StartedAt, se.EndedAt = first, last
	if se.Path != "" {
		se.Repo = gitRemote(se.Path)
		se.Commit, se.CheckpointID = checkpointFor(se.Path, first, last)
	}
	return &Transcript{Path: path, Session: se, Size: st.Size()}, nil
}

// contentText flattens a message's content: a string, or blocks of which
// only text blocks count.
func contentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func git(dir string, args ...string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		return "", err
	}
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func gitRemote(dir string) string {
	r, _ := git(dir, "remote", "get-url", "origin")
	return r
}

// GitRemote is the origin remote of a checkout, or "".
func GitRemote(dir string) string { return gitRemote(dir) }

// checkpointFor finds the last commit made in dir during the session, and
// its Entire-Checkpoint trailer if it has one. Entire writes that trailer
// on every commit it links to a checkpoint, so the session gets the link
// without reading Entire's own storage.
func checkpointFor(dir string, from, to time.Time) (commit, checkpoint string) {
	if from.IsZero() {
		return "", ""
	}
	if to.IsZero() {
		to = from
	}
	out, err := git(dir, "log", "--all", "--since="+from.Add(-time.Minute).Format(time.RFC3339), "--until="+to.Add(2*time.Hour).Format(time.RFC3339),
		"--format=%H%x00%(trailers:key=Entire-Checkpoint,valueonly,separator=%x2C)", "-n", "50")
	if err != nil || out == "" {
		return "", ""
	}
	for _, ln := range strings.Split(out, "\n") {
		sha, trailer, _ := strings.Cut(ln, "\x00")
		trailer = strings.TrimSpace(trailer)
		if trailer != "" {
			return sha, strings.Split(trailer, ",")[0]
		}
		if commit == "" {
			commit = sha
		}
	}
	return commit, ""
}

// FindTranscripts lists Claude Code transcripts, newest first. With roots
// set, only sessions whose cwd is under a root are kept.
func FindTranscripts(roots []string) ([]string, error) {
	dir := ClaudeProjectsDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(dir, e.Name(), "*.jsonl"))
		files = append(files, matches...)
	}
	sort.Slice(files, func(i, j int) bool {
		a, _ := os.Stat(files[i])
		b, _ := os.Stat(files[j])
		if a == nil || b == nil {
			return files[i] < files[j]
		}
		return a.ModTime().After(b.ModTime())
	})
	return files, nil
}

// underRoots reports whether path is inside one of roots (all if none).
func underRoots(path string, roots []string) bool {
	if len(roots) == 0 {
		return true
	}
	for _, r := range roots {
		r = filepath.Clean(expandHome(r))
		if path == r || strings.HasPrefix(path, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// SyncReport counts one sync.
type SyncReport struct {
	Files    int
	Sessions int
	New      int
	Skipped  int
	Brain    int
}

// Sync uploads every transcript that grew since the last sync, in batches.
// With all set, every transcript is read again (the server ignores sessions
// it already has).
func Sync(ctx context.Context, cfg *Config, api *API, all bool, log func(string, ...any)) (SyncReport, error) {
	var rep SyncReport
	files, err := FindTranscripts(cfg.Roots)
	if err != nil {
		return rep, err
	}
	var batch []store.Session
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := api.Ingest(ctx, batch)
		if err != nil {
			return err
		}
		rep.New += n
		batch = batch[:0]
		return cfg.Save()
	}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		rep.Files++
		if !all && cfg.Synced[f] >= st.Size() {
			rep.Skipped++
			continue
		}
		tr, err := ParseTranscript(f)
		if err != nil {
			log("skip %s: %v", filepath.Base(f), err)
			continue
		}
		cfg.Synced[f] = st.Size()
		if tr == nil || !underRoots(tr.Session.Path, cfg.Roots) {
			rep.Skipped++
			continue
		}
		rep.Sessions++
		batch = append(batch, tr.Session)
		if len(batch) >= 20 {
			if err := flush(); err != nil {
				return rep, err
			}
		}
	}
	if err := flush(); err != nil {
		return rep, err
	}
	n, err := SyncBrain(ctx, cfg, api, log)
	rep.Brain = n
	if err != nil {
		log("entire-brain: %v", err)
	}
	return rep, cfg.Save()
}
