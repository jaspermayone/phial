// Package store is phial's SQLite store: phials, subjects, the project
// registry, receipts and the full-text index. One Store is one tenant.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver; built with FTS5

	"github.com/jaspermayone/phial/internal/rank"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Kinds, statuses, origins and verify states. The store rejects any other
// value, so callers cannot invent a new one by accident.
var (
	Kinds        = []string{"decision", "invariant", "gotcha", "preference", "convention", "fact"}
	Origins      = []string{"authored", "extracted", "imported"}
	VerifyStates = []string{"ok", "moved", "stale", "orphaned", "unverifiable"}
)

const (
	StatusActive     = "active"
	StatusSuperseded = "superseded"
	StatusRetracted  = "retracted"
)

// Errors callers check for.
var (
	ErrNotFound  = errors.New("phial not found")
	ErrNotActive = errors.New("phial is not active")
	ErrInvalid   = errors.New("invalid input")
)

// Store is one tenant's database.
type Store struct {
	db   *sql.DB
	rank rank.Config
	// Now is the clock. Tests replace it.
	Now func() time.Time
}

// Open opens (or creates) the database at path and applies migrations.
func Open(path string, rc rank.Config) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer at a time is the SQLite model; one connection keeps it
	// simple and avoids SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, rank: rc.Merge(), Now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for backup tooling.
func (s *Store) DB() *sql.DB { return s.db }

// Rank returns the ranking config in use.
func (s *Store) Rank() rank.Config { return s.rank }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var have int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&have); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "%d_", &n); err != nil || n <= have {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, n); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---- types ----

// Subject is one memory subject.
type Subject struct {
	Path        string    `json:"path"`
	Kind        string    `json:"kind"`
	Description string    `json:"description"`
	Aliases     []string  `json:"aliases"`
	Active      int       `json:"active"` // active phials, filled by ListSubjects
	UpdatedAt   time.Time `json:"updated_at"`
}

// Provenance says where a phial came from.
type Provenance struct {
	SessionID    string `json:"session_id,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	Commit       string `json:"commit,omitempty"`
	Repo         string `json:"repo,omitempty"`
	Turn         int    `json:"turn,omitempty"`
	Source       string `json:"source,omitempty"`
}

func (p Provenance) empty() bool { return p == (Provenance{}) }

// Locus is a code anchor.
type Locus struct {
	Repo   string `json:"repo,omitempty"`
	File   string `json:"file,omitempty"`
	Symbol string `json:"symbol,omitempty"`
}

// String writes "file#symbol", the form agents pass back.
func (l Locus) String() string {
	if l.Symbol == "" {
		return l.File
	}
	return l.File + "#" + l.Symbol
}

// ParseLocus reads "file", "file#symbol" or "#symbol".
func ParseLocus(s string) Locus {
	file, sym, _ := strings.Cut(strings.TrimSpace(s), "#")
	return Locus{File: file, Symbol: sym}
}

// Phial is one memory record.
type Phial struct {
	ID            string       `json:"id"`
	Subject       string       `json:"subject"`
	SubjectKind   string       `json:"subject_kind"`
	Kind          string       `json:"kind"`
	Text          string       `json:"text"`
	Status        string       `json:"status"`
	Origin        string       `json:"origin"`
	Supersedes    string       `json:"supersedes,omitempty"`
	SupersededBy  string       `json:"superseded_by,omitempty"`
	RetractReason string       `json:"retract_reason,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	ConfirmedAt   time.Time    `json:"confirmed_at"`
	VerifiedAt    time.Time    `json:"verified_at,omitzero"`
	VerifyState   string       `json:"verify_state"`
	VerifyNote    string       `json:"verify_note,omitempty"`
	Paths         []string     `json:"paths"`
	Loci          []Locus      `json:"loci,omitempty"`
	Provenance    []Provenance `json:"provenance,omitempty"`
}

// Global reports whether the phial belongs to every project (profile,
// topics, people) rather than one area.
func (p *Phial) Global() bool { return p.SubjectKind != "area" }

