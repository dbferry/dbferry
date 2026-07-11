//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/dbferry/dbferry/pipeline"
)

// TestPostgresBackupRestore is the full happy path against real PostgreSQL: load
// the fixture, back it up through the pipeline to MinIO, restore into a fresh
// database, and assert the restored schema/data digest matches the source
// (poc-plan 0.3/0.4 acceptance: fixture verified automatically after restore).
func TestPostgresBackupRestore(t *testing.T) {
	cases := []struct{ name, dsn string }{
		{"pg14", pg14DSN},
		{"pg17", pg17DSN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			suffix := uniqueSuffix()
			srcDB := "it_src_" + suffix
			dstDB := "it_dst_" + suffix

			admin := openPG(t, tc.dsn)
			loadPGFixture(t, admin, tc.dsn, srcDB)
			t.Cleanup(func() {
				admin.Exec(`DROP DATABASE IF EXISTS "` + srcDB + `" WITH (FORCE)`)
				admin.Exec(`DROP DATABASE IF EXISTS "` + dstDB + `" WITH (FORCE)`)
			})

			prefix := "it/" + tc.name + "/" + suffix
			client := s3Client(t)
			if n := countObjects(t, client, prefix); n != 0 {
				t.Fatalf("prefix not empty before backup: %d", n)
			}

			res, err := pipeline.Run(context.Background(), pipeline.Config{
				DSN:          dsnWithDB(t, tc.dsn, srcDB),
				Dest:         "s3://" + s3Bucket + "/" + prefix,
				AgeRecipient: ageRecipient(t),
				S3Endpoint:   s3Endpoint,
				AppVersion:   "integration-test",
			})
			if err != nil {
				t.Fatalf("backup: %v", err)
			}

			// Both the ciphertext object and its manifest must be present.
			if n := countObjects(t, client, prefix); n != 2 {
				t.Fatalf("want object+manifest (2) under %s, got %d", prefix, n)
			}

			// Restore into a fresh database. Correctness is asserted by the
			// digest below, not pg_restore's exit code (see pgRestore).
			pgExec(t, admin, `CREATE DATABASE "`+dstDB+`"`)
			ct := getObject(t, client, res.Key)
			archive := decryptDecompress(t, ct, ageIdentityPath(t))
			if out, rerr := pgRestore(t, archive, dsnWithDB(t, tc.dsn, dstDB)); rerr != nil {
				t.Logf("pg_restore reported non-fatal issues (see poc-plan 5.3 client matching):\n%s", out)
			}

			src := openPG(t, dsnWithDB(t, tc.dsn, srcDB))
			dst := openPG(t, dsnWithDB(t, tc.dsn, dstDB))
			diffDigests(t, digestPostgres(t, src), digestPostgres(t, dst))
		})
	}
}
