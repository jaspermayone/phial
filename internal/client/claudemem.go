package client

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jaspermayone/phial/internal/store"
)

// MemorySource is the session source for Claude Code memory notes. The
// server's extraction reads them as notes the agent saved, not as the
// user's words.
const MemorySource = "claude-memory"

// memoryChunk is the most text one memory session carries. A project with
// more changed notes than this sends several sessions.
const memoryChunk = 60_000

// memoryNote is one changed memory file.
type memoryNote struct {
	path, name, text, hash string
}

// MemoryDirs lists the Claude Code memory directories:
// <projects>/<project>/memory.
func MemoryDirs() []string {
	dirs, _ := filepath.Glob(filepath.Join(ClaudeProjectsDir(), "*", "memory"))
	sort.Strings(dirs)
	return dirs
}

// changedNotes reads the notes in dir that are new or changed since the
// last sync. MEMORY.md is only an index of the other files, so it is
// skipped.
func changedNotes(dir string, seen map[string]string) []memoryNote {
	files, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	sort.Strings(files)
	var out []memoryNote
	for _, f := range files {
		if filepath.Base(f) == "MEMORY.md" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			continue
		}
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		if seen[f] == h {
			continue
		}
		out = append(out, memoryNote{path: f, name: filepath.Base(f), text: strings.TrimSpace(string(b)), hash: h})
	}
	return out
}

// projectCWD finds the working directory of a Claude Code project
// directory from the cwd field of its transcripts. The directory name is
// a lossy encoding of the path, so it is not decoded.
func projectCWD(projectDir string) string {
	files, _ := filepath.Glob(filepath.Join(projectDir, "*.jsonl"))
	for _, f := range files {
		if cwd := firstCWD(f); cwd != "" {
			return cwd
		}
	}
	return ""
}

func firstCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for i := 0; i < 50 && sc.Scan(); i++ {
		var l struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.CWD != "" {
			return l.CWD
		}
	}
	return ""
}

// MemorySessions groups notes into sessions of at most memoryChunk
// characters. Each session's ID is the hash of its text, so the server
// stores each version once.
func MemorySessions(notes []memoryNote, cwd, repo string, now time.Time) []store.Session {
	var out []store.Session
	var b strings.Builder
	emit := func() {
		if b.Len() == 0 {
			return
		}
		text := b.String()
		sum := sha256.Sum256([]byte(text))
		h := hex.EncodeToString(sum[:])
		out = append(out, store.Session{
			ID:        MemorySource + ":" + h[:20],
			Source:    MemorySource,
			Path:      cwd,
			Repo:      repo,
			Turns:     []store.Turn{{Index: 0, Role: "user", Text: text}},
			StartedAt: now,
			EndedAt:   now,
		})
		b.Reset()
	}
	for _, n := range notes {
		part := "### " + n.name + "\n\n" + n.text + "\n\n"
		if b.Len() > 0 && b.Len()+len(part) > memoryChunk {
			emit()
		}
		b.WriteString(part)
	}
	emit()
	return out
}

// SyncClaudeMemory sends new and changed Claude Code memory notes to phial
// for extraction, one project at a time. It returns the number of notes
// sent.
func SyncClaudeMemory(ctx context.Context, cfg *Config, api *API, log func(string, ...any)) (int, error) {
	sent := 0
	now := time.Now()
	for _, dir := range MemoryDirs() {
		notes := changedNotes(dir, cfg.MemorySeen)
		if len(notes) == 0 {
			continue
		}
		cwd := projectCWD(filepath.Dir(dir))
		repo := ""
		if cwd != "" {
			repo = gitRemote(cwd)
		}
		if _, err := api.Ingest(ctx, MemorySessions(notes, cwd, repo, now)); err != nil {
			return sent, err
		}
		for _, n := range notes {
			cfg.MemorySeen[n.path] = n.hash
		}
		sent += len(notes)
		if err := cfg.Save(); err != nil {
			return sent, err
		}
	}
	for p := range cfg.MemorySeen {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			delete(cfg.MemorySeen, p)
		}
	}
	return sent, nil
}
