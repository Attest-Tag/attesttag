-- Deleting a code-review type of the organisation's own.
--
--   review_types.deleted_at
--                     When the type was deleted, null while it stands. A deleted type is gone from
--                     the Types tab, branch rules, commands and the Start review dialog, but its row,
--                     rules and versions stay: a run names the {key, version} it ran with, so the
--                     key stays taken and can never come back meaning something else.
alter table review_types add column deleted_at text;
