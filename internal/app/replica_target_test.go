package app

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// clearReplicaEnv makes each case start from a deployment that has configured nothing, so a
// variable a previous case set cannot decide this one.
func clearReplicaEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LITESTREAM_S3_URL", "LITESTREAM_S3_KEY_ID", "LITESTREAM_S3_SECRET",
		"LITESTREAM_BUCKET", "DOCS_S3_URL", "DOCS_S3_KEY_ID", "DOCS_S3_SECRET",
	} {
		t.Setenv(k, "")
	}
}

const r2 = "s3://acme/docs?region=auto&endpoint=https://acct.r2.cloudflarestorage.com"

func TestTheReplicaGoesToWhicheverBucketYouGaveIt(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		want     string // "" means no replication at all
		derived  bool
		endpoint string
	}{
		{name: "no bucket at all is local, and not an error", want: ""},
		{
			name: "the documents bucket is borrowed when it is the only one",
			env:  map[string]string{"DOCS_S3_URL": r2, "DOCS_S3_KEY_ID": "k", "DOCS_S3_SECRET": "s"},
			// Under the documents prefix, because two deployments sharing a bucket are told
			// apart by exactly that prefix.
			want:     "s3://acme/docs/litestream/attesttag.db",
			derived:  true,
			endpoint: "https://acct.r2.cloudflarestorage.com",
		},
		{
			name: "a GCS bucket named for the database",
			env:  map[string]string{"LITESTREAM_BUCKET": "acme-data"},
			want: "gs://acme-data/litestream/attesttag.db",
		},
		{
			name: "a bucket named for the database beats the documents bucket",
			env: map[string]string{
				"DOCS_S3_URL": r2, "DOCS_S3_KEY_ID": "k", "DOCS_S3_SECRET": "s",
				"LITESTREAM_S3_URL": "s3://backups/db/attesttag.db?region=eu-west-2",
			},
			want:     "s3://backups/db/attesttag.db",
			endpoint: "https://s3.eu-west-2.amazonaws.com",
		},
		{
			name: "a bucket named for the database beats a GCS one too",
			env: map[string]string{
				"LITESTREAM_BUCKET": "acme-data",
				"LITESTREAM_S3_URL": "s3://backups?region=us-east-1", "LITESTREAM_S3_KEY_ID": "k", "LITESTREAM_S3_SECRET": "s",
			},
			want: "s3://backups/litestream/attesttag.db", // no prefix in the URL: the default key
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearReplicaEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := resolveReplica("litestream/attesttag.db")
			if err != nil {
				t.Fatalf("resolveReplica: %v", err)
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want no replication, got %s", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want %s, got no replication", tc.want)
			}
			if got.String() != tc.want {
				t.Errorf("replica = %s, want %s", got, tc.want)
			}
			if got.derived != tc.derived {
				t.Errorf("derived = %v, want %v", got.derived, tc.derived)
			}
			if tc.endpoint != "" && got.endpoint != tc.endpoint {
				t.Errorf("endpoint = %q, want %q", got.endpoint, tc.endpoint)
			}
		})
	}
}

// The borrowed bucket is the one case where the database and the documents share a prefix, so
// the keys they use must not be able to collide. Documents live under <prefix>/org-<id>/.
func TestTheBorrowedBucketCannotPutTheDatabaseInsideADocumentFolder(t *testing.T) {
	clearReplicaEnv(t)
	t.Setenv("DOCS_S3_URL", r2)
	t.Setenv("DOCS_S3_KEY_ID", "k")
	t.Setenv("DOCS_S3_SECRET", "s")

	target, err := resolveReplica("litestream/attesttag.db")
	if err != nil || target == nil {
		t.Fatalf("resolveReplica: %v, %v", target, err)
	}
	for _, org := range []int64{1, 42, 10001} {
		folder := path.Join("docs", orgFolder(org)) + "/"
		if strings.HasPrefix(target.key, folder) {
			t.Errorf("replica key %q is inside an organisation's document folder %q", target.key, folder)
		}
		if strings.HasPrefix(target.leaseKey(), folder) {
			t.Errorf("lease key %q is inside an organisation's document folder %q", target.leaseKey(), folder)
		}
	}
	if want := target.key + ".lock.json"; target.leaseKey() != want {
		t.Errorf("lease key = %q, want %q — it must sit beside the replica", target.leaseKey(), want)
	}
}

