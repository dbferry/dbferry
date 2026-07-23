package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	"golang.org/x/sync/errgroup"
)

// s3API is the subset of S3 operations the uploader needs. The real *s3.Client
// satisfies it; tests inject fakes to simulate part failures, an ambiguous
// CompleteMultipartUpload, and abort verification.
type s3API interface {
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
	ListParts(context.Context, *s3.ListPartsInput, ...func(*s3.Options)) (*s3.ListPartsOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// uploader owns the multipart lifecycle so failures are handled explicitly:
// aborts are verified via ListParts, and an ambiguous Complete is reconciled by
// the unique object key rather than blindly retried or reported failed
// (poc-plan 2.2/2.4).
type uploader struct {
	api         s3API
	partSize    int64
	concurrency int
	partTimeout time.Duration
}

// newS3Client builds the S3 client from Config: standard AWS chain unless the
// destination provides a region/profile/static creds, and checksum-compat +
// path-style for S3-compatible endpoints (ADR-0003).
func newS3Client(ctx context.Context, cfg Config) (*s3.Client, error) {
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = DefaultMaxRetries
	}
	opts := []func(*config.LoadOptions) error{
		config.WithRetryer(func() aws.Retryer {
			return retry.AddWithMaxAttempts(retry.NewStandard(), maxRetries)
		}),
	}
	if cfg.S3Region != "" {
		opts = append(opts, config.WithRegion(cfg.S3Region))
	}
	if cfg.S3Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(cfg.S3Profile))
	}
	if c := cfg.S3Credentials; c != nil {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, classify(KindUpload, "load AWS config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		// Disable the SDK's default-integrity per-part checksums for EVERY
		// target, not only S3-compatible endpoints. We do manual multipart and
		// build each CompletedPart with just ETag+PartNumber; with the v2 SDK
		// default (RequestChecksumCalculationWhenSupported) the SDK adds a CRC32
		// to each UploadPart, and real AWS S3 then rejects CompleteMultipartUpload
		// ("the complete request must include the checksum for each part").
		// End-to-end integrity is the manifest's ciphertext SHA-256, so the
		// per-part checksum is redundant. This branch used to be gated on
		// S3Endpoint, which silently broke every backup to a customer's own AWS
		// bucket (the headline use case) while MinIO CI stayed green (ADR-0003).
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
			o.UsePathStyle = true
		}
	}), nil
}

func newUploader(ctx context.Context, cfg Config) (*uploader, error) {
	client, err := newS3Client(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &uploader{
		api:         client,
		partSize:    cfg.PartSize,
		concurrency: cfg.Concurrency,
		partTimeout: cfg.PartTimeout,
	}, nil
}

// newUploaderWithAPI builds an uploader over an injected S3 API, for tests.
func newUploaderWithAPI(api s3API, partSize int64, concurrency int, partTimeout time.Duration) *uploader {
	return &uploader{api: api, partSize: partSize, concurrency: concurrency, partTimeout: partTimeout}
}

type uploadResult struct {
	bytes  int64
	sha256 string // hex SHA-256 of the ciphertext, for the manifest
}

// upload streams body into a multipart upload and completes it. It returns a
// classified error on failure and never leaves a completed object behind on a
// failure path (the multipart upload is aborted; an ambiguous Complete is
// reconciled by key).
func (u *uploader) upload(ctx context.Context, bucket, key string, body io.Reader, uploaded *atomic.Int64) (uploadResult, error) {
	create, err := u.api.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		return uploadResult{}, classify(KindUpload, "create multipart upload: %w", err)
	}
	uploadID := aws.ToString(create.UploadId)

	parts, total, sum, err := u.streamParts(ctx, bucket, key, uploadID, body, uploaded)
	if err != nil {
		// Body errors are already classified (KindDump) by the feed goroutine;
		// part errors are KindUpload. Either way, abort and surface it.
		u.abort(bucket, key, uploadID)
		return uploadResult{}, err
	}

	_, cerr := u.api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if cerr != nil {
		// Ambiguous completion: the server may have completed the object even
		// though the response failed. Reconcile by the unique key before
		// deciding — never blindly re-Complete, never lose a finished backup.
		if u.objectExists(bucket, key) {
			return uploadResult{bytes: total, sha256: sum}, nil
		}
		u.abort(bucket, key, uploadID)
		return uploadResult{}, classify(KindUpload, "complete multipart upload: %w", cerr)
	}
	return uploadResult{bytes: total, sha256: sum}, nil
}

