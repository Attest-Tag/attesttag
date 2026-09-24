package app

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Documents in an S3-compatible bucket.
//
// This is the backend that makes "runs on any cloud" true. The local one needs a disk and the
// GCS one needs Google, which between them cover a laptop and one deployment; every other
// platform either has no persistent disk at all (Cloudflare Containers) or gives you an object
// store and no FUSE mount (Fargate, Container Apps). S3's API is the one every one of them
// speaks: AWS S3, Cloudflare R2, MinIO, DigitalOcean Spaces, Backblaze B2, Wasabi, Ceph.
//
// It adds no dependency. The AWS SDK is about ten megabytes and this needs six requests, all of
// which are ordinary HTTP once signed — and signAWSv4 was already here, written for the proxy
// so a tenant could point it at an AWS API. The six are GET (list), GET, PUT, DELETE, HEAD, and
// PUT with a copy-source header.

type s3Docs struct {
	client   *http.Client
	endpoint string // https://s3.us-east-1.amazonaws.com or a provider's own
	bucket   string
	prefix   string // no leading or trailing slash
	cred     *Secret
	store    *Store
	orgID    int64
}

// newS3Docs reads the configuration one URL and two secrets carry:
//
//	DOCS_S3_URL=s3://bucket/prefix?endpoint=https://…&region=auto
//	DOCS_S3_KEY_ID, DOCS_S3_SECRET
//
// endpoint may be omitted for AWS itself, where it is derived from the region. region is
// required by the signature even where the provider ignores it — R2 wants "auto".
func newS3Docs(raw, keyID, secret string, st *Store) (*s3Docs, error) {
	// The URL is parsed by replica_target.go, which reads the same shape for the database
	// replica: one spelling of s3://bucket/prefix?endpoint=…&region=…, understood once.
	loc, err := parseS3URL(raw, "DOCS_S3_URL")
	if err != nil {
		return nil, err
	}
	if keyID == "" || secret == "" {
		return nil, fmt.Errorf("DOCS_S3_KEY_ID and DOCS_S3_SECRET are required with DOCS_S3_URL")
	}
	return &s3Docs{
		client:   &http.Client{Timeout: 60 * time.Second},
		endpoint: loc.endpoint,
		bucket:   loc.bucket,
		prefix:   loc.prefix,
		// AWSService is named explicitly rather than derived from the hostname: a custom
		// endpoint — R2, MinIO — is a host signAWSv4 has never heard of, and it would refuse
		// to guess rather than sign something wrong.
		cred:  &Secret{AWSKeyID: keyID, AWSSecret: secret, AWSRegion: loc.region, AWSService: "s3"},
		store: st,
	}, nil
}

func (s *s3Docs) For(orgID int64) DocStore {
	c := *s
	c.prefix = path.Join(s.prefix, orgFolder(orgID))
	c.orgID = orgID
	return &c
}

// key is the object name one relative path maps to, after the same guard gcsDocs applies: a
// path from a request must not climb out of the organisation's own prefix, and a dot-file is
// not a document.
func (s *s3Docs) key(rel string) (string, error) {
	clean, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	return path.Join(s.prefix, clean), nil
}

