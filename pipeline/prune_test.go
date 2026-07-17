package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// prune runs the internal pass (list → select → delete → dangling cleanup)
// against the fake store, mirroring Prune without a real S3 client.
func prune(t *testing.T, f *fakeObjectStore, policy RetentionPolicy, dryRun bool) PruneResult {
	t.Helper()
	res, err := pruneWith(context.Background(), f, "bucket", testScope, policy, PruneOptions{DryRun: dryRun})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	return res
}

func TestPruneDeletesPolicyDropsAndDangling(t *testing.T) {
	f := newFakeObjectStore()
	newest := f.putBackup(testScope, mustTime(t, "2026-07-15T02:00:00Z"), "a")
	old1 := f.putBackup(testScope, mustTime(t, "2026-07-15T01:00:00Z"), "b") // same day, older
	old2 := f.putBackup(testScope, mustTime(t, "2026-07-01T02:00:00Z"), "c") // out of the 1-day window

	orphan := testScope + "2026/07/20260716T020000Z-ORPHAN" + ciphertextSuffix
	f.objects[orphan] = []byte("interrupted upload")
	dangling := testScope + "2026/07/20260710T020000Z-GONE" + manifestSuffix
	f.objects[dangling] = []byte(`{"key_schema":1}`)

	res := prune(t, f, RetentionPolicy{KeepDaily: 1}, false)

	if len(res.Kept) != 1 || res.Kept[0].Key != newest {
		t.Fatalf("kept %v, want only %s", keys(res.Kept), newest)
	}
	if len(res.Deleted) != 2 {
		t.Fatalf("deleted %v, want %s and %s", keys(res.Deleted), old1, old2)
	}
	if len(res.DanglingDeleted) != 1 || res.DanglingDeleted[0] != dangling {
		t.Fatalf("dangling deleted %v, want %s", res.DanglingDeleted, dangling)
	}
	if len(res.Orphans) != 1 || res.Orphans[0].Key != orphan {
		t.Fatalf("orphans %v, want %s", res.Orphans, orphan)
	}

	// Bucket end state: newest pair + orphan survive, nothing else.
	for _, gone := range []string{old1, manifestKey(old1), old2, manifestKey(old2), dangling} {
		if _, ok := f.objects[gone]; ok {
			t.Errorf("%s still in bucket", gone)
		}
	}
	for _, kept := range []string{newest, manifestKey(newest), orphan} {
		if _, ok := f.objects[kept]; !ok {
			t.Errorf("%s missing from bucket", kept)
		}
	}
}

func TestPruneDeletesCiphertextBeforeManifest(t *testing.T) {
	f := newFakeObjectStore()
	f.putBackup(testScope, mustTime(t, "2026-07-15T02:00:00Z"), "keepme")
	dropped := f.putBackup(testScope, mustTime(t, "2026-07-01T02:00:00Z"), "dropme")

	prune(t, f, RetentionPolicy{KeepDaily: 1}, false)

	var cipherAt, manifestAt = -1, -1
	for i, k := range f.deleted {
		switch k {
		case dropped:
			cipherAt = i
		case manifestKey(dropped):
			manifestAt = i
		}
	}
	if cipherAt == -1 || manifestAt == -1 || cipherAt > manifestAt {
		t.Fatalf("delete order wrong (ciphertext must go first): %v", f.deleted)
	}
}

func TestPruneDryRunDeletesNothing(t *testing.T) {
	f := newFakeObjectStore()
	f.putBackup(testScope, mustTime(t, "2026-07-15T02:00:00Z"), "a")
	f.putBackup(testScope, mustTime(t, "2026-07-01T02:00:00Z"), "b")
	f.objects[testScope+"2026/07/20260710T020000Z-GONE"+manifestSuffix] = []byte(`{}`)
	before := len(f.objects)

	res := prune(t, f, RetentionPolicy{KeepDaily: 1}, true)

	if len(res.Deleted) != 1 || len(res.DanglingDeleted) != 1 {
		t.Fatalf("dry-run selection wrong: deleted=%v dangling=%v", keys(res.Deleted), res.DanglingDeleted)
	}
	if len(f.objects) != before || len(f.deleted) != 0 {
		t.Fatalf("dry-run touched the bucket: %v", f.deleted)
	}
}

