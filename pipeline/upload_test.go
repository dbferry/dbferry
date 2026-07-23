package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// fakeS3 is an in-memory s3API for exercising the multipart lifecycle and its
// failure/reconciliation paths without a real backend.
type fakeS3 struct {
	mu        sync.Mutex
	parts     map[int32]int
	object    bool // the completed object exists
	aborts    int
	completes int

	onCreate   error
	onPart     func(pn int32) error
	onComplete error
	// completeCreatesObject models an ambiguous Complete: the request errors
	// but the server did complete the object.
	completeCreatesObject bool
}

func newFakeS3() *fakeS3 { return &fakeS3{parts: map[int32]int{}} }

func (f *fakeS3) CreateMultipartUpload(_ context.Context, _ *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	if f.onCreate != nil {
		return nil, f.onCreate
	}
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("up-1")}, nil
}

func (f *fakeS3) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	pn := aws.ToInt32(in.PartNumber)
	if f.onPart != nil {
		if err := f.onPart(pn); err != nil {
			return nil, err
		}
	}
	b, _ := io.ReadAll(in.Body)
	f.mu.Lock()
	f.parts[pn] = len(b)
	f.mu.Unlock()
	return &s3.UploadPartOutput{ETag: aws.String("etag-" + strconv.Itoa(int(pn)))}, nil
}

