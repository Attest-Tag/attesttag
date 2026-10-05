-- How much of a fix job's prompt the provider served from its cache.
--
--   jobs.tokens_cached, job_events.tokens_cached
--                     the part of tokens_in that was a cache read, as the engine reported it. An
--                     agent re-sends its whole conversation every turn, so on a long job nearly
--                     all of tokens_in is the same prefix again; whether the provider cached it
--                     is most of what the job cost. Zero on rows from before, and on jobs whose
--                     worker did not report it: a floor, like usage.cached_in (0010).
alter table jobs add column tokens_cached integer not null default 0;
alter table job_events add column tokens_cached integer not null default 0;
