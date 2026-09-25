# Billing: selling a plan and prepaid credit

> **Off unless you configure it.** Without `STRIPE_SECRET_KEY` and `STRIPE_WEBHOOK_SECRET` the
> routes below do not exist, the console has no Billing tab, and no account has a credit floor —
> which is what every self-host wants. Half a configuration is treated as none: a deployment that
> can charge a card and cannot hear the result would take money and never credit it, so setting
> one key without the other logs a warning at startup and switches billing off.

Two things are sold, and they are different kinds of thing.

**The subscription** is a flat monthly fee set by a **plan size**: how many people may use the
bot in a month, counted rather than declared (see *What a size is counted in*). The ladder is
10, 25, 50, 100 and 250 users, and past 250 a size is a conversation — list it in `STRIPE_SIZES`
with no Price behind it and it is offered as "Talk to us". Each size is also sold with a number
of fix jobs a month (see *What a size includes*).

**Credit** is prepaid model spend. Every turn's cost is drawn from a balance at the provider's own
price, with no margin. It does not expire and it is not a subscription: an account buys an amount
and it goes down as the bot answers.

## The two limits, and which one binds

This is the paragraph support will be asked about, so it is the one worth reading twice.

| | what it is | who can move it |
|---|---|---|
| `monthly_budget_usd` | the account's own guard rail | the account, in the console |
| credit balance | money it has already paid | only a payment, or the operator |

The bot stops at whichever it reaches first, and the refusal says which. Telling somebody their
budget is spent when what ran out was the balance sends them to a field that will not help them,
so the answer is computed once, on the server, and carried as `paused_by` on `/api/me`,
`GET /api/billing` and `GET /v1/usage`. It is empty while the account is running, and then names
`credit` or `budget`. A script that wants to know *before* the bot goes quiet polls that field.

Because a pro account with live billing and the credit floor on is paying for its own model
spend, neither `PLATFORM_MONTHLY_BUDGET_USD_PER_ORG` nor a budget the operator granted with the plan
applies to it — a $25 deployment ceiling standing in front of a $500 balance would stop a customer
who has already paid, which is the worst refusal this product can produce, and a grant made by hand
before the account paid is stale. Its own `monthly_budget_usd` is then the only monthly ceiling, and
0 means none. A free account keeps the free plan's cap whatever credit it holds, and an enterprise
account keeps the figure in its deal (`budgetCeiling` in `plans.go`).

