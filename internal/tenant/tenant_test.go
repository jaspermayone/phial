package tenant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jaspermayone/phial/internal/rank"
	"github.com/jaspermayone/phial/internal/store"
)

func manager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(t.TempDir(), rank.Default(), []Pool{
		{Name: "crane", Members: []string{"jsp", "teammate"}, Readers: []string{"guest"}, Subjects: []string{"areas/crane"}},
	}, []string{"jsp", "teammate", "guest"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestAllowList(t *testing.T) {
	m := manager(t)
	if _, err := m.View("stranger"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("stranger got a view: %v", err)
	}
	if _, err := m.View("JSP/"); err != nil {
		t.Fatalf("identity spelling should normalize: %v", err)
	}
	ids, _ := m.Identities()
	if len(ids) != 1 || ids[0] != "jsp" {
		t.Fatalf("identities = %v", ids)
	}
	if _, err := os.Stat(filepath.Join(m.UserDir("jsp"), "phial.db")); err != nil {
		t.Fatal("no per-user database")
	}
}

func TestPoolRoutingAndAttribution(t *testing.T) {
	m := manager(t)
	ctx := context.Background()
	jsp, _ := m.View("jsp")
	teammate, _ := m.View("teammate")
	guest, _ := m.View("guest")

	shared, _, err := jsp.Remember(ctx, store.RememberInput{Subject: "areas/crane/ui", Text: "the toolbar is in crates/crane-ui"})
	if err != nil {
		t.Fatal(err)
	}
	private, _, _ := jsp.Remember(ctx, store.RememberInput{Subject: "areas/infra", Text: "alastor is the VPS"})
	if shared.Pool != "crane" || private.Pool != "" || shared.CreatedBy != "jsp" {
		t.Fatalf("routing: shared=%q private=%q by=%q", shared.Pool, private.Pool, shared.CreatedBy)
	}

	// teammate sees the shared phial but not jsp's private one.
	got, _ := teammate.Recall(ctx, store.Query{Text: "toolbar alastor"})
	if len(got) != 1 || got[0].ID != shared.ID || got[0].Pool != "crane" {
		t.Fatalf("teammate sees %v", got)
	}
	// teammate edits it; the ledger says so.
	nw, err := teammate.Supersede(ctx, store.SupersedeInput{ID: shared.ID, Text: "the toolbar moved to crates/crane-shell"})
	if err != nil || nw.CreatedBy != "teammate" {
		t.Fatalf("teammate supersede: %v %+v", err, nw)
	}
	es, _ := jsp.Ledger(ctx, "", "areas/crane", "", 0)
	if len(es) != 2 || es[0].Actor != "teammate" || es[0].Pool != "crane" || es[1].Actor != "jsp" {
		t.Fatalf("ledger = %+v", es)
	}
	// A reader cannot write.
	if _, _, err := guest.Remember(ctx, store.RememberInput{Subject: "areas/crane", Text: "x"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("guest write: %v", err)
	}
	if _, err := guest.Confirm(ctx, nw.ID); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("guest confirm: %v", err)
	}
	if rs, _ := guest.Recall(ctx, store.Query{Text: "toolbar"}); len(rs) != 1 {
		t.Fatal("guest cannot read the pool")
	}
	// A pool subject listed for jsp carries the pool name.
	subs, _ := jsp.ListSubjects(ctx)
	seen := map[string]string{}
	for _, s := range subs {
		seen[s.Path] = s.Pool
	}
	if seen["areas/crane/ui"] != "crane" || seen["areas/infra"] != "" {
		t.Fatalf("subjects = %v", seen)
	}
	st, _ := jsp.Stats(ctx)
	if st.Active != 2 || st.Superseded != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestSlug(t *testing.T) {
	a, b := Slug("https://me.example/"), Slug("https://ME.example")
	if a != b {
		t.Fatalf("spellings differ: %s %s", a, b)
	}
	if Slug("jsp") == Slug("jsp2") {
		t.Fatal("different identities collide")
	}
}
