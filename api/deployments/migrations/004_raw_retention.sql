-- Data minimisation for the raw material (the C5 gap).
--
-- "Raw" means the two objects that hold the document's actual content: the
-- uploaded original and the OCR text. findings.json and report.json are NOT
-- raw — by construction they record category and location, never values.
--
-- The pipeline needs the raw material for the duration of one run (milliseconds
-- for text, up to the OCR ceiling for a large PDF) and never again: re-running
-- detection against a newer detector_version is a deferred feature, and the
-- uploader still holds the source document. Keeping it afterwards buys nothing
-- and grows the blast radius of a breach, so it is destroyed on completion.
--
-- raw_shredded_at is both the sweep guard (NULL = still holds raw bytes) and
-- the audit answer to "when did you minimise this?".
ALTER TABLE documents ADD COLUMN IF NOT EXISTS raw_shredded_at TIMESTAMPTZ;

-- Partial index: the retention sweep only ever asks for rows still holding raw
-- bytes, which is the small minority once the pipeline is keeping up.
CREATE INDEX IF NOT EXISTS idx_documents_raw_pending
  ON documents (status, updated_at)
  WHERE raw_shredded_at IS NULL;
