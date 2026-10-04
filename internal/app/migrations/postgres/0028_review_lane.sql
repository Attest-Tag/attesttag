-- The Postgres twin of ../sqlite/0028_review_lane.sql: the installation a review run reaches its
-- repository through, what was asked of it, the engine's result it is posted and re-rendered
-- from, the findings kept off the diff because their file moved on before they were posted, and
-- where the engine put each finding (review_findings.place). The SQLite file also says what
-- review_prs.file_hashes really holds: review.PatchHash per path, not 0027's blob sha.
alter table review_runs add column installation_id bigint not null default 0;
alter table review_runs add column request_json text not null default '{}';
alter table review_runs add column outcome_json text not null default '{}';
alter table review_findings add column possibly_outdated bigint not null default 0;
alter table review_findings add column place text not null default '';
