-- 001: the phial store. See docs/design.md section 5.

CREATE TABLE subjects (
  path        TEXT PRIMARY KEY,            -- profile, areas/<name>, topics/<name>, people/<name>
  kind        TEXT NOT NULL,               -- profile | area | topic | people
  description TEXT NOT NULL DEFAULT '',
  aliases     TEXT NOT NULL DEFAULT '[]',  -- JSON array
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE phials (
  id             TEXT PRIMARY KEY,         -- ph_ + 24 hex
  subject        TEXT NOT NULL REFERENCES subjects(path),
  kind           TEXT NOT NULL,            -- decision | invariant | gotcha | preference | convention | fact
  text           TEXT NOT NULL,
  status         TEXT NOT NULL DEFAULT 'active', -- active | superseded | retracted
  origin         TEXT NOT NULL,            -- authored | extracted | imported
  supersedes     TEXT REFERENCES phials(id),
  superseded_by  TEXT REFERENCES phials(id),
  retract_reason TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  confirmed_at   TEXT NOT NULL,
  verified_at    TEXT NOT NULL DEFAULT '',
  verify_state   TEXT NOT NULL DEFAULT 'unverifiable', -- ok | moved | stale | orphaned | unverifiable
  verify_note    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX phials_subject_status ON phials(subject, status);

CREATE TABLE phial_paths (
  phial_id TEXT NOT NULL REFERENCES phials(id) ON DELETE CASCADE,
  path     TEXT NOT NULL,
  PRIMARY KEY (phial_id, path)
);
CREATE INDEX phial_paths_path ON phial_paths(path);

CREATE TABLE provenance (
  phial_id      TEXT NOT NULL REFERENCES phials(id) ON DELETE CASCADE,
  session_id    TEXT NOT NULL DEFAULT '',
  checkpoint_id TEXT NOT NULL DEFAULT '',
  commit_sha    TEXT NOT NULL DEFAULT '',
  repo          TEXT NOT NULL DEFAULT '',
  turn          INTEGER NOT NULL DEFAULT 0,
  source        TEXT NOT NULL DEFAULT '',  -- free text, e.g. "lard:areas/infra" for imports
  created_at    TEXT NOT NULL
);
CREATE INDEX provenance_phial ON provenance(phial_id);

CREATE TABLE loci (
  phial_id TEXT NOT NULL REFERENCES phials(id) ON DELETE CASCADE,
  repo     TEXT NOT NULL DEFAULT '',
  file     TEXT NOT NULL DEFAULT '',
  symbol   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX loci_phial ON loci(phial_id);
CREATE INDEX loci_file ON loci(file);

-- The project registry: a normalized git remote names one area subject.
CREATE TABLE project_remotes (
  remote     TEXT PRIMARY KEY,
  subject    TEXT NOT NULL REFERENCES subjects(path),
  created_at TEXT NOT NULL
);

-- Serve receipts. They never change the score (design 7.1 rule 3).
CREATE TABLE receipts (
  phial_id TEXT NOT NULL,
  surface  TEXT NOT NULL,
  at       TEXT NOT NULL
);
CREATE INDEX receipts_phial ON receipts(phial_id);

-- One line per purge. It never holds the purged text.
CREATE TABLE purge_log (
  phial_id TEXT NOT NULL,
  reason   TEXT NOT NULL,
  at       TEXT NOT NULL
);

-- Full-text index. The store keeps it in step with phials by hand, in the
-- same transaction as each write.
CREATE VIRTUAL TABLE phial_fts USING fts5(
  phial_id UNINDEXED,
  text,
  paths,
  subject,
  tokenize = 'porter unicode61'
);
