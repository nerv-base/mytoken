-- Growth step 2: a third session appears, one more finished turn lands in
-- sess-beta, and a failed turn must stay uncounted.
INSERT INTO model_usage
  (id, session_id, started_at, model_id, status, input_tokens, output_tokens,
   cache_read_input_tokens, cache_creation_input_tokens, provider, prompt_text) VALUES
  ('u-beta-2',  'sess-beta',  1772359440000, 'glm-5.3-flash', 'completed', 900, 110, 300, 100, NULL, NULL),
  ('u-gamma-1', 'sess-gamma', 1772359500000, 'glm-5.3',       'completed', 250,  25,   0,   0, NULL, NULL),
  ('u-beta-3',  'sess-beta',  1772359560000, 'glm-5.3-flash', 'failed',    100,  10,   0,   0, NULL, NULL);

INSERT INTO session (id, directory, title) VALUES
  ('sess-gamma', '/work/gamma', 'SENTINEL_PRIVATE_TEXT');
