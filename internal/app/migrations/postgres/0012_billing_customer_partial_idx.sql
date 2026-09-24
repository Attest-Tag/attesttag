-- The Postgres twin of ../sqlite/0012_billing_customer_partial_idx.sql. Same index, same predicate.
--
-- See the SQLite file for why the empty sentinel had to come out of the index: every billing row is
-- born with customer_id='', and a unique index over that value lets only one organisation per
-- deployment have a billing row without a Stripe customer.
--
-- The failure is worse on this dialect. A unique violation here is error 23505, which aborts the
-- surrounding transaction rather than just the statement, so the whole of MoveCredit or
-- PutSubscription goes back — which is the trap db_dialect.go documents.
drop index if exists billing_accounts_customer;
create unique index billing_accounts_customer on billing_accounts(customer_id) where customer_id <> '';
