-- 003: attribution, the ledger, sessions and proposals.

-- Who made a phial. An OAuth subject, "web", "import" or "extract".
ALTER TABLE phials ADD COLUMN created_by TEXT NOT NULL DEFAULT '';

-- The ledger: one row per change, with the text before and after, so a
-- phial, a subject or a store can show its changes over time.
CREATE TABLE ledger (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  at          TEXT NOT NULL,
  actor       TEXT NOT NULL DEFAULT '',
  action      TEXT NOT NULL,            -- remember | confirm | supersede | retract | verify | purge | propose | review
  phial_id    TEXT NOT NULL DEFAULT '',
  subject     TEXT NOT NULL DEFAULT '',
  text_before TEXT NOT NULL DEFAULT '',
  text_after  TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX ledger_phial ON ledger(phial_id);
CREATE INDEX ledger_subject ON ledger(subject);

-- Uploaded sessions, waiting for extraction or done with it.
CREATE TABLE sessions (
  id            TEXT PRIMARY KEY,        -- source + ":" + the source's id
  source        TEXT NOT NULL,           -- claude-code | entire | codex | ...
  repo          TEXT NOT NULL DEFAULT '',
  path          TEXT NOT NULL DEFAULT '',
  checkpoint_id TEXT NOT NULL DEFAULT '',
  commit_sha    TEXT NOT NULL DEFAULT '',
  uploaded_by   TEXT NOT NULL DEFAULT '',
  turns         TEXT NOT NULL,           -- JSON array of {index, role, text}
  started_at    TEXT NOT NULL DEFAULT '',
  ended_at      TEXT NOT NULL DEFAULT '',
  uploaded_at   TEXT NOT NULL,
  extracted_at  TEXT NOT NULL DEFAULT '',
  extract_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_pending ON sessions(extracted_at);

-- A proposed merge or supersede that waits for review.
CREATE TABLE proposals (
  id           TEXT PRIMARY KEY,         -- pr_ + hex
  action       TEXT NOT NULL,            -- supersede | merge
  target_id    TEXT NOT NULL REFERENCES phials(id),
  subject      TEXT NOT NULL,
  kind         TEXT NOT NULL,
  text         TEXT NOT NULL,            -- the candidate text
  paths        TEXT NOT NULL DEFAULT '[]',
  reason       TEXT NOT NULL DEFAULT '',
  session_id   TEXT NOT NULL DEFAULT '',
  proposed_by  TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL DEFAULT 'pending', -- pending | applied | rejected
  created_at   TEXT NOT NULL,
  resolved_at  TEXT NOT NULL DEFAULT '',
  resolved_by  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX proposals_state ON proposals(state);
