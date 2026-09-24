-- Self-serve billing: a flat monthly fee by employee band, and prepaid credit for model spend.
--
-- Three tables, and none of them exists for most deployments. A self-host runs on its own model
-- key and has no billing relationship with anybody; a free account has not bought anything. Both
-- get no row here at all, and every code path that touches credit is behind "does this
-- organisation have a row", so the whole mechanism costs them one index probe per turn.
--
-- Two limits live here and they are not the same limit. The subscription is the flat monthly fee
-- for the plan, set by an employee band — the company's headcount as it tells us, not seats, not
-- Slack members, and nothing counts anybody. The credit balance is prepaid model spend and it is
-- the hard floor: settings.monthly_budget_usd is the customer's own guard rail and they may raise
-- it in the console, while credit is money and only a payment moves it. The bot stops at
-- whichever binds first and the refusal says which.
--
-- Money is integer micro-dollars throughout, and the column names say so. A turn can cost
-- $0.0003, so cents would round it to nothing or to a hundred times itself; floats would make the
-- reconciliation between the balance and the ledger a tolerance rather than an equality, and
-- leave balances sitting at -1e-15 where the gate asks whether they are at or below zero.
-- usage.cost_usd stays the float it has always been: it records what a provider charged, which is
-- not the same kind of number as what a customer is owed.

-- One row per organisation, created the first time it engages with billing.
--
-- unit_price_micros and quantity rather than one amount, because a band is a decision about the
-- user interface and not about the schema: a band is quantity 1 at the band's price, and charging
-- per seat later is the same two columns with a different quantity.
create table billing_accounts (
  org_id                integer primary key,        -- orgs.id; the row IS this organisation's billing state
  provider              text not null default 'stripe',
  customer_id           text not null default '',   -- cus_…; the only handle an invoice webhook carries
  subscription_id       text not null default '',   -- sub_…; empty while only credit has been bought
  status                text not null default '',   -- '' | incomplete | trialing | active | past_due | canceled | unpaid | comped
  band                  text not null default '',   -- under_25 | 25_100 | 100_500 | 500_2000 | over_2000
  unit_price_micros     integer not null default 0,
  quantity              integer not null default 0, -- 1 for a band; seats, if that is ever sold
  currency              text not null default 'usd',
  period_start          text not null default '',   -- 'YYYY-MM-DD HH:MM:SS' UTC, like every timestamp here
  period_end            text not null default '',
  cancel_at_period_end  integer not null default 0,

  -- The balance, materialised. It is materialised because the only other way to know it would be
  -- to subtract the usage table from the top-ups, and usage is deleted on the organisation's own
  -- retention schedule (retention.go) — a balance that depends on rows a policy may delete is not
  -- a balance. Every read of the gate reads this column and nothing else.
  credit_balance_micros integer not null default 0,

  -- 1 once a payment or an operator grant has ever landed. Until then the credit floor does not
  -- apply at all, so a subscriber who has not topped up is not refused at a balance of zero they
  -- never agreed to. It never goes back to 0: an account that has bought credit is a credit
  -- account from then on, including after its subscription ends, because the money is still theirs.
  credit_enforced       integer not null default 0,

  -- Monotonic counters. lifetime_debit_micros is incremented by every turn; debit_rolled_micros is
  -- how much of it the ledger already states. The difference is the spend not yet written up,
  -- which makes this exact at every instant:
  --
  --   credit_balance_micros = sum(credit_ledger.amount_micros)
  --                           - (lifetime_debit_micros - debit_rolled_micros)
  --
  -- and equal to the plain sum immediately after a roll-up. A ledger row per turn would give the
  -- simpler invariant and an unreadable statement — thousands of $0.003 lines a month, in the one
  -- table nothing is allowed to sweep. This gives one line an hour and the same arithmetic.
  lifetime_topup_micros integer not null default 0,
  lifetime_debit_micros integer not null default 0,
  debit_rolled_micros   integer not null default 0,

  created_at            text not null,
  updated_at            text not null
);
-- invoice.* and customer.subscription.* carry a Stripe customer and nothing of ours, so this is
-- the index the webhook resolves an organisation through.
create unique index billing_accounts_customer on billing_accounts(customer_id);

-- The statement. Append-only: nothing in the code updates or deletes a row, and unlike every
-- other history in this database it is NOT swept by data_retention_days. A customer's own
-- retention policy deleting the record of what they paid is the one deletion that must not
-- happen; it goes when the organisation goes, and not before.
--
-- amount_micros is signed — topup, grant and an upward adjustment are positive, debit and refund
-- negative — so the balance is a plain sum and a reconciliation is one query.
--
-- external_id is the idempotency key and is never empty. A Stripe id for money that came from
-- Stripe (pi_…, re_…), 'debit:<org>:<hour>' for a roll-up, 'grant:<random>' for an operator's
-- grant, 'adj:<random>' for a correction. The unique index on it is the whole of what makes a
-- redelivered webhook, a retried roll-up and a double-clicked button all land exactly once.
--
-- That index is global rather than per organisation on purpose. Unique on (org_id, external_id)
-- would let one Stripe payment be credited to a second organisation by anyone who could get a
-- checkout session to name it, which is the attack this table has to survive.
create table credit_ledger (
  id                   integer primary key autoincrement,
  org_id               integer not null,
  created_at           text not null,
  kind                 text not null,                -- topup | debit | refund | adjustment | grant
  amount_micros        integer not null,
  balance_after_micros integer not null,             -- the balance this row produced, as it was written
  currency             text not null default 'usd',
  external_id          text not null,
  note                 text not null default '',
  actor                text not null default ''      -- 'stripe' | 'system' | an operator's address
);
create unique index credit_ledger_external on credit_ledger(external_id);
create index credit_ledger_org on credit_ledger(org_id, id);

-- Webhook idempotency. Deployment-level bookkeeping with no tenant content, like seen_events: an
-- event arrives before anyone knows which organisation it concerns, so giving it an org_id would
-- mean writing the row twice or lying in it once. Swept by its own age, not by anybody's policy,
-- which is also why it is named in tablesThatSurvive rather than deleted with an organisation.
create table billing_events (
  event_id    text primary key,                      -- evt_…; Stripe making it unique is the point
  provider    text not null default 'stripe',
  type        text not null default '',
  received_at text not null
);
create index billing_events_received on billing_events(received_at);
