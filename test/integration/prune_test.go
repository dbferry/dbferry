//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/dbferry/dbferry/pipeline"
)

// TestPruneRetention exercises the retention pass against real MinIO: two real
// backups made by the pipeline, plus hand-crafted back-dated backups and an
// orphan under the same scope. keep_daily:1 must leave exactly the newest
// backup of each of the last N distinct days that have backups — and never
// touch the orphan.
func TestPruneRetention(t *testing.T) {
	suffix := uniqueSuffix()
	srcDB := "it_prune_" + suffix

	admin := openPG(t, pg17DSN)
	loadPGFixture(t, admin, pg17DSN, srcDB)
	t.Cleanup(func() {
		admin.Exec(`DROP DATABASE IF EXISTS "` + srcDB + `" WITH (FORCE)`)
	})

	prefix := "it-prune/" + suffix
	cfg := pipeline.Config{
		DSN:          dsnWithDB(t, pg17DSN, srcDB),
		Dest:         "s3://" + s3Bucket + "/" + prefix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   s3Endpoint,
		AppVersion:   "integration-test",
	}

	// Two real backups (same day — only the newer of the two must survive
	// keep_daily, and the newer one is by definition the newest overall).
	ctx := context.Background()
	res1, err := pipeline.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("backup 1: %v", err)
	}
	res2, err := pipeline.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("backup 2: %v", err)
	}

	// The scope the pipeline actually used, derived from a real object key.
	scope := res1.Key[:strings.LastIndex(res1.Key, "/")]
	scope = scope[:strings.LastIndex(scope, "/")] // strip YYYY/MM
	scope = scope[:strings.LastIndex(scope, "/")+1]

	client := s3Client(t)
	// Hand-crafted back-dated backups pinned to the 15th of the two previous
	// months — month boundaries can't shift under the test whatever today is.
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	prevMonth := putFakeBackup(t, client, scope, monthStart.AddDate(0, -1, 14).Add(12*time.Hour))
	prevPrevMonth := putFakeBackup(t, client, scope, monthStart.AddDate(0, -2, 14).Add(12*time.Hour))
	// An orphan: ciphertext without manifest.
	orphanKey := scope + now.Format("2006/01") + "/" + now.Format("20060102T150405Z") + "-ORPHAN00000000000000000000.dump.zst.age"
	putRaw(t, client, orphanKey, []byte("interrupted upload"))

	policy := pipeline.RetentionPolicy{KeepDaily: 1, KeepMonthly: 3}
	pres, err := pipeline.Prune(ctx, cfg, policy, pipeline.PruneOptions{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	// Expected survivors: res2 (newest of today = day 1, month 1) and the two
	// back-dated backups (months 2 and 3). Deleted: res1 (older backup of
	// today, saved by no window). The orphan is reported and untouched.
	wantKept := map[string]bool{res2.Key: true, prevMonth: true, prevPrevMonth: true}
	if len(pres.Kept) != len(wantKept) {
		t.Fatalf("kept %d backups, want %d: %+v", len(pres.Kept), len(wantKept), keysOf(pres.Kept))
	}
	for _, b := range pres.Kept {
		if !wantKept[b.Key] {
			t.Errorf("unexpectedly kept %s", b.Key)
		}
	}
	if len(pres.Deleted) != 1 || pres.Deleted[0].Key != res1.Key {
		t.Fatalf("deleted %v, want exactly %s", keysOf(pres.Deleted), res1.Key)
	}
	if len(pres.Orphans) != 1 || pres.Orphans[0].Key != orphanKey {
		t.Fatalf("orphans %v, want %s", keysOf(pres.Orphans), orphanKey)
	}

	// Verify against the real bucket, not just the result struct.
	for key, want := range map[string]bool{
		res2.Key:      true,
		prevMonth:     true,
		prevPrevMonth: true,
		orphanKey:     true,
		res1.Key:      false,
	} {
		if got := objectExists(t, client, key); got != want {
			t.Errorf("object %s exists=%v, want %v", key, got, want)
		}
	}

	// Idempotence: a second pass over the pruned bucket deletes nothing.
	pres2, err := pipeline.Prune(ctx, cfg, policy, pipeline.PruneOptions{})
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if len(pres2.Deleted) != 0 || len(pres2.DanglingDeleted) != 0 {
		t.Fatalf("second prune not a no-op: deleted=%v dangling=%v", keysOf(pres2.Deleted), pres2.DanglingDeleted)
	}
}

// putFakeBackup writes a hand-crafted, back-dated ciphertext+manifest pair
// that is indistinguishable from a real backup for listing purposes.
func putFakeBackup(t *testing.T, client *s3.Client, scope string, created time.Time) string {
	t.Helper()
	id := created.UTC().Format("20060102T150405Z") + "-01BACKDATED" + created.UTC().Format("150405") + "000000000"
	key := scope + created.UTC().Format("2006/01") + "/" + id + ".dump.zst.age"
	putRaw(t, client, key, []byte("back-dated fake ciphertext"))

	manifest := map[string]any{
		"key_schema":        1,
		"backup_id":         id,
		"created_at":        created.UTC().Format(time.RFC3339),
		"engine":            "postgres",
		"cluster":           "integration",
		"database":          "it",
		"object":            key,
		"format":            "pg_dump -Fc -Z0 | zstd | age",
		"dump_client":       "fake",
		"ciphertext_bytes":  int64(len("back-dated fake ciphertext")),
		"ciphertext_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
		"part_size":         1,
		"concurrency":       1,
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	putRaw(t, client, strings.TrimSuffix(key, ".dump.zst.age")+".manifest.json", b)
	return key
}

func putRaw(t *testing.T, client *s3.Client, key string, body []byte) {
	t.Helper()
	if _, err := client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(string(body)),
	}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func objectExists(t *testing.T, client *s3.Client, key string) bool {
	t.Helper()
	_, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(key),
	})
	return err == nil
}

func keysOf(bs []pipeline.BackupInfo) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Key
	}
	return out
}
