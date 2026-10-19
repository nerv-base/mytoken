-- MiMo Code conformance seed, growth step 0 of 2: schema, sessions and the
-- first half of the rows. One SQL statement per line (see conformance_test.go);
-- '--' lines and blank lines are skipped. Hand-written and fully synthetic: no
-- real MiMo Code log, prompt or path is reproduced here.
--
-- The schema is the one MiMo Code's drizzle definitions create
-- (packages/cli/src/session/session.sql.ts): `session` carries parent_id,
-- directory and title, `message` carries (id, session_id, agent_id,
-- time_created, time_updated, data) and `part` carries the conversation
-- content next to the message. MiMo Code is an OpenCode fork, so `message.data`
-- holds the same MessageV2.Info JSON that OpenCode v1 writes; the usage rows
-- below therefore exercise exactly the OpenCode v1 mapping.
--
-- ses_mc_sub is a child session (parent_id): its messages must be reported
-- under the child id with the parent id attached, and counts once.
--
-- SENTINEL_PRIVATE_TEXT sits in the assistant text and in a `part` row: both
-- are conversation content and must never reach the parsed output. The session
-- titles carry conversation text too and are equally never stored.
CREATE TABLE project (id text PRIMARY KEY, worktree text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE session (id text PRIMARY KEY, project_id text NOT NULL, parent_id text, directory text NOT NULL, title text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, agent_id text NOT NULL DEFAULT 'main', time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
INSERT INTO project (id,worktree,time_created,time_updated) VALUES ('prj_mc','/Users/dev/mimoproj',1742500000000,1742500300000);
INSERT INTO session (id,project_id,parent_id,directory,title,time_created,time_updated) VALUES ('ses_mc_main','prj_mc',NULL,'/Users/dev/mimoproj','Fix the MiMo Code conformance harness',1742500000000,1742500300000);
INSERT INTO session (id,project_id,parent_id,directory,title,time_created,time_updated) VALUES ('ses_mc_sub','prj_mc','ses_mc_main','/Users/dev/mimoproj','Explore the token mapping',1742500100000,1742500250000);
INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES ('msg_mc_u0','ses_mc_main','main',1742500000500,1742500000500,'{"role":"user","time":{"created":1742500000500},"text":"Fix the MiMo Code conformance harness"}');
INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES ('msg_mc1','ses_mc_main','main',1742500010000,1742500010000,'{"role":"assistant","agent":"build","path":{"cwd":"/Users/dev/mimoproj","root":"/Users/dev/mimoproj"},"cost":0.0123,"tokens":{"total":1750,"input":1000,"output":200,"reasoning":50,"cache":{"write":100,"read":400}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742500010000,"completed":1742500012000},"text":"SENTINEL_PRIVATE_TEXT"}');
INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES ('msg_mc2','ses_mc_main','main',1742500020000,1742500020000,'{"role":"assistant","agent":"build","modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742500020000}}');
INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES ('msg_mc_u1','ses_mc_sub','main',1742500025000,1742500025000,'{"role":"user","time":{"created":1742500025000},"text":"Explore the token mapping"}');
INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES ('msg_mc3','ses_mc_sub','explore',1742500030000,1742500030000,'{"role":"assistant","agent":"explore","path":{"cwd":"/Users/dev/mimoproj","root":"/Users/dev/mimoproj"},"cost":0.004,"tokens":{"total":450,"input":300,"output":40,"reasoning":10,"cache":{"write":0,"read":100}},"modelID":"gpt-5","providerID":"openai","time":{"created":1742500030000,"completed":1742500030500}}');
INSERT INTO part (id,message_id,session_id,time_created,time_updated,data) VALUES ('prt_mc1','msg_mc1','ses_mc_main',1742500010500,1742500010500,'{"type":"text","text":"SENTINEL_PRIVATE_TEXT","tool":"bash","state":{"input":"SENTINEL_PRIVATE_TEXT"}}');
