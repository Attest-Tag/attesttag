-- The Postgres twin of ../sqlite/0011_tool_calls_channel_idx.sql. Same index, same columns.
--
-- See the SQLite file for which query needs it and why the table had none until now.
create index tool_calls_channel on tool_calls(team_id, channel, created_at);
