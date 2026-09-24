-- The enterprise plan: a deal written by hand rather than a rung on the ladder.
--
-- Every other plan size is configuration. STRIPE_SIZES names the ladder, sizeLabels/sizeUserLimits
-- /sizeJobLimits say what each rung is, and billing_accounts.size holds the key of the one an
-- account bought. Enterprise is the size past the top of that ladder, and its figures belong to one
-- account: how many people it is sold for, how many fix jobs a month, what model credit it includes,
-- what it costs and how it is paid. So they live here, one row per account, and the account's
-- billing_accounts.size reads 'enterprise' — which is what tells every reader to come here instead of
-- the configuration.
--
-- Only the operator writes this table (operator.go, behind OPERATOR_SECRET). A member of the account
-- can read what it says on Settings → Billing and change none of it. A row exists exactly while the
-- organisation is on the enterprise plan: moving it off deletes the row, so nothing stale is waiting
-- to come back if it is ever moved on again.
--
--   user_limit      people the deal is sold for; 0 = no ceiling. Shown and counted, never enforced,
--                   the same policy as every size on the ladder
--   job_limit       fix jobs a month in the deal; 0 = no figure printed. Also never enforced
--   fee_minor       what the deal costs per interval, in minor units like STRIPE_SIZES. Read from the
--                   Stripe Price when there is one; otherwise the figure the operator typed, and 0
--                   means "by agreement" and is not printed
--   fee_interval    'month' or 'year' — what fee_minor is per. Included credit is always a month's
--   included_minor  model credit the deal includes each month, spent before prepaid credit and
--                   expiring with the month, exactly like a size's allowance (0014)
--   price_id        a recurring Stripe Price made for this account. When set, the account pays by
--                   subscribing to it from Settings → Billing and the webhook keeps the record
--   pay_url         otherwise, a page the account pays at — a Stripe Payment Link, a hosted invoice.
--                   Nothing here can see those payments, so they are reconciled by hand
--   note            the operator's own note about the deal. Never shown to the account
create table enterprise_terms (
  org_id         integer primary key,
  user_limit     integer not null default 0,
  job_limit      integer not null default 0,
  fee_minor      integer not null default 0,
  fee_interval   text not null default 'month',
  included_minor integer not null default 0,
  price_id       text not null default '',
  pay_url        text not null default '',
  note           text not null default '',
  updated_by     text not null default '',
  created_at     text not null,
  updated_at     text not null
);