// do signs one request and runs it. The caller closes the body.
func (s *s3Docs) do(ctx context.Context, method, key string, body []byte, hdr map[string]string) (*http.Response, error) {
	u := s.endpoint + "/" + s.bucket
	if key != "" {
		// Each segment is escaped, but the separators are not: a key is a path, not one name.
		parts := strings.Split(key, "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		u += "/" + strings.Join(parts, "/")
	}
	req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if err := signAWSv4(req, body, s.cred, time.Now()); err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

// query is a signed GET with query parameters, which listing needs and nothing else does.
func (s *s3Docs) query(ctx context.Context, q url.Values) (*http.Response, error) {
	u := s.endpoint + "/" + s.bucket + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	if err := signAWSv4(req, nil, s.cred, time.Now()); err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

func drain(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	resp.Body.Close()
	return strings.TrimSpace(string(b))
}

func (s *s3Docs) List(ctx context.Context) ([]Document, error) {
	// The bucket says what exists; the documents table says what is known about each. Same
	// shape as gcsDocs: a row for a document the bucket no longer has is dropped, so deleting a
	// file out of the bucket by hand does not leave the console listing a document forever.
	meta, _ := s.store.Documents(ctx, s.orgID)
	byPath := map[string]Document{}
	for _, d := range meta {
		byPath[d.Path] = d
	}
	seen := map[string]bool{}
	var out []Document
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {s.prefix + "/"}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.query(ctx, q)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("listing %s: %s %s", s.bucket, resp.Status, drain(resp))
		}
		var res struct {
			Contents []struct {
				Key          string `xml:"Key"`
				Size         int64  `xml:"Size"`
				LastModified string `xml:"LastModified"`
			} `xml:"Contents"`
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", s.bucket, err)
		}
		for _, o := range res.Contents {
			rel := strings.TrimPrefix(strings.TrimPrefix(o.Key, s.prefix), "/")
			// A "directory marker" — a zero-byte object whose name ends in a slash — is how the
			// consoles of these providers show an empty folder. It is not a document.
			if rel == "" || strings.HasSuffix(rel, "/") || !docExts[strings.ToLower(path.Ext(rel))] {
				continue
			}
			d := byPath[rel]
			d.Path, d.Name, d.Size = rel, path.Base(rel), o.Size
			if d.UpdatedAt == "" {
				if t, err := time.Parse(time.RFC3339, o.LastModified); err == nil {
					d.UpdatedAt = t.UTC().Format(time.DateTime)
				}
			}
			if d.Status == "" {
				d.Status = "pending"
			}
			seen[rel] = true
			out = append(out, d)
		}
		if !res.Truncated || res.Next == "" {
			break
		}
		token = res.Next
	}
	for _, d := range meta {
		if !seen[d.Path] {
			s.store.DeleteDocument(ctx, s.orgID, d.Path)
		}
	}
	if out == nil {
		out = []Document{}
	}
	return out, nil
}

func (s *s3Docs) Get(ctx context.Context, rel string) (io.ReadCloser, error) {
	k, err := s.key(rel)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(ctx, "GET", k, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 404 {
		resp.Body.Close()
		return nil, fmt.Errorf("no such document %q", rel)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("reading %q: %s %s", rel, resp.Status, drain(resp))
	}
	return resp.Body, nil
}

func (s *s3Docs) Put(ctx context.Context, name string, r io.Reader, by, scope string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	rel, err := cleanRel(name)
	if err != nil {
		return err
	}
	k, err := s.key(name)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, "PUT", k, b, map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("uploading %q: %s %s", name, resp.Status, drain(resp))
	}
	resp.Body.Close()
	// Record the row, exactly as the local and GCS backends do. Without it the object exists but
	// its scope does not, and rag.go treats an absent scope as visible in every channel — so a
	// channel-restricted document would leak to the whole organisation on an S3/MinIO backend.
	return s.store.UpsertDocument(ctx, s.orgID, Document{Path: rel, Name: path.Base(rel), Size: int64(len(b)), UploadedBy: by, Scope: scope, Status: "pending"})
}

func (s *s3Docs) Delete(ctx context.Context, rel string) error {
	k, err := s.key(rel)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, "DELETE", k, nil, nil)
	if err != nil {
		return err
	}
	// S3 answers 204 whether or not the object was there, which is the semantics wanted here.
	if resp.StatusCode/100 != 2 && resp.StatusCode != 404 {
		return fmt.Errorf("deleting %q: %s %s", rel, resp.Status, drain(resp))
	}
	resp.Body.Close()
	return nil
}

func (s *s3Docs) Exists(ctx context.Context, rel string) (bool, error) {
	k, err := s.key(rel)
	if err != nil {
		return false, err
	}
	resp, err := s.do(ctx, "HEAD", k, nil, nil)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == 200:
		return true, nil
	case resp.StatusCode == 404:
		return false, nil
	}
	return false, fmt.Errorf("checking %q: %s", rel, resp.Status)
}

// Move is a server-side copy and then a delete: S3 has no rename. It refuses to land on a path
// that is already taken, which the interface requires and a bare copy would not give — a copy
// onto an existing key overwrites it silently.
func (s *s3Docs) Move(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	taken, err := s.Exists(ctx, to)
	if err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("%q already exists", to)
	}
	// The copy source is bucket-qualified and escaped the same way the path is.
	fromKey, err := s.key(from)
	if err != nil {
		return err
	}
	toKey, err := s.key(to)
	if err != nil {
		return err
	}
	parts := strings.Split(s.bucket+"/"+fromKey, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	resp, err := s.do(ctx, "PUT", toKey, nil, map[string]string{"x-amz-copy-source": "/" + strings.Join(parts, "/")})
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("copying %q to %q: %s %s", from, to, resp.Status, drain(resp))
	}
	resp.Body.Close()
	if err := s.Delete(ctx, from); err != nil {
		return err
	}
	// Carry the documents row across with the object, exactly as the local and GCS backends do.
	// Without this the row still points at the old key with its scope, the reindex then re-adds the
	// new key with an empty scope, and rag.go treats an empty scope as visible in every channel —
	// so a channel-restricted document would silently become readable org-wide on a rename.
	return s.store.MoveDocument(ctx, s.orgID, from, to)
}
