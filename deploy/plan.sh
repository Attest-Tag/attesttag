#!/bin/zsh
# Move an account between the free, pro and enterprise plans, or find the account behind a support
# email.
#
#   ./deploy/plan.sh <id>                  # that organisation → pro, with the default $25 a month
#   ./deploy/plan.sh <id> 50               # → pro with a $50 monthly budget (also: <id> pro 50)
#   ./deploy/plan.sh <id> free             # and back again
#   ./deploy/plan.sh <id> credit 100 "beta partner"   # grant $100 of API credit, no payment
#   ./deploy/plan.sh <id> credit -25 "correction"     # and take some back
#   ./deploy/plan.sh <id> size upto_50                # record a size, comped: nothing is charged
#   ./deploy/plan.sh <id> enterprise users 400 jobs 900 included 300 budget 2000 price price_1Q…
#                                          # → enterprise: its own deal, paid by subscribing to
#                                          #   a Stripe Price made for it, from its Billing screen
#   ./deploy/plan.sh <id> enterprise link https://buy.stripe.com/…   # …or paid at a link
#   ./deploy/plan.sh <id> enterprise fee 30000 per year              # …or invoiced by hand
#   ./deploy/plan.sh <id> enterprise users 600                       # change one figure of a deal
#   ./deploy/plan.sh someone@example.com   # which organisations does that address belong to?
#   ./deploy/plan.sh                       # every organisation on the deployment
#
# <id> is the organisation's public id: 32 hex characters, printed as "id" by the two listing
# forms above and carried by the operator link in a support email. It is not a row number — the
# serial stays inside the server, so that neither a link nor a listing says how many accounts
# exist or what the next one along is called.
#
# credit and size need billing configured on the server (STRIPE_SECRET_KEY and
# STRIPE_WEBHOOK_SECRET); without it those two routes do not exist and answer 404, like the rest
# of billing. A granted amount goes on the account's statement as a grant with no payment behind
# it, and switches the credit floor on — a comped account then stops when it runs out, exactly
# like one that paid. Everything here is also on the page at <base>/operator.
#
# enterprise takes key/value pairs, every one optional, and a change names only what it changes:
# users N (0 = no ceiling) · jobs N (fix jobs a month) · included USD (model credit each month) ·
# budget USD (monthly ceiling; 0 = none) · fee USD · per month|year · price price_… · link https://…
# · note "…" (yours; the account never sees it). A price is read from Stripe when it is named and
# the fee is taken from it; naming a price clears the link and the other way round, and price ""
# or link "" clears one. Moving an account off enterprise is `<id> pro` or `<id> free` — refused
# while it is still subscribed to its enterprise price, so cancel that first. No payment ever moves
# an enterprise account: a lapsed subscription mails support instead.
#
# The budget granted with a pro plan is that account's own ceiling: the console lets them set
# anything up to it and nothing past it, and PLATFORM_MONTHLY_BUDGET_USD_PER_ORG does not hold
# it down — the operator decided this one by hand.
#
# OPERATOR_SECRET comes from the shell, else from ENV_FILE, else .env.prod when there is one, else
# .env — the same file deploy/gcp/cloudrun.sh ships to Cloud Run, so the value here is the one the
# server holds. The target is BASE_URL, else ADMIN_BASE_URL from that file; it is required, with
# no default. The secret travels in the Authorization header, never in the URL.
set -euo pipefail
SELF="$0"
cd "$(dirname "$0")/.."
if [ -z "${ENV_FILE:-}" ]; then
  ENV_FILE=.env
  [ -f .env.prod ] && ENV_FILE=.env.prod
fi
# The last line with a value, as deploy/gcp/cloudrun.sh reads it: an empty template line must not
# hide a value appended below it, or this would send a different secret from the one deployed.
envval() {
  [ -f "$ENV_FILE" ] || return 0
  grep -E "^$1=." "$ENV_FILE" | tail -n 1 | cut -d= -f2- | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/"
}
SECRET="${OPERATOR_SECRET:-$(envval OPERATOR_SECRET || true)}"
[ -n "$SECRET" ] || { echo "OPERATOR_SECRET is neither in the shell nor in $ENV_FILE; nothing can be moved" >&2; exit 1; }
BASE="${BASE_URL:-$(envval ADMIN_BASE_URL || true)}"
BASE="${BASE%/}"
# No default target. Sending the operator secret to a hard-coded https://app.attesttag.com meant a
# self-host that forgot BASE_URL would hand its secret to the maintainer's service. Make the
# address explicit.
[ -n "$BASE" ] || { echo "set BASE_URL to your console's address, e.g. BASE_URL=https://app.example.com (or ADMIN_BASE_URL in $ENV_FILE)" >&2; exit 1; }

# One request: the body pretty-printed when it is JSON, and a plain-English reason on failure.
# The secret goes in via a stdin header file (curl -H @-), never as an argv element — an argument
# is visible in `ps` to any other user on the box for the life of the request.
run() {
  local out code body
  out="$(printf 'Authorization: Bearer %s' "$SECRET" | curl -sS -w '\n%{http_code}' -H @- "$@")"
  code="${out##*$'\n'}"
  body="${out%$'\n'*}"
  printf '%s' "$body" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$body"
  case "$code" in
    2*) ;;
    401) echo "▸ 401: the secret here is not the one the server holds. Redeploy after changing $ENV_FILE, or fix the file." >&2; exit 1 ;;
    404) echo "▸ 404: no such organisation — or OPERATOR_SECRET is not set on the server, in which case the routes do not exist. Run deploy/gcp/cloudrun.sh after adding it." >&2; exit 1 ;;
    429) echo "▸ 429: too many attempts from this address; try again in an hour." >&2; exit 1 ;;
    *)   echo "▸ HTTP $code" >&2; exit 1 ;;
  esac
}

