package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The reconciler is the restart and dead-worker path: every stuck state must end in a terminal
// one with the thread told, and a silent worker becomes stale before it is given up on.
func TestJobReconciler(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	st, r := h.b.store, h.b.jobs
	age := func(id int64, d time.Duration) {
		st.db.Exec(`update jobs set created_at=? where id=?`, time.Now().Add(-d).UTC().Format(time.DateTime), id)
	}
	mk := func(status string, timeoutS int) int64 {
		id, err := st.InsertJob(ctx, &Job{OrgID: orgID, TeamID: "T1", Status: status, Channel: "C1", ThreadTS: status + fmt.Sprint(timeoutS), ConnectionID: h.connID, Repo: "acme/app",
			Spec: "{}", TimeoutS: timeoutS, Dispatcher: "fake"})
		if err != nil {
			t.Fatal(err)
		}
		st.SetJobFields(ctx, orgID, id, map[string]any{"execution_ref": fmt.Sprintf("fake:%d", id), "token_hash": "h"})
		return id
	}
	queued := mk(jobQueued, 600)
	age(queued, 3*time.Minute)
	starting := mk(jobStarting, 600)
	age(starting, 25*time.Minute)
	silent := mk(jobRunning, 3600)
	age(silent, 6*time.Minute)
	cancelling := mk(jobCancelling, 600)
	st.db.Exec(`update jobs set cancel_requested=1, cancel_requested_at=? where id=?`, time.Now().Add(-3*time.Minute).UTC().Format(time.DateTime), cancelling)
	late := mk(jobRunning, 600)
	age(late, 20*time.Minute)
	fresh := mk(jobRunning, 600)

	r.reconcile(ctx)
	want := map[int64]string{queued: JobFailed, starting: JobFailed, silent: jobStale, cancelling: JobCancelled, late: JobTimeout, fresh: jobRunning}
	for id, status := range want {
		got, _ := st.Job(ctx, orgID, id)
		if got.Status != status {
			t.Errorf("job %d: %s, want %s (error %q)", id, got.Status, status, got.Error)
		}
	}
	if got, _ := st.Job(ctx, orgID, queued); !strings.Contains(got.Error, "dispatch") {
		t.Errorf("queued job error: %q", got.Error)
	}
	for _, id := range []int64{starting, cancelling, late} {
		if !h.fd.wasCancelled(fmt.Sprintf("fake:%d", id)) {
			t.Errorf("execution of job %d was not stopped", id)
		}
	}
	if h.fd.wasCancelled(fmt.Sprintf("fake:%d", silent)) {
		t.Error("a stale job's execution must not be stopped yet")
	}

	// The stale job's execution is gone: failed, with the platform's word in the error.
	h.fd.state = "failed"
	r.reconcile(ctx)
	if got, _ := st.Job(ctx, orgID, silent); got.Status != JobFailed || !strings.Contains(got.Error, "exited") {
		t.Errorf("stale job with a dead execution: %s %q", got.Status, got.Error)
	}
	// A stale job that speaks again is running again (the store does that on the event).
	h.fd.state = ""
	again := mk(jobRunning, 3600)
	age(again, 6*time.Minute)
	r.reconcile(ctx)
	st.AddJobEvents(ctx, orgID, again, []JobEvent{{Seq: 1, Kind: JobKindHeartbeat}})
	if got, _ := st.Job(ctx, orgID, again); got.Status != jobRunning {
		t.Errorf("stale then event: %s", got.Status)
	}

	// Every terminal transition told the thread.
	posts := h.fs.posted()
	if len(posts) < 5 {
		t.Errorf("reports posted: %d\n%s", len(posts), strings.Join(posts, "\n"))
	}
	// Finished jobs lose their tokens after the grace period.
	st.db.Exec(`update jobs set finished_at=? where status in ('failed','cancelled','timeout')`, "2000-01-01 00:00:00")
	r.reconcile(ctx)
	if got, _ := st.Job(ctx, orgID, late); got.TokenHash != "" {
		t.Error("token of a finished job not revoked")
	}
}
