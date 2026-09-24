package main

import (
	"os"

	"attesttag/internal/app"
	"attesttag/internal/worker"
)

// One binary, two roles: the Slack bot, and `attesttag worker`, the fix-job worker the bot
// starts in a separate container (or as a subprocess in development). The worker never loads
// the bot's configuration; everything it needs comes from three ATTEST_* variables and the
// claim call.
func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "worker":
			os.Exit(worker.Main(os.Args[2:]))
		case "bucket-init":
			// Make the documents bucket and prove the credentials can use it. One-shot, run
			// beside a MinIO that compose or the Helm chart has just started, or by hand
			// against R2 or S3 to find out whether a key is right before trusting it.
			os.Exit(app.BucketInit(os.Args[2:]))
		case "pg-import":
			// The one-shot cutover tool: copy a SQLite database into an empty Postgres one and
			// verify every row of it. Not part of serving anything.
			os.Exit(app.PGImport(os.Args[2:]))
		}
	}
	app.Run()
}
