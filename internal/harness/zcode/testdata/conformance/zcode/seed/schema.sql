-- ZCode 0.4.x local usage ledger (model_usage + session).
--
-- The required columns come from the published ZCode schema: model_usage.id,
-- session_id, started_at (epoch milliseconds), model_id, status, input_tokens,
-- output_tokens; session.id and session.directory. The cache-read and
-- cache-creation columns are optional in the wild, so the parser probes for
-- them instead of assuming they exist — this fixture includes them.
--
-- input_tokens is inclusive: it already contains the cached tokens, so the
-- parser subtracts the cache buckets back out.
CREATE TABLE model_usage (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  model_id TEXT NOT NULL,
  status TEXT NOT NULL,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
  provider TEXT,
  prompt_text TEXT
);

CREATE TABLE session (
  id TEXT PRIMARY KEY,
  directory TEXT NOT NULL,
  title TEXT
);
