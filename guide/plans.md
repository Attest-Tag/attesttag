# Plans and the operator API

> **Most deployments want none of this.** Plans exist for a service handing accounts to
> strangers on a key somebody else pays for. On a deployment spending its own key the whole
> mechanism is off by default: `FREE_PLAN_BUDGET_USD` is 0 (no free-plan cap — use
> `MONTHLY_BUDGET_USD` instead), `SUPPORT_EMAIL` is empty so budget refusals name nobody and
> the Upgrade button is not offered, `OPERATOR_SECRET` is empty so the operator routes do
> not exist, and the Stripe keys are empty so billing does not exist either — no Billing tab,
> no checkout, no credit floor. The rest of this section describes what happens when you turn
> them on.

Two ways an account becomes a paying one, and a deployment can have either, both or neither.
Without Stripe configured, only the first exists and this page reads as it always did: somebody
presses **Upgrade**, a person reads the request, and the operator moves the plan by hand. With
Stripe configured, an account buys its own plan with a card and the operator path stays as the
override — for a comp, a deal signed on paper, or an account that needs unpicking.
[`billing.md`](billing.md) is the second one. This page is the first, and the operator API both
of them share.

## The free plan and the Upgrade button

Every organisation starts on the **free** plan — with `SIGNUP_MODE=open`, that means everybody who
signs up: `FREE_PLAN_BUDGET_USD` a month on the shared model key, whatever its own
`monthly_budget_usd` says. The Settings page shows
the enforced number with the field locked, and **Upgrade** beneath it: one press and
`POST /api/plan/raise-budget` writes the request and sends it to `SUPPORT_EMAIL`, from the
deployment's own sending address with the asker on `Reply-To`, so answering the mail answers them.
Three asks per account per day, and only from a member who holds `settings.manage` — the permission
on the field the answer opens. The ask is recorded on the organisation (`plan_request_at`), so the
button reads **Requested** afterwards rather than sending a second message, and moving the plan
either way clears it. When the month's spend reaches the budget the console also carries a strip
under the topbar on every page — "the bot has stopped replying" — with the same Upgrade button on it
(a pro account gets a link to its own budget field instead); `/api/me` carries the plan, the spend
and the budget that decide it. The bot's refusal in Slack and the alert name the address as before.
There is no self-serve upgrade: a person reads the request and decides.

## The operator page

