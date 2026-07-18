package onboard

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPostgresGrants(t *testing.T) {
	sql := PostgresGrants("dbferry_backup", "shop")
	for _, want := range []string{
		`CREATE ROLE "dbferry_backup" LOGIN;`,
		`\password "dbferry_backup"`, // interactive — never a literal in SQL
		`GRANT pg_read_all_data TO "dbferry_backup"`,
		`GRANT CONNECT ON DATABASE "shop" TO "dbferry_backup"`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("postgres grants miss %q", want)
		}
	}
	// The password never appears as a SQL literal (quoting/injection-proof).
	if strings.Contains(sql, "PASSWORD '") {
		t.Error("postgres grants embed a password literal")
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
		"CREATE USER 'dbferry_backup'@'%' IDENTIFIED BY RANDOM PASSWORD",
		"GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON `shop`.*",
		"GRANT SHOW_ROUTINE ON *.*",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("mysql grants miss %q", want)
		}
	}
	// Neither LOCK TABLES (--single-transaction) nor PROCESS
	// (--no-tablespaces) may appear, and no password literal exists.
	for _, banned := range []string{"LOCK TABLES", "PROCESS", "IDENTIFIED BY '", "INSERT", "UPDATE", "DELETE", "ALL PRIVILEGES"} {
		if strings.Contains(sql, banned) {
			t.Errorf("mysql grants contain %q", banned)
		}
	}
	// Hostile usernames: quote AND backslash escaping.
	q := MySQLGrants(`o'malley\`, "d`b")
	if !strings.Contains(q, `'o''malley\\'`) {
		t.Errorf("mysql username escaping broken: %s", q)
	}
	if !strings.Contains(q, "`d``b`") {
		t.Errorf("mysql database quoting broken: %s", q)
	}
}

func TestValidatePrefix(t *testing.T) {
	for _, bad := range []string{"a*", "a?b", "a//b", "a/../b", "..", "."} {
		if _, err := ValidatePrefix(bad); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
	}
	for raw, want := range map[string]string{"/a/b/": "a/b", "": "", "dbferry": "dbferry"} {
		got, err := ValidatePrefix(raw)
		if err != nil || got != want {
			t.Errorf("ValidatePrefix(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestS3Policy(t *testing.T) {
	pol, err := S3Policy("my-bucket", "backups", true)
	if err != nil {
		t.Fatal(err)
	}
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
	appendOnly, err := S3Policy("my-bucket", "backups", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(appendOnly, "DeleteObject") {
		t.Error("append-only policy still grants DeleteObject")
	}

	// Empty prefix scopes objects to the whole bucket.
	whole, err := S3Policy("b", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(whole, `"arn:aws:s3:::b/*"`) {
		t.Errorf("empty prefix policy: %s", whole)
	}

	// Wildcards and navigation are refused outright — a user-supplied `*`
	// must never become an IAM wildcard.
	for _, bad := range []string{"a*", "a/../b"} {
		if _, err := S3Policy("b", bad, true); err == nil {
			t.Errorf("policy accepted hostile prefix %q", bad)
		}
	}
	if _, err := S3Policy("bad*bucket", "p", true); err == nil {
		t.Error("policy accepted a wildcard bucket name")
	}
}