// LastConfirmed is the time ranking measures age from.
func (p *Phial) LastConfirmed() time.Time {
	if p.UpdatedAt.After(p.ConfirmedAt) {
		return p.UpdatedAt
	}
	return p.ConfirmedAt
}

// ---- validation ----

var segment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// SubjectKind validates a subject path and returns its kind. Areas and
// topics may nest ("areas/homelab/netbox"); people may not.
func SubjectKind(path string) (string, error) {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".md")
	if path == "profile" {
		return "profile", nil
	}
	dir, rest, ok := strings.Cut(path, "/")
	if !ok || rest == "" {
		return "", fmt.Errorf("%w: subject must be profile, areas/<name>, topics/<name> or people/<name>, got %q", ErrInvalid, path)
	}
	kinds := map[string]string{"areas": "area", "topics": "topic", "people": "people"}
	kind, ok := kinds[dir]
	if !ok {
		return "", fmt.Errorf("%w: unknown subject folder %q", ErrInvalid, dir)
	}
	parts := strings.Split(rest, "/")
	if kind == "people" && len(parts) > 1 {
		return "", fmt.Errorf("%w: people subjects do not nest: %q", ErrInvalid, path)
	}
	for _, p := range parts {
		if !segment.MatchString(p) {
			return "", fmt.Errorf("%w: subject segment %q must be lowercase letters, digits, '.', '_' or '-'", ErrInvalid, p)
		}
	}
	return kind, nil
}

// CleanSubject trims a subject path to its canonical form.
func CleanSubject(path string) string {
	return strings.TrimSuffix(strings.Trim(strings.TrimSpace(path), "/"), ".md")
}

var pathRe = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)