That message carries a link, `<your-origin>/operator/plan?org=<id>`, and the reply is one
click on it. The link is written into the message rather than shown to the account asking: the console
used to open a pre-filled `mailto:` draft carrying it, which put the URL that moves plans in front of
everybody who wanted moving. The page asks for `OPERATOR_SECRET` the first time (the browser then
remembers a proof derived from it for 12 hours — rotating `OPERATOR_SECRET` or `MASTER_KEY` ends
every such cookie) and shows the organisation, its spend, a monthly budget field ($25 unless you
change it) and the button that moves it: **Move to Pro**, or **Move back to Free** for an account
already on pro. The **Enterprise** card is below it (see [Enterprise](#enterprise)), and on a
deployment that sells plans so are the credit, size and subscription cards [`billing.md`](billing.md)
describes. The link is safe wherever it ends up: without the secret the page shows nothing about the
organisation and changes nothing, and a GET never changes a plan, so a mail client prefetching the
link cannot either.

## The budget a pro plan grants

The budget granted with a pro plan (`orgs.plan_budget_usd`) is that account's own ceiling: it starts as
the account's `monthly_budget_usd`, the console lets a pro account lower it or raise it back up to the
grant, and `PLATFORM_MONTHLY_BUDGET_USD_PER_ORG` does not hold it down, since the operator decided this
one by hand. A pro account moved without a figure is granted $25, which is then its ceiling like any
grant; one a payment moved has no grant, and sits under `PLATFORM_MONTHLY_BUDGET_USD_PER_ORG` until it
holds credit. On a deployment that sells plans, a pro account with a live subscription and credit is
held by neither: its own monthly budget and its credit are what stop it
([billing.md](billing.md#the-two-limits-and-which-one-binds)).
`!usage`, the Overview page and `GET /v1/usage` all report the plan and the budget actually enforced.

## The operator API and plan.sh

The same two things are reachable from a terminal. `deploy/plan.sh` takes `OPERATOR_SECRET` from the
shell, else from `ENV_FILE`, else `.env.prod` when there is one, else `.env`; and the deployment's
address from `BASE_URL`, else from that file's `ADMIN_BASE_URL`. There is no default address — a
self-host that forgot to name one would otherwise send its secret to somebody else's service — so
set `BASE_URL` if your env file does not pin the origin. Then it does the curl for you:

```bash
./deploy/plan.sh <id>                  # that organisation → pro with the default $25 a month
./deploy/plan.sh <id> 50               # → pro with a $50 monthly budget
./deploy/plan.sh <id> free             # and back
./deploy/plan.sh someone@example.com   # which organisations that address belongs to
./deploy/plan.sh                       # every organisation on the deployment
```

`<id>` is the organisation's public id, the 32 hex characters the two listing forms print as `id`.

Underneath, it is two requests. The secret goes in the `Authorization` header, never a query string;
every attempt from an address counts against a limit of 100 an hour, right or wrong; and with
`OPERATOR_SECRET` unset the routes answer 404. A secret shorter than 16 characters counts as unset,
with a warning at startup: a short secret in front of a write API is a guessable one.

```bash
# The organisations an address belongs to (omit ?email= for every organisation on the deployment).
curl -sS "$BASE/api/operator/orgs?email=someone@example.com" \
  -H "Authorization: Bearer $OPERATOR_SECRET"

# Move one to pro with a $50 monthly budget (omit budget_usd for $25), or back to free.
curl -sS -X PUT "$BASE/api/operator/orgs/$ORG_ID/plan" \
  -H "Authorization: Bearer $OPERATOR_SECRET" -H "Content-Type: application/json" \
  -d '{"plan":"pro","budget_usd":50}'
```

On a deployment that sells plans there are two more, answering 404 without the Stripe keys:
`POST /api/operator/orgs/<id>/credit` with `{"amount_usd": 100, "note": "…"}` grants credit (a
negative amount takes it back), and `POST /api/operator/orgs/<id>/size` with `{"size": "upto_50"}`
records a size as comped. `plan.sh <id> credit` and `plan.sh <id> size` are those two.

## The plan record and its two writers

The `plan` and `plan_budget_usd` columns on `orgs` are the whole record; a plan change is logged as
`org plan changed` with the budget and who moved it — `by=operator`, or `stripe:<event id>` for a
payment — is written to the organisation's own audit log as `plan.changed`, and takes effect on the
next turn. The operator's address stays in the server's log and never reaches the organisation's
records.

**Two writers, and which one wins.** With billing configured, `orgs.plan` is written by an
operator's hand *and* by a Stripe webhook, and they can disagree — an account moved to free here
while a subscription is still live at Stripe goes back to pro on that subscription's next event.
The webhook wins, because Stripe is what is actually charging the card. So the operator page shows
the subscription when there is one, and the honest way to stop charging somebody is **Cancel the
subscription** on that page rather than moving the plan: that cancels at Stripe and lets the
webhook move the plan, so there is one path and nothing to disagree about. `POST
/api/plan/raise-budget` is refused outright on a deployment that sells plans — it asks a person to
do by hand, over hours, what a card does in thirty seconds.

## Enterprise

The top of the ladder is "Talk to us", and this is what the conversation ends in: the operator moves
the account onto the **enterprise** plan and writes its deal — its own user ceiling, fix jobs a month,
model credit included each month, fee and monthly budget — instead of picking a size. The account's
Settings → Billing shows the deal in place of the size picker, and nothing on that screen changes it.

On the operator page (`<your-origin>/operator/plan?org=<id>`) it is the **Enterprise** card. From a
terminal it is `plan.sh <id> enterprise` followed by the fields you are setting, each one optional; a
change names only what it changes:

```bash
./deploy/plan.sh <id> enterprise users 400 jobs 900 included 300 budget 2000 price price_1Q…
./deploy/plan.sh <id> enterprise users 600                  # the size, and nothing else, changes
./deploy/plan.sh <id> credit 1000 "prepaid per the order form"   # credit is the same as on any plan
./deploy/plan.sh <id> pro                                  # off enterprise (the deal is deleted)
```

Underneath it is the same `PUT /api/operator/orgs/<id>/plan`, with `"plan":"enterprise"` and any of
`users`, `jobs`, `included_usd`, `budget_usd`, `fee_usd`, `interval` (`month`/`year`), `price_id`,
`pay_url` and `note`. `budget_usd` is the monthly ceiling on model spend and is part of the deal:
bought credit does not lift it and the deployment ceiling does not hold it down. 0 means the deal sets
no ceiling, and the account's own monthly budget and its credit are what stop it. `note` is yours; it
is never shown to the account or written into its audit log.

### How an enterprise account pays

**How the account pays** is one of three, and the Billing screen offers what fits:

- **A Stripe Price made for it** (`price_id`). In the Stripe dashboard, add a Price to your product —
  recurring, flat, monthly or yearly, in the deployment's currency — and give its id. It is read from
  Stripe when you name it, so a typo or an archived, one-off, tiered or wrong-currency price is refused
  there and then, and the fee shown is the price's. The account's admin presses **Subscribe**, pays
  through Checkout, and the webhook keeps the subscription on record exactly as it does for a size.
  The deal's included credit starts with the subscription.
- **A link** (`pay_url`, https only) — a Stripe Payment Link, a hosted invoice, anything with a page.
  Billing shows a **Pay** button that opens it. Nothing here can see those payments; reconcile them by
  hand.
- **Neither**: invoiced by hand. Billing says so and offers nothing to press. An invoice sent from the
  Stripe dashboard to the account's customer is recorded when it is paid and renews nothing — only a
  subscription's invoice does that.

A deal paid at a link or by invoice is live from the moment it is saved: its included credit starts
then and renews a month at a time whether or not a payment has been seen here. Included credit always
comes a month at a time, even on a yearly price, so a year's fee never hands over a year's credit at
once.

Naming a price clears the link and the other way round; `price ""` or `link ""` clears one. The deal's
price cannot be swapped while the account is subscribed to it — cancel the subscription on the
operator page first.

### What a payment does to an enterprise deal

**No payment ever moves an enterprise account.** A subscription that renews confirms the deal, and one
that is cancelled or goes unpaid mails `SUPPORT_EMAIL` and stops the included credit, but the account
stays on enterprise until you move it. Moving it off is refused while it is still subscribed to its
enterprise price, so Stripe is never left charging a deal the plan no longer says it has. The other way
round, an account with a live subscription to a size on the ladder has to end it before a deal can
replace it.

Without Stripe keys an enterprise deal is still a plan: the budget and the figures apply and Billing is
simply not there to show them. A price needs Stripe, and so do credit and the included allowance, like
everything else in [`billing.md`](billing.md).

### Its own model key

An organisation can run on its own model provider instead of the deployment's: its admins open
Settings → Models → **Your model key**, pick OpenAI, OpenRouter or any OpenAI-compatible endpoint
(Azure OpenAI's v1 address is `https://<resource>.openai.azure.com/openai/v1`, and its models are the
deployment names), paste a key, choose the default and embedding models, and save. They can replace,
test or remove it whenever they like; nobody writes to support. Who may do this is `ORG_MODEL_KEYS`:
a self-host defaults to `all`, so every organisation there can rotate its key from the console rather
than by redeploying; `enterprise` limits it to accounts on the enterprise plan, which is what the
hosted service sets; and `off` lets nobody bring one. Where an account may not, the section is a
locked line naming the Enterprise plan rather than a form.

The embedding model is optional, but without one document search is off while the key is in use —
it says so, and an admin can choose one under Settings → Models — rather than embedding the account's
documents on the deployment's key.

While a key is in use, **everything the account causes runs on it**: replies in every channel, the
watcher, the allow-rule checker and the thread summariser, the console assistant, its documents'
embeddings, and its fix jobs. Its spend is its provider's to bill — recorded here like any other
(`usage.key_owner = 'org'`), charged to no credit, and held by none of the deployment's ceilings: not
the free plan's, not `PLATFORM_MONTHLY_BUDGET_USD_PER_ORG`, not a deal's `budget_usd`. Its own monthly
budget still applies, and one it never set is no limit. OpenAI reports tokens and no price, so calls
there are priced at list price from the deployment's catalogue and marked `~$` in the reply footer;
an endpoint whose models are in no catalogue is unpriced, and a monthly budget cannot stop it.

#### When the model key fails

**Nothing falls back.** A key that is refused, out of quota or unreachable stops the account's model
calls with a sentence that says why and where to fix it — in the thread, as a red strip on every
console page, and once an hour in the alert channel — and nothing is sent to the deployment's key
instead. Where `ORG_MODEL_KEYS=enterprise`, an account moved off enterprise keeps its key stored and
its calls refuse until an admin removes it. `PUT /api/operator/orgs/<id>/plan` — and so `plan.sh` —
warns about that when it makes the move, and `GET /api/operator/orgs` shows where an account's key
points and whether it works. Never the key itself.

#### Saving, replacing and removing the key

Saving is checked before it is stored: one real completion on the chosen model, and one embedding
when there is an embedding model. A new key, a new address, or removing the key asks for the same
proof as turning two-factor off — each decides where every conversation goes — is written to the audit
log as `model_key.saved` or `model_key.removed`, and is mailed to every member holding
`settings.manage`. A stored key is only ever sent to the address it was saved for, and read in the same
query as that address, so a key rotated on one instance is never sent to the old address by another.
While a key is in use, the account's monthly budget measures what that key spends; once it is removed,
the deployment's ceilings measure only what the deployment's key spends, so an account never comes back
to the included models already over a ceiling it spent nothing against. Changing the
embedding endpoint or model re-indexes the account's documents in the background; a search in the
meantime says the documents are being indexed again. A fix job runs the repository's own code with
the model key in its environment, so the key form has a **Fix jobs** switch. It is off for a new key,
switching it on asks for the same proof as a new key, and while it is off fix jobs are refused while
the key is in use.
