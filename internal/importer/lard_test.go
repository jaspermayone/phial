package importer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaspermayone/phial/internal/rank"
	"github.com/jaspermayone/phial/internal/store"
)

const infraFile = `---
name: infra
kind: area
description: jaspermayone/infra Nix flake
aliases: [infra-repo, nix flake]
repos: [github.com/jaspermayone/infra]
updated: 2026-10-30T21:28:38Z
---

- [stated] Uses agenix for secrets
- [stated 2026-09-11] Forks go in the jaspermayone-forks org,
  never the personal account
- 2026-10-14: Hosted VLAN 75 holds other people's devices

## Build

- linux-builder needs 8GB
`

func TestParseFile(t *testing.T) {
	sub := ParseFile("areas/infra.md", infraFile)
	if sub.Path != "areas/infra" || sub.Description != "jaspermayone/infra Nix flake" ||
		len(sub.Aliases) != 2 || sub.Repos[0] != "github.com/jaspermayone/infra" || sub.Updated.IsZero() {
		t.Fatalf("subject = %+v", sub)
	}
	items := ParseBody(sub.Body)
	if len(items) != 4 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Tag != "stated" || items[0].Text != "Uses agenix for secrets" {
		t.Errorf("item 0 = %+v", items[0])
	}
	if items[1].Text != "Forks go in the jaspermayone-forks org, never the personal account" ||
		!items[1].Date.Equal(time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("item 1 = %+v", items[1])
	}
	if items[2].Date.IsZero() || items[2].Tag != "" {
		t.Errorf("item 2 = %+v", items[2])
	}
	if items[3].Heading != "Build" {
		t.Errorf("item 3 = %+v", items[3])
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "phial.db"), rank.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestImportIsIdempotentAndKeepsDates(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	subs := []LardSubject{ParseFile("areas/infra.md", infraFile), {Path: "areas/Bad Name", Body: "- x"}}

	dry, err := Import(ctx, st, subs, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Items != 5 || dry.Created != 0 || dry.Failed != 1 {
		t.Fatalf("dry run = %+v", dry)
	}
	if s, _ := st.Stats(ctx); s.Active != 0 {
		t.Fatal("dry run wrote phials")
	}

	rep, err := Import(ctx, st, subs, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 4 || rep.Failed != 1 {
		t.Fatalf("import = %+v", rep)
	}
	again, _ := Import(ctx, st, subs, false)
	if again.Created != 0 || again.Existing != 4 {
		t.Fatalf("second import = %+v", again)
	}

	rs, err := st.Recall(ctx, store.Query{Text: "forks org"})
	if err != nil || len(rs) == 0 {
		t.Fatalf("recall: %v %v", rs, err)
	}
	p := rs[0]
	if p.Origin != "imported" || !p.CreatedAt.Equal(time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)) ||
		!p.ConfirmedAt.Equal(p.CreatedAt) || p.Provenance[0].Source != "lard:areas/infra [stated]" {
		t.Fatalf("imported phial = %+v", p.Phial)
	}
	build, _ := st.Recall(ctx, store.Query{PathPrefix: "build"})
	if len(build) != 1 {
		t.Fatalf("heading did not become a path: %d", len(build))
	}
	if sub, _ := st.ResolveProject(ctx, store.ProjectHints{GitRemote: "git@github.com:jaspermayone/infra.git"}); sub != "areas/infra" {
		t.Fatalf("repos not linked: %q", sub)
	}
	// A dry run now counts what is already there.
	if dry2, _ := Import(ctx, st, subs, true); dry2.Existing != 4 || dry2.Created != 0 {
		t.Fatalf("dry run after import = %+v", dry2)
	}
	// A superseded import is not brought back by a re-run.
	if _, err := st.Supersede(ctx, store.SupersedeInput{ID: p.ID, Text: "Forks go in jaspermayone-forks"}); err != nil {
		t.Fatal(err)
	}
	if third, _ := Import(ctx, st, subs, false); third.Created != 0 {
		t.Fatalf("re-import resurrected a superseded phial: %+v", third)
	}
	// Nor is a purged one.
	agenix, err := st.Recall(ctx, store.Query{Text: "agenix"})
	if err != nil || len(agenix) != 1 {
		t.Fatalf("recall agenix: %v %v", agenix, err)
	}
	if err := st.Purge(ctx, agenix[0].ID, "test"); err != nil {
		t.Fatal(err)
	}
	if fourth, _ := Import(ctx, st, subs, false); fourth.Created != 0 {
		t.Fatalf("re-import resurrected a purged phial: %+v", fourth)
	}
}

func TestReadDirAcceptsBackupTenant(t *testing.T) {
	root := t.TempDir()
	mem := filepath.Join(root, "memory")
	os.MkdirAll(filepath.Join(mem, "areas", "homelab"), 0o755)
	os.WriteFile(filepath.Join(mem, "profile.md"), []byte("- Name: Jasper\n"), 0o644)
	os.WriteFile(filepath.Join(mem, "areas", "homelab", "netbox.md"), []byte("- runs on ollivander\n"), 0o644)
	os.WriteFile(filepath.Join(mem, "notes.md"), []byte("- not a subject\n"), 0o644)
	subs, err := ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 || subs[0].Path != "areas/homelab/netbox" || subs[1].Path != "profile" {
		t.Fatalf("subjects = %+v", subs)
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "no", 401)
			return
		}
		switch {
		case r.URL.Path == "/memory":
			json.NewEncoder(w).Encode([]map[string]string{{"path": "areas/infra.md"}})
		case r.URL.Path == "/memory/areas/infra" && r.URL.Query().Get("format") == "json":
			json.NewEncoder(w).Encode(map[string]any{"description": "d", "repos": []string{"github.com/a/b"}, "body": "- one\n- two"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	subs, err := Fetch(context.Background(), srv.URL+"/", "secret-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Path != "areas/infra" || len(ParseBody(subs[0].Body)) != 2 {
		t.Fatalf("subs = %+v", subs)
	}
	if _, err := Fetch(context.Background(), srv.URL, "wrong", nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token: err = %v", err)
	}
}
