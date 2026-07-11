//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbferry/dbferry/pipeline"
)

func mysqlURL(db string) string {
	return fmt.Sprintf("mysql://%s:%s@%s:%s/%s", myUser, myPass, myHost, myPort, db)
}

// TestMySQLBackupRestore is the full happy path for MySQL through the pipeline:
// load the fixture, back it up to MinIO with mysqldump, restore into a fresh
// database, and assert tables, routines, events and triggers all match
// (poc-plan 4.2/4.3 acceptance).
func TestMySQLBackupRestore(t *testing.T) {
	const src, dst = "app", "app_restored"
	runMySQLStdin(t, "load fixture", "", filepath.Join("fixtures", "mysql", "fixture.sql"))
	t.Cleanup(func() {
		runMySQL(t, "-e", "DROP DATABASE IF EXISTS "+src+"; DROP DATABASE IF EXISTS "+dst+";")
	})

	suffix := uniqueSuffix()
	prefix := "it/mysql/" + suffix
	client := s3Client(t)

	res, err := pipeline.Run(context.Background(), pipeline.Config{
		DSN:          mysqlURL(src),
		Dest:         "s3://" + s3Bucket + "/" + prefix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   s3Endpoint,
		AppVersion:   "integration-test",
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if n := countObjects(t, client, prefix); n != 2 {
		t.Fatalf("want object+manifest (2), got %d", n)
	}

	// Manifest must record the mysql engine.
	var m struct {
		Engine string `json:"engine"`
	}
	if err := json.Unmarshal(getObject(t, client, res.ManifestKey), &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if m.Engine != "mysql" {
		t.Errorf("manifest engine = %q, want mysql", m.Engine)
	}

	// Restore: download → decrypt → decompress → mysql, then compare digests.
	dumpSQL := decryptDecompress(t, getObject(t, client, res.Key), ageIdentityPath(t))
	dumpPath := filepath.Join(t.TempDir(), "app.sql")
	if err := os.WriteFile(dumpPath, dumpSQL, 0o600); err != nil {
		t.Fatal(err)
	}
	runMySQL(t, "-e", "DROP DATABASE IF EXISTS "+dst+"; CREATE DATABASE "+dst)
	runMySQLStdin(t, "restore", dst, dumpPath)

	diffDigests(t, digestMySQL(t, openMySQL(t, src), src), digestMySQL(t, openMySQL(t, dst), dst))
}

// TestMySQLNonInnoDBGate: a non-InnoDB (MyISAM) table is refused without
// --allow-nontransactional (KindDump, no object), and backs up with a warning
// when the flag is set (poc-plan 4.2).
func TestMySQLNonInnoDBGate(t *testing.T) {
	suffix := uniqueSuffix()
	dbName := "it_myisam_" + suffix
	prefix := "it/myisam/" + suffix
	client := s3Client(t)

	runMySQL(t, "-e", "CREATE DATABASE "+dbName)
	t.Cleanup(func() { runMySQL(t, "-e", "DROP DATABASE IF EXISTS "+dbName) })
	runMySQL(t, "-e", fmt.Sprintf(
		"CREATE TABLE %s.m (id int) ENGINE=MyISAM; INSERT INTO %s.m VALUES (1);", dbName, dbName))

	base := pipeline.Config{
		DSN:          mysqlURL(dbName),
		Dest:         "s3://" + s3Bucket + "/" + prefix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   s3Endpoint,
	}

	// Without the flag: refused, no object.
	_, err := pipeline.Run(context.Background(), base)
	if err == nil {
		t.Fatal("expected refusal for a non-InnoDB table without --allow-nontransactional")
	}
	if !strings.Contains(err.Error(), "non-InnoDB") || pipeline.KindOf(err) != pipeline.KindDump {
		t.Errorf("unexpected error (want KindDump non-InnoDB): kind=%v err=%v", pipeline.KindOf(err), err)
	}
	if n := countObjects(t, client, prefix); n != 0 {
		t.Fatalf("refused run must leave no object, found %d", n)
	}

	// With the flag: succeeds and warns.
	var warns []string
	allow := base
	allow.AllowNonTransactional = true
	allow.Warn = func(s string) { warns = append(warns, s) }
	if _, err := pipeline.Run(context.Background(), allow); err != nil {
		t.Fatalf("with --allow-nontransactional the backup should succeed: %v", err)
	}
	if len(warns) == 0 {
		t.Error("expected a warning about non-InnoDB tables")
	}
	if n := countObjects(t, client, prefix); n != 2 {
		t.Fatalf("want object+manifest (2), got %d", n)
	}
}
