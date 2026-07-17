package pipeline

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeObjectStore is an in-memory s3ObjectAPI for listing/retention tests.
type fakeObjectStore struct {
	objects map[string][]byte // key → body

	pageSize  int32 // ListObjectsV2 page size override (default 1000)
	onGet     error
	onDelete  error
	deleteErr []types.Error // per-key errors returned in the DeleteObjects output
	deleted   []string      // keys deleted, in call order
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string][]byte{}}
}

// putBackup stores a ciphertext+manifest pair whose manifest is consistent.
func (f *fakeObjectStore) putBackup(scope string, created time.Time, body string) string {
	id := created.UTC().Format("20060102T150405Z") + "-01FAKEULID" + fmt.Sprintf("%06d", len(f.objects))
	key := scope + "2026/07/" + id + ciphertextSuffix
	f.objects[key] = []byte(body)
	m := Manifest{
		KeySchema: keySchemaVersion,
		BackupID:  id,
		CreatedAt: created.UTC().Format(time.RFC3339),
		Object:    key,
	}
	b, _ := m.marshal()
	f.objects[manifestKey(key)] = b
	return key
}

func (f *fakeObjectStore) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) && k > aws.ToString(in.ContinuationToken) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	pageSize := f.pageSize
	if pageSize == 0 {
		pageSize = 1000
	}
	truncated := false
	if int32(len(keys)) > pageSize {
		keys = keys[:pageSize]
		truncated = true
	}
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(truncated)}
	if truncated {
		out.NextContinuationToken = aws.String(keys[len(keys)-1])
	}
	for _, k := range keys {
		out.Contents = append(out.Contents, types.Object{Key: aws.String(k), Size: aws.Int64(int64(len(f.objects[k])))})
	}
	return out, nil
}

func (f *fakeObjectStore) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.onGet != nil {
		return nil, f.onGet
	}
	body, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s", aws.ToString(in.Key))
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func (f *fakeObjectStore) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	if f.onDelete != nil {
		return nil, f.onDelete
	}
	if len(f.deleteErr) > 0 {
		return &s3.DeleteObjectsOutput{Errors: f.deleteErr}, nil
	}
	for _, o := range in.Delete.Objects {
		k := aws.ToString(o.Key)
		delete(f.objects, k)
		f.deleted = append(f.deleted, k)
	}
	return &s3.DeleteObjectsOutput{}, nil
}

const testScope = "pfx/postgres/host/db/"

func mustList(t *testing.T, f *fakeObjectStore) Listing {
	t.Helper()
	l, err := listBackups(context.Background(), f, "bucket", testScope)
	if err != nil {
		t.Fatalf("listBackups: %v", err)
	}
	return l
}

