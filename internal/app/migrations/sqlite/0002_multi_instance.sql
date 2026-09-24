-- What a second instance needs before it is safe to run one.
--
-- Each of these replaces something that lived in one process's memory. That was correct while
-- there was exactly one process and silently wrong the moment there were two: an OAuth callback
-- landing on the instance that did not start the flow, two schedulers running the same routine
-- and posting the same answer twice, two copies of every rate limit.
--
-- Everything here is written so that one instance behaves exactly as it did before — a lease
-- nobody contends is taken on the first attempt — which is why this ships and soaks long before
-- a second instance exists.

-- Who is running the loops that must run once across the deployment: the scope sync, the
-- document ingest, the Drive sync, the job reconciler. One row per role rather than one global
-- leader, so the work spreads and a slow ingest does not hold up the reconciler.
create table leader_leases (
  role       text primary key,
  holder     text not null,               -- instance id, for a human reading this during an incident
  expires_at integer not null             -- UnixNano, like slack_deliveries
);

-- The half-finished MCP sign-in, moved out of process memory. The provider redirects the
-- browser to a callback registered outside requireAdmin — it will send no session cookie — so
-- the state token is the only thing tying the callback to the organisation that started it. If
-- that lives in one instance's map, a callback arriving at the other fails with "unknown or
-- expired state" and the person is told to try again, forever. Shaped after connect_states,
-- which already does this correctly.
create table oauth_pendings (
  state        text primary key,
  org_id       integer not null,
  user_id      integer not null default 0,
  conn_id      integer not null,
  verifier_enc blob not null,             -- the PKCE verifier, sealed: it is a credential
  created_at   text default (datetime('now')),
  expires_at   text not null
);

-- Rate limits, which are a security control rather than a convenience: signup, invitations and
-- the login lockout. N instances holding their own counters means N times the budget, so a
-- stranger with a script gets N attempts where the number said one.
create table throttle_events (
  key text not null,
  at  integer not null                    -- UnixNano
);
create index throttle_events_key on throttle_events(key, at);

-- The routine scheduler's claim. It was a map in the agent, so two instances would both find
-- the same routine due and both run it — one routine, two runs, two identical messages in a
-- channel. The lease is on the row every scheduler already reads.
alter table routines add column run_lease integer not null default 0;   -- UnixNano; 0 = free
alter table routines add column run_holder text not null default '';

-- "stop" typed in a thread cancels the turns registered on the instance that received it. The
-- turn it means to stop may be running on the other one, which would go on answering a question
-- somebody has already asked it to drop.
alter table sessions add column stop_requested_at text;
