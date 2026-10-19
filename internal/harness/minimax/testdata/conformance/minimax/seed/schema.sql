-- MiniMax Code's local runtime ledger, as declared by the official package
-- packages/local-runtime-v2/src/infra/db/schema/usage.ts.
CREATE TABLE local_runtime_token_usage (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  agent_name TEXT NOT NULL,
  framework_type TEXT NOT NULL,
  turn_id TEXT,
  model TEXT,
  ts INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  reasoning_tokens INTEGER NOT NULL,
  cache_read_tokens INTEGER NOT NULL,
  cache_write_tokens INTEGER NOT NULL,
  cost_usd REAL,
  raw TEXT
);
CREATE INDEX idx_local_runtime_token_usage_session_ts
  ON local_runtime_token_usage (session_id, ts, id);
CREATE INDEX idx_local_runtime_token_usage_ts
  ON local_runtime_token_usage (ts, id);
CREATE INDEX idx_local_runtime_token_usage_model_ts
  ON local_runtime_token_usage (model, ts, id);

-- An unrelated table stands in for the runtime's conversation storage. The
-- parser must never read it: only local_runtime_token_usage is queried, and
-- nothing outside the usage columns may reach an event (docs/ARCHITECTURE.md#deduplication-and-privacy).
CREATE TABLE runtime_message_text (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  body TEXT NOT NULL
);
INSERT INTO runtime_message_text (session_id, body) VALUES
  ('sess-alpha', 'SENTINEL_PRIVATE_TEXT sk-minimax-DO-NOT-LEAK');
