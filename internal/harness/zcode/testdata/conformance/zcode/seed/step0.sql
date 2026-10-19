-- Baseline ledger: two sessions, one finished turn and one still in flight.
-- The sentinel text stands in for conversation content that must never be
-- parsed into an event or session field (see harnesstest.SentinelText).
INSERT INTO session (id, directory, title) VALUES
  ('sess-alpha', '/work/alpha', 'SENTINEL_PRIVATE_TEXT'),
  ('sess-beta',  '/work/beta',  NULL);

INSERT INTO model_usage
  (id, session_id, started_at, model_id, status, input_tokens, output_tokens,
   cache_read_input_tokens, cache_creation_input_tokens, provider, prompt_text) VALUES
  ('u-alpha-1', 'sess-alpha', 1772359200000, 'glm-5.3',       'completed', 1200, 300, 400, 100, 'zai',  'SENTINEL_PRIVATE_TEXT'),
  ('u-beta-1',  'sess-beta',  1772359260000, 'glm-5.3-flash', 'completed',  800,  90,   0,   0, NULL,   NULL),
  ('u-alpha-2', 'sess-alpha', 1772359320000, 'glm-5.3',       'running',    600,  60,   0,   0, 'zai',  NULL);
