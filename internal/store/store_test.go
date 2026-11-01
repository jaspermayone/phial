package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaspermayone/phial/internal/rank"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)} }

// must panics on error; a panic fails the test with a stack trace.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func open(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "phial.db"), rank.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := newClock()
	s.Now = c.now
	return s, c
}

func remember(t *testing.T, s *Store, in RememberInput) *Phial {
	t.Helper()
	p, _, err := s.Remember(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenTwiceKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phial.db")
	s := must(Open(path, rank.Default()))
	ctx := context.Background()
	if _, _, err := s.Remember(ctx, RememberInput{Subject: "profile", Text: "Name: Jasper"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = must(Open(path, rank.Default()))
	defer s.Close()
	st := must(s.Stats(ctx))
	if st.Active != 1 || st.Subjects != 1 {
		t.Fatalf("stats after reopen = %+v", st)
	}
}

func TestRememberValidates(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	cases := []RememberInput{
		{Subject: "nope/x", Text: "a"},
		{Subject: "areas/Bad Name", Text: "a"},
		{Subject: "people/a/b", Text: "a"},
		{Subject: "areas/x", Text: "  "},
		{Subject: "areas/x", Text: "a", Kind: "rumour"},
		{Subject: "areas/x", Text: "a", Paths: []string{"Deploy Target"}},
	}
	for _, in := range cases {
		if _, _, err := s.Remember(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("Remember(%+v) err = %v, want ErrInvalid", in, err)
		}
	}
}

func TestRememberDedupesAndConfirms(t *testing.T) {
	s, c := open(t)
	ctx := context.Background()
	first, created, err := s.Remember(ctx, RememberInput{Subject: "areas/phial", Text: "Deploys to  alastor.", Paths: []string{"deploy.target"}})
	if err != nil || !created {
		t.Fatalf("first remember: created=%v err=%v", created, err)
	}
	c.add(48 * time.Hour)
	again, created, err := s.Remember(ctx, RememberInput{Subject: "areas/phial", Text: "Deploys to alastor.", Provenance: Provenance{SessionID: "s2"}})
	if err != nil || created {
		t.Fatalf("second remember: created=%v err=%v", created, err)
	}
	if again.ID != first.ID {
		t.Fatalf("got a new phial %s, want %s", again.ID, first.ID)
	}
	if !again.ConfirmedAt.Equal(c.now()) {
		t.Fatalf("confirmed_at = %v, want %v", again.ConfirmedAt, c.now())
	}
	if len(again.Provenance) != 1 || again.Provenance[0].SessionID != "s2" {
		t.Fatalf("provenance = %+v", again.Provenance)
	}
	// The same text in another subject is a different phial.
	other := remember(t, s, RememberInput{Subject: "areas/lard", Text: "Deploys to alastor."})
	if other.ID == first.ID {
		t.Fatal("dedupe crossed subjects")
	}
}

func TestSupersedeRetractConfirm(t *testing.T) {
	s, c := open(t)
	ctx := context.Background()
	old := remember(t, s, RememberInput{Subject: "areas/phial", Kind: "decision", Text: "Retain for 30 days.", Paths: []string{"retention"}})
	c.add(time.Hour)
	nw := must(s.Supersede(ctx, SupersedeInput{ID: old.ID, Text: "Keep every phial forever."}))
	if nw.Supersedes != old.ID || nw.Kind != "decision" || len(nw.Paths) != 1 || nw.Paths[0] != "retention" {
		t.Fatalf("new phial = %+v", nw)
	}
	gone := must(s.Get(ctx, old.ID))
	if gone.Status != StatusSuperseded || gone.SupersededBy != nw.ID {
		t.Fatalf("old phial = %+v", gone)
	}
	if _, err := s.Supersede(ctx, SupersedeInput{ID: old.ID, Text: "x"}); !errors.Is(err, ErrNotActive) {
		t.Fatalf("supersede of superseded: err = %v", err)
	}
	hist := must(s.History(ctx, old.ID))
	if len(hist) != 2 || hist[0].ID != old.ID || hist[1].ID != nw.ID {
		t.Fatalf("history = %v", ids(hist))
	}

	if _, err := s.Retract(ctx, nw.ID, " ", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retract without reason: err = %v", err)
	}
	r := must(s.Retract(ctx, nw.ID, "changed my mind", ""))
	if r.Status != StatusRetracted || r.RetractReason != "changed my mind" {
		t.Fatalf("retracted = %+v", r)
	}
	if _, err := s.Confirm(ctx, nw.ID, ""); !errors.Is(err, ErrNotActive) {
		t.Fatalf("confirm retracted: err = %v", err)
	}
	if _, err := s.Confirm(ctx, "ph_missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("confirm missing: err = %v", err)
	}
}

func TestRecallHidesInactiveUnlessAll(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	p := remember(t, s, RememberInput{Subject: "areas/phial", Text: "uses sqlite fts5"})
	must(s.Retract(ctx, p.ID, "wrong", ""))
	if got := must(s.Recall(ctx, Query{Text: "sqlite"})); len(got) != 0 {
		t.Fatalf("default recall returned retracted phial")
	}
	if got := must(s.Recall(ctx, Query{Text: "sqlite", Status: "all"})); len(got) != 1 {
		t.Fatalf("recall all = %d results", len(got))
	}
}

func TestRecallFilters(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := remember(t, s, RememberInput{Subject: "areas/homelab/netbox", Kind: "gotcha", Text: "netbox diode needs a token", Paths: []string{"diode.auth"}})
	b := remember(t, s, RememberInput{Subject: "areas/homelab", Text: "homelab uses proxmox", Paths: []string{"hypervisor"},
		Loci: []Locus{{File: "hosts/pve.nix", Symbol: "pve01"}}})
	c := remember(t, s, RememberInput{Subject: "topics/preferences", Kind: "preference", Text: "no em dashes in prose"})

	check := func(name string, q Query, want ...*Phial) {
		t.Helper()
		got := must(s.Recall(ctx, q))
		gotIDs := map[string]bool{}
		for _, r := range got {
			gotIDs[r.ID] = true
		}
		if len(got) != len(want) {
			t.Errorf("%s: got %d results %v, want %d", name, len(got), gotIDs, len(want))
			return
		}
		for _, w := range want {
			if !gotIDs[w.ID] {
				t.Errorf("%s: missing %s", name, w.ID)
			}
		}
	}
	check("subject prefix", Query{Subject: "areas/homelab"}, a, b)
	check("subject exact", Query{Subject: "areas/homelab/netbox"}, a)
	check("kind", Query{Kind: "gotcha"}, a)
	check("path prefix", Query{PathPrefix: "diode"}, a)
	check("path no partial word", Query{PathPrefix: "dio"})
	check("scope global", Query{Scope: "global"}, c)
	check("scope area", Query{Scope: "area"}, a, b)
	check("locus file", Query{Locus: "hosts/pve.nix"}, b)
	check("locus symbol", Query{Locus: "#pve01"}, b)
	check("text", Query{Text: "proxmox"}, b)
	check("text stemmed", Query{Text: "token"}, a)
	check("text with fts syntax is safe", Query{Text: `"dashes" AND (NOT x) *`}, c)
}

func TestRecallRanksFreshAndTrustedHigher(t *testing.T) {
	s, c := open(t)
	ctx := context.Background()
	old := remember(t, s, RememberInput{Subject: "areas/phial", Text: "port is 7480"})
	c.add(400 * 24 * time.Hour)
	fresh := remember(t, s, RememberInput{Subject: "areas/phial", Text: "port is 7481"})
	got := must(s.Recall(ctx, Query{Text: "port"}))
	if len(got) != 2 || got[0].ID != fresh.ID {
		t.Fatalf("fresh phial should rank first, got %v", resultIDs(got))
	}
	if got[1].Rank.Freshness != 0.3 {
		t.Fatalf("old fact freshness = %v, want the 0.3 floor", got[1].Rank.Freshness)
	}

	// Confirming the old phial resets its age.
	must(s.Confirm(ctx, old.ID, ""))
	must(s.SetVerify(ctx, fresh.ID, "stale", "symbol gone", ""))
	got = must(s.Recall(ctx, Query{Text: "port"}))
	if got[0].ID != old.ID {
		t.Fatalf("confirmed phial should beat a stale one, got %v", resultIDs(got))
	}
	if got[1].Rank.Trust != 0.3 {
		t.Fatalf("stale trust = %v", got[1].Rank.Trust)
	}

	low := must(s.Recall(ctx, Query{Lowest: true}))
	if low[0].ID != fresh.ID {
		t.Fatalf("Lowest should list the stale phial first, got %v", resultIDs(low))
	}
}

func TestProfileNeverAges(t *testing.T) {
	s, c := open(t)
	p := remember(t, s, RememberInput{Subject: "profile", Text: "Name: Jasper"})
	c.add(10 * 365 * 24 * time.Hour)
	if f := s.Score(must(s.Get(context.Background(), p.ID))).Freshness; f != 1 {
		t.Fatalf("profile freshness after 10 years = %v", f)
	}
}

func TestPurge(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	p := remember(t, s, RememberInput{Subject: "areas/phial", Text: "the token is hunter2", Paths: []string{"secret"}})
	n := must(s.Supersede(ctx, SupersedeInput{ID: p.ID, Text: "the token lives in agenix"}))
	if err := s.Purge(ctx, p.ID, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("purge without reason: %v", err)
	}
	if err := s.Purge(ctx, p.ID, "held a secret", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged phial still readable: %v", err)
	}
	if got := must(s.Recall(ctx, Query{Text: "hunter2", Status: "all"})); len(got) != 0 {
		t.Fatal("purged text still in the full-text index")
	}
	if np := must(s.Get(ctx, n.ID)); np.Supersedes != "" {
		t.Fatalf("successor still points at purged phial: %q", np.Supersedes)
	}
	var reason string
	var count int
	if err := s.db.QueryRow(`SELECT reason, (SELECT COUNT(*) FROM purge_log) FROM purge_log WHERE phial_id=?`, p.ID).Scan(&reason, &count); err != nil {
		t.Fatal(err)
	}
	if reason != "held a secret" || count != 1 {
		t.Fatalf("purge log = %q, %d rows", reason, count)
	}
	if err := s.Purge(ctx, p.ID, "again", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second purge: %v", err)
	}
}

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:Jaspermayone/Phial.git":           "github.com/jaspermayone/phial",
		"https://github.com/jaspermayone/phial":           "github.com/jaspermayone/phial",
		"https://user@github.com/jaspermayone/phial.git/": "github.com/jaspermayone/phial",
		"ssh://git@tangled.org:22/dunkirk.sh/lard":        "tangled.org/dunkirk.sh/lard",
		"github.com/a/b": "github.com/a/b",
		"":               "",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveProject(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	must(s.EnsureSubject(ctx, "areas/phial", "the memory server", []string{"lard replacement"}))
	must(s.EnsureSubject(ctx, "areas/crane", "", []string{"crane-browser"}))

	sub := must(s.ResolveProject(ctx, ProjectHints{GitRemote: "git@github.com:jaspermayone/phial.git"}))
	if sub != "areas/phial" {
		t.Fatalf("by remote name: %q", sub)
	}
	// The remote is now linked, so a renamed checkout still resolves.
	sub = must(s.ResolveProject(ctx, ProjectHints{GitRemote: "https://github.com/jaspermayone/phial", Path: "/x/other"}))
	if sub != "areas/phial" {
		t.Fatalf("by linked remote: %q", sub)
	}
	sub = must(s.ResolveProject(ctx, ProjectHints{Path: "/Users/jsp/dev/projects/crane-browser"}))
	if sub != "areas/crane" {
		t.Fatalf("by alias: %q", sub)
	}
	sub = must(s.ResolveProject(ctx, ProjectHints{Path: "/tmp/unknown"}))
	if sub != "" {
		t.Fatalf("unknown project resolved to %q", sub)
	}
}

func TestRememberLinksRepoToArea(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	remember(t, s, RememberInput{Subject: "areas/dots", Text: "nix dotfiles", Provenance: Provenance{Repo: "git@github.com:jaspermayone/dots.git"}})
	remember(t, s, RememberInput{Subject: "topics/git", Text: "use ssh", Provenance: Provenance{Repo: "github.com/jaspermayone/other"}})
	sub := must(s.ResolveProject(ctx, ProjectHints{GitRemote: "https://github.com/jaspermayone/dots"}))
	if sub != "areas/dots" {
		t.Fatalf("resolved %q", sub)
	}
	if r := must(s.Remotes(ctx, "topics/git")); len(r) != 0 {
		t.Fatalf("a topic got a remote: %v", r)
	}
}

func ids(ps []*Phial) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func resultIDs(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func TestPurgeLeavesNoTextOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "phial.db")
	s := must(Open(path, rank.Default()))
	ctx := context.Background()
	const secret = "SECRETzqxv9876API"
	p := remember(t, s, RememberInput{Subject: "areas/x", Text: "the key is " + secret})
	for i := 0; i < 50; i++ { // more rows, so the FTS index has segments
		remember(t, s, RememberInput{Subject: "areas/x", Text: fmt.Sprintf("filler phial number %d", i)})
	}
	if err := s.Purge(ctx, p.ID, "held a secret", ""); err != nil {
		t.Fatal(err)
	}
	if ok := must(s.WasPurged(ctx, "areas/x", "the key is "+secret)); !ok {
		t.Fatal("purge hash not recorded")
	}
	s.Close()
	files, _ := filepath.Glob(path + "*")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(bytes.ToLower(b), bytes.ToLower([]byte(secret))) {
			t.Errorf("%s still holds the purged text", filepath.Base(f))
		}
	}
}

func TestRecallWithoutTextRanksEveryRow(t *testing.T) {
	s, c := open(t)
	ctx := context.Background()
	old := c.now().Add(-3 * 365 * 24 * time.Hour)
	for i := 0; i < 1100; i++ {
		remember(t, s, RememberInput{Subject: "areas/x", Text: fmt.Sprintf("old fact %d", i), CreatedAt: old})
	}
	fresh := remember(t, s, RememberInput{Subject: "areas/x", Text: "the newest fact"})
	got := must(s.Recall(ctx, Query{Subject: "areas/x", Limit: 5}))
	if len(got) == 0 || got[0].ID != fresh.ID {
		t.Fatalf("newest phial not ranked first among 1101: got %v", resultIDs(got))
	}
	all := must(s.Recall(ctx, Query{Subject: "areas/x", All: true}))
	if len(all) != 1101 {
		t.Fatalf("All returned %d, want 1101", len(all))
	}
}

func TestRecallExactSubject(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	parent := remember(t, s, RememberInput{Subject: "areas/homelab", Text: "parent fact"})
	remember(t, s, RememberInput{Subject: "areas/homelab/netbox", Text: "child fact"})
	got := must(s.Recall(ctx, Query{Subject: "areas/homelab", Exact: true, All: true}))
	if len(got) != 1 || got[0].ID != parent.ID {
		t.Fatalf("exact recall = %v", resultIDs(got))
	}
}

func TestLedgerRecordsEveryChange(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	p, _, err := s.Remember(ctx, RememberInput{Subject: "areas/crane", Text: "uses rust", Actor: "jsp"})
	if err != nil {
		t.Fatal(err)
	}
	if p.CreatedBy != "jsp" {
		t.Fatalf("created_by = %q", p.CreatedBy)
	}
	must(s.Confirm(ctx, p.ID, "teammate"))
	n := must(s.Supersede(ctx, SupersedeInput{ID: p.ID, Text: "uses rust and c++", Actor: "teammate", Note: "added c++"}))
	must(s.Retract(ctx, n.ID, "rewrite", "jsp"))
	es := must(s.Ledger(ctx, "", "areas/crane", "", 0))
	var actions []string
	for _, e := range es {
		actions = append(actions, e.Actor+":"+e.Action)
	}
	want := "jsp:retract teammate:supersede teammate:confirm jsp:remember"
	if got := strings.Join(actions, " "); got != want {
		t.Fatalf("ledger = %q, want %q", got, want)
	}
	if es[1].TextBefore != "uses rust" || es[1].TextAfter != "uses rust and c++" || es[1].Note != "added c++" {
		t.Fatalf("supersede entry = %+v", es[1])
	}
	if by := must(s.Ledger(ctx, "", "", "teammate", 0)); len(by) != 2 {
		t.Fatalf("teammate's entries = %d", len(by))
	}
}

func TestSessionsAndProposals(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	n := must(s.PutSessions(ctx, []Session{
		{ID: "claude-code:abc", Source: "claude-code", Repo: "git@github.com:a/b.git", Turns: []Turn{{Index: 0, Role: "user", Text: "hi"}}},
		{ID: "claude-code:abc", Source: "claude-code", Turns: []Turn{{Role: "user", Text: "dup"}}},
		{ID: "", Source: "x", Turns: []Turn{{Role: "user", Text: "no id"}}},
	}))
	if n != 1 {
		t.Fatalf("stored %d sessions, want 1", n)
	}
	pend := must(s.PendingSessions(ctx, 10))
	if len(pend) != 1 || pend[0].Repo != "github.com/a/b" || pend[0].Turns[0].Text != "hi" {
		t.Fatalf("pending = %+v", pend)
	}
	if err := s.MarkExtracted(ctx, "claude-code:abc", ""); err != nil {
		t.Fatal(err)
	}
	if left := must(s.PendingSessions(ctx, 10)); len(left) != 0 {
		t.Fatal("marked session still pending")
	}

	target := remember(t, s, RememberInput{Subject: "areas/b", Text: "deploys to alastor on port 7480", Paths: []string{"deploy"}})
	near, score := func() (*Phial, float64) {
		p, sc, err := s.Near(ctx, "areas/b", "deploys to alastor on port 7481 behind traefik")
		if err != nil {
			t.Fatal(err)
		}
		return p, sc
	}()
	if near == nil || near.ID != target.ID || score < 0.5 {
		t.Fatalf("near = %v score %v", near, score)
	}
	pr := must(s.Propose(ctx, Proposal{TargetID: target.ID, Text: "deploys to alastor on port 7481 behind traefik", Reason: "newer session", SessionID: "claude-code:abc", ProposedBy: "extract"}))
	if again := must(s.Propose(ctx, Proposal{TargetID: target.ID, Text: "deploys to alastor on port 7481 behind traefik"})); again.ID != pr.ID {
		t.Fatal("duplicate proposal made")
	}
	if ps := must(s.Proposals(ctx, "", 0)); len(ps) != 1 || ps[0].Target == nil {
		t.Fatalf("proposals = %+v", ps)
	}
	np := must(s.Resolve(ctx, pr.ID, true, "jsp"))
	if np == nil || np.Supersedes != target.ID || np.Origin != "extracted" || len(np.Paths) != 1 {
		t.Fatalf("applied phial = %+v", np)
	}
	if _, err := s.Resolve(ctx, pr.ID, false, "jsp"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("resolve twice: %v", err)
	}
	if ps := must(s.Proposals(ctx, "applied", 0)); len(ps) != 1 || ps[0].ResolvedBy != "jsp" {
		t.Fatalf("applied list = %+v", ps)
	}
}