An account on its own model key ([plans.md](plans.md#its-own-model-key)) spends no credit at all: its
provider bills it, its usage rows say `key_owner = 'org'`, `LogUsageBy` charges none of them, and
`paused_by` never names `credit` for it. Its own `monthly_budget_usd` is the one limit left, and
Billing says credit is not used while the key is.

## The overdraft, and why there is one

A turn is checked before it runs and priced after it finishes, so whatever is already in flight
when the balance reaches zero will still be charged. The window is wider than it looks: the
in-flight cap is counted per container rather than per account, the routine scheduler adds its own,
the job budget check reserves nothing, and one turn can run fifty tool rounds.

So the balance is allowed to go negative, down to a named constant — `maxOverdraftMicros`, **$50** —
and everything is refused below it. Nothing clamps at zero: clamping would destroy money that is
owed and make the ledger stop adding up. The figure is in the terms, so it is a number the customer
has agreed to, and a test cites the constant so that moving it is a deliberate act.

## What to configure

| variable | what it is |
|---|---|
| `STRIPE_SECRET_KEY` | Developers → API keys. A secret; ship it through your platform's secret store, never `--set-env-vars`. A restricted key (`rk_…`) works and is the better choice, below |
| `STRIPE_WEBHOOK_SECRET` | Developers → Webhooks → add `<your-origin>/api/billing/webhook` |
| `STRIPE_SIZES` | `size=<price id>:<minor units>[:<included minor units>]`, comma separated, in the order to offer them. The third field is the model credit the size includes each month, which expires with that month; leave it off for a size that includes none. An entry with no Price id — `over_250=:0` — is listed and not sold: "Talk to us" in the console and on the site, refused at checkout |
| `BILLING_CURRENCY` | `usd`. A payment in any other currency is refused rather than converted |
| `BILLING_TOPUP_MIN_USD`, `BILLING_TOPUP_MAX_USD` | one top-up's bounds, enforced server-side. `25` and `10000` |
| `BILLING_LOW_BALANCE_USD` | `10`. Warn the account, in its alert channel and by mail to whoever founded it, at this much prepaid credit left — at most once every 72 hours |

**A restricted key is enough, and safer.** The server calls five kinds of Stripe object and
nothing else — every call is in `billing_stripe.go` — so the key needs write on Checkout Sessions,
the customer portal, Subscriptions and Subscription Schedules, read on Prices, and none on anything
else. Leaked, it cannot refund a payment or register a webhook of its own. The mode is read from
the prefix, `sk_live_` or `rk_live_`, and compared with every event's `livemode`. Give the
test-mode key the same permissions: a full-access test key lets a call added later pass every
sandbox run and be refused only in production.

**Buying wants a confirmed address.** Checkout refuses an account whose email has not been
confirmed — it reaches out from the organisation with money, and the receipt has to arrive
somewhere real — though the card portal does not, because that is where somebody cancels. Checkout
sessions are limited to 12 an hour per organisation and 20 an hour per client address, and an
account that already has a subscription is sent to **Card and invoices** rather than into a second
one, which would be a second monthly charge.

### Changing plan size

**Changing size needs no portal configuration.** Settings → Billing → **Change**, on the Plan size
line, opens a picker and `POST /api/billing/size` swaps the subscription item's Price at Stripe. The
size is a key and never a price: everything is looked up from `STRIPE_SIZES` on this side, so a
browser cannot name a Price of its own. Which way the move goes decides what happens. **Moving up**
is charged straight away: Stripe invoices the difference for the rest of the period and takes it
then, and a card that is declined, or that asks its owner to confirm the payment, leaves the size
where it was. **Moving down** waits for the end of the period already paid for — the account keeps
the larger size and what it includes until then, and the local record does not move until Stripe
rolls the subscription over. Picking the size the account is already on calls off a move down that
has not happened yet.

This used to be a link into Stripe's billing portal, and it was wrong for every deployment that
had not gone and configured one: the portal offers a plan switch only when *Customers can switch
plans* is on and the size Prices are listed against it, so the button led to a page whose only
option was *Cancel subscription*. Switch cancellation on in the portal, though — that is still
where it happens.

### Webhook events to select

The events to select on the webhook endpoint:

```
checkout.session.completed          checkout.session.async_payment_succeeded
payment_intent.succeeded            charge.refunded
charge.dispute.created              charge.dispute.closed
invoice.paid                        invoice.payment_failed
customer.subscription.created       customer.subscription.updated
customer.subscription.deleted
```

`charge.refunded` is the one people leave out, and without it a refund is free model spend: the
money goes back to the card and the credit stays on the balance. The two dispute events are the
same for a chargeback: the disputed amount stops being spendable while the dispute is open, and
comes back if it is won. Refunds and disputes are tied to an account through the top-up the payment
was credited under — a dispute carries no customer and none of our metadata — so they only ever
take back what a top-up put in, and a refunded subscription fee takes nothing.

### Out-of-order events and organisation metadata

**Stripe does not deliver these in order.** One real $499 purchase arrived `invoice.paid`,
`customer.subscription.updated` (active), `customer.subscription.created` (incomplete),
`checkout.session.completed` — the third describing the subscription as it was before the second.
Four rules follow, all in `billing.go`. An invoice that names no organisation we know is resolved
through `parent.subscription_details.metadata`, because the first one routinely lands before
anything has bound the customer. And an `incomplete` snapshot never unsets a subscription already
recorded as `active` or `trialing`: a subscription that has gone live never returns to
`incomplete`, so that snapshot is an echo. Only `incomplete` is treated that way — `past_due` and
`active` genuinely alternate as a card fails and recovers. A subscription that has ended stays
ended, too: Stripe gives a returning customer a new subscription with a new id, so a checkout,
invoice or subscription event that names one already cancelled is a late delivery and is recorded,
not acted on. And an event already acted on is skipped when Stripe delivers it again.

**Only this server's own metadata names an organisation.** Checkout puts `attesttag_org` on the
session, the subscription and the payment intent, and that is the one claim an event is read by.
`client_reference_id` is not: this checkout sets it, but so can anybody paying a Stripe Payment
Link, by adding it to the URL. So a one-off payment without the metadata — a Payment Link, an
enterprise deal's `pay_url` — is recorded and never becomes credit. An event that would bind a
second Stripe customer to an account, or a customer to a second account, is refused and raised
with the operator rather than written over the real ones.

### `invoice.paid` and the included allowance

`invoice.paid` is the one a size with an included allowance depends on. It is the event that says
a period was paid for — the first invoice at checkout and every renewal after it. The allowance is
keyed on the period it belongs to, which is what makes a redelivery a no-op and next month a
genuine second grant. A subscription Stripe has marked `past_due` is handed no new allowance when
its period moves on, because Stripe moves the period while it is still retrying the card; the month
arrives with the `invoice.paid` that shows it was paid. Leave that event out and subscribers are
charged and never credited.

An upgrade charged on the spot is paid by an `invoice.paid` too, but that invoice has no line for
the subscription itself: only two prorations over what is left of the period, first the credit for
the unused time on the old Price and then the charge on the new one, both dated from the moment of
the change. So the size is read from the subscription's own line where there is one and otherwise
from the line that charges, and a period only ever from the subscription's own line — the
subscription events carry the real one.

### The webhook's API version

**A webhook is not delivered in the version `stripeAPIVersion` pins.** That constant governs the
replies to calls this code makes. An event is rendered in the API version of the endpoint it is
delivered to, set from Stripe's account default on the day that endpoint was created — so an
endpoint added today speaks a version years newer than the parser, and says nothing about it. Four
fields moved in `2025-04-30.basil`: a subscription's `current_period_start`/`_end` went onto its
items, an invoice's `subscription` went to `parent.subscription_details.subscription`, an invoice
line's `price` went to `pricing.price_details.price`, and its `proration` flag went under the line's
`parent`. `billing.go` reads both spellings, so neither version needs configuring — but a fifth
field moving would be just as quiet, and the symptom to recognise is a paid account whose plan is
right and whose size, fee or renewal date is blank. `curl https://api.stripe.com/v1/webhook_endpoints`
prints each endpoint's `api_version`; comparing it with the constant is the five-second version of
that diagnosis.

## What a size is counted in

Users. A user is somebody in a connected Slack workspace or Microsoft Teams organisation who asked
the bot something in the last 30 days, rolling. The figure is on Settings → Billing, next to the balance and the month's spend,
and the rows behind it are on Activity → People.

Deliberately not counted: a console sign-in (most admins configure the thing and never use it in
Slack), the owner of an API key or an MCP client, the creator of a routine that has been running on
its own since they left, and forwarded email — the intake lane is keyed per channel rather than per
sender, so there is no person there to count. The rule is one function, `countsAsUser`, and one call
site, in the turn path past every gate a refused turn hits.

**Nothing is refused for being over the size.** The console says so in an amber note, the operator
index shows it beside the console seat count, and that is all. Stopping the twenty-sixth person
mid-conversation in Slack, for a reason only an admin can fix, is the wrong end of the product to
put a commercial limit on.

The count is stored rather than derived. `active_users` holds one row per person per day and is
**not** swept by `data_retention_days` — the note in `retention.go` says why: it is the basis of
what an account is charged, and a customer able to shrink it by shortening their own retention
would be holding a control over their own bill. It has its own 400-day ceiling instead. Removing
a workspace does not take its people out of the count, nor its spend out of the month — otherwise
removing one and adding it back would reset both.

Somebody in two connected workspaces is two ids — Slack only promises a user id is unique inside
one workspace. They are counted once where both give an email address — a Slack install that
granted `users:read.email`, or any Teams organisation, which always does — by a hash of the
address; where the scope was never granted there is nothing to match on and they count twice. The per-workspace breakdown on the Billing screen is what makes that visible rather than
mysterious. The hash is a dedupe key and not anonymisation: it is stored instead of the address
because this table outlives the tenant's retention policy, not because a hashed address at a known
domain is hard to guess.

Size ceilings live in `sizeUserLimits` beside `sizeLabels` in `config.go`, not in `STRIPE_SIZES` —
a limit typed into a deployment's environment is a limit that can disagree with the name printed
next to it.

## The size that is a conversation

`over_250=:0` in `STRIPE_SIZES` lists a size with no Price. On the Billing screen it is a row like
the others, and picking it turns *Continue to payment* into **Talk to us**: `POST
/api/billing/size-request` mails `SUPPORT_EMAIL` — subject "Upgrade request from <organisation>",
Reply-To the person who pressed it — with what whoever answers needs before replying: the plan the
account is on, how many people used the bot in the last 30 days and per workspace, this month's
spend and fix jobs, the credit left, and an optional note typed on the screen — up to 1,000
characters, quoted in the mail so that a link typed into it stays text. Three a day per
organisation, because each one lands in a person's inbox; the organisation remembers it asked
(`size_request_at`), so the screen says when after a reload; and with no `SUPPORT_EMAIL` the route
refuses and the row stays quoted rather than mailing a stranger. It is a server-side send, never a
`mailto:` — the figures belong in the message, not in a draft the customer has to send.

## What a size includes

A size may carry model credit with the fee: `upto_50=price_x:19900:2000` is $199 a month with $20
of credit in it. The allowance is a separate figure from the fee on purpose — a size can be
repriced without silently repricing what it includes, and the fee itself is never credited, which
would hand every subscriber their money straight back as model spend.

**It expires with the month it came with, and a top-up does not.** That is the whole difference
between the two, and the console says it on the screen. An allowance belongs to the period it was
billed for: $100 a month means $100 this month, not $100 added to a pile that grows for a customer
who underspends. Credit somebody bought is theirs until they spend it.

So a turn spends the allowance FIRST and prepaid credit second — the expiring money goes first,
because the alternative spends a customer's own balance while an allowance they already paid for
inside the fee evaporates beside it.

It is **not on the statement**, and that is deliberate. `credit_ledger` is the record of money
taken and money spent, which is why nothing sweeps it; an allowance is part of a fee rather than a
purchase. Putting it there would also mean writing a negative "unused" row every month for every
subscriber in that same table. It lives in four columns on `billing_accounts` instead
(`0014_credit_allowance.sql`), and `credit_balance_micros` stays exactly what it was: a cache of
`sum(credit_ledger)` minus unrolled spend.

### Metering and granting the allowance

An account with a live allowance **is metered** — it stops at the same $50 overdraft a paying one
does, or "includes $100" would be a figure nothing ever held anyone to. Note what that does not
do: it does not set `credit_enforced`. That flag never goes back off, so setting it from a plan
would leave an account whose subscription later ended with the floor on, no allowance and a zero
balance. The floor applies when `credit_enforced` is 1 **or** the allowance period is live, and
the second half lapses on its own.

The allowance is a property of the size and the period rather than an event, which is why
`SyncAllowance` runs from subscription events, invoice payment, the console's size change, the
operator's comp, saving an enterprise deal, and the hourly billing loop as a catch-all. The first
version granted it from `invoice.paid` alone, and a size change then produced no invoice — Stripe
prorated it onto the next one — so an account that upgraded saw its new plan promising credit next
to a balance of zero until its next renewal.

**A move up mid-period brings the share of the difference the period has left**, the way Stripe
charges the fee: upgrading on the last day of the month pays a thirtieth of the difference, so it
brings a thirtieth of the extra allowance, not all of it — otherwise an account could upgrade,
take the whole allowance and schedule the downgrade back for nothing. Billing's *Credit included*
line then names this month's grant beside the monthly figure, because the two differ. An operator
raising the figure of the same deal is not a purchase, and that arrives whole. A move down changes
nothing until the period it was paid for ends.

### Fix jobs a size includes

**And fix jobs.** A size is also sold with a number of fix jobs a month — `sizeJobLimits` beside
the user ceilings in `config.go`, and `freePlanJobsPerMonth` for the free plan. A fix job is a
container run, the one cost credit does not meter, so no plan is sold without a figure and none
is sold as "unlimited". The figure is printed on the pricing page and beside each size in the
picker, and Billing shows this calendar month's count against it (`jobs` on `GET /api/billing`).
Nothing is refused past it, for the reason nothing is refused over the user ceiling; a size
absent from the map, which is what the "Talk to us" size is, prints the count alone.

## What the webhook does

Every event is recorded in `billing_events` by its own id once everything it does has happened,
and an event already on record is skipped when Stripe delivers it again — replaying one is not
harmless, because it would write back a state a later event has since changed. A failure before
that point answers 500, so Stripe's retry runs the event again rather than being told a cheerful
200 over money that never moved. Event ids are kept for 30 days; the record that a subscription has
ended is kept for good.

A top-up is keyed on the **payment**, not the event, because Stripe reports one payment under
several event types, and every ledger row is keyed on the payment, refund or dispute behind it — so
four deliveries of one payment credit once, and a retry never moves money twice. A duplicate
answers 200: a replay is a success, and anything but a 2xx makes Stripe retry, for about three
days.

- A **failed invoice** sets `past_due` and mails the account. It does **not** drop the plan — Stripe
  retries a card for weeks, and cutting a paying customer off on the first decline is the wrong
  failure.
- A subscription that is **cancelled, unpaid or expired** moves the plan back to free and leaves
  the credit alone. It is prepaid money and still theirs, and the floor keeps binding on what is
  left. An enterprise account is the exception: a payment never moves it, so it stays on its deal
  and `SUPPORT_EMAIL` is mailed that the subscription has lapsed.
- A **refund** writes a negative line and lets the balance go below zero. Clamping it would make a
  chargeback a way of getting free credit.

## Coming back from Stripe

A success page proves nothing — the money is Stripe's news to give, not the browser's. So the
console asks the server to read the session back once on the way back (`GET /api/billing?settle=1`),
which removes the race rather than papering over it, and is idempotent with the webhook because both
key the ledger on the same payment. `pending_checkout` says whether that worked: false means the
money is already in the figures beside it, true means the page should say "updating…" and poll
rather than show a balance it is about to contradict.

The session id arrives in a URL the browser controls, so it is a claim and is checked like one.
Only a member who may manage billing settles, at most 20 times an hour per organisation, because
each one is a read at Stripe on the deployment's key; past that the page shows "updating…" and
waits for the webhook. The session must carry this organisation in our own metadata, be less than
an hour old and complete, and a subscription session must be paid and must not name a subscription
that has since ended — otherwise a replayed old session would restore the size it bought.

If somebody writes in saying "I paid and nothing happened", look at `billing_events` for the event
id, then the Stripe dashboard's own delivery log. The usual answer is a webhook delivered while the
service was restarting — the gate answers 503 to everything during a deploy, and Stripe retries for
about three days.

## The statement

`credit_ledger` is append-only and is the record of money. Two things about it are worth knowing:

- **Retention never touches it.** `usage` is swept on each account's own `data_retention_days`, so
  spend can never be recomputed; the ledger is the only surviving record and an admin shortening
  their own history must not shorten it. It goes when the organisation goes, and not before.
- **Model spend is written up once an hour, not once a turn.** The balance moves immediately — that
  is what the gate reads — and the roll-up turns the hour's spend into one line. A line per turn
  would be thousands of $0.003 rows a month in the one table nothing is allowed to sweep.

## Granting credit without a payment

`<your-origin>/operator` is the account list, and each account's page has the money actions on it:
move the plan, record a size as **comped** (nothing is charged, but every other page reads it like a
paid one), grant credit, and cancel a real subscription. All of it is behind `OPERATOR_SECRET` and
none of it is reachable from a console session — a member who could grant their own credit would
make the whole thing advisory.

**A comped size cannot be changed by the account.** `compSize` refuses to touch an organisation
that has a live subscription at Stripe — that guard is what stops the local record disagreeing with
what is being charged — and an account whose plan was granted has no subscription to send to the
portal. So the console tells them to ask whoever set it up, and that is you, here.

A grant goes on the statement as a grant, with no payment behind it, and **switches the credit floor
on**: a comped account with $100 stops when it runs out, exactly like one that paid. A negative
amount takes credit back and is recorded as an adjustment.

The same two from a terminal:

```bash
./deploy/plan.sh <id> credit 100 "beta partner"   # grant $100, no payment
./deploy/plan.sh <id> credit -25 "correction"     # and take some back
./deploy/plan.sh <id> size upto_50                # record a size, comped
```

An account past the top of the ladder gets an **enterprise** deal instead of a size: its own user
ceiling, jobs, included credit and fee, paid by subscribing to a Stripe Price made for it, at a link,
or by invoice. See [Enterprise](plans.md#enterprise).
