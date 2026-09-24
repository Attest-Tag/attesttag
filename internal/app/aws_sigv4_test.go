package app

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A signature is either exactly right or worthless, so these are known-answer
// tests: the expected headers were computed by a second implementation written
// from the specification, not by this one.

const (
	testAWSKey    = "AKIDEXAMPLE"
	testAWSSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

func TestAWSEndpointFromHost(t *testing.T) {
	cases := []struct {
		host, wantService, wantRegion string
		service, region               string
	}{
		{host: "monitoring.eu-west-1.amazonaws.com", service: "monitoring", region: "eu-west-1"},
		{host: "bucket.s3.eu-west-1.amazonaws.com", service: "s3", region: "eu-west-1"},
		{host: "xyz.execute-api.ap-southeast-2.amazonaws.com", service: "execute-api", region: "ap-southeast-2"},
		{host: "logs.us-gov-west-1.amazonaws.com", service: "logs", region: "us-gov-west-1"},
		// Global: signed against us-east-1 even when the connection says otherwise.
		{host: "sts.amazonaws.com", wantRegion: "eu-central-1", service: "sts", region: "us-east-1"},
		{host: "iam.amazonaws.com", service: "iam", region: "us-east-1"},
		// A host that names no region and no known global service falls back.
		{host: "vpce-1234.execute-api.internal", wantRegion: "eu-central-1", wantService: "execute-api", service: "execute-api", region: "eu-central-1"},
	}
	for _, c := range cases {
		service, region := awsEndpoint(c.host, c.wantService, c.wantRegion)
		if service != c.service || region != c.region {
			t.Errorf("%s → %s/%s, want %s/%s", c.host, service, region, c.service, c.region)
		}
	}
}

func TestSignAWSv4JSONProtocol(t *testing.T) {
	body := `{"logGroupName":"/aws/lambda/checkout","limit":5}`
	hr, err := http.NewRequest("POST", "https://logs.eu-west-1.amazonaws.com/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	hr.Header.Set("Content-Type", "application/x-amz-json-1.1")
	hr.Header.Set("X-Amz-Target", "Logs_20140328.FilterLogEvents") // must be signed, or AWS refuses
	hr.Header.Set("User-Agent", "attesttag/0.2 (+proxy)")          // must not be

	s := &Secret{AWSKeyID: testAWSKey, AWSSecret: testAWSSecret, AWSRegion: "us-east-1"}
	if err := signAWSv4(hr, []byte(body), s, time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260903/eu-west-1/logs/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date;x-amz-target, " +
		"Signature=7a0ccc1a6de7ec05d305799e8238dbd3a445cdbe2d1cd3bfa8dd80cd756a150d"
	if got := hr.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
	if got := hr.Header.Get("X-Amz-Date"); got != "20260903T120000Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
}

func TestSignAWSv4QueryProtocol(t *testing.T) {
	// The query is out of order on purpose: the signature sorts it, the request
	// keeps it as written.
	hr, err := http.NewRequest("GET", "https://monitoring.us-east-1.amazonaws.com/?Action=DescribeAlarms&StateValue=ALARM&Version=2010-08-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Secret{AWSKeyID: testAWSKey, AWSSecret: testAWSSecret}
	if err := signAWSv4(hr, nil, s, time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260903/us-east-1/monitoring/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
		"Signature=9b43da677372ce9f5ff1a8e7e088bff3e9f7f16c172b299feaaf31bef0d6f5bb"
	if got := hr.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

// The body is part of what is signed, so two requests that differ only in their
// payload must not share a signature — this is the property that stops a held
// write from being replayed with different arguments.
func TestSignAWSv4CoversTheBody(t *testing.T) {
	sign := func(body string) string {
		hr, _ := http.NewRequest("POST", "https://logs.eu-west-1.amazonaws.com/", strings.NewReader(body))
		s := &Secret{AWSKeyID: testAWSKey, AWSSecret: testAWSSecret}
		if err := signAWSv4(hr, []byte(body), s, time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
		return hr.Header.Get("Authorization")
	}
	if sign(`{"limit":5}`) == sign(`{"limit":500}`) {
		t.Error("the signature ignores the body")
	}
}

func TestSignAWSv4NeedsBothHalvesOfTheKey(t *testing.T) {
	hr, _ := http.NewRequest("GET", "https://sts.amazonaws.com/", nil)
	if err := signAWSv4(hr, nil, &Secret{AWSKeyID: testAWSKey}, time.Now()); err == nil {
		t.Error("signed with no secret access key")
	}
	if hr.Header.Get("Authorization") != "" {
		t.Error("a failed signing still set Authorization")
	}
}

func TestAWSCanonicalQueryEscaping(t *testing.T) {
	hr, _ := http.NewRequest("GET", "https://ec2.eu-west-1.amazonaws.com/?b=2&a=hello+world&a=1&t=~x", nil)
	if got, want := awsCanonicalQuery(hr.URL), "a=1&a=hello%20world&b=2&t=~x"; got != want {
		t.Errorf("canonical query = %q, want %q", got, want)
	}
}
