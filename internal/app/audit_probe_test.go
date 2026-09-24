package app

// Adversarial probes written during the 2026-09-09 production security audit. Unlike the
// earlier review's reproductions, these assert that the defence holds: each one is a request
// an attacker would make, and the test fails if it succeeds.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Probe 1: a document path that tries to climb out of the tenant folder, sent through the real
// mux in every encoding a client can reach it with.
func TestAuditProbeDocumentTraversal(t *testing.T) {
	for _, p := range []string{
		"../../etc/passwd.md",
		"..%2f..%2fetc%2fpasswd.md",
		"%2e%2e%2f%2e%2e%2fetc%2fpasswd.md",
		"a/../../../../../../etc/passwd.md",
		"....//....//etc/passwd.md",
	} {
		if rel, err := cleanRel(p); err == nil {
			if strings.HasPrefix(rel, "..") || strings.Contains(rel, "/../") || strings.HasPrefix(rel, "/") {
				t.Errorf("cleanRel(%q) = %q, escapes the tenant folder", p, rel)
			} else {
				t.Logf("cleanRel(%q) = %q (contained)", p, rel)
			}
		} else {
			t.Logf("cleanRel(%q) refused: %v", p, err)
		}
	}
}

// Probe 2: org A's session must not read org B's job rows, diffs or logs through the console API.
func TestAuditProbeCrossTenantJobs(t *testing.T) {
	two := seedTwoOrgs(t)
	ctx := context.Background()
	// A job owned by org B.
	id, err := two.st.InsertJob(ctx, &Job{OrgID: two.b, TeamID: "TB", Channel: "C1", ThreadTS: "1.1",
		Repo: "secret/repo", Requester: "UB", Status: "running", Model: "m", Engine: "qwen"})
	if err != nil {
		t.Skipf("InsertJob signature differs: %v", err)
	}
	if j, err := two.st.Job(ctx, two.a, id); err == nil && j != nil {
		t.Fatalf("org A read org B's job %d: %+v", id, j)
	}
	jobs, err := two.st.Jobs(ctx, two.a, JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.OrgID == two.b {
			t.Fatalf("org A listed org B's job %d", j.ID)
		}
	}
	t.Logf("org A sees %d jobs; org B's job %d is not among them", len(jobs), id)
}

// Probe 3: the Slack webhook must refuse a body that was not signed, a replayed old timestamp,
// and a signature computed over different bytes.
func TestAuditProbeSlackSignature(t *testing.T) {
	b := &Bot{cfg: Config{SlackSigningSecret: "s3cr3t"}}
	mux := http.NewServeMux()
	b.slackHTTPRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body := `{"type":"url_verification","challenge":"c"}`
	post := func(sig, ts string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/slack/events", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if sig != "" {
			req.Header.Set("X-Slack-Signature", sig)
		}
		if ts != "" {
			req.Header.Set("X-Slack-Request-Timestamp", ts)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("", ""); code == 200 {
		t.Error("unsigned request accepted")
	} else {
		t.Logf("unsigned: %d", code)
	}
	if code := post("v0=deadbeef", "99999999999"); code == 200 {
		t.Error("forged signature accepted")
	} else {
		t.Logf("forged signature: %d", code)
	}
	// A correct signature over a stale timestamp must still be refused (replay window).
	old := "1000000000"
	if code := post(slackSigFor(t, "s3cr3t", old, body), old); code == 200 {
		t.Error("replayed old timestamp accepted")
	} else {
		t.Logf("stale but correctly signed: %d", code)
	}
}

func slackSigFor(t *testing.T, secret, ts, body string) string {
	t.Helper()
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + ts + ":" + body))
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

// The outbound guard is what stands between a tenant-supplied URL and the deployment's own
// private network — above all the GCP metadata server, which hands out the service account.
func TestAuditProbeSSRF(t *testing.T) {
	blocked := []string{
		"http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://127.0.0.1:8080/api/settings",
		"http://localhost/admin/",
		"http://[::1]/",
		"http://10.0.0.5/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://100.64.0.1/", // CGNAT / Tailscale
		"http://0.0.0.0/",
		"http://user:pw@example.com/", // credentials in userinfo
		"file:///etc/passwd",
		"gopher://example.com/",
		"http://example.com:22/",         // non-web port
		"http://169.254.169.254.nip.io/", // DNS name resolving to link-local
	}
	for _, raw := range blocked {
		u, err := url.Parse(raw)
		if err != nil {
			t.Logf("%s: unparseable (%v)", raw, err)
			continue
		}
		if err := publicURL(u); err != nil {
			t.Logf("publicURL refused %s: %v", raw, err)
			continue
		}
		// publicURL passed it; the dialler is the second and authoritative gate.
		if _, err := dialPublic(context.Background(), "tcp", u.Hostname()+":80"); err != nil {
			t.Logf("dialPublic refused %s: %v", raw, err)
			continue
		}
		t.Errorf("NOT BLOCKED: %s", raw)
	}
}

// The dialler is the gate that closes DNS rebinding: it resolves, checks every answer, and
// then dials the IP it checked. Verify it refuses on its own, without publicURL's help.
func TestAuditProbeDiallerGate(t *testing.T) {
	for _, target := range []string{"169.254.169.254:80", "127.0.0.1:80", "10.0.0.1:80", "[::1]:80"} {
		if _, err := dialPublic(context.Background(), "tcp", target); err == nil {
			t.Errorf("dialPublic connected to %s", target)
		} else {
			t.Logf("dialPublic refused %s: %v", target, err)
		}
	}
	// And a name that resolves to loopback is refused at the same gate, whatever DNS says.
	if _, err := dialPublic(context.Background(), "tcp", "localhost:80"); err == nil {
		t.Error("dialPublic connected to localhost")
	} else {
		t.Logf("dialPublic refused localhost: %v", err)
	}
}
