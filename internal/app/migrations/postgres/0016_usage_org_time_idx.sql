-- The Postgres twin of ../sqlite/0016_usage_org_time_idx.sql. Same index, same columns.
--
-- See the SQLite file for which reads need it. It matters more here: this is the dialect the
-- hosted deployment runs on, with months of rows in the table.
create index usage_org_time on usage(org_id, created_at);
