package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// The thread sees two messages per job: a checklist posted at dispatch and edited as events
// arrive, and a report posted when the job ends so the thread notifies. Long output (diff, log)
// goes up as files and is recorded as artifacts.

// jobSteps are the checklist rows; several phases fold into "pull request".
var jobSteps = []struct {
	label  string
	phases []string
}{
	{"clone", []string{"clone"}},
	// The install and the baseline read as one step in the thread — nobody watching wants two
	// lines for "getting ready" — but they are separate phases underneath, so a failed install
	// is still reported as a failed install rather than as a failing suite.
	{"set up and check", []string{"setup", "build_before", "test_before"}},
	{"fix", []string{"engine"}},
	{"build and test", []string{"build_after", "test_after"}},
	{"pull request", []string{"commit", "push", "pr"}},
}

// UpdateMarkdown rewrites a posted message as Markdown over an optional footer.
func (s *Chat) UpdateMarkdown(ctx context.Context, channel, ts, md, footer string) error {
	return s.t.updateMarkdown(ctx, channel, ts, md, footer, truncate(oneLine(md), 200))
}

func jobStateLine(j *Job) string {
	switch j.Status {
	case jobQueued:
		return "_queued_"
	case jobStarting:
		return "_starting the worker…_"
	case jobRunning:
		if j.Phase != "" {
			return "_running: " + strings.ReplaceAll(j.Phase, "_", " ") + "_"
		}
		return "_running_"
	case jobStale:
		return "_no word from the worker for a few minutes; checking…_"
	case jobCancelling:
		return "_cancelling…_"
	case JobSucceeded:
		return "_done_"
	case JobFailed:
		return "_failed_"
	case JobCancelled:
		if j.CancelBy != "" && j.CancelReason == "user" {
			return fmt.Sprintf("_cancelled by <@%s>_", j.CancelBy)
		}
		return "_cancelled_"
	case JobTimeout:
		return "_timed out_"
	}
	return "_" + j.Status + "_"
}

// jobChecklist renders the status message from the job row and its events.
func jobChecklist(j *Job, events []JobEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, ":hammer_and_wrench: *Fix job #%d* on `%s`", j.ID, j.Repo)
	if j.Branch != "" {
		fmt.Fprintf(&b, " → `%s`", j.Branch)
	}
	b.WriteString(" — " + jobStateLine(j))
	if j.Title != "" {
		b.WriteString("\n" + truncate(oneLine(j.Title), 120))
	}
	state := map[string]string{} // phase → started|ok|failed|skipped
	last := ""
	for _, e := range events {
		switch e.Kind {
		case JobKindPhase:
			if e.Phase != "" && e.Status != "" {
				state[e.Phase] = e.Status
			}
		case JobKindLog, JobKindTests, JobKindWarn:
			if m := strings.TrimSpace(e.Message); m != "" {
				last = m
			}
		}
	}
	b.WriteString("\n")
	for i, st := range jobSteps {
		glyph := "○"
		agg := ""
		for _, p := range st.phases {
			switch state[p] {
			case "failed":
				agg = "failed"
			case "started":
				if agg == "" {
					agg = "started"
				}
			case "ok":
				if agg == "" || agg == "started" {
					agg = "ok"
				}
			case "skipped":
				if agg == "" {
					agg = "skipped"
				}
			}
		}
		// A step whose last phase is done is done; one with any failure failed.
		if agg == "ok" {
			if state[st.phases[len(st.phases)-1]] != "ok" {
				agg = "started"
			}
		}
		switch agg {
		case "started":
			glyph = "◐"
		case "ok":
			glyph = "●"
		case "failed":
			glyph = "✕"
		case "skipped":
			glyph = "–"
		}
		if i > 0 {
			b.WriteString(" → ")
		}
		b.WriteString(glyph + " " + st.label)
	}
	if last != "" && !jobTerminal(j.Status) {
		b.WriteString("\n_" + escapeMrkdwn(truncate(oneLine(last), 200)) + "_")
	}
	if j.Error != "" && (j.Status == JobFailed || j.Status == JobTimeout) {
		b.WriteString("\n" + escapeMrkdwn(truncate(oneLine(j.Error), 300)))
	}
	return b.String()
}

