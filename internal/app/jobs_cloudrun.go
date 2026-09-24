package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	run "google.golang.org/api/run/v2"
)

// cloudRunDispatcher runs one execution of the worker job per fix job. The bot's service account
// needs roles/run.developer on the job (run, runWithOverrides, executions.get/cancel); the job's
// own service account needs nothing. Only the three ATTEST_* variables are overridden, so the
// execution spec anyone can read in the console never holds a repository token or a model key.
type cloudRunDispatcher struct {
	project, region, job string
	svc                  *run.Service
}

func newCloudRunDispatcher(ctx context.Context, cfg Config) (*cloudRunDispatcher, error) {
	project := cfg.WorkerProject
	if project == "" {
		if p, err := metadata.ProjectIDWithContext(ctx); err == nil {
			project = p
		}
	}
	if project == "" {
		return nil, errors.New("WORKER_PROJECT is not set and the metadata server did not answer")
	}
	region := cfg.WorkerRegion
	if region == "" {
		region = cloudRunRegion()
	}
	if region == "" {
		region = "us-central1"
	}
	svc, err := run.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("cloud run client: %w", err)
	}
	return &cloudRunDispatcher{project: project, region: region, job: nonEmpty(cfg.WorkerJobName, "attesttag-worker"), svc: svc}, nil
}

func (d *cloudRunDispatcher) Name() string { return "cloudrun" }

func (d *cloudRunDispatcher) jobName(target string) string {
	name := d.job
	if t := strings.TrimSpace(target); t != "" && safeJobName(t) {
		name = t
	}
	return fmt.Sprintf("projects/%s/locations/%s/jobs/%s", d.project, d.region, name)
}

// safeJobName keeps a routed name to what a Cloud Run job may be called, so a value from
// configuration cannot become a path of its own.
func safeJobName(s string) bool {
	if len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (d *cloudRunDispatcher) Start(ctx context.Context, j *Job, l JobLaunch) (string, error) {
	env := []*run.GoogleCloudRunV2EnvVar{}
	for _, kv := range l.env("cloudrun") {
		k, v, _ := strings.Cut(kv, "=")
		env = append(env, &run.GoogleCloudRunV2EnvVar{Name: k, Value: v})
	}
	req := &run.GoogleCloudRunV2RunJobRequest{Overrides: &run.GoogleCloudRunV2Overrides{
		TaskCount: 1, Timeout: fmt.Sprintf("%ds", j.TimeoutS+120),
		ContainerOverrides: []*run.GoogleCloudRunV2ContainerOverride{{Env: env}},
	}}
	op, err := d.svc.Projects.Locations.Jobs.Run(d.jobName(l.Target), req).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("run job %s: %w", nonEmpty(l.Target, d.job), err)
	}
	var meta struct {
		Name string `json:"name"`
	}
	if len(op.Metadata) > 0 {
		json.Unmarshal(op.Metadata, &meta)
	}
	if meta.Name == "" {
		return "", errors.New("Cloud Run did not return an execution name")
	}
	return meta.Name, nil
}

func (d *cloudRunDispatcher) Cancel(ctx context.Context, ref string) error {
	_, err := d.svc.Projects.Locations.Jobs.Executions.Cancel(ref, &run.GoogleCloudRunV2CancelExecutionRequest{}).Context(ctx).Do()
	return err
}

func (d *cloudRunDispatcher) Status(ctx context.Context, ref string) (DispatchStatus, error) {
	ex, err := d.svc.Projects.Locations.Jobs.Executions.Get(ref).Context(ctx).Do()
	if err != nil {
		return DispatchStatus{State: "unknown", Message: err.Error()}, err
	}
	msg := ""
	for _, c := range ex.Conditions {
		if c != nil && c.Type == "Completed" && c.Message != "" {
			msg = c.Message
		}
	}
	switch {
	case ex.SucceededCount > 0:
		return DispatchStatus{State: "succeeded", Message: msg}, nil
	case ex.FailedCount > 0:
		return DispatchStatus{State: "failed", Message: msg}, nil
	case ex.CancelledCount > 0:
		return DispatchStatus{State: "cancelled", Message: msg}, nil
	case ex.RunningCount > 0:
		return DispatchStatus{State: "running"}, nil
	case ex.CompletionTime != "":
		return DispatchStatus{State: "failed", Message: nonEmpty(msg, "completed without a successful task")}, nil
	}
	return DispatchStatus{State: "pending"}, nil
}

// cloudRunConsoleURL is the Cloud Console page for an execution, for the Jobs page. The
// per-execution deep link (/run/jobs/executions/details/...) only resolves from inside the
// console's own navigation, so link the job's Executions tab, which always opens.
func cloudRunConsoleURL(ref string) string {
	// projects/P/locations/R/jobs/J/executions/E
	parts := strings.Split(ref, "/")
	if len(parts) != 8 {
		return ""
	}
	return fmt.Sprintf("https://console.cloud.google.com/run/jobs/details/%s/%s/executions?project=%s", parts[3], parts[5], parts[1])
}

// cloudRunRegion asks the metadata server; it answers "projects/N/regions/us-central1".
func cloudRunRegion() string {
	req, err := http.NewRequest("GET", "http://metadata.google.internal/computeMetadata/v1/instance/region", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	s := strings.TrimSpace(string(raw))
	return s[strings.LastIndex(s, "/")+1:]
}
