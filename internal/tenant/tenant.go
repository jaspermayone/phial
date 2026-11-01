// Package tenant gives each identity its own store and merges it with the
// shared pools the identity belongs to.
//
// A user's phials live in <data_dir>/users/<slug>/phial.db. A pool, such as
// "crane", lives in <data_dir>/pools/<name>/phial.db, has members, and owns
// subjects (areas/crane and its children). A View is one identity's picture:
// its own store plus its pools. Reads merge every source and tag results
// with the pool they came from; a write goes to the pool that owns the
// subject, or to the user's own store.
package tenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jaspermayone/phial/internal/rank"
	"github.com/jaspermayone/phial/internal/store"
)

// ErrReadOnly is returned when an identity may read a pool but not write it.
var ErrReadOnly = errors.New("this subject belongs to a shared pool you can read but not write")

// ErrNotAllowed is returned for an identity that is not on the allow list.
var ErrNotAllowed = errors.New("this user is not allowed on this server")

// Pool is one shared store.
type Pool struct {
	Name string `toml:"name"`
	// Members read and write. Readers only read.
	Members []string `toml:"members"`
	Readers []string `toml:"readers"`
	// Subjects the pool owns. A subject covers its children: "areas/crane"
	// owns "areas/crane/ui" too.
	Subjects []string `toml:"subjects"`
}

func (p Pool) owns(subject string) bool {
	subject = store.CleanSubject(subject)
	for _, s := range p.Subjects {
		s = store.CleanSubject(s)
		if subject == s || strings.HasPrefix(subject, s+"/") {
			return true
		}
	}
	return false
}

func (p Pool) role(identity string) (read, write bool) {
	for _, m := range p.Members {
		if Normalize(m) == identity {
			return true, true
		}
	}
	for _, r := range p.Readers {
		if Normalize(r) == identity {
			return true, false
		}
	}
	return false, false
}

// Manager opens stores on demand and keeps them open.
type Manager struct {
	dataDir string
	rank    rank.Config
	pools   []Pool
	allowed map[string]bool // nil means everyone
	mu      sync.Mutex
	stores  map[string]*store.Store
}

// New makes a Manager. allowed lists the identities that may have a store;
// empty means any identity the authorization server vouches for.
func New(dataDir string, rc rank.Config, pools []Pool, allowed []string) (*Manager, error) {
	m := &Manager{dataDir: dataDir, rank: rc, pools: pools, stores: map[string]*store.Store{}}
	if len(allowed) > 0 {
		m.allowed = map[string]bool{}
		for _, a := range allowed {
			m.allowed[Normalize(a)] = true
		}
	}
	for _, p := range pools {
		if !slugOK(p.Name) {
			return nil, fmt.Errorf("pool name %q must be lowercase letters, digits or dashes", p.Name)
		}
		if len(p.Subjects) == 0 {
			return nil, fmt.Errorf("pool %q owns no subjects", p.Name)
		}
		for _, s := range p.Subjects {
			if _, err := store.SubjectKind(s); err != nil {
				return nil, fmt.Errorf("pool %q: %w", p.Name, err)
			}
		}
	}
	return m, nil
}

func slugOK(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// Normalize is the one spelling of an identity: lowercase, no trailing
// slash. An authorization server may return https://Me.example/ one day and
// https://me.example the next.
func Normalize(id string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(id)), "/")
}

// Slug names an identity's directory: a readable part plus a hash, so two
// identities that clean to the same word do not share a store.
func Slug(identity string) string {
	identity = Normalize(identity)
	sum := sha256.Sum256([]byte(identity))
	var b strings.Builder
	for _, r := range identity {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	readable := strings.Trim(b.String(), "-")
	if len(readable) > 32 {
		readable = readable[:32]
	}
	if readable == "" {
		readable = "user"
	}
	return readable + "-" + hex.EncodeToString(sum[:6])
}

// Allowed reports whether an identity may use this server.
func (m *Manager) Allowed(identity string) bool {
	return m.allowed == nil || m.allowed[Normalize(identity)]
}

func (m *Manager) open(key, path string) (*store.Store, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.stores[key]; ok {
		return st, nil
	}
	st, err := store.Open(path, m.rank)
	if err != nil {
		return nil, err
	}
	m.stores[key] = st
	return st, nil
}

// UserDir is where an identity's store lives.
func (m *Manager) UserDir(identity string) string {
	return filepath.Join(m.dataDir, "users", Slug(identity))
}

// User opens an identity's own store.
func (m *Manager) User(identity string) (*store.Store, error) {
	identity = Normalize(identity)
	if identity == "" {
		return nil, errors.New("empty identity")
	}
	if !m.Allowed(identity) {
		return nil, ErrNotAllowed
	}
	dir := m.UserDir(identity)
	st, err := m.open("user:"+identity, filepath.Join(dir, "phial.db"))
	if err != nil {
		return nil, err
	}
	// The slug cannot be turned back into the identity, so write it down.
	idFile := filepath.Join(dir, "identity")
	if _, err := os.Stat(idFile); err != nil {
		_ = os.WriteFile(idFile, []byte(identity+"\n"), 0o600)
	}
	return st, nil
}

// PoolStore opens a pool's store.
func (m *Manager) PoolStore(name string) (*store.Store, error) {
	return m.open("pool:"+name, filepath.Join(m.dataDir, "pools", name, "phial.db"))
}

// Identities lists every identity that has a store on disk.
func (m *Manager) Identities() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.dataDir, "users"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(m.dataDir, "users", e.Name(), "identity"))
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(b)); id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Pools returns the configured pools.
func (m *Manager) Pools() []Pool { return m.pools }

