#!/usr/bin/env bash
# Run the deploy scripts against fake aws/az/gcloud/docker CLIs and assert on what they did.
#
#   ./deploy/test/run.sh            everything
#   ./deploy/test/run.sh azure      only scenarios whose name contains "azure"
#   ./deploy/test/run.sh -v aws     and print each script's output
#
# No cloud account, no network, no credentials: every scenario gets a throwaway env file of
# obvious fakes and its own state directory under $TMPDIR. deploy/test/README.md says what this
# does and does not prove.
set -euo pipefail
cd "$(dirname "$0")/../.."
command -v python3 >/dev/null 2>&1 || { echo "python3 is not on PATH" >&2; exit 1; }
# -u so progress streams when the output is redirected to a file or a pipe.
exec python3 -u deploy/test/harness.py "$@"
