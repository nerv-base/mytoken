-- MiMo Code conformance seed, growth step 1 of 2: the second half of the
-- usage rows, all newer than every row of step 0, so the cursor's
-- (time_updated, id) watermark has to resume this database.
--
-- msg_mc5 reports zero tokens and zero cost: MiMo Code, like OpenCode, writes
-- such a row for an aborted or cached turn and it must not emit an event.
-- msg_mc6 is an assistant row without `cost`, whose usage must still count.
INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES ('msg_mc4','ses_mc_main','main',1742500040000,1742500040000,'{"role":"assistant","agent":"build","path":{"cwd":"/Users/dev/mimoproj","root":"/Users/dev/mimoproj"},"cost":0.002,"tokens":{"total":580,"input":500,"output":80,"reasoning":0,"cache":{"write":50,"read":0}},"modelID":"kimi-k2","providerID":"moonshot","time":{"created":1742500040000}}');
INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES ('msg_mc5','ses_mc_main','main',1742500045000,1742500045000,'{"role":"assistant","agent":"build","cost":0,"tokens":{"total":0,"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742500045000}}');
INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES ('msg_mc6','ses_mc_sub','explore',1742500050000,1742500050000,'{"role":"assistant","agent":"explore","path":{"cwd":"/Users/dev/mimoproj","root":"/Users/dev/mimoproj"},"tokens":{"input":120,"output":12,"cache":{"read":0,"write":0}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742500050000}}');
