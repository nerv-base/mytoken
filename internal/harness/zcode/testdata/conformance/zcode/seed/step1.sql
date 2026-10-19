-- Growth step 1: the in-flight turn finished. Its row keeps the same id and
-- rowid and is updated in place, which is exactly what a rowid watermark must
-- not miss; a new turn then starts and completes in the same session.
UPDATE model_usage SET status = 'completed', output_tokens = 120 WHERE id = 'u-alpha-2';

INSERT INTO model_usage
  (id, session_id, started_at, model_id, status, input_tokens, output_tokens,
   cache_read_input_tokens, cache_creation_input_tokens, provider, prompt_text) VALUES
  ('u-alpha-3', 'sess-alpha', 1772359380000, 'glm-5.3', 'completed', 500, 50, 120, 30, 'zai', NULL);
