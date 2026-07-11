//go:build faultinjection

package integration

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/dbferry/dbferry/pipeline"
)

// TestConnectErrorClassified: an unreachable/nonexistent database fails the
// preflight and is classified KindConnect, with no object written.
func TestConnectErrorClassified(t *testing.T) {
	suffix := uniqueSuffix()
	prefix := "fault/connect/" + suffix
	client := s3Client(t)

	_, err := pipeline.Run(context.Background(), pipeline.Config{
		DSN:          dsnWithDB(t, pg17DSN, "definitely_absent_"+suffix),
		Dest:         "s3://" + s3Bucket + "/" + prefix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   s3Endpoint,
	})
	if err == nil {
		t.Fatal("expected error for a missing database")
	}
	if k := pipeline.KindOf(err); k != pipeline.KindConnect {
		t.Errorf("kind = %v, want connect (%v)", k, err)
	}
	if n := countObjects(t, client, prefix); n != 0 {
		t.Fatalf("connect failure must leave no object, found %d", n)
	}
}

// TestDumpErrorClassified: a role that can connect (preflight passes) but lacks
// access to a table makes pg_dump fail — classified KindDump, no object.
func TestDumpErrorClassified(t *testing.T) {
	suffix := uniqueSuffix()
	dbName := "it_dumpfail_" + suffix
	role := "limited_" + suffix
	prefix := "fault/dump/" + suffix
	client := s3Client(t)

	admin := openPG(t, pg17DSN)
	pgExec(t, admin, `CREATE DATABASE "`+dbName+`"`)
	t.Cleanup(func() {
		admin.Exec(`DROP DATABASE IF EXISTS "` + dbName + `" WITH (FORCE)`)
		admin.Exec(`DROP ROLE IF EXISTS "` + role + `"`)
	})

	dbAdmin := openPG(t, dsnWithDB(t, pg17DSN, dbName))
	pgExec(t, dbAdmin,
		`CREATE TABLE secret (x int)`,
		`INSERT INTO secret VALUES (1)`,
		`CREATE ROLE "`+role+`" LOGIN PASSWORD 'p'`,
		`REVOKE ALL ON secret FROM PUBLIC`,
	)

	limitedDSN := "postgres://" + role + ":p@localhost:5417/" + dbName
	_, err := pipeline.Run(context.Background(), pipeline.Config{
		DSN:          limitedDSN,
		Dest:         "s3://" + s3Bucket + "/" + prefix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   s3Endpoint,
	})
	if err == nil {
		t.Fatal("expected pg_dump permission error")
	}
	if k := pipeline.KindOf(err); k != pipeline.KindDump {
		t.Errorf("kind = %v, want dump (%v)", k, err)
	}
	if n := countObjects(t, client, prefix); n != 0 {
		t.Fatalf("dump failure must leave no object, found %d", n)
	}
}

// TestUploadErrorClassified: a reachable database but an unusable S3 endpoint
// fails the upload — classified KindUpload, no object at the real bucket.
func TestUploadErrorClassified(t *testing.T) {
	suffix := uniqueSuffix()
	prefix := "fault/upload/" + suffix
	client := s3Client(t)

	_, err := pipeline.Run(context.Background(), pipeline.Config{
		DSN:          pg17DSN,
		Dest:         "s3://" + s3Bucket + "/" + prefix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   "http://127.0.0.1:1", // nothing listens here
		MaxRetries:   1,
	})
	if err == nil {
		t.Fatal("expected upload error")
	}
	if k := pipeline.KindOf(err); k != pipeline.KindUpload {
		t.Errorf("kind = %v, want upload (%v)", k, err)
	}
	if n := countObjects(t, client, prefix); n != 0 {
		t.Fatalf("upload failure must leave no object, found %d", n)
	}
}

// TestCancelDuringDumpKillsChild cancels mid-dump and proves pg_dump is killed:
// its server backend disappears. Without the kill, pg_dump would block forever
// writing to a stdout pipe nobody drains, so the backend would never go away
// (poc-plan 2.1). Also asserts KindCanceled and no object.
func TestCancelDuringDumpKillsChild(t *testing.T) {
	suffix := uniqueSuffix()
	dbName := "it_cancel_" + suffix
	prefix := "fault/midcancel/" + suffix
	client := s3Client(t)

	admin := openPG(t, pg17DSN)
	pgExec(t, admin, `CREATE DATABASE "`+dbName+`"`)
	t.Cleanup(func() { admin.Exec(`DROP DATABASE IF EXISTS "` + dbName + `" WITH (FORCE)`) })

	// ~120 MB of incompressible data so the dump runs for well over a second,
	// leaving a wide window to cancel mid-dump.
	big := openPG(t, dsnWithDB(t, pg17DSN, dbName))
	pgExec(t, big,
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		`CREATE TABLE blob (id int primary key, payload bytea)`,
		`INSERT INTO blob SELECT g, gen_random_bytes(1024) FROM generate_series(1, 100000) g`,
	)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := pipeline.Run(ctx, pipeline.Config{
			DSN:          dsnWithDB(t, pg17DSN, dbName),
			Dest:         "s3://" + s3Bucket + "/" + prefix,
			AgeRecipient: ageRecipient(t),
			S3Endpoint:   s3Endpoint,
		})
		errc <- err
	}()

	time.Sleep(250 * time.Millisecond) // let the dump get underway
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
		if k := pipeline.KindOf(err); k != pipeline.KindCanceled {
			t.Errorf("kind = %v, want canceled (%v)", k, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// The pg_dump backend must terminate; poll until it is gone.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if activePgDumpBackends(t, admin, dbName) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pg_dump backend still active after cancel — child was not killed")
		}
		time.Sleep(200 * time.Millisecond)
	}

	if n := countObjects(t, client, prefix); n != 0 {
		t.Fatalf("cancelled backup must leave no object, found %d", n)
	}
}

func activePgDumpBackends(t *testing.T, db *sql.DB, dbName string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND application_name='pg_dump'`,
		dbName).Scan(&n); err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	return n
}

// TestContextCancelClassified: a cancelled context aborts before any object is
// completed and is classified KindCanceled.
func TestContextCancelClassified(t *testing.T) {
	suffix := uniqueSuffix()
	prefix := "fault/cancel/" + suffix
	client := s3Client(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the run starts

	_, err := pipeline.Run(ctx, pipeline.Config{
		DSN:          pg17DSN,
		Dest:         "s3://" + s3Bucket + "/" + prefix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   s3Endpoint,
	})
	if err == nil {
		t.Fatal("expected error for a cancelled context")
	}
	if k := pipeline.KindOf(err); k != pipeline.KindCanceled {
		t.Errorf("kind = %v, want canceled (%v)", k, err)
	}
	if n := countObjects(t, client, prefix); n != 0 {
		t.Fatalf("cancelled backup must leave no object, found %d", n)
	}
}
