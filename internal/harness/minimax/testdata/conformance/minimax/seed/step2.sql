-- Growth step 2: an older session continues with an explicit cost_usd 0 (the
-- official writer stores 0 only when the provider reported a finite zero, so the
-- adapter keeps it as a trustworthy $0 rather than "unknown"; see the parser doc
-- and docs/ARCHITECTURE.md#harness-coverage-status), and a reasoning-only turn.
INSERT INTO local_runtime_token_usage
  (session_id, agent_name, framework_type, turn_id, model, ts,
   input_tokens, output_tokens, reasoning_tokens, cache_read_tokens,
   cache_write_tokens, cost_usd, raw)
VALUES
  ('sess-alpha', 'build', 'pi-agent', 'turn-3', 'MiniMax-M2.7', 1772452800000,
   50, 20, 5, 900, 0, 0,
   '{"input":50,"output":20,"text":"SENTINEL_PRIVATE_TEXT"}'),
  ('sess-beta', 'plan', 'pi-agent', 'turn-3', 'MiniMax-M3', 1772454600000,
   0, 0, 7, 0, 0, NULL,
   '{"output":0,"reasoning":7,"text":"SENTINEL_PRIVATE_TEXT"}');
