"""Shared state for the fake aws/az/gcloud/docker CLIs.

Every fake records what it was called with and keeps a tiny resource registry, so a second run
of a deploy script sees the resources the first run created. That is the only way to test the
idempotency branches, which are where the interesting bugs are: a script that mints a second
IAM access key on every rerun looks fine on the first run.

Nothing here talks to a network. State lives under $FAKE_CLOUD_STATE, a directory the harness
makes in a temp dir and throws away.
"""
import json
import os
import pathlib
import sys

ROOT = pathlib.Path(os.environ.get("FAKE_CLOUD_STATE", "/tmp/fake-cloud"))
CAPTURES = ROOT / "captures"


def _load():
    f = ROOT / "state.json"
    if f.exists():
        return json.loads(f.read_text())
    return {}


def _save(state):
    (ROOT / "state.json").write_text(json.dumps(state, indent=2, sort_keys=True))


def exists(kind, name):
    return name in _load().get(kind, {})


def get(kind, name, default=None):
    return _load().get(kind, {}).get(name, default)


def put(kind, name, value=True):
    state = _load()
    state.setdefault(kind, {})[name] = value
    _save(state)
    return value


# Flags whose value is a credential the script generated rather than one from the env file.
# The transcript is written to a temp directory, but a test tool that records passwords is one
# bad default away from recording a real one, so it never sees them.
REDACT = ("--master-user-password", "--admin-password", "--azure-file-account-key",
          "--secret-string", "--password-stdin")


def record(tool, argv):
    """Append the invocation to calls.log, with generated credentials redacted."""
    ROOT.mkdir(parents=True, exist_ok=True)
    safe, skip = [], False
    for a in argv:
        if skip:
            safe.append("<redacted>")
            skip = False
            continue
        safe.append(a)
        skip = a in REDACT
    with (ROOT / "calls.log").open("a") as fh:
        fh.write(json.dumps([tool] + safe) + "\n")


def capture(name, content):
    """Keep a payload a script generated, so the harness can assert on it.

    The Container Apps spec is a mktemp deleted on exit and the ECS task definition is a
    command-substituted string; neither survives the script, so the fake saves a copy as it is
    handed one.
    """
    CAPTURES.mkdir(parents=True, exist_ok=True)
    (CAPTURES / name).write_text(content)


def counter(name):
    """Bump and return a per-name counter. Used for 'was this called exactly once'."""
    state = _load()
    n = state.setdefault("_counters", {}).get(name, 0) + 1
    state["_counters"][name] = n
    _save(state)
    return n


def die(msg, code=254):
    sys.stderr.write(msg + "\n")
    sys.exit(code)


def flag(argv, name, default=None):
    """Value of --name, or default. The fakes only ever need the single-valued form."""
    if name in argv:
        i = argv.index(name)
        if i + 1 < len(argv):
            return argv[i + 1]
    return default


def out(text):
    sys.stdout.write(str(text) + "\n")