func TestDeleteBackupsScopeGuard(t *testing.T) {
	f := newFakeObjectStore()
	smuggled := []BackupInfo{{
		Key:         "other-tenant/postgres/host/db/2026/07/x" + ciphertextSuffix,
		ManifestKey: "other-tenant/postgres/host/db/2026/07/x" + manifestSuffix,
	}}
	err := deleteBackups(context.Background(), f, "bucket", testScope, smuggled)
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("scope guard did not refuse a foreign key: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("guard failed after deleting %v", f.deleted)
	}

	// A key inside the scope but with a foreign suffix is refused too.
	weird := []BackupInfo{{Key: testScope + "x" + ciphertextSuffix, ManifestKey: testScope + "x.txt"}}
	if err := deleteBackups(context.Background(), f, "bucket", testScope, weird); err == nil {
		t.Fatal("suffix guard did not refuse a non-artifact key")
	}
}

func TestPrunePartialDeleteErrorPropagates(t *testing.T) {
	f := newFakeObjectStore()
	f.putBackup(testScope, mustTime(t, "2026-07-15T02:00:00Z"), "a")
	f.putBackup(testScope, mustTime(t, "2026-07-01T02:00:00Z"), "b")
	f.deleteErr = []types.Error{{
		Key:     aws.String(testScope + "whatever"),
		Code:    aws.String("AccessDenied"),
		Message: aws.String("no delete for you"),
	}}

	_, err := pruneWith(context.Background(), f, "bucket", testScope, RetentionPolicy{KeepDaily: 1}, PruneOptions{})
	if err == nil || !strings.Contains(err.Error(), "s3:DeleteObject") {
		t.Fatalf("partial delete failure must surface the missing permission, got: %v", err)
	}
	if KindOf(err) != KindUpload {
		t.Fatalf("delete failure kind = %v, want KindUpload", KindOf(err))
	}
}

func TestBackupScope(t *testing.T) {
	key := "pfx/postgres/host_5433/db/2026/07/20260717T120000Z-X" + ciphertextSuffix
	if got := BackupScope(key); got != "pfx/postgres/host_5433/db/" {
		t.Fatalf("BackupScope(%q) = %q", key, got)
	}
}

func TestPruneRequireScopeMismatch(t *testing.T) {
	// Prune with a RequireScope that cannot match any derived scope must
	// refuse before touching the bucket.
	cfg := Config{
		DSN:  "postgres://u@host:5432/db",
		Dest: "s3://bucket/pfx",
	}
	_, err := Prune(context.Background(), cfg, RetentionPolicy{KeepDaily: 1},
		PruneOptions{RequireScope: "elsewhere/postgres/other/db/"})
	if err == nil || !strings.Contains(err.Error(), "scope mismatch") {
		t.Fatalf("Prune with foreign RequireScope = %v, want ErrScopeMismatch", err)
	}
}

func TestPruneManyDropsBatchesDeletes(t *testing.T) {
	f := newFakeObjectStore()
	base := mustTime(t, "2026-01-01T00:00:00Z")
	for i := 0; i < deleteBatchSize+3; i++ {
		f.putBackup(testScope, base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("c%d", i))
	}
	res := prune(t, f, RetentionPolicy{KeepDaily: 1}, false)
	if len(res.Deleted) != deleteBatchSize+2 {
		t.Fatalf("deleted %d, want %d", len(res.Deleted), deleteBatchSize+2)
	}
	if got := len(f.deleted); got != 2*(deleteBatchSize+2) {
		t.Fatalf("bucket deletions %d, want %d (ciphertexts+manifests)", got, 2*(deleteBatchSize+2))
	}
}
