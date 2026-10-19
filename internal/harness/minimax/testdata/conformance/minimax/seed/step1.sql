-- Growth step 1: a second model, a cache-only turn, and a subagent session.
INSERT INTO local_runtime_token_usage
  (session_id, agent_name, framework_type, turn_id, model, ts,
   input_tokens, output_tokens, reasoning_tokens, cache_read_tokens,
   cache_write_tokens, cost_usd, raw)
VALUES
  ('sess-beta', 'plan', 'pi-agent', 'turn-2', 'MiniMax-M3', 1772449800000,
   100, 40, 10, 6000, 0, 0.004,
   '{"input":100,"output":40,"text":"SENTINEL_PRIVATE_TEXT"}'),
  ('sub-agent-01', 'build', 'pi-agent', 'turn-1', 'MiniMax-M2.7', 1772449920000,
   500, 210, 60, 1000, 0, NULL,
   '{"input":500,"output":210,"text":"SENTINEL_PRIVATE_TEXT"}');