// streamParts reads body in partSize chunks and uploads up to concurrency parts
// in parallel, hashing the ciphertext in read order. It bounds memory to about
// (concurrency+1) part buffers.
func (u *uploader) streamParts(ctx context.Context, bucket, key, uploadID string, body io.Reader, uploaded *atomic.Int64) ([]types.CompletedPart, int64, string, error) {
	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, u.concurrency)
	var mu sync.Mutex
	var parts []types.CompletedPart
	hasher := sha256.New()
	var total int64
	var partNum int32
	var readErr error

	// Part buffers are pooled and the concurrency slot is acquired BEFORE a
	// buffer is taken, so at most `concurrency` part buffers are ever live and
	// they are reused rather than re-allocated. This bounds peak RSS to about
	// concurrency×partSize plus runtime overhead, independent of dump size
	// (poc-plan 6.3 memory budget).
	bufPool := sync.Pool{New: func() any { b := make([]byte, u.partSize); return &b }}

readLoop:
	for {
		select {
		case sem <- struct{}{}:
		case <-gctx.Done():
			readErr = gctx.Err()
			break readLoop
		}
		bufp := bufPool.Get().(*[]byte)
		buf := *bufp
		n, err := io.ReadFull(body, buf)
		switch {
		case err == nil || err == io.ErrUnexpectedEOF:
			partNum++
			if partNum > maxUploadParts {
				bufPool.Put(bufp)
				<-sem
				readErr = classify(KindUpload,
					"upload exceeded the %d-part S3 limit; increase --part-size and retry", maxUploadParts)
				break readLoop
			}
			hasher.Write(buf[:n])
			total += int64(n)
			pn := partNum
			data := buf[:n]
			g.Go(func() error {
				defer func() { bufPool.Put(bufp); <-sem }()
				etag, uerr := u.uploadPart(gctx, bucket, key, uploadID, pn, data)
				if uerr != nil {
					return classify(KindUpload, "upload part %d: %w", pn, uerr)
				}
				if uploaded != nil {
					uploaded.Add(int64(len(data)))
				}
				mu.Lock()
				parts = append(parts, types.CompletedPart{ETag: aws.String(etag), PartNumber: aws.Int32(pn)})
				mu.Unlock()
				return nil
			})
			if err == io.ErrUnexpectedEOF {
				break readLoop // partial final part dispatched
			}
		case err == io.EOF:
			bufPool.Put(bufp)
			<-sem
			break readLoop // clean end
		default:
			bufPool.Put(bufp)
			<-sem
			readErr = err // body/dump error (already classified by feed)
			break readLoop
		}
	}

	werr := g.Wait()
	if werr != nil {
		return nil, 0, "", werr
	}
	if readErr != nil {
		return nil, 0, "", readErr
	}
	sort.Slice(parts, func(i, j int) bool {
		return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber)
	})
	return parts, total, hex.EncodeToString(hasher.Sum(nil)), nil
}

func (u *uploader) uploadPart(ctx context.Context, bucket, key, uploadID string, pn int32, data []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, u.partTimeout)
	defer cancel()
	out, err := u.api.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(pn),
		Body:       bytes.NewReader(data), // seekable, so the SDK can retry
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.ETag), nil
}

// abort tears down an incomplete multipart upload, verifying via ListParts and
// retrying. It runs on a fresh context so cancelling the run doesn't skip
// cleanup. If it can't confirm removal, a bucket lifecycle rule reclaims the
// upload (poc-plan 2.2).
func (u *uploader) abort(bucket, key, uploadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		u.api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		})
		_, err := u.api.ListParts(ctx, &s3.ListPartsInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		})
		if isNoSuchUpload(err) {
			return // confirmed gone
		}
		if err != nil {
			continue // transient; retry
		}
		// Parts still listed: the upload survived; retry the abort.
	}
}

// objectExists reports whether the completed object is present, on a fresh
// context so run cancellation doesn't skew reconciliation.
func (u *uploader) objectExists(bucket, key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := u.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	return err == nil
}

// putObject writes a small object (the manifest) in a single request.
func (u *uploader) putObject(ctx context.Context, bucket, key string, body []byte, contentType string) error {
	_, err := u.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
	})
	return err
}

func isNoSuchUpload(err error) bool {
	if err == nil {
		return false
	}
	var nsu *types.NoSuchUpload
	if errors.As(err, &nsu) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode() == "NoSuchUpload"
	}
	return false
}
