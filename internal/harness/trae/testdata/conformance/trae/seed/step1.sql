-- Trae ai-agent database fixture, step 1: the turns that must NOT become
-- events, plus one more real turn so the incremental check has something to
-- merge. A deleted turn, a turn without token_usage, a turn whose usage is all
-- zeros and a turn whose context is not JSON all stay out of the snapshot; the
-- third session has no usable turn and therefore no session metadata either.
INSERT INTO chat_turn (session_id, turn_id, turn_status, context, created_at, deleted_at) VALUES ('sess-alpha', 'turn-4', 'completed', '{"chat_start_time":1792114400000,"token_usage":{"prompt_tokens":800,"completion_tokens":60,"total_tokens":860,"reasoning_tokens":20,"cache_creation_input_tokens":100,"cache_read_input_tokens":500},"persist_user_message_context":{"model_info":{"config_name":"deepseek-v4.1-flash","model_name":"deepseek-v4.1-flash","display_model_name":"DeepSeek-V4.1-Flash"}}}', 1792114400, 0)
INSERT INTO chat_turn (session_id, turn_id, turn_status, context, created_at, deleted_at) VALUES ('sess-beta', 'turn-5', 'completed', '{"chat_start_time":1792115100000,"token_usage":{"prompt_tokens":400,"completion_tokens":40,"total_tokens":440}}', 1792115100, 5)
INSERT INTO chat_turn (session_id, turn_id, turn_status, context, created_at, deleted_at) VALUES ('sess-beta', 'turn-6', 'completed', '{"chat_start_time":1792115200000,"persist_user_message_context":{"model_info":{"config_name":"kimi-k2"}}}', 1792115200, 0)
INSERT INTO chat_turn (session_id, turn_id, turn_status, context, created_at, deleted_at) VALUES ('sess-beta', 'turn-7', 'failed', '{"chat_start_time":1792115300000,"token_usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}', 1792115300, 0)
INSERT INTO chat_turn (session_id, turn_id, turn_status, context, created_at, deleted_at) VALUES ('sess-empty', 'turn-8', 'completed', 'not json at all', 1792116000, 0)
INSERT INTO chat_turn (session_id, turn_id, turn_status, context, created_at, deleted_at) VALUES ('sess-empty', 'turn-10', 'completed', '{"token_usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"reasoning_tokens":0}}', 1792116100, 0)
