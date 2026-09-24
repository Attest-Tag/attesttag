package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ecsDispatcher runs one Fargate task per fix job. The task definition — image, CPU, memory,
// the execution role that pulls from ECR — is created by deploy/aws/worker.sh; all this does is
// RunTask with the four ATTEST_* variables as a container override, so the definition anyone can
// read in the console never holds a repository token or a model key.
//
// The bot's task role needs ecs:RunTask, ecs:DescribeTasks and ecs:StopTask on the worker
// definition, and iam:PassRole for the task's own roles. The worker's task role is granted
// nothing: everything it uses arrives over the claim call.
//
// There is no AWS SDK here on purpose. ECS speaks AWS JSON 1.1 — one POST per call with the
// operation in X-Amz-Target — and aws_sigv4.go already signs requests for the proxy, so the
// whole client is the hundred lines below rather than a third of a gigabyte of vendored SDK.
type ecsDispatcher struct {
	region, cluster, container string
	subnets, groups            []string
	publicIP                   bool
	definition                 string
	creds                      *awsCreds
	client                     *http.Client
}

const ecsTarget = "AmazonEC2ContainerServiceV20141113."

func newECSDispatcher(cfg Config) (*ecsDispatcher, error) {
	if cfg.WorkerRegion == "" {
		return nil, errors.New("WORKER_REGION (or AWS_REGION) is required in ecs mode")
	}
	// Fargate has no default placement: a task without subnets is refused by the API with a
	// message that does not say which setting is missing, so say it here instead.
	if len(cfg.WorkerECSSubnets) == 0 {
		return nil, errors.New("WORKER_ECS_SUBNETS is required in ecs mode: a Fargate task has to be told which subnets to start in")
	}
	return &ecsDispatcher{
		region:     cfg.WorkerRegion,
		cluster:    nonEmpty(cfg.WorkerECSCluster, "attesttag"),
		container:  "worker", // the container name in the task definition worker.sh registers
		subnets:    cfg.WorkerECSSubnets,
		groups:     cfg.WorkerECSSecurityGroups,
		publicIP:   cfg.WorkerECSPublicIP,
		definition: nonEmpty(cfg.WorkerJobName, "attesttag-worker"),
		creds:      newAWSCreds(),
		client:     &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (d *ecsDispatcher) Name() string { return "ecs" }

// call signs and sends one ECS JSON 1.1 request. region is a parameter rather than d.region so
// that Status and Cancel can follow a task ARN back to the region it was started in, which
// matters after a config change and for the reconciler's first pass at startup.
func (d *ecsDispatcher) call(ctx context.Context, region, op string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://ecs.%s.amazonaws.com/", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", ecsTarget+op)
	cred, err := d.creds.get(ctx, "ecs", region)
	if err != nil {
		return err
	}
	if err := signAWSv4(req, body, cred, time.Now()); err != nil {
		return fmt.Errorf("sign %s: %w", op, err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	raw, err := readAllLimit(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return apiError(op, resp, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// safeECSName keeps a routed target to what a task definition may be called — a family, or a
// family:revision — so a value from configuration cannot become something else.
func safeECSName(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == ':') {
			return false
		}
	}
	return true
}

func (d *ecsDispatcher) Start(ctx context.Context, j *Job, l JobLaunch) (string, error) {
	def := d.definition
	if t := strings.TrimSpace(l.Target); t != "" && safeECSName(t) {
		def = t
	}
	env := []map[string]string{}
	for _, kv := range envPairs(l.env("ecs")) {
		env = append(env, map[string]string{"name": kv[0], "value": kv[1]})
	}
	assign := "DISABLED"
	if d.publicIP {
		assign = "ENABLED"
	}
	in := map[string]any{
		"cluster":        d.cluster,
		"taskDefinition": def,
		"launchType":     "FARGATE",
		"count":          1,
		"networkConfiguration": map[string]any{"awsvpcConfiguration": map[string]any{
			"subnets": d.subnets, "securityGroups": d.groups, "assignPublicIp": assign,
		}},
		// Only the container's environment is overridden. RunTask will not let an override
		// name a container the definition does not have, so a rename in worker.sh surfaces
		// here as a clear API error rather than a task that starts with no job to do.
		"overrides": map[string]any{"containerOverrides": []map[string]any{
			{"name": d.container, "environment": env},
		}},
		"startedBy": fmt.Sprintf("attesttag-job-%d", j.ID),
	}
	var out struct {
		Tasks []struct {
			TaskArn string `json:"taskArn"`
		} `json:"tasks"`
		Failures []struct {
			Arn, Reason, Detail string
		} `json:"failures"`
	}
	if err := d.call(ctx, d.region, "RunTask", in, &out); err != nil {
		return "", fmt.Errorf("run task %s: %w", def, err)
	}
	// RunTask answers 200 with an empty task list when placement fails — no capacity, a subnet
	// with no route, a definition that does not exist. The reason is in `failures`, and losing
	// it would leave the job stuck on "dispatched but never claimed" fifteen minutes later.
	if len(out.Tasks) == 0 {
		why := "no reason given"
		if len(out.Failures) > 0 {
			why = strings.TrimSpace(out.Failures[0].Reason + " " + out.Failures[0].Detail)
		}
		return "", fmt.Errorf("run task %s: ECS started nothing: %s", def, why)
	}
	return out.Tasks[0].TaskArn, nil
}

// ecsRefParts reads the region and cluster back out of a task ARN, which is what makes an
// execution reference self-contained: the reconciler can ask after a task started before a
// restart, or before the cluster setting changed, without consulting the current config.
//
//	arn:aws:ecs:eu-west-2:123456789012:task/attesttag/0123456789abcdef…
func ecsRefParts(ref string) (region, cluster, id string, ok bool) {
	parts := strings.Split(ref, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "ecs" {
		return "", "", "", false
	}
	seg := strings.Split(parts[5], "/")
	if len(seg) != 3 || seg[0] != "task" {
		return "", "", "", false
	}
	return parts[3], seg[1], seg[2], true
}

func (d *ecsDispatcher) Cancel(ctx context.Context, ref string) error {
	region, cluster, _, ok := ecsRefParts(ref)
	if !ok {
		return fmt.Errorf("not an ECS task ARN: %q", ref)
	}
	return d.call(ctx, region, "StopTask", map[string]any{
		"cluster": cluster, "task": ref, "reason": "cancelled from attest_tag",
	}, nil)
}

func (d *ecsDispatcher) Status(ctx context.Context, ref string) (DispatchStatus, error) {
	region, cluster, _, ok := ecsRefParts(ref)
	if !ok {
		return DispatchStatus{State: "unknown", Message: "not an ECS task ARN"}, nil
	}
	var out struct {
		Tasks []struct {
			LastStatus    string `json:"lastStatus"`
			StopCode      string `json:"stopCode"`
			StoppedReason string `json:"stoppedReason"`
			Containers    []struct {
				ExitCode *int   `json:"exitCode"`
				Reason   string `json:"reason"`
			} `json:"containers"`
		} `json:"tasks"`
	}
	if err := d.call(ctx, region, "DescribeTasks", map[string]any{"cluster": cluster, "tasks": []string{ref}}, &out); err != nil {
		return DispatchStatus{State: "unknown", Message: err.Error()}, err
	}
	// ECS keeps a stopped task describable for about an hour and then forgets it. An empty
	// answer is therefore "cannot tell", not "failed" — the reconciler's silence rule decides
	// what to do about a job whose worker stopped reporting, and it is the right judge of it.
	if len(out.Tasks) == 0 {
		return DispatchStatus{State: "unknown", Message: "ECS no longer has this task"}, nil
	}
	t := out.Tasks[0]
	msg := strings.TrimSpace(t.StoppedReason)
	var exit *int
	if len(t.Containers) > 0 {
		exit = t.Containers[0].ExitCode
		if msg == "" {
			msg = strings.TrimSpace(t.Containers[0].Reason)
		}
	}
	switch t.LastStatus {
	case "STOPPED":
		switch {
		case t.StopCode == "UserInitiated":
			return DispatchStatus{State: "cancelled", Message: msg}, nil
		case exit != nil && *exit == 0:
			return DispatchStatus{State: "succeeded", Message: msg}, nil
		case exit != nil:
			return DispatchStatus{State: "failed", Message: nonEmpty(msg, fmt.Sprintf("exit status %d", *exit))}, nil
		default:
			// Stopped with no exit code at all: the container never ran. A pull failure and an
			// out-of-memory kill both land here, and both are worth saying out loud.
			return DispatchStatus{State: "failed", Message: nonEmpty(msg, "the task stopped before the container ran")}, nil
		}
	case "RUNNING", "DEACTIVATING", "STOPPING", "DEPROVISIONING":
		return DispatchStatus{State: "running"}, nil
	default: // PROVISIONING, PENDING, ACTIVATING
		return DispatchStatus{State: "pending"}, nil
	}
}

func ecsConsoleURL(ref string) string {
	region, cluster, id, ok := ecsRefParts(ref)
	if !ok {
		return ""
	}
	return fmt.Sprintf("https://%s.console.aws.amazon.com/ecs/v2/clusters/%s/tasks/%s?region=%s", region, cluster, id, region)
}
