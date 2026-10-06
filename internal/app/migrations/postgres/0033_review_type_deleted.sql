-- The Postgres twin of ../sqlite/0033_review_type_deleted.sql: when a type of the organisation's
-- own was deleted, null while it stands. The row and its history are kept so its key stays taken.
alter table review_types add column deleted_at text;
