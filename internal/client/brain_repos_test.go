package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindRepos(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(append([]string{root}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("top", ".git")                         // depth 1
	mk("projects", "phial", ".git")           // depth 2
	mk("school", "fall", "cs101", ".git")     // depth 3
	mk("deep", "a", "b", "c", ".git")         // depth 4: too deep
	mk(".hidden", "repo", ".git")             // hidden: skipped
	mk("node_modules", "pkg", ".git")         // skipped
	mk("projects", "phial", "vendor", ".git") // below a repo: not entered
	mk("plain")                               // no repo
	// A worktree checkout has a .git file, not a directory.
	mk("wt")
	if err := os.WriteFile(filepath.Join(root, "wt", ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := findRepos(root, repoDepth)
	want := []string{
		filepath.Join(root, "projects", "phial"),
		filepath.Join(root, "school", "fall", "cs101"),
		filepath.Join(root, "top"),
		filepath.Join(root, "wt"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
