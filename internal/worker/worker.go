package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// Main is `attesttag worker`. Exit codes: 0 result posted, 1 result could not be posted,
// 2 bad invocation, 3 the bot refused the claim.
func Main(args []string) int {
	opts, err := optionsFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "attesttag worker:", err)
		fmt.Fprintln(os.Stderr, "usage: attesttag worker  (with ATTEST_JOB_ID, ATTEST_BOT_URL and ATTEST_JOB_TOKEN in the environment)")
		return 2
	}
	scrub := newScrubber(opts.Token)
	lvl := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		lvl = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(&scrubWriter{w: os.Stderr, s: scrub}, &slog.HandlerOptions{Level: lvl})))
	slog.Info("attesttag worker", "job", opts.JobID, "bot", opts.BotURL, "mode", opts.Mode, "work", opts.WorkDir, "version", opts.Version)
	if err := os.MkdirAll(opts.WorkDir, 0o700); err != nil {
		slog.Error("work dir", "err", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	r := &Runner{opts: opts, client: newClient(opts), scrub: scrub}
	return r.Run(ctx)
}
