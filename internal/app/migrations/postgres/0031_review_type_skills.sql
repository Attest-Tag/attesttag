-- The Postgres twin of ../sqlite/0031_review_type_skills.sql: the skill folders in GitHub
-- repositories a code-review type follows besides its rules, as JSON [{repo, path, ref}].
alter table review_types add column skills_json text not null default '[]';
