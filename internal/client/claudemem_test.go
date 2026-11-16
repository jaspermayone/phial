package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChangedNotesSkipsIndexAndSeen(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("MEMORY.md", "- [A](a.md) — index")
	a := write("a.md", "---\nname: a\n---\nfact a")
	write("b.md", "---\nname: b\n---\nfact b")
	write("empty.md", "  \n")

	notes := changedNotes(dir, map[string]string{})
	if len(notes) != 2 || notes[0].name != "a.md" || notes[1].name != "b.md" {
		t.Fatalf("want a.md and b.md, got %+v", notes)
	}
	seen := map[string]string{a: notes[0].hash}
	if got := changedNotes(dir, seen); len(got) != 1 || got[0].name != "b.md" {
		t.Fatalf("a seen note was sent again: %+v", got)
	}
	write("a.md", "---\nname: a\n---\nfact a, edited")
	if got := changedNotes(dir, seen); len(got) != 2 {
		t.Fatalf("an edited note was not sent: %+v", got)
	}
}

func TestMemorySessionsChunkAndStableIDs(t *testing.T) {
	big := strings.Repeat("x", memoryChunk/2+1)
	notes := []memoryNote{{name: "a.md", text: big}, {name: "b.md", text: big}, {name: "c.md", text: "small"}}
	now := time.Now()
	ses := MemorySessions(notes, "/w", "git@example:r", now)
	if len(ses) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(ses))
	}
	if !strings.Contains(ses[1].Turns[0].Text, "### c.md") || ses[0].Source != MemorySource || ses[0].Repo != "git@example:r" {
		t.Fatalf("bad sessions: %+v", ses[1])
	}
	again := MemorySessions(notes, "/w", "git@example:r", now.Add(time.Hour))
	if again[0].ID != ses[0].ID || ses[0].ID == ses[1].ID {
		t.Fatalf("ids not stable per text: %s %s %s", ses[0].ID, again[0].ID, ses[1].ID)
	}
}
