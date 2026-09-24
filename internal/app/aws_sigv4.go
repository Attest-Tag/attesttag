package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4.
//
// AWS is the one credential in the catalogue that cannot be pasted into a
// header. There is no bearer token: every request carries a signature computed
// over its own method, path, query, headers and body, so the proxy has to do
// the arithmetic itself. The upside is that the secret access key is never on
// the wire at all — what travels is a hash — which is the same property the
// rest of this file's neighbours get from short-lived tokens.
//
// The signature covers the request as it will be sent, so this runs last, after
// inject() has settled every other header.
const awsAlgorithm = "AWS4-HMAC-SHA256"

// A region label inside a hostname: us-east-1, eu-west-3, ap-southeast-2, and
// the partitions (us-gov-west-1, us-iso-east-1).
var awsRegionLabel = regexp.MustCompile(`^[a-z]{2}(-gov|-isob?)?-[a-z]+-\d+$`)

// The services AWS serves from one place. They are signed against the region
// below whatever the connection's default region is, which is what a signing
// error against sts.amazonaws.com is nearly always about.
var awsGlobalServices = map[string]string{
	"iam": "us-east-1", "sts": "us-east-1", "cloudfront": "us-east-1",
	"route53": "us-east-1", "organizations": "us-east-1", "support": "us-east-1",
	"health": "us-east-1", "shield": "us-east-1", "budgets": "us-east-1",
	"globalaccelerator": "us-west-2",
}

// awsEndpoint reads the service and region out of an AWS hostname, which is
// where both actually live: monitoring.eu-west-1.amazonaws.com is CloudWatch in
// Ireland, and bucket.s3.eu-west-1.amazonaws.com is still S3. A host naming no
// region is either global (signed per the table above) or the connection's
// default. What the connection stores explicitly wins over both, because it is
// the escape hatch for the endpoints that follow no pattern.
func awsEndpoint(host, wantService, wantRegion string) (service, region string) {
	labels := strings.Split(strings.ToLower(strings.TrimSuffix(host, ".")), ".")
	for i, l := range labels {
		if awsRegionLabel.MatchString(l) {
			region = l
			if i > 0 {
				service = labels[i-1]
			}
			break
		}
	}
	if service == "" { // no region in the host: the label before amazonaws is the service
		for i, l := range labels {
			if (l == "amazonaws" || l == "api") && i > 0 {
				service = labels[i-1]
				break
			}
		}
	}
	if wantService != "" {
		service = wantService
	}
	if region == "" {
		switch {
		case awsGlobalServices[service] != "":
			region = awsGlobalServices[service]
		case wantRegion != "":
			region = wantRegion
		default:
			region = "us-east-1"
		}
	}
	return service, region
}

// signAWSv4 adds the Authorization header (and the x-amz-* headers it signs) to
// a request that is otherwise ready to send. `payload` is the exact body bytes,
// because their hash is part of what gets signed.
func signAWSv4(hr *http.Request, payload []byte, s *Secret, now time.Time) error {
	if s.AWSKeyID == "" || s.AWSSecret == "" {
		return errors.New("access key id and secret access key are required")
	}
	service, region := awsEndpoint(hr.URL.Hostname(), s.AWSService, s.AWSRegion)
	if service == "" {
		return fmt.Errorf("cannot tell which AWS service %q belongs to; name it on the connection", hr.URL.Hostname())
	}

	utc := now.UTC()
	amzDate, dateStamp := utc.Format("20060102T150405Z"), utc.Format("20060102")
	sum := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(sum[:])

	hr.Header.Set("X-Amz-Date", amzDate)
	hr.Header.Set("X-Amz-Content-Sha256", payloadHash) // S3 insists on it; the rest ignore it
	if s.AWSSessionToken != "" {
		hr.Header.Set("X-Amz-Security-Token", s.AWSSessionToken)
	}

	// Host and every x-amz-* header must be signed — that last rule is why a
	// model may set X-Amz-Target for the JSON services and still be believed.
	// Content-Type joins them when it is set, because the JSON protocols carry
	// their protocol version in it.
	values := map[string]string{"host": hr.URL.Host}
	if ct := hr.Header.Get("Content-Type"); ct != "" {
		values["content-type"] = ct
	}
	for name, vs := range hr.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-") {
			values[lower] = strings.Join(vs, ",")
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, name := range names {
		canonHeaders.WriteString(name + ":" + awsTrimValue(values[name]) + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		hr.Method,
		awsCanonicalPath(hr.URL),
		awsCanonicalQuery(hr.URL),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	crHash := sha256.Sum256([]byte(canonicalRequest))

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{awsAlgorithm, amzDate, scope, hex.EncodeToString(crHash[:])}, "\n")

	key := awsHMAC([]byte("AWS4"+s.AWSSecret), dateStamp)
	key = awsHMAC(key, region)
	key = awsHMAC(key, service)
	key = awsHMAC(key, "aws4_request")
	signature := hex.EncodeToString(awsHMAC(key, stringToSign))

	hr.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		awsAlgorithm, s.AWSKeyID, scope, signedHeaders, signature))
	return nil
}

func awsHMAC(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// The path as it will be sent. Everything the proxy signs was already escaped
// once by url.Parse, and every AWS path a model writes here is plain ASCII, so
// this is the identity for real requests and matches what S3 expects for the
// keys that are not.
func awsCanonicalPath(u *url.URL) string {
	if p := u.EscapedPath(); p != "" {
		return p
	}
	return "/"
}

// Query parameters sorted by name, then by value, each escaped the way the
// signature expects: RFC 3986, so a space is %20 rather than +.
func awsCanonicalQuery(u *url.URL) string {
	q := u.Query()
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(q))
	for _, name := range names {
		vals := append([]string(nil), q[name]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsEscape(name)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func awsEscape(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "%7E", "~")
	return e
}

// A signed header value is trimmed, and runs of spaces inside it collapse to one.
func awsTrimValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}
