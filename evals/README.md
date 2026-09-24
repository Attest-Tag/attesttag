# Evals

`cases.json` holds real Slack-style asks with the tool(s) each should trigger and a regex the
answer must match. Run them against the live bot (needs `SELF_TEST=1` and the pilot channel):

    go test -run TestEvals -eval ./...

Each case posts a `[selftest]` message, waits for the reply, then checks the `tool_calls` table
and the reply text. Re-run whenever the model, provider or system prompt changes.

The `connection_*` cases need a bundle attached to the pilot channel with a bearer connection
for `httpbin.org` (any token) — create it in the console under Access bundles.

## HTTP staging setup

Live evals must run on a staging host where the bot and eval process share the same `DB_PATH`.
Use a separate Slack app/workspace, configure its Signing Secret as `SLACK_SIGNING_SECRET`,
and point its HTTPS Request URLs at `/slack/events` and `/slack/interactions`. A local staging
host needs an HTTPS tunnel. Disable Socket Mode in that testing app.

Start the bot with `SELF_TEST=1`, sign up in its console, and connect the staging workspace
through Add to Slack so its bot token is stored with the correct organization. Invite it to
an isolated eval channel. Set `EVAL_CHANNEL`, `DB_PATH`, and `SLACK_BOT_TOKEN` for the eval
process; that token is only how the test driver posts messages, not how the bot authenticates
or discovers installations. Provide model credentials to the staging bot. Never use production.

Run `go test ./internal/app -run '^TestEvals$' -eval -v -timeout 30m` from the checkout on that
host. The existing bundle/repository prerequisites still apply.

## CI

`.github/workflows/evals.yml` runs offline unit/integration tests and the Go vulnerability
scan. It is manual (`workflow_dispatch`) — it does not run on a push or a pull request. Its signed HTTP tests cover verification, tenant routing,
retries, queue limits and restart recovery without Slack credentials. The old automatically
started Socket Mode live-eval job has been removed: a fresh GitHub runner lacks a configured
public Request URL and an installed staging workspace in its database. Live evals are a
separate staging release check until an ingress-and-install provisioning harness exists.
