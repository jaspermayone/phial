-- 002: purge removes text for good.
-- FTS5 secure-delete overwrites a deleted row's terms in the index instead
-- of leaving them in old segments.
INSERT INTO phial_fts(phial_fts, rank) VALUES('secure-delete', 1);
-- A hash of the subject and normalized text, so an importer does not bring
-- a purged phial back. It is not the text.
ALTER TABLE purge_log ADD COLUMN text_hash TEXT NOT NULL DEFAULT '';
CREATE INDEX purge_log_hash ON purge_log(text_hash);
