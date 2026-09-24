-- The Postgres twin of ../sqlite/0022_doc_chunks_embed_id.sql: which endpoint and model embedded a
-- chunk, so a change of either re-embeds rather than silently finding nothing.
alter table doc_chunks add column embed_id text not null default '';
