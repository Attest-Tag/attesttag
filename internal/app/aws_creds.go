package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Where an AWS credential comes from when nobody pasted one in.
//
// The proxy's AWS connections carry a key pair somebody typed (aws_sigv4.go). The ECS dispatcher
// cannot: it signs calls as the bot's *own* task role, which arrives over an HTTP endpoint on a
// link-local address and expires every few hours. This is that chain, in the order the AWS SDKs
// use it and stopping at the first that answers:
//
//  1. AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (+ AWS_SESSION_TOKEN) — an operator's own keys
//  2. AWS_CONTAINER_CREDENTIALS_RELATIVE_URI / _FULL_URI — the ECS task role, which is what
//     deploy/aws/fargate.sh gives the service and the only one that matters in production
//  3. IMDSv2 — an EC2 instance role, for a deployment running on an instance rather than Fargate
//
// Only the pieces those three need are here. A deployment that needs profiles, SSO or
// AssumeRole can set the three variables in (1) from whatever produced them.
type awsCreds struct {
	mu      sync.Mutex
	cur     *Secret
	expires time.Time
	client  *http.Client
}

func newAWSCreds() *awsCreds {
	return &awsCreds{client: &http.Client{Timeout: 10 * time.Second}}
}

const awsCredsSkew = 5 * time.Minute // refresh this far before expiry, as the SDKs do

// get returns a Secret ready for signAWSv4, refreshing a role credential that is about to
// expire. service and region are stamped on it because a container endpoint names neither.
func (c *awsCreds) get(ctx context.Context, service, region string) (*Secret, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur != nil && (c.expires.IsZero() || time.Now().Add(awsCredsSkew).Before(c.expires)) {
		s := *c.cur
		s.AWSService, s.AWSRegion = service, region
		return &s, nil
	}
	s, exp, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}
	c.cur, c.expires = s, exp
	out := *s
	out.AWSService, out.AWSRegion = service, region
	return &out, nil
}

func (c *awsCreds) fetch(ctx context.Context) (*Secret, time.Time, error) {
	if id, sec := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"); id != "" && sec != "" {
		// Static keys do not expire as far as this process can tell. A session token alongside
		// them does, but whoever set it also has to refresh it, so there is nothing to cache.
		return &Secret{AWSKeyID: id, AWSSecret: sec, AWSSessionToken: os.Getenv("AWS_SESSION_TOKEN")}, time.Time{}, nil
	}
	if uri := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); uri != "" {
		return c.fromEndpoint(ctx, "http://169.254.170.2"+uri, os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"))
	}
	if uri := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"); uri != "" {
		tok := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
		if f := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); tok == "" && f != "" {
			if b, err := os.ReadFile(f); err == nil {
				tok = strings.TrimSpace(string(b))
			}
		}
		return c.fromEndpoint(ctx, uri, tok)
	}
	return c.fromIMDS(ctx)
}

// The container credential endpoint and IMDS answer in the same shape.
type awsCredDoc struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

func (c *awsCreds) fromEndpoint(ctx context.Context, url, auth string) (*Secret, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("task role credentials: %w", err)
	}
	raw, err := readAllLimit(resp) // closes the body
	if err != nil {
		return nil, time.Time{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("task role credentials: %s", resp.Status)
	}
	return decodeAWSCreds(raw)
}

// IMDSv2 only: a token has to be fetched by PUT before the credential can be read. IMDSv1 is
// off by default on new instances and turning it back on to save a round trip is the wrong
// trade, so a v1-only instance is simply told to set the three variables itself.
func (c *awsCreds) fromIMDS(ctx context.Context) (*Secret, time.Time, error) {
	const base = "http://169.254.169.254"
	tokReq, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/latest/api/token", nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	tokReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
	tokResp, err := c.client.Do(tokReq)
	if err != nil {
		return nil, time.Time{}, errors.New("no AWS credentials: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or run where a task or instance role is available")
	}
	tok, _ := readAllLimit(tokResp)
	if tokResp.StatusCode != http.StatusOK || len(tok) == 0 {
		return nil, time.Time{}, errors.New("no AWS credentials: the instance metadata service did not issue a token")
	}
	role, err := c.imdsGet(ctx, base+"/latest/meta-data/iam/security-credentials/", string(tok))
	if err != nil {
		return nil, time.Time{}, err
	}
	name := strings.TrimSpace(strings.Split(strings.TrimSpace(role), "\n")[0])
	if name == "" {
		return nil, time.Time{}, errors.New("no AWS credentials: this instance has no role attached")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/latest/meta-data/iam/security-credentials/"+name, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	raw, err := readAllLimit(resp)
	if err != nil {
		return nil, time.Time{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("instance role credentials: %s", resp.Status)
	}
	return decodeAWSCreds(raw)
}

func (c *awsCreds) imdsGet(ctx context.Context, url, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token", token)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	b, err := readAllLimit(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("instance metadata %s: %s", url, resp.Status)
	}
	return string(b), nil
}

func decodeAWSCreds(b []byte) (*Secret, time.Time, error) {
	var doc awsCredDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, time.Time{}, fmt.Errorf("credential document: %w", err)
	}
	if doc.AccessKeyID == "" || doc.SecretAccessKey == "" {
		return nil, time.Time{}, errors.New("credential document had no key pair")
	}
	// A role credential always expires, so an expiry that will not parse must not become the
	// zero time — get() reads that as "never expires" and would hold a dead credential until
	// the process restarted, with every RunTask failing on a signature nobody would connect to
	// a date format. Fifteen minutes is short enough to recover on its own.
	exp, err := time.Parse(time.RFC3339, doc.Expiration)
	if err != nil {
		slog.Warn("AWS credential expiry could not be read; refreshing shortly instead", "value", doc.Expiration, "err", err)
		exp = time.Now().Add(15 * time.Minute)
	}
	return &Secret{AWSKeyID: doc.AccessKeyID, AWSSecret: doc.SecretAccessKey, AWSSessionToken: doc.Token}, exp, nil
}
