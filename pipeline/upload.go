package pipeline

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// uploader wraps the S3 multipart manager. On a streaming (non-seekable) Body
// it buffers one part at a time and uploads Concurrency parts in parallel, and
// it aborts the multipart upload if the Body returns an error before EOF — so a
// failed dump never leaves a completed object.
//
// TODO(migration): feature/s3/manager is deprecated in favour of
// feature/s3/transfermanager; migrate once that module reaches a stable (v1)
// release. It is currently v0.x (developer preview).
type uploader struct {
	mgr *manager.Uploader
}

func newUploader(ctx context.Context, cfg Config) (*uploader, error) {
	// Credentials and region come from the standard AWS chain (env vars,
	// shared config); no secret is taken on argv.
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("pipeline: load AWS config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			// S3-compatible endpoint (e.g. MinIO): path-style addressing so a
			// bucket name isn't required to be a DNS label.
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
			o.UsePathStyle = true
		}
	})
	mgr := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = cfg.PartSize
		u.Concurrency = cfg.Concurrency
		u.MaxUploadParts = maxUploadParts
	})
	return &uploader{mgr: mgr}, nil
}

type uploadResult struct{ bytes int64 }

func (u *uploader) upload(ctx context.Context, bucket, key string, body io.Reader) (uploadResult, error) {
	cr := &countingReader{r: body}
	_, err := u.mgr.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   cr,
	})
	if err != nil {
		return uploadResult{}, uploadError(err)
	}
	return uploadResult{bytes: cr.n.Load()}, nil
}

// uploadError adds an actionable hint when the 10k-part S3 limit is hit
// (DECISIONS.md): the fix is a larger part size, not a retry.
func uploadError(err error) error {
	if strings.Contains(err.Error(), "MaxUploadParts") {
		return fmt.Errorf("pipeline: upload exceeded the %d-part S3 limit; increase --part-size and retry: %w", maxUploadParts, err)
	}
	return fmt.Errorf("pipeline: S3 upload: %w", err)
}

// countingReader counts the ciphertext bytes handed to the uploader without
// buffering them, so Result.Bytes reflects what actually landed in S3.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