// Close closes every open store.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var first error
	for k, st := range m.stores {
		if err := st.Close(); err != nil && first == nil {
			first = err
		}
		delete(m.stores, k)
	}
	return first
}

// Source is one store inside a View.
type Source struct {
	Pool  string // "" for the user's own store
	Store *store.Store
	Write bool
	pool  Pool
}

// View is one identity's merged picture.
type View struct {
	Identity string
	Own      *store.Store
	Pools    []Source
}

// View builds the View for an identity.
func (m *Manager) View(identity string) (*View, error) {
	identity = Normalize(identity)
	own, err := m.User(identity)
	if err != nil {
		return nil, err
	}
	v := &View{Identity: identity, Own: own}
	for _, p := range m.pools {
		read, write := p.role(identity)
		if !read {
			continue
		}
		st, err := m.PoolStore(p.Name)
		if err != nil {
			return nil, err
		}
		v.Pools = append(v.Pools, Source{Pool: p.Name, Store: st, Write: write, pool: p})
	}
	return v, nil
}

// Sources lists the user's store first, then the pools.
func (v *View) Sources() []Source {
	out := make([]Source, 0, len(v.Pools)+1)
	out = append(out, Source{Store: v.Own, Write: true})
	return append(out, v.Pools...)
}

// For returns the source that owns a subject for writing.
func (v *View) For(subject string) (Source, error) {
	for _, p := range v.Pools {
		if p.pool.owns(subject) {
			if !p.Write {
				return p, ErrReadOnly
			}
			return p, nil
		}
	}
	return Source{Store: v.Own, Write: true}, nil
}

// Where finds the source that holds a phial id.
func (v *View) Where(ctx context.Context, id string) (Source, *store.Phial, error) {
	for _, src := range v.Sources() {
		p, err := src.Store.Get(ctx, id)
		if err == nil {
			p.Pool = src.Pool
			return src, p, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return src, nil, err
		}
	}
	return Source{}, nil, fmt.Errorf("%s: %w", id, store.ErrNotFound)
}

func (v *View) actor(a string) string {
	if a != "" {
		return a
	}
	return v.Identity
}

// ---- reads ----

// Get returns one phial from whichever source holds it.
func (v *View) Get(ctx context.Context, id string) (*store.Phial, error) {
	_, p, err := v.Where(ctx, id)
	return p, err
}

// History returns a phial's supersede chain.
func (v *View) History(ctx context.Context, id string) ([]*store.Phial, error) {
	src, _, err := v.Where(ctx, id)
	if err != nil {
		return nil, err
	}
	hist, err := src.Store.History(ctx, id)
	for _, p := range hist {
		p.Pool = src.Pool
	}
	return hist, err
}

