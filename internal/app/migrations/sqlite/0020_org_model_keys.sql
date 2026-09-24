-- An organisation's own model endpoint: the OpenAI-compatible base URL and key it brought, so
-- that every model call made on its behalf — its turns, its documents' embeddings, its fix jobs —
-- is sent there and paid for there, instead of on the deployment's own key (model_keys.go).
--
-- One row per organisation, written only from Settings → Models by a member holding
-- settings.manage, and never read back to anybody: the key is sealed under MASTER_KEY like every
-- other stored credential, and everything the console, the operator and the settings cache are told
-- comes from the other columns.
--
--   preset         which form the console shows it in: openai | openrouter | compatible. The
--                  request shape is decided by the base URL's host (dialectFor), not by this
--   base_url       an https URL on a public address
--   key_enc        the key, sealed
--   key_hint       "…abcd", so a person can tell which of their keys this is
--   key_fp         secretFingerprint of the key: which key it is, to a machine, without the key.
--                  With updated_at it is how every instance notices a rotation
--   default_model  the model a call uses when nothing more specific names one; checked with a
--                  real completion before the row was written
--   embed_model    the embedding model for this organisation's documents; empty means document
--                  search is off while this key is in use, rather than on somebody else's key
--   fix_jobs       1 when fix jobs may run on this key. A job runs the repository's own code with
--                  the key in its environment, so that is the owner's call
--   last_ok_at, last_error, last_error_at
--                  the last call's outcome, written at most once a minute, for the console
--
-- A row survives a change of plan. An organisation moved off the plan that entitles it keeps its
-- key here and its model calls refuse, rather than quietly going to the deployment's provider: it
-- brought a key so that its conversations would go where it chose.
create table org_model_keys (
  org_id         integer primary key,
  preset         text not null default '',
  base_url       text not null,
  key_enc        blob not null,
  key_hint       text not null default '',
  key_fp         text not null default '',
  default_model  text not null default '',
  embed_model    text not null default '',
  fix_jobs       integer not null default 1,
  updated_by     text not null default '',
  created_at     text not null,
  updated_at     text not null,
  last_ok_at     text not null default '',
  last_error     text not null default '',
  last_error_at  text not null default ''
);