case "${1:-}" in
  "")
    run "$BASE/api/operator/orgs" ;;
  *@*)
    # The support mailbox knows who wrote; this finds which organisation they meant.
    EMAIL="$(printf '%s' "$1" | python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read().strip()))' 2>/dev/null || printf '%s' "$1")"
    run "$BASE/api/operator/orgs?email=$EMAIL" ;;
  *)
    usage() { echo "usage: $SELF [<org id> [pro|free] [<budget usd>] | <org id> <budget usd> |
                  <org id> credit <usd> [note] | <org id> size <size key> |
                  <org id> enterprise [users N] [jobs N] [included USD] [budget USD] [fee USD]
                                      [per month|year] [price price_…] [link https://…] [note …] |
                  <email>]
  <org id> is the 32-character public id shown as \"id\" by '$SELF' and '$SELF <email>'." >&2; exit 2; }
    # A public id, not a row number. Anything else — a stray digit, half a pasted id — is a
    # typo, and the check is here so it reads as one instead of as a 404 from the server.
    [[ "$1" =~ '^[0-9a-fA-F]{32}$' ]] || usage
    ORG="${1:l}"
    # Credit and size before the plan parsing below, because both take a value where a plan
    # would go and neither is a plan.
    if [ "${2:-}" = credit ]; then
      AMOUNT="${3:-}"
      [[ "$AMOUNT" =~ '^-?[0-9]+(\.[0-9]+)?$' ]] || usage
      NOTE="${4:-Granted by the operator}"
      BODY="$(AMOUNT="$AMOUNT" NOTE="$NOTE" python3 -c 'import json,os; print(json.dumps({"amount_usd": float(os.environ["AMOUNT"]), "note": os.environ["NOTE"]}))')"
      run -X POST "$BASE/api/operator/orgs/$ORG/credit" -H "Content-Type: application/json" -d "$BODY"
      echo "▸ credit granted; the balance above is what the account may now spend" >&2
      exit 0
    fi
    if [ "${2:-}" = enterprise ]; then
      shift 2
      # Key/value pairs into JSON, with the field names the server takes. Only what was named is
      # sent, which is what makes "enterprise users 600" change the users and nothing else.
      BODY="$(python3 - "$@" <<'PY'
import json, sys
keys = {"users": ("users", int), "jobs": ("jobs", int), "included": ("included_usd", float),
        "budget": ("budget_usd", float), "fee": ("fee_usd", float), "per": ("interval", str),
        "price": ("price_id", str), "link": ("pay_url", str), "note": ("note", str)}
args, out = sys.argv[1:], {"plan": "enterprise"}
if len(args) % 2:
    sys.exit("enterprise takes key value pairs; the keys are " + ", ".join(keys))
for k, v in zip(args[::2], args[1::2]):
    if k not in keys:
        sys.exit(f"no field called {k!r}; the keys are " + ", ".join(keys))
    name, kind = keys[k]
    try:
        out[name] = kind(v)
    except ValueError:
        sys.exit(f"{k} wants a number, not {v!r}")
print(json.dumps(out))
PY
)" || usage
      run -X PUT "$BASE/api/operator/orgs/$ORG/plan" -H "Content-Type: application/json" -d "$BODY"
      echo "▸ organisation $ORG is on the enterprise plan; \"enterprise\" above is the deal its Billing screen shows" >&2
      exit 0
    fi
    if [ "${2:-}" = size ]; then
      SIZE="${3:-}"
      [ -n "$SIZE" ] || usage
      run -X POST "$BASE/api/operator/orgs/$ORG/size" -H "Content-Type: application/json" -d "{\"size\":\"$SIZE\"}"
      echo "▸ size recorded as comped — nothing is being charged for it" >&2
      exit 0
    fi
    PLAN="${2:-pro}"; BUDGET="${3:-}"
    # A number where the plan would go means "pro, with this budget".
    if [[ "$PLAN" =~ '^[0-9]+(\.[0-9]+)?$' ]]; then BUDGET="$PLAN"; PLAN=pro; fi
    [[ "$PLAN" == pro || "$PLAN" == free ]] || usage
    if [ -n "$BUDGET" ]; then
      [[ "$BUDGET" =~ '^[0-9]+(\.[0-9]+)?$' ]] || usage
      BODY="{\"plan\":\"$PLAN\",\"budget_usd\":$BUDGET}"
    else
      BODY="{\"plan\":\"$PLAN\"}"
    fi
    run -X PUT "$BASE/api/operator/orgs/$ORG/plan" -H "Content-Type: application/json" -d "$BODY"
    if [ "$PLAN" = pro ]; then
      echo "▸ organisation $ORG is now on the pro plan with a \$${BUDGET:-25} monthly budget" >&2
    else
      echo "▸ organisation $ORG is now on the free plan" >&2
    fi ;;
esac
