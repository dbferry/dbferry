// Package onboard generates the exact access a customer must grant dbferry:
// read-only database credentials scoped to what the dump tools actually run,
// and a minimal S3 policy scoped to the backup prefix. The snippets are the
// contract of least privilege — anything they omit, the pipeline does not
// need.
package onboard

import (
	"encoding/json"
	"fmt"
	"strings"
)

// pgIdent quotes a PostgreSQL identifier.
func pgIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// myIdent quotes a MySQL identifier.
func myIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// PostgresGrants returns the SQL creating a read-only backup role for one
// database, matching what `pg_dump -Fc` needs: CONNECT, USAGE on schemas,
// SELECT on tables/sequences — now and for future objects. The password is
// deliberately a placeholder the customer replaces.
func PostgresGrants(user, database string) string {
	u, d := pgIdent(user), pgIdent(database)
	return fmt.Sprintf(`-- Run as a superuser (or the database owner).
-- 1. The backup role (replace the password; it is what you store in dbferry):
CREATE ROLE %[1]s LOGIN PASSWORD 'REPLACE-WITH-A-STRONG-PASSWORD';

-- 2. Let it connect to the database being backed up:
GRANT CONNECT ON DATABASE %[2]s TO %[1]s;

-- 3. Inside that database (\connect %[2]s), read access to everything —
--    current objects and ones created later:
GRANT USAGE ON SCHEMA public TO %[1]s;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO %[1]s;
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO %[1]s;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO %[1]s;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON SEQUENCES TO %[1]s;

-- Using more schemas than public? Repeat step 3 per schema.
-- Shortcut on PostgreSQL 14+: instead of step 3,
--   GRANT pg_read_all_data TO %[1]s;
-- (reads EVERY database the role can connect to — broader, but simpler).
`, u, d)
}

// MySQLGrants returns the SQL creating a read-only backup user for one
// database, matching exactly what our mysqldump invocation runs
// (--single-transaction --routines --events --triggers): SELECT for data,
// SHOW VIEW / EVENT / TRIGGER for definitions, and SHOW_ROUTINE (8.0.20+)
// for stored programs. LOCK TABLES is NOT needed — --single-transaction
// snapshots InnoDB without locking.
func MySQLGrants(user, database string) string {
	d := myIdent(database)
	return fmt.Sprintf(`-- Run as an administrative user.
-- 1. The backup user (replace the password; it is what you store in dbferry):
CREATE USER '%[1]s'@'%%' IDENTIFIED BY 'REPLACE-WITH-A-STRONG-PASSWORD';

-- 2. Read access to the database being backed up:
GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON %[2]s.* TO '%[1]s'@'%%';

-- 3. Stored procedures/functions in the dump (MySQL 8.0.20+):
GRANT SHOW_ROUTINE ON *.* TO '%[1]s'@'%%';
-- (on older servers use: GRANT SELECT ON mysql.proc TO '%[1]s'@'%%';)

FLUSH PRIVILEGES;
`, strings.ReplaceAll(user, "'", "''"), d)
}

// S3Policy returns a minimal AWS-style bucket policy for the backup prefix.
// The object statement covers the streaming multipart upload (PutObject also
// authorizes CreateMultipartUpload/UploadPart/Complete), abort-on-failure
// with its ListParts verification, and read-back; ListBucket is
// prefix-limited (retention listing; s3:prefix is only valid on ListBucket,
// so it is the sole bucket-level action). withDelete adds DeleteObject —
// required for GFS retention; without it backups are append-only and
// pruning cannot work.
func S3Policy(bucket, prefix string, withDelete bool) string {
	objectActions := []string{
		"s3:PutObject",
		"s3:AbortMultipartUpload",
		"s3:ListMultipartUploadParts", // abort verification (ListParts)
		"s3:GetObject",
	}
	if withDelete {
		objectActions = append(objectActions, "s3:DeleteObject")
	}
	objARN := "arn:aws:s3:::" + bucket + "/*"
	listPrefix := ""
	if prefix != "" {
		objARN = "arn:aws:s3:::" + bucket + "/" + strings.Trim(prefix, "/") + "/*"
		listPrefix = strings.Trim(prefix, "/") + "/*"
	}

	policy := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Sid":      "DbferryBackupObjects",
				"Effect":   "Allow",
				"Action":   objectActions,
				"Resource": objARN,
			},
			{
				"Sid":      "DbferryListPrefix",
				"Effect":   "Allow",
				"Action":   []string{"s3:ListBucket"},
				"Resource": "arn:aws:s3:::" + bucket,
				"Condition": map[string]any{
					"StringLike": map[string]any{"s3:prefix": listPrefixOrAll(listPrefix)},
				},
			},
		},
	}
	out, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		panic(err) // static structure; cannot fail
	}
	return string(out) + "\n"
}

func listPrefixOrAll(p string) string {
	if p == "" {
		return "*"
	}
	return p
}
