-- The Postgres twin of ../sqlite/0030_review_notify.sql: where a pull request's announcement in a
-- chat channel is, so later events edit it and reply in its thread, and the last event announced.
alter table review_prs add column notify_team text not null default '';
alter table review_prs add column notify_channel text not null default '';
alter table review_prs add column notify_ts text not null default '';
alter table review_prs add column notify_last text not null default '';