// Recall searches every source and merges the results by score.
func (v *View) Recall(ctx context.Context, q store.Query) ([]store.Result, error) {
	var all []store.Result
	for _, src := range v.Sources() {
		rs, err := src.Store.Recall(ctx, q)
		if err != nil {
			return nil, err
		}
		for i := range rs {
			rs[i].Pool = src.Pool
		}
		all = append(all, rs...)
	}
	if len(v.Pools) == 0 {
		return all, nil
	}
	order := map[string]int{store.StatusActive: 0, store.StatusSuperseded: 1, store.StatusRetracted: 2}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Status != b.Status {
			return order[a.Status] < order[b.Status]
		}
		if q.Lowest {
			return a.Rank.Score < b.Rank.Score
		}
		return a.Rank.Score > b.Rank.Score
	})
	limit := q.Limit
	if limit <= 0 && !q.All {
		limit = 20
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// Recent merges the most recently changed phials.
func (v *View) Recent(ctx context.Context, limit int) ([]store.Result, error) {
	var all []store.Result
	for _, src := range v.Sources() {
		rs, err := src.Store.Recent(ctx, limit)
		if err != nil {
			return nil, err
		}
		for i := range rs {
			rs[i].Pool = src.Pool
		}
		all = append(all, rs...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].LastConfirmed().After(all[j].LastConfirmed()) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// ListSubjects merges the subject lists. A pool's subjects carry its name.
func (v *View) ListSubjects(ctx context.Context) ([]store.Subject, error) {
	var all []store.Subject
	for _, src := range v.Sources() {
		subs, err := src.Store.ListSubjects(ctx)
		if err != nil {
			return nil, err
		}
		for i := range subs {
			subs[i].Pool = src.Pool
		}
		all = append(all, subs...)
	}
	kindOrder := map[string]int{"profile": 0, "area": 1, "topic": 2, "people": 3}
	sort.SliceStable(all, func(i, j int) bool {
		if kindOrder[all[i].Kind] != kindOrder[all[j].Kind] {
			return kindOrder[all[i].Kind] < kindOrder[all[j].Kind]
		}
		return all[i].Path < all[j].Path
	})
	return all, nil
}

// GetSubject finds a subject in the source that owns it, else anywhere.
func (v *View) GetSubject(ctx context.Context, path string) (*store.Subject, error) {
	src, _ := v.For(path)
	if sub, err := src.Store.GetSubject(ctx, path); err == nil {
		sub.Pool = src.Pool
		return sub, nil
	}
	for _, s := range v.Sources() {
		if sub, err := s.Store.GetSubject(ctx, path); err == nil {
			sub.Pool = s.Pool
			return sub, nil
		}
	}
	return nil, fmt.Errorf("subject %q: %w", path, store.ErrNotFound)
}

// Stats adds up every source.
func (v *View) Stats(ctx context.Context) (store.Stats, error) {
	var total store.Stats
	for _, src := range v.Sources() {
		st, err := src.Store.Stats(ctx)
		if err != nil {
			return total, err
		}
		total.Subjects += st.Subjects
		total.Active += st.Active
		total.Superseded += st.Superseded
		total.Retracted += st.Retracted
	}
	return total, nil
}

// Ledger merges the change logs, newest first.
func (v *View) Ledger(ctx context.Context, phialID, subject, actor string, limit int) ([]store.Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	var all []store.Entry
	for _, src := range v.Sources() {
		es, err := src.Store.Ledger(ctx, phialID, subject, actor, limit)
		if err != nil {
			return nil, err
		}
		for i := range es {
			es[i].Pool = src.Pool
		}
		all = append(all, es...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].At.After(all[j].At) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// Proposals merges pending proposals.
func (v *View) Proposals(ctx context.Context, state string, limit int) ([]store.Proposal, error) {
	var all []store.Proposal
	for _, src := range v.Sources() {
		ps, err := src.Store.Proposals(ctx, state, limit)
		if err != nil {
			return nil, err
		}
		for i := range ps {
			ps[i].Pool = src.Pool
		}
		all = append(all, ps...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	return all, nil
}

// ReceiptCount reads a phial's serve count.
func (v *View) ReceiptCount(ctx context.Context, id string) (int, store.Phial, error) {
	src, p, err := v.Where(ctx, id)
	if err != nil {
		return 0, store.Phial{}, err
	}
	n, _, err := src.Store.ReceiptCount(ctx, id)
	return n, *p, err
}

// RecordReceipts notes served phials in whichever store holds each one.
func (v *View) RecordReceipts(ctx context.Context, surface string, rs []store.Result) {
	byPool := map[string][]string{}
	for _, r := range rs {
		byPool[r.Pool] = append(byPool[r.Pool], r.ID)
	}
	for _, src := range v.Sources() {
		if ids := byPool[src.Pool]; len(ids) > 0 {
			_ = src.Store.RecordReceipts(ctx, surface, ids)
		}
	}
}

// ResolveProject tries the user's store first, then the pools.
func (v *View) ResolveProject(ctx context.Context, h store.ProjectHints) (string, error) {
	for _, src := range v.Sources() {
		sub, err := src.Store.ResolveProject(ctx, h)
		if err != nil || sub != "" {
			return sub, err
		}
	}
	return "", nil
}

// Remotes reads a subject's remotes from the source that owns it.
func (v *View) Remotes(ctx context.Context, subject string) ([]string, error) {
	src, _ := v.For(subject)
	return src.Store.Remotes(ctx, subject)
}

// ---- writes ----

// Remember routes a new phial to the pool that owns its subject, or to the
// user's own store.
func (v *View) Remember(ctx context.Context, in store.RememberInput) (*store.Phial, bool, error) {
	src, err := v.For(in.Subject)
	if err != nil {
		return nil, false, err
	}
	in.Actor = v.actor(in.Actor)
	p, created, err := src.Store.Remember(ctx, in)
	if p != nil {
		p.Pool = src.Pool
	}
	return p, created, err
}

func (v *View) writable(ctx context.Context, id string) (Source, error) {
	src, _, err := v.Where(ctx, id)
	if err != nil {
		return src, err
	}
	if !src.Write {
		return src, ErrReadOnly
	}
	return src, nil
}

// Supersede replaces a phial wherever it lives.
func (v *View) Supersede(ctx context.Context, in store.SupersedeInput) (*store.Phial, error) {
	src, err := v.writable(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	in.Actor = v.actor(in.Actor)
	p, err := src.Store.Supersede(ctx, in)
	if p != nil {
		p.Pool = src.Pool
	}
	return p, err
}

// Retract marks a phial as no longer true.
func (v *View) Retract(ctx context.Context, id, reason string) (*store.Phial, error) {
	src, err := v.writable(ctx, id)
	if err != nil {
		return nil, err
	}
	return src.Store.Retract(ctx, id, reason, v.Identity)
}

// Confirm records that a phial is still true.
func (v *View) Confirm(ctx context.Context, id string) (*store.Phial, error) {
	src, err := v.writable(ctx, id)
	if err != nil {
		return nil, err
	}
	return src.Store.Confirm(ctx, id, v.Identity)
}

// SetVerify records a verify result. Readers may verify too: it changes
// trust, not text.
func (v *View) SetVerify(ctx context.Context, id, state, note string) (*store.Phial, error) {
	src, _, err := v.Where(ctx, id)
	if err != nil {
		return nil, err
	}
	return src.Store.SetVerify(ctx, id, state, note, v.Identity)
}

// Near finds the closest active phial to text in the source that owns the subject.
func (v *View) Near(ctx context.Context, subject, text string) (*store.Phial, float64, error) {
	src, _ := v.For(subject)
	return src.Store.Near(ctx, subject, text)
}

// Propose queues a change to a phial wherever it lives.
func (v *View) Propose(ctx context.Context, p store.Proposal) (*store.Proposal, error) {
	src, _, err := v.Where(ctx, p.TargetID)
	if err != nil {
		return nil, err
	}
	if p.ProposedBy == "" {
		p.ProposedBy = v.Identity
	}
	return src.Store.Propose(ctx, p)
}

// Resolve applies or rejects a proposal.
func (v *View) Resolve(ctx context.Context, id string, apply bool) (*store.Phial, error) {
	for _, src := range v.Sources() {
		if _, err := src.Store.GetProposal(ctx, id); err == nil {
			if !src.Write {
				return nil, ErrReadOnly
			}
			return src.Store.Resolve(ctx, id, apply, v.Identity)
		}
	}
	return nil, fmt.Errorf("proposal %s: %w", id, store.ErrNotFound)
}

// Loci lists the active phials with a code anchor in repo, from every source.
func (v *View) Loci(ctx context.Context, repo string) ([]store.Result, error) {
	repo = store.NormalizeRemote(repo)
	var all []store.Result
	for _, src := range v.Sources() {
		rs, err := src.Store.Recall(ctx, store.Query{All: true})
		if err != nil {
			return nil, err
		}
		for i := range rs {
			for _, l := range rs[i].Loci {
				if l.Repo == repo || l.Repo == "" {
					rs[i].Pool = src.Pool
					all = append(all, rs[i])
					break
				}
			}
		}
	}
	return all, nil
}

// ---- importer surface ----

// EnsureSubject creates a subject in the source that owns it.
func (v *View) EnsureSubject(ctx context.Context, path, description string, aliases []string) (*store.Subject, error) {
	src, err := v.For(path)
	if err != nil {
		return nil, err
	}
	return src.Store.EnsureSubject(ctx, path, description, aliases)
}

// LinkRemote links a repo to a subject in the source that owns it.
func (v *View) LinkRemote(ctx context.Context, subject, remote string) error {
	src, err := v.For(subject)
	if err != nil {
		return err
	}
	return src.Store.LinkRemote(ctx, subject, remote)
}

// FindByText looks for a text in the source that owns the subject.
func (v *View) FindByText(ctx context.Context, subject, text string) (*store.Phial, error) {
	src, _ := v.For(subject)
	return src.Store.FindByText(ctx, subject, text)
}

// WasPurged asks the source that owns the subject.
func (v *View) WasPurged(ctx context.Context, subject, text string) (bool, error) {
	src, _ := v.For(subject)
	return src.Store.WasPurged(ctx, subject, text)
}

// Purge removes a phial from whichever store holds it.
func (v *View) Purge(ctx context.Context, id, reason string) error {
	src, _, err := v.Where(ctx, id)
	if err != nil {
		return err
	}
	return src.Store.Purge(ctx, id, reason, v.Identity)
}