func checkPaths(paths []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		if !pathRe.MatchString(p) {
			return nil, fmt.Errorf("%w: path %q must be dotted lowercase words, e.g. deploy.target", ErrInvalid, p)
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// normText is the form used to find identical phials.
func normText(t string) string { return strings.Join(strings.Fields(t), " ") }

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "ph_" + hex.EncodeToString(b[:])
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// ---- subjects ----

// EnsureSubject creates the subject if it is missing. A non-empty
// description or aliases replaces the stored one.
func (s *Store) EnsureSubject(ctx context.Context, path, description string, aliases []string) (*Subject, error) {
	path = CleanSubject(path)
	kind, err := SubjectKind(path)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.ensureSubjectTx(tx, path, kind, description, aliases); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetSubject(ctx, path)
}

func (s *Store) ensureSubjectTx(tx *sql.Tx, path, kind, description string, aliases []string) error {
	now := ts(s.Now())
	al, _ := json.Marshal(nonNil(aliases))
	if _, err := tx.Exec(`INSERT INTO subjects (path, kind, description, aliases, created_at, updated_at)
		VALUES (?,?,?,?,?,?) ON CONFLICT(path) DO NOTHING`, path, kind, description, string(al), now, now); err != nil {
		return err
	}
	if description != "" {
		if _, err := tx.Exec(`UPDATE subjects SET description=?, updated_at=? WHERE path=?`, description, now, path); err != nil {
			return err
		}
	}
	if len(aliases) > 0 {
		if _, err := tx.Exec(`UPDATE subjects SET aliases=?, updated_at=? WHERE path=?`, string(al), now, path); err != nil {
			return err
		}
	}
	return nil
}

func nonNil(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

// GetSubject returns one subject, or ErrNotFound.
func (s *Store) GetSubject(ctx context.Context, path string) (*Subject, error) {
	row := s.db.QueryRowContext(ctx, `SELECT s.path, s.kind, s.description, s.aliases, s.updated_at,
		(SELECT COUNT(*) FROM phials p WHERE p.subject = s.path AND p.status = 'active')
		FROM subjects s WHERE s.path = ?`, CleanSubject(path))
	sub, err := scanSubject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("subject %q: %w", path, ErrNotFound)
	}
	return sub, err
}

// ListSubjects returns every subject with its count of active phials.
func (s *Store) ListSubjects(ctx context.Context) ([]Subject, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.path, s.kind, s.description, s.aliases, s.updated_at,
		(SELECT COUNT(*) FROM phials p WHERE p.subject = s.path AND p.status = 'active')
		FROM subjects s ORDER BY CASE s.kind WHEN 'profile' THEN 0 WHEN 'area' THEN 1 WHEN 'topic' THEN 2 ELSE 3 END, s.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subject
	for rows.Next() {
		sub, err := scanSubject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanSubject(r scanner) (*Subject, error) {
	var sub Subject
	var aliases, updated string
	if err := r.Scan(&sub.Path, &sub.Kind, &sub.Description, &aliases, &updated, &sub.Active); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(aliases), &sub.Aliases)
	sub.Aliases = nonNil(sub.Aliases)
	sub.UpdatedAt = parseTS(updated)
	return &sub, nil
}

// ---- writes ----

// RememberInput is one new phial.
type RememberInput struct {
	Subject     string
	Kind        string
	Text        string
	Paths       []string
	Loci        []Locus
	Provenance  Provenance
	Origin      string // default "authored"
	Description string // subject description, used only when the subject is new or this is set
	// CreatedAt overrides the creation time. Importers use it to keep the
	// original date; zero means now.
	CreatedAt time.Time
}

// Remember makes a phial. If an active phial in the same subject has the
// same text, Remember confirms it and returns it with created=false.
func (s *Store) Remember(ctx context.Context, in RememberInput) (p *Phial, created bool, err error) {
	in.Subject = CleanSubject(in.Subject)
	subjectKind, err := SubjectKind(in.Subject)
	if err != nil {
		return nil, false, err
	}
	text := normText(in.Text)
	if text == "" {
		return nil, false, fmt.Errorf("%w: text is empty", ErrInvalid)
	}
	if in.Kind == "" {
		in.Kind = "fact"
	}
	if !oneOf(in.Kind, Kinds) {
		return nil, false, fmt.Errorf("%w: kind %q must be one of %s", ErrInvalid, in.Kind, strings.Join(Kinds, ", "))
	}
	if in.Origin == "" {
		in.Origin = "authored"
	}
	if !oneOf(in.Origin, Origins) {
		return nil, false, fmt.Errorf("%w: origin %q", ErrInvalid, in.Origin)
	}
	paths, err := checkPaths(in.Paths)
	if err != nil {
		return nil, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := s.ensureSubjectTx(tx, in.Subject, subjectKind, in.Description, nil); err != nil {
		return nil, false, err
	}

	now := s.Now()
	var existing string
	err = tx.QueryRow(`SELECT id FROM phials WHERE subject=? AND status='active' AND text=?`, in.Subject, text).Scan(&existing)
	switch {
	case err == nil:
		if _, err := tx.Exec(`UPDATE phials SET confirmed_at=? WHERE id=?`, ts(now), existing); err != nil {
			return nil, false, err
		}
		if !in.Provenance.empty() {
			if err := insertProvenance(tx, existing, in.Provenance, now); err != nil {
				return nil, false, err
			}
		}
		if err := s.linkRemoteTx(tx, in.Subject, subjectKind, in.Provenance.Repo); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		p, err := s.Get(ctx, existing)
		return p, false, err
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}

	createdAt := now
	if !in.CreatedAt.IsZero() {
		createdAt = in.CreatedAt.UTC()
	}
	id := newID()
	if err := insertPhial(tx, id, in.Subject, in.Kind, text, in.Origin, "", createdAt, createdAt); err != nil {
		return nil, false, err
	}
	if err := insertExtras(tx, id, paths, in.Loci, in.Provenance, now); err != nil {
		return nil, false, err
	}
	if err := indexPhial(tx, id, text, paths, in.Subject); err != nil {
		return nil, false, err
	}
	if err := s.linkRemoteTx(tx, in.Subject, subjectKind, in.Provenance.Repo); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	p, err = s.Get(ctx, id)
	return p, true, err
}

func insertPhial(tx *sql.Tx, id, subject, kind, text, origin, supersedes string, created, now time.Time) error {
	var sup any
	if supersedes != "" {
		sup = supersedes
	}
	_, err := tx.Exec(`INSERT INTO phials (id, subject, kind, text, status, origin, supersedes, created_at, updated_at, confirmed_at)
		VALUES (?,?,?,?,'active',?,?,?,?,?)`, id, subject, kind, text, origin, sup, ts(created), ts(created), ts(now))
	return err
}

func insertExtras(tx *sql.Tx, id string, paths []string, loci []Locus, prov Provenance, now time.Time) error {
	for _, p := range paths {
		if _, err := tx.Exec(`INSERT INTO phial_paths (phial_id, path) VALUES (?,?)`, id, p); err != nil {
			return err
		}
	}
	for _, l := range loci {
		if l.File == "" && l.Symbol == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO loci (phial_id, repo, file, symbol) VALUES (?,?,?,?)`, id, NormalizeRemote(l.Repo), l.File, l.Symbol); err != nil {
			return err
		}
	}
	if !prov.empty() {
		return insertProvenance(tx, id, prov, now)
	}
	return nil
}

func insertProvenance(tx *sql.Tx, id string, p Provenance, now time.Time) error {
	_, err := tx.Exec(`INSERT INTO provenance (phial_id, session_id, checkpoint_id, commit_sha, repo, turn, source, created_at)
		VALUES (?,?,?,?,?,?,?,?)`, id, p.SessionID, p.CheckpointID, p.Commit, NormalizeRemote(p.Repo), p.Turn, p.Source, ts(now))
	return err
}

func indexPhial(tx *sql.Tx, id, text string, paths []string, subject string) error {
	_, err := tx.Exec(`INSERT INTO phial_fts (phial_id, text, paths, subject) VALUES (?,?,?,?)`,
		id, text, strings.Join(paths, " "), strings.ReplaceAll(subject, "/", " "))
	return err
}

// SupersedeInput replaces a phial. Empty fields keep the old value.
type SupersedeInput struct {
	ID         string
	Text       string
	Kind       string
	Paths      []string
	Loci       []Locus
	Provenance Provenance
	Origin     string
}

// Supersede replaces an active phial with a new one and returns the new one.
func (s *Store) Supersede(ctx context.Context, in SupersedeInput) (*Phial, error) {
	old, err := s.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if old.Status != StatusActive {
		return nil, fmt.Errorf("%s is %s: %w", old.ID, old.Status, ErrNotActive)
	}
	text := normText(in.Text)
	if text == "" {
		return nil, fmt.Errorf("%w: text is empty", ErrInvalid)
	}
	kind := in.Kind
	if kind == "" {
		kind = old.Kind
	}
	if !oneOf(kind, Kinds) {
		return nil, fmt.Errorf("%w: kind %q", ErrInvalid, kind)
	}
	origin := in.Origin
	if origin == "" {
		origin = "authored"
	}
	if !oneOf(origin, Origins) {
		return nil, fmt.Errorf("%w: origin %q", ErrInvalid, origin)
	}
	paths := old.Paths
	if in.Paths != nil {
		if paths, err = checkPaths(in.Paths); err != nil {
			return nil, err
		}
	}
	loci := old.Loci
	if in.Loci != nil {
		loci = in.Loci
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.Now()
	id := newID()
	if err := insertPhial(tx, id, old.Subject, kind, text, origin, old.ID, now, now); err != nil {
		return nil, err
	}
	if err := insertExtras(tx, id, paths, loci, in.Provenance, now); err != nil {
		return nil, err
	}
	if err := indexPhial(tx, id, text, paths, old.Subject); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`UPDATE phials SET status='superseded', superseded_by=?, updated_at=? WHERE id=? AND status='active'`, id, ts(now), old.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("%s: %w", old.ID, ErrNotActive)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Retract marks an active phial as no longer true.
func (s *Store) Retract(ctx context.Context, id, reason string) (*Phial, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: a reason is required", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE phials SET status='retracted', retract_reason=?, updated_at=? WHERE id=? AND status='active'`,
		reason, ts(s.Now()), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		if _, err := s.Get(ctx, id); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s: %w", id, ErrNotActive)
	}
	return s.Get(ctx, id)
}

// Confirm records that an active phial is still true.
func (s *Store) Confirm(ctx context.Context, id string) (*Phial, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE phials SET confirmed_at=? WHERE id=? AND status='active'`, ts(s.Now()), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		if _, err := s.Get(ctx, id); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s: %w", id, ErrNotActive)
	}
	return s.Get(ctx, id)
}

// SetVerify records a verify result. "ok" also confirms the phial.
func (s *Store) SetVerify(ctx context.Context, id, state, note string) (*Phial, error) {
	if !oneOf(state, VerifyStates) {
		return nil, fmt.Errorf("%w: verify state %q", ErrInvalid, state)
	}
	now := ts(s.Now())
	q := `UPDATE phials SET verify_state=?, verify_note=?, verified_at=? WHERE id=?`
	args := []any{state, note, now, id}
	if state == "ok" {
		q = `UPDATE phials SET verify_state=?, verify_note=?, verified_at=?, confirmed_at=? WHERE id=?`
		args = []any{state, note, now, now, id}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	return s.Get(ctx, id)
}

// Purge removes a phial for good and writes an audit line without its text.
// It is for a phial that must not exist; no MCP tool calls it.
func (s *Store) Purge(ctx context.Context, id, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: a reason is required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found string
	if err := tx.QueryRow(`SELECT id FROM phials WHERE id=?`, id).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s: %w", id, ErrNotFound)
		}
		return err
	}
	for _, q := range []string{
		`UPDATE phials SET supersedes=NULL WHERE supersedes=?`,
		`UPDATE phials SET superseded_by=NULL WHERE superseded_by=?`,
		`DELETE FROM phial_fts WHERE phial_id=?`,
		`DELETE FROM receipts WHERE phial_id=?`,
		`DELETE FROM phials WHERE id=?`, // cascades to paths, loci, provenance
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO purge_log (phial_id, reason, at) VALUES (?,?,?)`, id, reason, ts(s.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordReceipts notes that a read surface served these phials.
func (s *Store) RecordReceipts(ctx context.Context, surface string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := ts(s.Now())
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT INTO receipts (phial_id, surface, at) VALUES (?,?,?)`, id, surface, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- reads ----

const phialCols = `p.id, p.subject, s.kind, p.kind, p.text, p.status, p.origin,
	COALESCE(p.supersedes, ''), COALESCE(p.superseded_by, ''), p.retract_reason,
	p.created_at, p.updated_at, p.confirmed_at, p.verified_at, p.verify_state, p.verify_note`

func scanPhial(r scanner, extra ...any) (*Phial, error) {
	var p Phial
	var created, updated, confirmed, verified string
	dest := []any{&p.ID, &p.Subject, &p.SubjectKind, &p.Kind, &p.Text, &p.Status, &p.Origin,
		&p.Supersedes, &p.SupersededBy, &p.RetractReason,
		&created, &updated, &confirmed, &verified, &p.VerifyState, &p.VerifyNote}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt, p.ConfirmedAt, p.VerifiedAt = parseTS(created), parseTS(updated), parseTS(confirmed), parseTS(verified)
	return &p, nil
}

// Get returns one phial with its paths, loci and provenance.
func (s *Store) Get(ctx context.Context, id string) (*Phial, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+phialCols+` FROM phials p JOIN subjects s ON s.path = p.subject WHERE p.id = ?`, id)
	p, err := scanPhial(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if err := s.fill(ctx, []*Phial{p}); err != nil {
		return nil, err
	}
	return p, nil
}

// History returns the supersede chain that contains id, oldest first.
func (s *Store) History(ctx context.Context, id string) ([]*Phial, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	for p.Supersedes != "" {
		prev, err := s.Get(ctx, p.Supersedes)
		if err != nil {
			break
		}
		p = prev
	}
	chain := []*Phial{p}
	for p.SupersededBy != "" {
		next, err := s.Get(ctx, p.SupersededBy)
		if err != nil {
			break
		}
		chain = append(chain, next)
		p = next
	}
	return chain, nil
}

// fill loads paths, loci and provenance for a batch of phials.
func (s *Store) fill(ctx context.Context, ps []*Phial) error {
	if len(ps) == 0 {
		return nil
	}
	byID := make(map[string]*Phial, len(ps))
	ids := make([]any, 0, len(ps))
	for _, p := range ps {
		byID[p.ID] = p
		p.Paths = []string{}
		ids = append(ids, p.ID)
	}
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"

	rows, err := s.db.QueryContext(ctx, `SELECT phial_id, path FROM phial_paths WHERE phial_id IN `+in+` ORDER BY path`, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return err
		}
		byID[id].Paths = append(byID[id].Paths, path)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT phial_id, repo, file, symbol FROM loci WHERE phial_id IN `+in, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var l Locus
		if err := rows.Scan(&id, &l.Repo, &l.File, &l.Symbol); err != nil {
			rows.Close()
			return err
		}
		byID[id].Loci = append(byID[id].Loci, l)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT phial_id, session_id, checkpoint_id, commit_sha, repo, turn, source
		FROM provenance WHERE phial_id IN `+in+` ORDER BY created_at`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p Provenance
		if err := rows.Scan(&id, &p.SessionID, &p.CheckpointID, &p.Commit, &p.Repo, &p.Turn, &p.Source); err != nil {
			return err
		}
		byID[id].Provenance = append(byID[id].Provenance, p)
	}
	return rows.Err()
}

// Query selects phials for Recall. Zero values mean "no filter".
type Query struct {
	Text       string // full-text query; empty ranks by freshness × trust only
	Subject    string // exact subject or a parent: "areas/homelab" includes "areas/homelab/netbox"
	Kind       string
	PathPrefix string // "deploy" matches "deploy" and "deploy.target"
	Scope      string // "global" (profile, topics, people) or "area"
	Locus      string // "file", "file#symbol" or "#symbol"
	Status     string // "" = active only; "all" = every status; or one status
	Limit      int    // default 20, max 200
	Lowest     bool   // sort ascending, for rank-report
}

// Result is one ranked phial.
type Result struct {
	*Phial
	Rank rank.Parts `json:"rank"`
}

// Recall searches and ranks phials.
func (s *Store) Recall(ctx context.Context, q Query) ([]Result, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	var where []string
	var args []any
	from := `phials p JOIN subjects s ON s.path = p.subject`
	cols := phialCols + `, 0.0`
	match := ftsQuery(q.Text)
	if match != "" {
		from = `phial_fts f JOIN phials p ON p.id = f.phial_id JOIN subjects s ON s.path = p.subject`
		cols = phialCols + `, bm25(phial_fts, 0.0, 10.0, 3.0, 1.0)`
		where = append(where, `phial_fts MATCH ?`)
		args = append(args, match)
	} else if strings.TrimSpace(q.Text) != "" {
		return nil, nil // only stop characters: nothing can match
	}
	switch q.Status {
	case "":
		where = append(where, `p.status = 'active'`)
	case "all":
	case StatusActive, StatusSuperseded, StatusRetracted:
		where = append(where, `p.status = ?`)
		args = append(args, q.Status)
	default:
		return nil, fmt.Errorf("%w: status %q", ErrInvalid, q.Status)
	}
	if q.Subject != "" {
		sub := CleanSubject(q.Subject)
		where = append(where, `(p.subject = ? OR p.subject LIKE ? ESCAPE '\')`)
		args = append(args, sub, likeEscape(sub)+"/%")
	}
	if q.Kind != "" {
		where = append(where, `p.kind = ?`)
		args = append(args, q.Kind)
	}
	if q.PathPrefix != "" {
		pp := strings.ToLower(strings.TrimSpace(q.PathPrefix))
		where = append(where, `EXISTS (SELECT 1 FROM phial_paths pp WHERE pp.phial_id = p.id AND (pp.path = ? OR pp.path LIKE ? ESCAPE '\'))`)
		args = append(args, pp, likeEscape(pp)+".%")
	}
	switch q.Scope {
	case "":
	case "global":
		where = append(where, `s.kind <> 'area'`)
	case "area":
		where = append(where, `s.kind = 'area'`)
	default:
		return nil, fmt.Errorf("%w: scope %q must be global or area", ErrInvalid, q.Scope)
	}
	if q.Locus != "" {
		l := ParseLocus(q.Locus)
		cond := []string{}
		if l.File != "" {
			cond = append(cond, `l.file = ?`)
			args = append(args, l.File)
		}
		if l.Symbol != "" {
			cond = append(cond, `l.symbol = ?`)
			args = append(args, l.Symbol)
		}
		if len(cond) > 0 {
			where = append(where, `EXISTS (SELECT 1 FROM loci l WHERE l.phial_id = p.id AND `+strings.Join(cond, " AND ")+`)`)
		}
	}
	sqlq := `SELECT ` + cols + ` FROM ` + from
	if len(where) > 0 {
		sqlq += ` WHERE ` + strings.Join(where, ` AND `)
	}
	// Rank a bounded candidate set; FTS order is the first cut.
	if match != "" {
		sqlq += ` ORDER BY bm25(phial_fts, 0.0, 10.0, 3.0, 1.0)`
	}
	sqlq += ` LIMIT 1000`

	rows, err := s.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, fmt.Errorf("recall: %w", err)
	}
	var ps []*Phial
	var bms []float64
	for rows.Next() {
		var bm float64
		p, err := scanPhial(rows, &bm)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ps = append(ps, p)
		bms = append(bms, bm)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rel := rank.Normalize(bms)
	now := s.Now()
	out := make([]Result, len(ps))
	for i, p := range ps {
		out[i] = Result{Phial: p, Rank: s.score(p, rel[i], now)}
	}
	statusOrder := map[string]int{StatusActive: 0, StatusSuperseded: 1, StatusRetracted: 2}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Status != b.Status {
			return statusOrder[a.Status] < statusOrder[b.Status]
		}
		if q.Lowest {
			return a.Rank.Score < b.Rank.Score
		}
		return a.Rank.Score > b.Rank.Score
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	batch := make([]*Phial, len(out))
	for i := range out {
		batch[i] = out[i].Phial
	}
	if err := s.fill(ctx, batch); err != nil {
		return nil, err
	}
	return out, nil
}

// Score ranks one phial with no query.
func (s *Store) Score(p *Phial) rank.Parts { return s.score(p, 1, s.Now()) }

func (s *Store) score(p *Phial, relevance float64, now time.Time) rank.Parts {
	age := now.Sub(p.LastConfirmed())
	return rank.Score(relevance, s.rank.Freshness(p.SubjectKind, p.Kind, age), s.rank.TrustFor(p.VerifyState))
}

// ftsQuery turns free text into a safe FTS5 query: every word is quoted and
// the words are OR-ed, so BM25 does the ranking and no input can be parsed as
// FTS syntax.
func ftsQuery(text string) string {
	var terms []string
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r > 127)
	}) {
		w = strings.Trim(w, "-._")
		if w == "" {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " OR ")
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ---- project registry ----

// ProjectHints are what an agent knows about its workspace.
type ProjectHints struct {
	GitRemote string
	Path      string
	Name      string
}

// ResolveProject finds the area subject for a workspace, or "" if none.
// A known remote wins. Otherwise a name (or the last element of the path)
// that matches an area subject or one of its aliases is used, and the
// remote is linked to it so the next lookup is exact.
func (s *Store) ResolveProject(ctx context.Context, h ProjectHints) (string, error) {
	remote := NormalizeRemote(h.GitRemote)
	if remote != "" {
		var sub string
		err := s.db.QueryRowContext(ctx, `SELECT subject FROM project_remotes WHERE remote=?`, remote).Scan(&sub)
		if err == nil {
			return sub, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	var names []string
	if h.Name != "" {
		names = append(names, h.Name)
	}
	if remote != "" {
		names = append(names, remote[strings.LastIndex(remote, "/")+1:])
	}
	if h.Path != "" {
		names = append(names, filepath.Base(filepath.Clean(h.Path)))
	}
	subjects, err := s.ListSubjects(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range names {
		n = slug(n)
		if n == "" {
			continue
		}
		for _, sub := range subjects {
			if sub.Kind != "area" {
				continue
			}
			match := strings.TrimPrefix(sub.Path, "areas/") == n
			for _, a := range sub.Aliases {
				if slug(a) == n {
					match = true
				}
			}
			if !match {
				continue
			}
			if remote != "" {
				if err := s.LinkRemote(ctx, sub.Path, remote); err != nil {
					return "", err
				}
			}
			return sub.Path, nil
		}
	}
	return "", nil
}

// LinkRemote records that a git remote belongs to an area subject. An
// existing link for the remote is kept.
func (s *Store) LinkRemote(ctx context.Context, subject, remote string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kind, err := SubjectKind(subject)
	if err != nil {
		return err
	}
	if err := s.linkRemoteTx(tx, CleanSubject(subject), kind, remote); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) linkRemoteTx(tx *sql.Tx, subject, kind, remote string) error {
	remote = NormalizeRemote(remote)
	if remote == "" || kind != "area" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO project_remotes (remote, subject, created_at) VALUES (?,?,?) ON CONFLICT(remote) DO NOTHING`,
		remote, subject, ts(s.Now()))
	return err
}

// Remotes returns the remotes linked to a subject.
func (s *Store) Remotes(ctx context.Context, subject string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT remote FROM project_remotes WHERE subject=? ORDER BY remote`, CleanSubject(subject))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NormalizeRemote turns any git remote form into host/owner/repo:
// "git@github.com:Owner/Repo.git" and "https://github.com/owner/repo" both
// become "github.com/owner/repo".
func NormalizeRemote(r string) string {
	r = strings.TrimSpace(r)
	if r == "" {
		return ""
	}
	if i := strings.Index(r, "://"); i >= 0 {
		r = r[i+3:]
	} else if at := strings.Index(r, "@"); at >= 0 && strings.Contains(r[at:], ":") {
		// scp form: user@host:owner/repo
		r = r[at+1:]
		r = strings.Replace(r, ":", "/", 1)
	}
	if at := strings.Index(r, "@"); at >= 0 && at < strings.Index(r+"/", "/") {
		r = r[at+1:]
	}
	r = strings.TrimSuffix(strings.TrimSuffix(r, "/"), ".git")
	host, rest, _ := strings.Cut(r, "/")
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h // drop a port
	}
	return strings.ToLower(host + "/" + rest)
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// Stats counts phials by status.
type Stats struct {
	Subjects   int `json:"subjects"`
	Active     int `json:"active"`
	Superseded int `json:"superseded"`
	Retracted  int `json:"retracted"`
}

// Stats returns store-wide counts.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM subjects),
		(SELECT COUNT(*) FROM phials WHERE status='active'),
		(SELECT COUNT(*) FROM phials WHERE status='superseded'),
		(SELECT COUNT(*) FROM phials WHERE status='retracted')`).Scan(&st.Subjects, &st.Active, &st.Superseded, &st.Retracted)
	return st, err
}

// FindByText returns the phial in subject whose text matches, in any
// status, or ErrNotFound. Importers use it to stay idempotent: a phial that
// was imported and later superseded is not imported again.
func (s *Store) FindByText(ctx context.Context, subject, text string) (*Phial, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM phials WHERE subject=? AND text=? ORDER BY status='active' DESC LIMIT 1`,
		CleanSubject(subject), normText(text)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// ReceiptCount returns how many times read surfaces served a phial, and
// when it was last served.
func (s *Store) ReceiptCount(ctx context.Context, id string) (int, time.Time, error) {
	var n int
	var last sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(at) FROM receipts WHERE phial_id=?`, id).Scan(&n, &last)
	return n, parseTS(last.String), err
}

// Recent returns the phials changed most recently, in any status, ranked
// with no query so callers can show their level.
func (s *Store) Recent(ctx context.Context, limit int) ([]Result, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+phialCols+` FROM phials p JOIN subjects s ON s.path = p.subject
		ORDER BY MAX(p.updated_at, p.confirmed_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var ps []*Phial
	for rows.Next() {
		p, err := scanPhial(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := s.fill(ctx, ps); err != nil {
		return nil, err
	}
	out := make([]Result, len(ps))
	for i, p := range ps {
		out[i] = Result{Phial: p, Rank: s.Score(p)}
	}
	return out, rows.Err()
}
