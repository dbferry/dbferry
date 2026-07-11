//go:build integration || faultinjection

// Package integration holds dbferry's integration and fault-injection suites
// (poc-plan 0.4). They run against the local stand (`make stand-up`): real
// PostgreSQL 14/17, MySQL 8, and MinIO. They are build-tagged so the default
// `go test ./...` (the unit suite) never needs the stand.
package integration

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Stand configuration, overridable by env so the same suite runs in CI.
var (
	pg14DSN    = env("DBFERRY_TEST_PG14_DSN", "postgres://dbferry:dbferry@localhost:5414/postgres")
	pg17DSN    = env("DBFERRY_TEST_PG17_DSN", "postgres://dbferry:dbferry@localhost:5417/postgres")
	mysqlDSN   = env("DBFERRY_TEST_MYSQL_DSN", "root:dbferry@tcp(127.0.0.1:3308)/")
	s3Endpoint = env("DBFERRY_TEST_S3_ENDPOINT", "http://localhost:9000")
	s3Bucket   = env("DBFERRY_TEST_BUCKET", "dbferry-backups")
	standDir   = "."
)

// TestMain ensures pipeline.Run (which uses the standard AWS chain) can reach
// MinIO by defaulting the AWS env to the stand credentials when unset.
func TestMain(m *testing.M) {
	setDefaultEnv("AWS_ACCESS_KEY_ID", "minioadmin")
	setDefaultEnv("AWS_SECRET_ACCESS_KEY", "minioadmin")
	setDefaultEnv("AWS_REGION", "us-east-1")
	os.Exit(m.Run())
}

func setDefaultEnv(k, v string) {
	if os.Getenv(k) == "" {
		os.Setenv(k, v)
	}
}

func ageRecipient(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(standDir, ".stand", "age-recipient.txt"))
	if err != nil {
		t.Fatalf("read age recipient (run `make stand-up`?): %v", err)
	}
	return strings.TrimSpace(string(b))
}

func ageIdentityPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(standDir, ".stand", "age-identity.txt")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("age identity missing (run `make stand-up`?): %v", err)
	}
	return p
}

// uniqueSuffix yields a per-run token so parallel/repeat runs don't collide.
func uniqueSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// --- S3 helpers -----------------------------------------------------------

func s3Client(t *testing.T) *s3.Client {
	t.Helper()
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", "")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3Endpoint)
		o.UsePathStyle = true
	})
}

func countObjects(t *testing.T, client *s3.Client, prefix string) int {
	t.Helper()
	out, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String(s3Bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	return int(aws.ToInt32(out.KeyCount))
}

func getObject(t *testing.T, client *s3.Client, key string) []byte {
	t.Helper()
	out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("get object %s: %v", key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read object %s: %v", key, err)
	}
	return b
}

// --- restore (inverse of the pipeline) ------------------------------------

// decryptDecompress runs the documented inverse chain (age -d | zstd -d) in
// process and returns the pg_dump custom archive bytes.
func decryptDecompress(t *testing.T, ciphertext []byte, identityPath string) []byte {
	t.Helper()
	idBytes, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	ids, err := age.ParseIdentities(bytes.NewReader(idBytes))
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	dec, err := age.Decrypt(bytes.NewReader(ciphertext), ids...)
	if err != nil {
		t.Fatalf("age decrypt: %v", err)
	}
	zr, err := zstd.NewReader(dec)
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("zstd decompress: %v", err)
	}
	return plain
}

// pgRestore restores a custom-format archive into targetDSN via pg_restore and
// returns its combined output and exit error. It does NOT fail the test on a
// non-zero exit: restoring a dump made by a newer pg_dump into an older server
// can emit benign "unrecognized configuration parameter" notices (e.g.
// transaction_timeout, added in PG17) that pg_restore ignores while still
// loading all data — matching-client selection is poc-plan 5.3. Correctness is
// asserted by the digest comparison, not by pg_restore's exit code.
func pgRestore(t *testing.T, archive []byte, targetDSN string) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "restore-*.dump")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(archive); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cmd := exec.Command("pg_restore", "-d", targetDSN, "--no-owner", f.Name())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- postgres helpers -----------------------------------------------------

func openPG(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping pg (run `make stand-up`?): %v", err)
	}
	return db
}

// dsnWithDB returns adminDSN pointed at a different database.
func dsnWithDB(t *testing.T, adminDSN, db string) string {
	t.Helper()
	i := strings.LastIndex(adminDSN, "/")
	if i < 0 {
		t.Fatalf("malformed DSN: %s", adminDSN)
	}
	// Preserve any query string on the admin DSN.
	rest := adminDSN[i+1:]
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		return adminDSN[:i+1] + db + rest[q:]
	}
	return adminDSN[:i+1] + db
}

func pgExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// loadPGFixture creates a fresh database and loads fixture.sql into it via psql
// (which handles the dollar-quoted function bodies).
func loadPGFixture(t *testing.T, adminDB *sql.DB, adminDSN, dbName string) {
	t.Helper()
	pgExec(t, adminDB, `DROP DATABASE IF EXISTS "`+dbName+`"`, `CREATE DATABASE "`+dbName+`"`)
	cmd := exec.Command("psql", "-v", "ON_ERROR_STOP=1", "-q",
		"-d", dsnWithDB(t, adminDSN, dbName),
		"-f", filepath.Join("fixtures", "postgres", "fixture.sql"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("load pg fixture: %v\n%s", err, out)
	}
}

// digestPostgres builds a canonical fingerprint of the app schema: per-table
// row count and content hash, plus index and trigger definitions. Comparing the
// source and restored fingerprints proves the restore is faithful.
func digestPostgres(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	if _, err := db.Exec("SET TIME ZONE 'UTC'"); err != nil {
		t.Fatalf("set tz: %v", err)
	}
	d := map[string]string{}

	tables := queryStrings(t, db, `SELECT tablename FROM pg_tables WHERE schemaname='app' ORDER BY 1`)
	for _, tbl := range tables {
		var count int
		var hash sql.NullString
		q := fmt.Sprintf(`SELECT count(*), md5(coalesce(string_agg(md5(x::text), '' ORDER BY md5(x::text)), '')) FROM app.%q x`, tbl)
		if err := db.QueryRow(q).Scan(&count, &hash); err != nil {
			t.Fatalf("digest table %s: %v", tbl, err)
		}
		d["table:"+tbl+":count"] = strconv.Itoa(count)
		d["table:"+tbl+":hash"] = hash.String
	}
	for _, ix := range queryStrings(t, db,
		`SELECT indexname||' = '||indexdef FROM pg_indexes WHERE schemaname='app' ORDER BY 1`) {
		d["index:"+ix] = "1"
	}
	for _, tr := range queryStrings(t, db,
		`SELECT event_object_table||':'||trigger_name||':'||action_timing||':'||event_manipulation
		 FROM information_schema.triggers WHERE trigger_schema='app' ORDER BY 1`) {
		d["trigger:"+tr] = "1"
	}
	return d
}

func queryStrings(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func diffDigests(t *testing.T, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("mismatch %q: source=%q restored=%q", k, v, got[k])
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("restored has extra key %q=%q", k, got[k])
		}
	}
}
