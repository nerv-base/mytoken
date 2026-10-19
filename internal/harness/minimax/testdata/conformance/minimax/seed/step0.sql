-- Growth step 0: the first committed assistant turns of two sessions. `ts` is
-- Unix milliseconds (the runtime writes Date.now()/message timestamps).
INSERT INTO local_runtime_token_usage
  (session_id, agent_name, framework_type, turn_id, model, ts,
   input_tokens, output_tokens, reasoning_tokens, cache_read_tokens,
   cache_write_tokens, cost_usd, raw)
VALUES
  ('sess-alpha', 'build', 'pi-agent', 'turn-1', 'MiniMax-M2.7', 1772445600000,
   1200, 340, 120, 8000, 512, 0.0125,
   '{"input":1200,"output":340,"text":"SENTINEL_PRIVATE_TEXT"}'),
  -- The writer leaves model/cost null when the provider did not report them.
  ('sess-alpha', 'build', 'pi-agent', 'turn-2', NULL, 1772445900000,
   300, 90, 0, 0, 0, NULL,
   '{"input":300,"output":90,"text":"SENTINEL_PRIVATE_TEXT"}'),
  ('sess-beta', 'plan', 'pi-agent', 'turn-1', 'MiniMax-M3', 1772449200000,
   2000, 700, 250, 0, 4096, 0.031,
   '{"input":2000,"output":700,"text":"SENTINEL_PRIVATE_TEXT"}');
