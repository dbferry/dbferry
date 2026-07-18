package onboard

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPostgresGrants(t *testing.T) {
	sql := PostgresGrants("dbferry_backup", "shop")
	for _, want := range []string{
		`CREATE ROLE "dbferry_backup" LOGIN PASSWORD`,
		`GRANT CONNECT ON DATABASE "shop" TO "dbferry_backup"`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA public TO "dbferry_backup"`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES`,
		`pg_read_all_data`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("postgres grants miss %q", want)
		}
	}
	// No write privilege sneaks in.
	for _, banned := range []string{"INSERT", "UPDATE ", "DELETE", "CREATE TABLE", "SUPERUSER"} {
		if strings.Contains(sql, banned) {
			t.Errorf("postgres grants contain %q — more than read-only", banned)
		}
	}
	// Identifier quoting survives hostile names.
	if q := PostgresGrants(`we"ird`, `d"b`); !strings.Contains(q, `"we""ird"`) || !strings.Contains(q, `"d""b"`) {
		t.Errorf("identifier quoting broken: %s", q)
	}
}

func TestMySQLGrants(t *testing.T) {
	sql := MySQLGrants("dbferry_backup", "shop")
	for _, want := range []string{
		"CREATE USER 'dbferry_backup'@'%'",
		"GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON `shop`.*",
		"GRANT SHOW_ROUTINE ON *.*",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("mysql grants miss %q", want)
		}
	}
	// LOCK TABLES is deliberately absent: --single-transaction needs none.
	if strings.Contains(sql, "LOCK TABLES") {
		t.Error("mysql grants contain LOCK TABLES — not needed with --single-transaction")
	}
	for _, banned := range []string{"INSERT", "UPDATE", "DELETE", "ALL PRIVILEGES"} {
		if strings.Contains(sql, banned) {
			t.Errorf("mysql grants contain %q — more than read-only", banned)
		}
	}
}

func TestS3Policy(t *testing.T) {
	pol := S3Policy("my-bucket", "backups", true)
	var parsed struct {
		Statement []struct {
			Action   []string `json:"Action"`
			Resource any      `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(pol), &parsed); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, pol)
	}
	if len(parsed.Statement) != 2 {
		t.Fatalf("want 2 statements, got %d", len(parsed.Statement))
	}
	// s3:prefix conditions are only valid on ListBucket — anything else in
	// the bucket statement breaks real policy engines (MinIO rejects it).
	if joined := strings.Join(parsed.Statement[1].Action, " "); joined != "s3:ListBucket" {
		t.Errorf("bucket statement must be exactly ListBucket, got %q", joined)
	}
	obj := parsed.Statement[0]
	if obj.Resource != "arn:aws:s3:::my-bucket/backups/*" {
		t.Errorf("object resource not prefix-scoped: %v", obj.Resource)
	}
	joined := strings.Join(obj.Action, " ")
	for _, want := range []string{"s3:PutObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts", "s3:GetObject", "s3:DeleteObject"} {
		if !strings.Contains(joined, want) {
			t.Errorf("policy misses %s", want)
		}
	}

	// Without delete: append-only, no DeleteObject anywhere.
	appendOnly := S3Policy("my-bucket", "backups", false)
	if strings.Contains(appendOnly, "DeleteObject") {
		t.Error("append-only policy still grants DeleteObject")
	}

	// Empty prefix scopes objects to the whole bucket.
	whole := S3Policy("b", "", true)
	if !strings.Contains(whole, `"arn:aws:s3:::b/*"`) {
		t.Errorf("empty prefix policy: %s", whole)
	}
}
