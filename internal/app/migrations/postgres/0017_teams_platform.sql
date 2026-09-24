-- The Postgres twin of ../sqlite/0017_teams_platform.sql: which chat platform a connected
-- workspace is on. Every existing row is a Slack install. See the SQLite file for why the registry
-- refuses a platform it does not know instead of guessing.
alter table teams add column platform text not null default 'slack';
