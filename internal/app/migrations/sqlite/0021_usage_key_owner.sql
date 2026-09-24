-- Whose key paid for a model call: the deployment's ('platform') or the organisation's own
-- ('org', model_keys.go).
--
-- Spend on an organisation's own key is billed to it by its provider, so it must never be charged
-- to its credit here, and a figure that did not say which key it came from could not be told
-- apart afterwards. The column says so on every usage row, and on every fix job, whose spend is
-- written from the worker's report long after the job was dispatched on one key or the other.
--
-- Existing rows are all the deployment's: no organisation could bring a key before this.
alter table usage add column key_owner text not null default 'platform';
alter table jobs add column key_owner text not null default 'platform';
