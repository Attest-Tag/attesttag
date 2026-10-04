-- The Postgres twin of ../sqlite/0029_review_snippet.sql: the masked lines a code-review finding
-- points at, kept with it so the summary can show them on every re-render.
alter table review_findings add column snippet text not null default '';
