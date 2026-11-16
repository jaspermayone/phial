package extract

import (
	"strings"
	"testing"

	"github.com/jaspermayone/phial/internal/client"
	"github.com/jaspermayone/phial/internal/store"
)

func TestBriefSourceMatchesClient(t *testing.T) {
	if BriefSource != client.GranolaSource {
		t.Fatalf("extract.BriefSource %q != client.GranolaSource %q", BriefSource, client.GranolaSource)
	}
}

func TestBuildPromptBrief(t *testing.T) {
	se := store.Session{ID: "granola-brief:x", Source: BriefSource,
		Turns: []store.Turn{{Role: "user", Text: "## Role and scope\nRuns DxC support."}}}
	got := buildPrompt([]store.Subject{{Path: "profile"}}, se)
	if !strings.Contains(got, "SOURCE: GRANOLA AGENT BRIEF") || !strings.Contains(got, "Runs DxC support.") {
		t.Fatalf("brief prompt missing source marker or text:\n%s", got)
	}
	if strings.Contains(got, "USER TURNS") || strings.Contains(got, "[turn 0]") {
		t.Fatalf("brief prompt framed as a coding session:\n%s", got)
	}
}

func TestMemorySourceMatchesClient(t *testing.T) {
	if MemorySource != client.MemorySource {
		t.Fatalf("extract.MemorySource %q != client.MemorySource %q", MemorySource, client.MemorySource)
	}
}

func TestBuildPromptMemory(t *testing.T) {
	se := store.Session{ID: "claude-memory:x", Source: MemorySource, Repo: "https://github.com/x/y",
		Turns: []store.Turn{{Role: "user", Text: "### a.md\n\nUse pnpm."}}}
	got := buildPrompt(nil, se)
	if !strings.Contains(got, "SOURCE: CLAUDE CODE MEMORY NOTES") || !strings.Contains(got, "Use pnpm.") || !strings.Contains(got, "github.com/x/y") {
		t.Fatalf("memory prompt missing marker, text or repo:\n%s", got)
	}
	if strings.Contains(got, "GRANOLA") || strings.Contains(got, "USER TURNS") {
		t.Fatalf("memory prompt framed as another source:\n%s", got)
	}
}