func TestAMisspeltBucketIsAStartupErrorRatherThanSilentlyNoReplication(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		env        map[string]string
	}{
		{name: "not a URL at all", want: "LITESTREAM_S3_URL", env: map[string]string{"LITESTREAM_S3_URL": "my-bucket"}},
		{name: "https where s3 was meant", want: "LITESTREAM_S3_URL", env: map[string]string{"LITESTREAM_S3_URL": "https://acme.s3.amazonaws.com/db"}},
		{
			name: "a bucket with no key to sign with",
			want: "LITESTREAM_S3_KEY_ID",
			env:  map[string]string{"LITESTREAM_S3_URL": "s3://backups?region=us-east-1"},
		},
		{
			name: "documents bucket with no key to sign with",
			want: "DOCS_S3_KEY_ID",
			env:  map[string]string{"DOCS_S3_URL": r2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearReplicaEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := resolveReplica("litestream/attesttag.db")
			if err == nil {
				t.Fatalf("want an error naming %s, got target %v", tc.want, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

// litestreamConf is the part of Litestream's configuration this renders.
type litestreamConf struct {
	DBs []struct {
		Path     string `yaml:"path"`
		Replicas []struct {
			Type             string `yaml:"type"`
			Bucket           string `yaml:"bucket"`
			Path             string `yaml:"path"`
			Region           string `yaml:"region"`
			Endpoint         string `yaml:"endpoint"`
			ForcePathStyle   bool   `yaml:"force-path-style"`
			SyncInterval     string `yaml:"sync-interval"`
			Retention        string `yaml:"retention"`
			SnapshotInterval string `yaml:"snapshot-interval"`
			Bogus            string `yaml:"bogus"`
		} `yaml:"replicas"`
	} `yaml:"dbs"`
}

func renderConf(t *testing.T, target *replicaTarget, dbPath string) (litestreamConf, string) {
	t.Helper()
	raw := target.litestreamYAML(dbPath)
	var conf litestreamConf
	if err := yaml.Unmarshal([]byte(raw), &conf); err != nil {
		t.Fatalf("the generated config is not valid YAML: %v\n%s", err, raw)
	}
	if len(conf.DBs) != 1 || len(conf.DBs[0].Replicas) != 1 {
		t.Fatalf("want one database with one replica, got %+v\n%s", conf, raw)
	}
	if conf.DBs[0].Path != dbPath {
		t.Errorf("db path = %q, want %q", conf.DBs[0].Path, dbPath)
	}
	r := conf.DBs[0].Replicas[0]
	if r.SyncInterval != replicaSyncInterval {
		t.Errorf("sync-interval = %q, want %q", r.SyncInterval, replicaSyncInterval)
	}
	// Litestream 0.5 accepts unknown keys without a word and honours neither of these: a config
	// that carried them would be stating a retention policy nothing enforces.
	if r.Retention != "" || r.SnapshotInterval != "" {
		t.Errorf("the config states a retention policy Litestream 0.5 ignores: %+v", r)
	}
	return conf, raw
}

func TestTheGeneratedConfigNamesTheReplicaItResolved(t *testing.T) {
	t.Run("gcs", func(t *testing.T) {
		conf, _ := renderConf(t, &replicaTarget{bucket: "acme-data", key: "litestream/attesttag.db"}, "/data/attesttag.db")
		r := conf.DBs[0].Replicas[0]
		if r.Type != "gs" || r.Bucket != "acme-data" || r.Path != "litestream/attesttag.db" {
			t.Errorf("gcs replica = %+v", r)
		}
	})

	t.Run("an S3-compatible store is addressed path-style", func(t *testing.T) {
		target := &replicaTarget{s3: true, bucket: "acme", key: "docs/litestream/attesttag.db",
			endpoint: "https://acct.r2.cloudflarestorage.com", region: "auto", keyID: "AKIA", secret: "shhh"}
		conf, raw := renderConf(t, target, "/data/attesttag.db")
		r := conf.DBs[0].Replicas[0]
		if r.Type != "s3" || r.Bucket != "acme" || r.Path != "docs/litestream/attesttag.db" || r.Region != "auto" {
			t.Errorf("s3 replica = %+v", r)
		}
		// MinIO and Ceph only answer path-style, and R2 accepts it.
		if r.Endpoint != target.endpoint || !r.ForcePathStyle {
			t.Errorf("endpoint = %q, force-path-style = %v", r.Endpoint, r.ForcePathStyle)
		}
		// The file is readable out of a running container, so no credential may be in it.
		if strings.Contains(raw, "shhh") || strings.Contains(raw, "AKIA") {
			t.Errorf("the generated config contains a credential:\n%s", raw)
		}
		env := strings.Join(target.childEnv(), " ")
		if !strings.Contains(env, "LITESTREAM_ACCESS_KEY_ID=AKIA") || !strings.Contains(env, "LITESTREAM_SECRET_ACCESS_KEY=shhh") {
			t.Errorf("the child cannot sign its requests: %v", target.childEnv())
		}
	})

	t.Run("AWS itself is left to virtual-host addressing", func(t *testing.T) {
		target := &replicaTarget{s3: true, bucket: "acme", key: "db/attesttag.db",
			endpoint: "https://s3.eu-west-2.amazonaws.com", region: "eu-west-2", keyID: "k", secret: "s"}
		conf, _ := renderConf(t, target, "/data/attesttag.db")
		r := conf.DBs[0].Replicas[0]
		if r.Endpoint != "" || r.ForcePathStyle {
			t.Errorf("AWS should keep its own addressing, got endpoint %q force-path-style %v", r.Endpoint, r.ForcePathStyle)
		}
	})
}

// The binary that will read this config is the only authority on whether it is valid — and it
// accepts unknown keys in silence, so a rendering mistake would otherwise show up as a replica
// that is quietly not replicating. Skipped where litestream is absent, which is most laptops;
// the container image has it.
func TestLitestreamItselfReadsTheGeneratedConfig(t *testing.T) {
	bin, err := exec.LookPath("litestream")
	if err != nil {
		t.Skip("no litestream on PATH")
	}
	for _, tc := range []struct {
		name, want string
		target     *replicaTarget
	}{
		{"gcs", "gs", &replicaTarget{bucket: "acme-data", key: "litestream/attesttag.db"}},
		{"s3", "s3", &replicaTarget{s3: true, bucket: "acme", key: "docs/litestream/attesttag.db",
			endpoint: "http://127.0.0.1:9000", region: "us-east-1", keyID: "k", secret: "s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "attesttag.db")
			conf := filepath.Join(t.TempDir(), "litestream.yml")
			if err := os.WriteFile(conf, []byte(tc.target.litestreamYAML(dbPath)), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bin, "databases", "-config", conf).CombinedOutput()
			if err != nil {
				t.Fatalf("litestream databases: %v\n%s", err, out)
			}
			// It prints the database and the replica type it resolved, which is the whole of
			// what this file has to get right.
			if !strings.Contains(string(out), dbPath) || !strings.Contains(string(out), tc.want) {
				t.Errorf("litestream read the config as:\n%s\nwant %s replicating to %s", out, dbPath, tc.want)
			}
		})
	}
}
