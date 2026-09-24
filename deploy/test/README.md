# Testing the deploy scripts without a cloud account

```bash
./deploy/test/run.sh              # everything
./deploy/test/run.sh azure        # scenarios whose name contains "azure"
./deploy/test/run.sh -v aws       # and print each script's output
```

`make test-deploy` runs the same thing. It needs `python3`, and `shellcheck` for the scenario
that lints every deploy script, which fails without it.

No AWS account, no Azure subscription, no network, no credentials. Each scenario gets a
throwaway env file of obvious fakes and its own state directory under `$TMPDIR`, with
[`fakes/`](fakes/) first on `PATH` so `aws`, `az`, `gcloud` and `docker` resolve to recorders
rather than to the real things.

The scripts run under whichever bash is first on `PATH`. On macOS that is 3.2 — the oldest they
claim to support, and the version [`../aws/worker.sh`](../aws/worker.sh) warns about by name.
The two `gcp/` scripts run under zsh, their shebang.

## What this proves, and what it does not

It proves the parts that are ours:

- both scripts run start to finish, under bash 3.2, with no unbound variable or broken heredoc
- the refusals fire before anything is created — no `MASTER_KEY`, no Slack OAuth pair, no
  `DOMAIN`, one subnet instead of two — and a certificate that is not issued yet stops the
  script before the load balancer is created, after the bucket, the key, the secrets and the
  image, which come first
- **the idempotency branches work**, which is the thing you cannot see by reading. A rerun mints
  no second IAM access key, creates no second bucket, load balancer, listener, database or
  Container Apps job, and rolls the existing service instead of creating another
- the generated ECS task definition and Container Apps spec say what they are meant to say:
  one writer, `stopTimeout` 45, `minReplicas` 1, `/health` probes, http redirecting to https
- every secret travels by reference — a Secrets Manager ARN, a Container Apps `secretRef` — and
  never as a plain environment value
- no secret from the env file, and no password either script generates, reaches stdout or the
  call transcript
- the two scripts per platform agree with each other: `worker.sh` creates the role and the job
  that the deploy script then looks for, and running them in order produces no warnings
- every script reads the env file the way a person edits it: a value appended below the
  template's empty line for that key is the one deployed, on all three clouds
- a deployment nobody configured is one organisation: `gcp/cloudrun.sh` deploys
  `SIGNUP_MODE=first-run` and none of the hosted service's policy, whatever its env file is
  called, unless that file or the shell says `HOSTED=1`; a `SIGNUP_MODE` the env file does set is
  deployed as it is, hosted or not
- what `gcp/cloudrun.sh` hands Cloud Run: a comma-separated list such as `STRIPE_SIZES` or
  `WORKER_JOB_NAMES` arrives whole, the fix-job cache has a bucket on Postgres, and the lease's
  503 warning and pointers appear only on SQLite. The `gcloud` fake answers every call as if the
  resource existed, so for Google Cloud this, and `gcp/worker.sh` making the bot's account before
  granting it anything on a new project, is all it proves

It does **not** prove that AWS and Azure accept these calls. Every flag name, field name and API
shape here is answered by a fake that was written from the same reading of the docs as the script
it is answering. A renamed flag, a required field nobody passed, an IAM policy that is one action
short — none of that shows up until a real account sees it. That first real run is still the
first real run.

So: this catches the bugs that come from changing the scripts, and it is a regression net, not a
substitute for [`../aws/README.md`](../aws/README.md)'s warning that these are not run against
a live account routinely.

## The fakes

| | |
|---|---|
| [`fakes/aws`](fakes/aws) | the awscli v2 calls `deploy/aws/*.sh` make, and only those |
| [`fakes/az`](fakes/az) | the same for `deploy/azure/*.sh` |
| [`fakes/docker`](fakes/docker) | records `build`/`pull`/`push`/`tag`/`login` and succeeds |
| [`fakes/gcloud`](fakes/gcloud) | answers every call as if the resource existed, and keeps `run deploy`'s arguments: enough to test the policy `gcp/cloudrun.sh` deploys, and unlike the two above it never says `unhandled call`. With `FAKE_GCLOUD_NEW_PROJECT=1` a service account exists only once it has been created, and a binding naming one that does not is refused, as IAM refuses it |
| [`fakes/_cloudstate.py`](fakes/_cloudstate.py) | the resource registry, the call transcript and the payload captures |

Each fake answers in the shape the real CLI answers — tab-separated text for `--output text`, a
non-zero exit for a resource that does not exist — and records what it was asked. Anything a
script calls that a fake does not know about **fails the scenario** with `unhandled call`, which
is deliberate: a script that grew a new API call gets a failing test until the fake and its
assertions catch up.

Two things the fakes capture rather than answer, because they do not otherwise survive the
script: the ECS task definition, which is a command-substituted string, and the Container Apps
spec, which is a `mktemp` deleted on the way out. Those captures are what most of the assertions
read.

Generated passwords are redacted from the transcript. The harness never reads the repository's
own `.env` — `ENV_FILE` is always set explicitly, because the scripts default it to the real one.

## Adding a scenario

Everything is in [`harness.py`](harness.py). A scenario is a function under `@scenario("name")`
that calls `run(...)` and then `ok(condition, "what went wrong")`. Write the message as the
consequence, not the assertion — `"a second access key was minted; the one in Secrets Manager is
the one the task uses"` tells you what broke, `"count == 1 failed"` does not.
