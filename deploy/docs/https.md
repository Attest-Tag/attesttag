# Getting a public HTTPS address

This is the first thing every self-hoster hits, so it is worth being blunt about it:

**attest_tag cannot run on localhost.** Slack will not deliver events to a private address, and
will not accept an `http://` OAuth redirect URL. There is no Socket Mode fallback in this
codebase — delivery is the HTTP Events API and nothing else. So before the bot can answer
anything, it needs an address on the internet with a valid certificate.

Three ways, cheapest first.

## Quick tunnel — no account, no domain, two minutes

```bash
docker compose --profile quicktunnel up -d
docker compose logs quicktunnel
```

(Until the first release there is no bot image to pull; build one first, as
[`../local/README.md`](../local/README.md) shows.)

The logs print a `https://something-random.trycloudflare.com` address. Use it as `BASE_URL`
when generating the Slack manifest, set `ADMIN_BASE_URL` in `.env` to the same value, and apply
it by running the same `docker compose --profile quicktunnel up -d` again — not
`docker compose restart`, which restarts the tunnel as well, giving it a new name, and does not
reread `.env`.

This is genuinely the shortest path from `git clone` to a bot replying in Slack, and it needs
no Cloudflare account at all.

**The hostname changes every time the container restarts.** When it does, the Slack app's Event
Subscriptions Request URL, its Interactivity Request URL and both OAuth redirect URLs all point
at an address that no longer exists, and you have to re-verify all four by hand — and set
`ADMIN_BASE_URL` to the new name, or links and sign-ins keep going to the old one. So: fine for
finding out whether you want this, not fine for anything you intend to keep.

A caution worth stating, since the address is public and unguessable rather than private: while
that tunnel is up, anyone who learns the hostname can reach your console's sign-in page. Keep
`SIGNUP_MODE=first-run` (the default) so that only the first sign-up founds anything.

## Named Cloudflare tunnel — free, stable, works behind NAT

The one to graduate to. It needs a Cloudflare account and a domain on Cloudflare, both free,
and it opens no inbound ports — the tunnel dials out, so this works on a home connection behind
NAT with no port forwarding and no static address.

1. In the Cloudflare dashboard: **Zero Trust → Networks → Tunnels → Create a tunnel**, pick
   **Cloudflared**, name it.
2. Copy the tunnel token. Put it in `.env` as `CLOUDFLARE_TUNNEL_TOKEN=`.
3. Add a **public hostname** on the tunnel: your subdomain, service `HTTP`, URL
   `attesttag:8080`.
4. Set `ADMIN_BASE_URL=https://your-subdomain.example.com` in `.env`.

```bash
docker compose --profile tunnel up -d
```

The hostname is now yours and stable, so the Slack app is configured once.

## Caddy — you own the domain and the machine has a public address

For a VPS or anything with a real address and a DNS record pointing at it. Caddy obtains and
renews a Let's Encrypt certificate on its own; there is nothing to configure for TLS.

1. Point an A or AAAA record at the machine.
2. In `.env`: `ATTEST_DOMAIN=bot.example.com`. `ATTEST_ACME_EMAIL=you@example.com` is optional —
   the contact address Caddy registers with the certificate authority, which Caddy recommends in
   case a certificate ever runs into trouble. Left empty, the `Caddyfile` has no `email` line
   at all.
3. Open ports 80 and 443 to the internet — Let's Encrypt needs 80 to issue the certificate.

```bash
docker compose --profile caddy up -d
```

## Whichever you pick: set `ADMIN_BASE_URL`

This matters more than it looks. Without it the bot learns its public origin from the first
authenticated request's `Host` header and then keeps it — and that origin is what password
reset, email verification and invitation links are built from, for every account on the
deployment. Behind a proxy that is a header you do not fully control.

Set `ADMIN_BASE_URL` whenever you know the URL, and change it when the URL changes. Leaving it
unset does not let the bot follow a quick tunnel to its new name: once an origin is learned,
only the same host may replace it, so links and Slack and Microsoft sign-ins go on pointing at
the old one. `PUBLIC_ORIGIN_HOSTS` is for a fixed set of names that are all legitimate — it
turns first-writer-wins into an allowlist, and a host not on the list is never learned — so it
cannot follow a random tunnel name either.

## Checking it works before involving Slack

```bash
curl -fsS https://your-host/health          # -> ok
curl -fsS -o /dev/null -w '%{http_code}\n' https://your-host/admin/
```

`/health` answers `ok` from the moment the process is listening, even before the database is
open — it is a liveness check. `/admin/` is the readiness one: it returns 503 until the store
is ready and 200 after, and a 200 also proves the console was actually embedded in the binary.

If `/health` answers and Slack still reports that it cannot verify your Request URL, the
signing secret is the usual culprit — the bot refuses a delivery it cannot verify and says so
in its logs, with a different message for each of "the URL was never set", "the secret is
wrong" and "Socket Mode is swallowing this".