func TestListBackupsPairsAndStates(t *testing.T) {
	f := newFakeObjectStore()
	validKey := f.putBackup(testScope, mustTime(t, "2026-07-15T02:00:00Z"), "cipher")

	orphanKey := testScope + "2026/07/20260716T020000Z-ORPHAN" + ciphertextSuffix
	f.objects[orphanKey] = []byte("cipher-no-manifest")

	danglingKey := testScope + "2026/07/20260710T020000Z-GONE" + manifestSuffix
	f.objects[danglingKey] = []byte(`{"key_schema":1}`)

	corruptKey := f.putBackup(testScope, mustTime(t, "2026-07-13T02:00:00Z"), "c2")
	f.objects[manifestKey(corruptKey)] = []byte("{not json")

	futureKey := f.putBackup(testScope, mustTime(t, "2026-07-12T02:00:00Z"), "c3")
	f.objects[manifestKey(futureKey)] = []byte(`{"key_schema":99,"object":"` + futureKey + `"}`)

	mismatchKey := f.putBackup(testScope, mustTime(t, "2026-07-11T02:00:00Z"), "c4")
	f.objects[manifestKey(mismatchKey)] = []byte(`{"key_schema":1,"object":"somebody/else` + ciphertextSuffix + `"}`)

	// JSON zero values must not pass as valid: a manifest with only the
	// object reference (key_schema 0, no backup_id, no created_at) is corrupt.
	skeletonKey := f.putBackup(testScope, mustTime(t, "2026-07-10T02:00:00Z"), "c5")
	f.objects[manifestKey(skeletonKey)] = []byte(`{"object":"` + skeletonKey + `"}`)

	badTimeKey := f.putBackup(testScope, mustTime(t, "2026-07-09T02:00:00Z"), "c6")
	f.objects[manifestKey(badTimeKey)] = []byte(`{"key_schema":1,"backup_id":"x","created_at":"yesterday","object":"` + badTimeKey + `"}`)

	f.objects[testScope+"README.txt"] = []byte("not a backup artifact")
	f.objects["outside/scope/20260715T020000Z-X"+ciphertextSuffix] = []byte("different database")

	l := mustList(t, f)
	states := map[string]BackupState{}
	for _, b := range l.Backups {
		id := b.Key
		if id == "" {
			id = b.ManifestKey
		}
		states[id] = b.State
	}
	want := map[string]BackupState{
		validKey:    BackupValid,
		orphanKey:   BackupOrphan,
		danglingKey: BackupDanglingManifest,
		corruptKey:  BackupCorruptManifest,
		futureKey:   BackupUnsupportedSchema,
		mismatchKey: BackupCorruptManifest,
		skeletonKey: BackupCorruptManifest,
		badTimeKey:  BackupCorruptManifest,
	}
	if len(states) != len(want) {
		t.Fatalf("listed %d artifacts %v, want %d", len(states), states, len(want))
	}
	for k, s := range want {
		if states[k] != s {
			t.Errorf("state of %s = %v, want %v", k, states[k], s)
		}
	}

	for _, b := range l.Backups {
		if b.Key == validKey {
			if b.Manifest == nil || b.Bytes != int64(len("cipher")) ||
				!b.CreatedAt.Equal(mustTime(t, "2026-07-15T02:00:00Z")) {
				t.Errorf("valid backup metadata wrong: %+v", b)
			}
		}
		if b.Key == orphanKey && !b.CreatedAt.Equal(mustTime(t, "2026-07-16T02:00:00Z")) {
			t.Errorf("orphan created-at not recovered from key: %v", b.CreatedAt)
		}
	}
}

func TestListBackupsNewestFirst(t *testing.T) {
	f := newFakeObjectStore()
	f.putBackup(testScope, mustTime(t, "2026-07-13T02:00:00Z"), "a")
	f.putBackup(testScope, mustTime(t, "2026-07-15T02:00:00Z"), "b")
	f.putBackup(testScope, mustTime(t, "2026-07-14T02:00:00Z"), "c")

	l := mustList(t, f)
	for i := 1; i < len(l.Backups); i++ {
		if l.Backups[i].CreatedAt.After(l.Backups[i-1].CreatedAt) {
			t.Fatalf("listing not newest-first: %v", l.Backups)
		}
	}
}

func TestListBackupsPagination(t *testing.T) {
	f := newFakeObjectStore()
	f.pageSize = 7 // force many pages (each backup is 2 objects)
	base := mustTime(t, "2026-01-01T00:00:00Z")
	for i := 0; i < 40; i++ {
		f.putBackup(testScope, base.Add(time.Duration(i)*time.Hour), "x")
	}
	l := mustList(t, f)
	if got := len(l.Valid()); got != 40 {
		t.Fatalf("pagination lost backups: got %d valid, want 40", got)
	}
}

func TestListBackupsOversizedManifestIsCorrupt(t *testing.T) {
	f := newFakeObjectStore()
	key := f.putBackup(testScope, mustTime(t, "2026-07-15T02:00:00Z"), "cipher")
	f.objects[manifestKey(key)] = make([]byte, maxManifestBytes+1)

	l := mustList(t, f)
	if len(l.Backups) != 1 || l.Backups[0].State != BackupCorruptManifest {
		t.Fatalf("oversized manifest not flagged corrupt: %+v", l.Backups)
	}
}

func TestListBackupsGetErrorFailsListing(t *testing.T) {
	f := newFakeObjectStore()
	f.putBackup(testScope, mustTime(t, "2026-07-15T02:00:00Z"), "cipher")
	f.onGet = fmt.Errorf("connection reset")

	if _, err := listBackups(context.Background(), f, "bucket", testScope); err == nil {
		t.Fatal("transport error during manifest read must fail the listing (retryable), got nil")
	}
}
