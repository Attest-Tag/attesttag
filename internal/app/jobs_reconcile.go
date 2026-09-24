package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// The reconciler is what makes jobs survive a bot restart, a worker that dies without a word, or
// a cancel the worker never acknowledges. It runs every 30 s like the routine scheduler, and its
// first pass at startup is the restart path: the rows are in SQLite, the checklist message ts is
// on each row, so a job that is still alive keeps updating the same message.
const (
	jobQueuedGrace   = 2 * time.Minute  // queued but never dispatched
	jobClaimGrace    = 10 * time.Minute // dispatched but never claimed
	jobSilentStale   = 5 * time.Minute  // running without an event → stale
	jobStaleFail     = 15 * time.Minute // stale for this long more → failed
	jobCancelGrace   = 90 * time.Second // cancel asked, no result → execution stopped
	jobDeadlineGrace = 5 * time.Minute  // on top of the job timeout before the bot gives up
	jobTokenGrace    = 2 * time.Minute  // after a terminal state the token is forgotten
)

func (r *JobRunner) RunReconciler(ctx context.Context) {
	if r == nil {
		return
	}
	r.reconcile(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	var lastPurge time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.reconcile(ctx)
		if time.Since(lastPurge) > time.Hour {
			lastPurge = time.Now()
			r.purgeRetainedJobData(ctx)
		}
	}
}

// dispatcherNamed finds the dispatcher a job was started with, which may differ from the current
// mode after a config change; a job it cannot reach reports unknown.
func (r *JobRunner) dispatcherNamed(ctx context.Context, name string) Dispatcher {
	r.mu.Lock()
	d, ok := r.dispatchers[name]
	r.mu.Unlock()
	if ok {
		return d
	}
	if d, err := newDispatcherFor(ctx, r.cfg, name); err == nil {
		r.mu.Lock()
		r.dispatchers[name] = d
		r.mu.Unlock()
		return d
	}
	return nil
}

func (r *JobRunner) execStatus(ctx context.Context, j *Job) DispatchStatus {
	if j.ExecutionRef == "" {
		return DispatchStatus{State: "unknown"}
	}
	d := r.dispatcherNamed(ctx, j.Dispatcher)
	if d == nil {
		return DispatchStatus{State: "unknown", Message: "no " + j.Dispatcher + " dispatcher in this process"}
	}
	st, err := d.Status(ctx, j.ExecutionRef)
	if err != nil {
		slog.Debug("execution status", "job", j.ID, "err", err)
	}
	return st
}

func (r *JobRunner) stopExecution(ctx context.Context, j *Job) {
	if j.ExecutionRef == "" {
		return
	}
	if d := r.dispatcherNamed(ctx, j.Dispatcher); d != nil {
		if err := d.Cancel(ctx, j.ExecutionRef); err != nil {
			slog.Warn("stop execution", "job", j.ID, "execution", j.ExecutionRef, "err", err)
		}
	}
}

func execDetail(st DispatchStatus) string {
	if st.State == "unknown" || st.State == "" {
		return ""
	}
	s := " (execution " + st.State
	if st.Message != "" {
		s += ": " + truncate(st.Message, 200)
	}
	return s + ")"
}

func (r *JobRunner) reconcile(ctx context.Context) {
	jobs, err := r.store.ActiveJobs(ctx)
	if err != nil {
		return
	}
	nowT := time.Now().UTC()
	for i := range jobs {
		j := &jobs[i]
		created, _ := parseStoreTime(j.CreatedAt)
		lastWord := nonEmpty(j.LastEventAt, nonEmpty(j.StartedAt, j.CreatedAt))
		lastT, _ := parseStoreTime(lastWord)
		silent := nowT.Sub(lastT)
		deadline := created.Add(time.Duration(j.TimeoutS)*time.Second + jobDeadlineGrace)
		fail := func(code, msg string) {
			r.finish(ctx, j.OrgID, j.ID, JobFailed, &JobResult{Error: JobError{Code: code, Message: msg}})
		}
		switch j.Status {
		case jobQueued:
			if nowT.Sub(created) > jobQueuedGrace {
				fail("dispatch_failed", "the dispatch never completed (was the bot restarted?)")
			}
		case jobStarting:
			if nowT.Sub(created) < jobClaimGrace {
				continue
			}
			st := r.execStatus(ctx, j)
			if (st.State == "running" || st.State == "pending") && nowT.Sub(created) < 2*jobClaimGrace {
				continue // still coming up; give it the second window
			}
			r.stopExecution(ctx, j)
			fail("dispatch_failed", "the worker never claimed the job"+execDetail(st))
		case jobRunning:
			if nowT.After(deadline) {
				r.stopExecution(ctx, j)
				r.finish(ctx, j.OrgID, j.ID, JobTimeout, &JobResult{Error: JobError{Code: "timeout", Message: fmt.Sprintf("no result within %s", fmtDuration(time.Duration(j.TimeoutS)*time.Second))}})
				continue
			}
			if silent < jobSilentStale {
				continue
			}
			st := r.execStatus(ctx, j)
			switch st.State {
			case "succeeded", "failed", "cancelled":
				fail("stale", "the worker exited without reporting a result"+execDetail(st))
			default:
				if ok, _ := r.store.SetJobStatus(ctx, j.OrgID, j.ID, []string{jobRunning}, jobStale, nil); ok {
					slog.Warn("job stale", "job", j.ID, "silent", silent.Round(time.Second), "execution", st.State)
					r.refresh(j.OrgID, j.ID)
					if r.agent != nil {
						r.agent.alert(ctx, j.OrgID, fmt.Sprintf("job:%d:stale", j.ID), fmt.Sprintf(":hourglass: Fix job #%d on %s in <#%s> has not reported for %s.", j.ID, j.Repo, j.Channel, fmtDuration(silent)))
					}
				}
			}
		case jobStale:
			st := r.execStatus(ctx, j)
			switch {
			case st.State == "succeeded" || st.State == "failed" || st.State == "cancelled":
				fail("stale", "the worker exited without reporting a result"+execDetail(st))
			case nowT.After(deadline) || silent > jobSilentStale+jobStaleFail:
				r.stopExecution(ctx, j)
				fail("stale", fmt.Sprintf("no word from the worker for %s; its execution was stopped", fmtDuration(silent)))
			}
		case jobCancelling:
			at, ok := parseStoreTime(j.CancelAt)
			if !ok || nowT.Sub(at) > jobCancelGrace {
				r.stopExecution(ctx, j)
				r.finish(ctx, j.OrgID, j.ID, JobCancelled, &JobResult{Error: JobError{Code: "cancelled", Message: "the worker did not confirm the cancel in time; its execution was stopped"}})
			}
		}
	}
	r.store.RevokeJobTokens(ctx, jobTokenGrace)
}

// Purge all organizations even when their workers are currently disabled.
func (r *JobRunner) purgeRetainedJobData(ctx context.Context) {
	orgs, err := r.store.OrgIDs(ctx)
	if err != nil {
		slog.Warn("job retention enumeration", "err", err)
		return
	}
	for _, orgID := range orgs {
		days := r.settings.Get(ctx, orgID).WorkerEventRetentionDays
		if days <= 0 {
			days = 30
		}
		if _, err := r.store.PurgeJobData(ctx, orgID, time.Duration(days)*24*time.Hour); err != nil {
			slog.Warn("job retention", "org", orgID, "err", err)
		}
	}
}