// jobSpendLine closes the report with what the run cost: the money, how long it took, and the
// tokens behind it. A job whose provider never reported a price says so rather than claiming
// $0.00 — the worker meters OpenRouter spend, and nothing else can be metered from here.
func jobSpendLine(j *Job) string {
	money := fmtCost(j.CostUSD)
	if j.CostUSD == 0 {
		// fmtCost calls zero "<$0.0001", which is right for a cheap turn and wrong here:
		// either the job burned tokens nobody priced, or it stopped before spending anything.
		money = "$0.00"
		if j.TokensIn > 0 || j.TokensOut > 0 {
			money = "not reported by the provider"
		}
	}
	parts := []string{money}
	if d := jobDuration(j); d > 0 {
		parts = append(parts, fmtDuration(d))
	}
	if j.TokensIn > 0 || j.TokensOut > 0 {
		parts = append(parts, fmtTokens(j.TokensIn)+" in / "+fmtTokens(j.TokensOut)+" out")
	}
	return strings.Join(parts, " · ")
}

func jobFooter(j *Job) string {
	// No engine and no model id. This card is read by whoever asked for the fix, and the same
	// reasoning that took the model out of the reply footer applies here: which worker and which
	// provider ran it is an operator's fact, kept on the job record and in the console, not
	// something to print in the channel. What is left is what a reader can act on.
	parts := []string{fmtCost(j.CostUSD)}
	if d := jobDuration(j); d > 0 {
		parts = append(parts, fmtDuration(d))
	}
	if !jobTerminal(j.Status) {
		parts = append(parts, "say \"stop\" here to cancel")
	}
	return strings.Join(parts, " · ")
}

func parseStoreTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.DateTime, time.RFC3339, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func jobDuration(j *Job) time.Duration {
	start, ok := parseStoreTime(nonEmpty(j.StartedAt, j.CreatedAt))
	if !ok {
		return 0
	}
	end := time.Now().UTC()
	if t, ok := parseStoreTime(j.FinishedAt); ok {
		end = t
	}
	return end.Sub(start)
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// ---- checklist updates, coalesced ----

// refresh schedules a checklist edit; edits within two seconds of each other become one.
func (r *JobRunner) refresh(orgID, id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, pending := r.updaters[id]; pending {
		return
	}
	r.updaters[id] = time.AfterFunc(2*time.Second, func() {
		r.mu.Lock()
		delete(r.updaters, id)
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if j, err := r.store.Job(ctx, orgID, id); err == nil && j != nil {
			r.renderChecklist(ctx, j)
		}
	})
}

func (r *JobRunner) cancelUpdater(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.updaters[id]; t != nil {
		t.Stop()
		delete(r.updaters, id)
	}
}

func (r *JobRunner) renderChecklist(ctx context.Context, j *Job) {
	sl, err := r.slacks.For(ctx, j.TeamID)
	if j.StatusTS == "" || err != nil {
		return
	}
	events, _ := r.store.JobEvents(ctx, j.OrgID, j.ID, 500)
	if err := sl.UpdateMarkdown(ctx, j.Channel, j.StatusTS, jobChecklist(j, events), jobFooter(j)); err != nil {
		slog.Debug("job checklist update", "job", j.ID, "err", err)
	}
}

// ---- the report ----

// postReport posts the outcome as a new message and attaches the diff and log. It returns the
// one-line note kept in the thread's history.
func (r *JobRunner) postReport(ctx context.Context, j *Job, res *JobResult) string {
	text := jobReport(j, res)
	sl, slErr := r.slacks.For(ctx, j.TeamID)
	if slErr != nil {
		slog.Warn("job report not posted: workspace not connected", "job", j.ID, "err", slErr)
		return text
	}
	if _, err := sl.PostMarkdown(ctx, j.Channel, j.ThreadTS, text, ""); err != nil {
		slog.Warn("job report post failed", "job", j.ID, "err", err)
	}
	// Attachments: the diff the worker uploaded, and the log tail when there is one.
	if diff, _, _, err := r.store.JobFile(ctx, j.OrgID, j.ID, "diff"); err == nil && strings.TrimSpace(diff) != "" {
		r.attach(ctx, j, "diff", fmt.Sprintf("fix-job-%d.diff", j.ID), fmt.Sprintf("Fix job #%d diff", j.ID), diff, "diff", "txt")
	}
	if tail := strings.TrimSpace(res.LogTail); tail != "" && (j.Status != JobSucceeded || len(tail) > 1500) {
		r.attach(ctx, j, "log", fmt.Sprintf("fix-job-%d-log.txt", j.ID), fmt.Sprintf("Fix job #%d log", j.ID), tail, "text", "txt")
	}
	return text
}

// jobReport is the outcome message: what happened, what the checks said in every package the job
// checked, what changed without being checked, and what it cost.
func jobReport(j *Job, res *JobResult) string {
	var b strings.Builder
	switch j.Status {
	case JobSucceeded:
		fmt.Fprintf(&b, ":white_check_mark: *Fix job #%d finished*", j.ID)
		if res.PR != nil && res.PR.URL != "" {
			label := fmt.Sprintf("#%d %s", res.PR.Number, escapeMrkdwn(truncate(oneLine(j.Title), 80)))
			if res.PR.Number == 0 {
				label = "pull request"
			}
			fmt.Fprintf(&b, " — <%s|%s>", res.PR.URL, label)
			if res.PR.Draft {
				b.WriteString(" (draft)")
			}
		}
		if j.Branch != "" {
			fmt.Fprintf(&b, " from `%s`", j.Branch)
		}
	case JobFailed:
		fmt.Fprintf(&b, ":x: *Fix job #%d failed*", j.ID)
		if j.Phase != "" {
			fmt.Fprintf(&b, " at %s", strings.ReplaceAll(j.Phase, "_", " "))
		}
		if j.Error != "" {
			b.WriteString(" — " + escapeMrkdwn(truncate(oneLine(j.Error), 300)))
		}
	case JobCancelled:
		fmt.Fprintf(&b, ":octagonal_sign: *Fix job #%d cancelled*", j.ID)
		if j.CancelBy != "" && j.CancelReason == "user" {
			fmt.Fprintf(&b, " by <@%s>", j.CancelBy)
		} else if j.CancelReason != "" {
			b.WriteString(" (" + j.CancelReason + ")")
		}
	case JobTimeout:
		fmt.Fprintf(&b, ":hourglass: *Fix job #%d timed out* after %s", j.ID, fmtDuration(time.Duration(j.TimeoutS)*time.Second))
	}
	if j.Status != JobSucceeded {
		if res.PR != nil && res.PR.URL != "" {
			fmt.Fprintf(&b, "\nA pull request was opened before it stopped: <%s|%s>", res.PR.URL, res.PR.URL)
		} else if res.Branch != "" && res.HeadSHA != "" {
			fmt.Fprintf(&b, "\nBranch `%s` was pushed; no pull request was opened.", res.Branch)
		} else {
			b.WriteString("\nNothing was pushed.")
		}
	}
	if s := strings.TrimSpace(res.Summary); s != "" {
		b.WriteString("\n*Summary:* " + escapeMrkdwn(truncate(s, 1500)))
	}
	if res.Note != "" {
		b.WriteString("\n:warning: " + escapeMrkdwn(truncate(oneLine(res.Note), 300)))
	}
	if res.CheckNote != "" {
		b.WriteString("\n:warning: " + escapeMrkdwn(truncate(oneLine(res.CheckNote), 300)))
	}
	// With more than one package checked, the primary's line says which one it is.
	label := "*Tests:*"
	if len(res.Packages) > 0 {
		label = "*Tests* (" + dirLabel(res.Checked()[0].Workdir) + "):"
	}
	var facts []string
	if res.Tests.Before.Ran || res.Tests.After.Ran {
		facts = append(facts, label+" before "+testWord(res.Tests.Before)+" / after "+testWord(res.Tests.After))
	} else if res.Tests.Skipped != "" {
		facts = append(facts, label+" not run ("+escapeMrkdwn(truncate(oneLine(res.Tests.Skipped), 160))+")")
	} else if j.Status == JobSucceeded {
		facts = append(facts, label+" none found")
	}
	if res.DiffStat.Files > 0 {
		facts = append(facts, fmt.Sprintf("*Diff:* %d files, +%d −%d", res.DiffStat.Files, res.DiffStat.Insertions, res.DiffStat.Deletions))
	}
	facts = append(facts, "*Cost:* "+jobSpendLine(j))
	b.WriteString("\n" + strings.Join(facts, "   "))
	for _, p := range res.Packages {
		b.WriteString("\n" + packageLine(p))
	}
	if len(res.Unchecked) > 0 {
		b.WriteString("\n*Also changed, not checked:* " + dirList(res.Unchecked, 8))
	}
	if res.TicketComment != "" && j.Status == JobSucceeded {
		b.WriteString("\n_The worker drafted a ticket comment; ask me to post it if you want it on the ticket._")
	}
	return b.String()
}

// packageLine is one more package's checks, in the words the primary's line uses.
func packageLine(p JobPackage) string {
	head := "*" + dirLabel(p.Workdir) + ":*"
	if p.Skipped != "" {
		return head + " not checked (" + escapeMrkdwn(truncate(oneLine(p.Skipped), 160)) + ")"
	}
	var parts []string
	switch {
	case p.Tests.Before.Ran || p.Tests.After.Ran:
		parts = append(parts, "tests before "+testWord(p.Tests.Before)+" / after "+testWord(p.Tests.After))
		if p.Build.After.Ran && !p.Build.After.OK {
			parts = append(parts, "build still fails")
		}
	case p.Build.Before.Ran || p.Build.After.Ran:
		parts = append(parts, "build before "+passWord(p.Build.Before)+" / after "+passWord(p.Build.After))
	default:
		why := nonEmpty(p.Tests.Skipped, "nothing to run")
		parts = append(parts, "not run ("+escapeMrkdwn(truncate(oneLine(why), 160))+")")
	}
	if p.Setup.Ran && !p.Setup.OK {
		parts = append(parts, "dependencies did not install")
	}
	line := head + " " + strings.Join(parts, " · ")
	if p.Note != "" {
		line += " — " + escapeMrkdwn(truncate(oneLine(p.Note), 200))
	}
	return line
}

// passWord is testWord for a gate that has no count to give, like a build.
func passWord(t JobTestRun) string {
	switch {
	case !t.Ran:
		return "not run"
	case t.OK:
		return "pass"
	}
	return "fail"
}

// dirLabel is a package directory as the report prints it. The name comes from the repository,
// so it is escaped, and a backtick in it would end the code span early.
func dirLabel(dir string) string {
	if dir == "" || dir == "." {
		return "the root"
	}
	return "`" + strings.ReplaceAll(escapeMrkdwn(truncate(oneLine(dir), 80)), "`", "'") + "`"
}

func dirList(dirs []string, max int) string {
	var out []string
	for i, d := range dirs {
		if i == max {
			out = append(out, fmt.Sprintf("and %d more", len(dirs)-max))
			break
		}
		out = append(out, dirLabel(d))
	}
	return strings.Join(out, ", ")
}

func testWord(t JobTestRun) string {
	switch {
	case !t.Ran:
		return "not run"
	case t.OK:
		return "pass"
	case t.Failed > 0:
		return fmt.Sprintf("%d failed", t.Failed)
	}
	return "fail"
}

// attach uploads a file to the thread and records it as an artifact and on the job.
func (r *JobRunner) attach(ctx context.Context, j *Job, kind, filename, title, content, snippet, artifactKind string) {
	sl, slErr := r.slacks.For(ctx, j.TeamID)
	if slErr != nil {
		return
	}
	fileID, link, err := sl.uploadContent(ctx, j.Channel, j.ThreadTS, filename, title, content, snippet)
	if err != nil {
		slog.Warn("job file upload failed", "job", j.ID, "kind", kind, "err", err)
		return
	}
	if kind == "log" {
		r.store.PutJobFile(ctx, j.OrgID, j.ID, "log", content)
	}
	r.store.SetJobFileLink(ctx, j.OrgID, j.ID, kind, fileID, link)
	r.store.AddArtifact(ctx, j.OrgID, &Artifact{TeamID: j.TeamID, Channel: j.Channel, ThreadTS: j.ThreadTS, CreatedBy: j.Requester, Title: title, Kind: artifactKind,
		Bytes: len(content), Content: content, FileID: fileID, Permalink: link})
}
