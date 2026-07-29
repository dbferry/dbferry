package pipeline

import (
	"context"
	"testing"
	"time"
)

// TestNewS3ClientOptionBranches exercises the config-assembly branches
// (region, static credentials, custom endpoint) — no network involved.
func TestNewS3ClientOptionBranches(t *testing.T) {
	c, err := newS3Client(context.Background(), Config{
		S3Endpoint: "http://127.0.0.1:1",
		S3Region:   "eu-central-1",
		S3Credentials: &S3Credentials{
			AccessKeyID: "ak", SecretAccessKey: "sk", SessionToken: "st",
		},
	})
	if err != nil || c == nil {
		t.Fatalf("newS3Client: %v", err)
	}
}

func TestNewUploaderMissingProfileFails(t *testing.T) {
	_, err := newUploader(context.Background(), Config{
		S3Profile: "dbferry-definitely-missing-profile",
	})
	if err == nil {
		t.Fatal("a nonexistent shared-config profile must fail client construction")
	}
	if KindOf(err) != KindUpload {
		t.Errorf("kind = %v, want upload", KindOf(err))
	}
}

// TestProbeDestinationUnreachable: with nothing listening on the endpoint the
// probe must report a write failure (and probe no further), not panic or hang.
func TestProbeDestinationUnreachable(t *testing.T) {
	// Short deadline: the SDK retries connection-refused with backoff, and the
	// WriteErr branch is covered the moment the first attempt fails.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p := ProbeDestination(ctx, Config{
		Dest:       "s3://bucket/prefix",
		S3Endpoint: "http://127.0.0.1:1", // reserved port — connection refused
		S3Credentials: &S3Credentials{
			AccessKeyID: "ak", SecretAccessKey: "sk",
		},
		S3Region: "us-east-1",
	})
	if p.Write || p.WriteErr == nil {
		t.Fatalf("probe against a dead endpoint must fail the write: %+v", p)
	}

	// A bad destination URL fails before any request is attempted.
	p = ProbeDestination(ctx, Config{Dest: "http://not-s3"})
	if p.Write || p.WriteErr == nil {
		t.Fatalf("probe with a non-s3 dest must fail: %+v", p)
	}
}