func (f *fakeS3) CompleteMultipartUpload(_ context.Context, _ *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.mu.Lock()
	f.completes++
	f.mu.Unlock()
	if f.onComplete != nil {
		if f.completeCreatesObject {
			f.mu.Lock()
			f.object = true
			f.mu.Unlock()
		}
		return nil, f.onComplete
	}
	f.mu.Lock()
	f.object = true
	f.mu.Unlock()
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (f *fakeS3) AbortMultipartUpload(_ context.Context, _ *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	f.aborts++
	f.parts = map[int32]int{}
	f.mu.Unlock()
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (f *fakeS3) ListParts(_ context.Context, _ *s3.ListPartsInput, _ ...func(*s3.Options)) (*s3.ListPartsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aborts > 0 {
		return nil, &types.NoSuchUpload{}
	}
	return &s3.ListPartsOutput{}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.object {
		return nil, errors.New("NotFound")
	}
	return &s3.HeadObjectOutput{}, nil
}

func (f *fakeS3) PutObject(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return &s3.PutObjectOutput{}, nil
}

func doUpload(t *testing.T, f *fakeS3, partSize int64, data []byte) (uploadResult, error) {
	t.Helper()
	u := newUploaderWithAPI(f, partSize, 4, time.Minute)
	return u.upload(context.Background(), "bucket", "key", bytes.NewReader(data), new(atomic.Int64))
}

func TestUploadHappyPathSinglePart(t *testing.T) {
	f := newFakeS3()
	data := bytes.Repeat([]byte("x"), 100)
	res, err := doUpload(t, f, 32<<20, data)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !f.object {
		t.Fatal("object not completed")
	}
	if res.bytes != 100 {
		t.Errorf("bytes = %d, want 100", res.bytes)
	}
	want := hex.EncodeToString(sha256Sum(data))
	if res.sha256 != want {
		t.Errorf("sha256 = %s, want %s", res.sha256, want)
	}
	if len(f.parts) != 1 {
		t.Errorf("parts = %d, want 1", len(f.parts))
	}
}

func TestUploadMultiPartHashOrder(t *testing.T) {
	f := newFakeS3()
	// 2.5 parts at a 10-byte part size.
	data := bytes.Repeat([]byte("abcde"), 5) // 25 bytes
	res, err := doUpload(t, f, 10, data)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(f.parts) != 3 {
		t.Errorf("parts = %d, want 3", len(f.parts))
	}
	if want := hex.EncodeToString(sha256Sum(data)); res.sha256 != want {
		t.Errorf("multipart sha256 = %s, want %s", res.sha256, want)
	}
}

func TestUploadPartFailureAbortsNoObject(t *testing.T) {
	f := newFakeS3()
	f.onPart = func(pn int32) error {
		if pn == 2 {
			return errors.New("boom")
		}
		return nil
	}
	data := bytes.Repeat([]byte("y"), 25)
	_, err := doUpload(t, f, 10, data)
	if err == nil {
		t.Fatal("expected error")
	}
	if KindOf(err) != KindUpload {
		t.Errorf("kind = %v, want upload", KindOf(err))
	}
	if f.object {
		t.Error("no object must exist after a part failure")
	}
	if f.aborts == 0 {
		t.Error("abort must have been attempted and verified")
	}
}

func TestAmbiguousCompleteReconcilesToSuccess(t *testing.T) {
	f := newFakeS3()
	f.onComplete = errors.New("i/o timeout reading complete response")
	f.completeCreatesObject = true // server completed it despite the error
	data := bytes.Repeat([]byte("z"), 50)
	res, err := doUpload(t, f, 10, data)
	if err != nil {
		t.Fatalf("ambiguous complete should reconcile to success, got: %v", err)
	}
	if res.bytes != 50 {
		t.Errorf("bytes = %d, want 50", res.bytes)
	}
	if f.aborts != 0 {
		t.Error("a completed object must not be aborted")
	}
}

func TestCompleteGenuineFailureAbortsNoObject(t *testing.T) {
	f := newFakeS3()
	f.onComplete = errors.New("access denied")
	f.completeCreatesObject = false // truly failed; no object
	data := bytes.Repeat([]byte("z"), 50)
	_, err := doUpload(t, f, 10, data)
	if err == nil {
		t.Fatal("expected error")
	}
	if KindOf(err) != KindUpload {
		t.Errorf("kind = %v, want upload", KindOf(err))
	}
	if f.aborts == 0 {
		t.Error("a failed completion must abort the upload")
	}
}

func TestMaxPartsExceeded(t *testing.T) {
	f := newFakeS3()
	data := bytes.Repeat([]byte("p"), maxUploadParts+1) // one byte per part
	_, err := doUpload(t, f, 1, data)
	if err == nil {
		t.Fatal("expected 10k-part limit error")
	}
	if KindOf(err) != KindUpload {
		t.Errorf("kind = %v, want upload", KindOf(err))
	}
	if f.object {
		t.Error("must not complete past the part limit")
	}
}

func TestIsNoSuchUpload(t *testing.T) {
	if !isNoSuchUpload(&types.NoSuchUpload{}) {
		t.Error("typed NoSuchUpload not detected")
	}
	apiErr := &smithy.GenericAPIError{Code: "NoSuchUpload", Message: "gone"}
	if !isNoSuchUpload(apiErr) {
		t.Error("smithy NoSuchUpload code not detected")
	}
	if isNoSuchUpload(errors.New("other")) {
		t.Error("false positive")
	}
}

func sha256Sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// TestS3ClientChecksumDisabledEverywhere pins the P1 fix: the SDK's
// default-integrity per-part checksums must be off for BOTH a real-AWS target
// (no endpoint) and an S3-compatible endpoint. Left on for AWS, the SDK adds a
// CRC32 to each UploadPart and AWS rejects our manual CompleteMultipartUpload
// (which carries no per-part checksums). Regressing this silently breaks every
// backup to a customer's own AWS bucket while MinIO CI stays green.
func TestS3ClientChecksumDisabledEverywhere(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
	}{
		{"real AWS (no endpoint)", ""},
		{"S3-compatible endpoint", "http://minio.local:9000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := newS3Client(context.Background(), Config{
				S3Region: "us-east-1", S3Endpoint: tc.endpoint,
				S3Credentials: &S3Credentials{AccessKeyID: "k", SecretAccessKey: "s"},
			})
			if err != nil {
				t.Fatal(err)
			}
			o := client.Options()
			if o.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
				t.Errorf("RequestChecksumCalculation = %v, want WhenRequired (SDK default breaks AWS multipart Complete)", o.RequestChecksumCalculation)
			}
			if o.ResponseChecksumValidation != aws.ResponseChecksumValidationWhenRequired {
				t.Errorf("ResponseChecksumValidation = %v, want WhenRequired", o.ResponseChecksumValidation)
			}
		})
	}
}
