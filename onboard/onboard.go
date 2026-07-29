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

// validIdent rejects names that cannot be safely embedded in a generated
// script. Quoting handles quote characters, but control characters —
// newlines above all — would let a hostile name break out of the line-based
// psql meta-commands (\password) or mangle the script; no real database or
// role name contains them.
func validIdent(kind, s string) error {
	if s == "" {
		return fmt.Errorf("%s name must not be empty", kind)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s name must not contain control characters", kind)
		}
	}
	return nil
}

// pgIdent quotes a PostgreSQL identifier.
func pgIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// myIdent quotes a MySQL identifier.
func myIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// myGrantDB quotes a database name for a database-level GRANT ... ON db.*
// clause. There — and only there — MySQL treats `_` and `%` as LIKE-style
// wildcards even inside backticks, so a grant on `my_db`.* silently also
// covers a sibling `my1db`. Escaping with a backslash makes them literal.
// Caveat (verified against MySQL 8.4): with partial_revokes=ON the escape
// character is itself literal, so the escaped form names the wrong database —
// callers must offer the unescaped form for that case (wildcards are already
// off there, so it is safe).
func myGrantDB(s string) string {
	s = strings.NewReplacer(`\`, `\\`, "_", `\_`, "%", `\%`).Replace(s)
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// myGrantNeedsEscape reports whether a database name contains characters that
// are LIKE wildcards (or their escape) in a database-level GRANT.
func myGrantNeedsEscape(s string) bool {
	return strings.ContainsAny(s, `_%\`)
}

// myString quotes a MySQL string literal: both quote AND backslash must be
// escaped (default sql_mode treats backslash as an escape character).
func myString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return s
}

// PostgresGrants returns the SQL creating a read-only backup role for one
// database. The complete-coverage path is pg_read_all_data (PostgreSQL 14+,
// which dbferry requires anyway): it spans every schema and future object,
// while CONNECT on a single database keeps the scope to that database. The
// password is set interactively via psql's \password — it never appears in
// SQL text, shell history or server logs, and cannot break quoting.
func PostgresGrants(user, database string) (string, error) {
	if err := validIdent("role", user); err != nil {
		return "", err
	}
	if err := validIdent("database", database); err != nil {
		return "", err
	}
	u, d := pgIdent(user), pgIdent(database)
	return fmt.Sprintf(`-- Run in psql as a superuser (or the database owner).
-- 1. The backup role. The password prompt stores it encrypted — it never
--    appears in this script, your shell history or the server log:
CREATE ROLE %[1]s LOGIN;
\password %[1]s

-- 2. Read access: pg_read_all_data covers every schema, table, sequence
--    and future object; CONNECT on just this database keeps the scope to it
--    (the role cannot connect anywhere you do not grant CONNECT).
GRANT pg_read_all_data TO %[1]s;
REVOKE CONNECT ON DATABASE %[2]s FROM PUBLIC;
GRANT CONNECT ON DATABASE %[2]s TO %[1]s;

-- Note: REVOKE ... FROM PUBLIC is what makes CONNECT meaningful — without
-- it, PostgreSQL lets any role connect to any database by default. Skip
-- that line if your application roles rely on the default; the backup role
-- still cannot connect to databases where PUBLIC is already revoked.

-- 3. Scope check: pg_read_all_data reads ANY database this role can connect
--    to, and other databases in this cluster may still allow everyone to
--    connect (PostgreSQL's default). List the ones still open:
--
--      SELECT datname FROM pg_database
--      WHERE datallowconn AND NOT datistemplate
--        AND (datacl IS NULL OR EXISTS (
--              SELECT 1 FROM aclexplode(datacl) a
--              WHERE a.grantee = 0 AND a.privilege_type = 'CONNECT'));
--
--    For each database this role must NOT read, close the default (grant
--    your application roles their own CONNECT first if they lack one):
--
--      REVOKE CONNECT ON DATABASE <name> FROM PUBLIC;

-- Note: large objects (lo), if you use them, carry per-object ACLs that no
-- role-level grant covers; the dbferry doctor lists any this role cannot
-- read (fix: GRANT SELECT ON LARGE OBJECT <oid>, or change their owner).
`, u, d), nil
}

// MySQLGrants returns the SQL creating a read-only backup user for one
// database, matching exactly what our mysqldump invocation runs
// (--single-transaction --no-tablespaces --routines --events --triggers):
// SELECT for data, SHOW VIEW / EVENT / TRIGGER for definitions, and
// SHOW_ROUTINE (8.0.20+) for stored programs. Neither LOCK TABLES nor
// PROCESS is needed: --single-transaction snapshots InnoDB without locking,
// and --no-tablespaces removes mysqldump's PROCESS demand. The password is
// generated by the server (RANDOM PASSWORD) — it never appears in this
// script and cannot break quoting; MySQL prints it once, store it then.
func MySQLGrants(user, database string) (string, error) {
	if err := validIdent("user", user); err != nil {
		return "", err
	}
	if err := validIdent("database", database); err != nil {
		return "", err
	}
	u, d := myString(user), myGrantDB(database)
	grantSection := fmt.Sprintf(`-- 2. Read access to the database being backed up:
GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON %[2]s.* TO '%[1]s'@'%%';
`, u, d)
	if myGrantNeedsEscape(database) {
		// In a database-level GRANT, _ and % are LIKE wildcards even inside
		// backticks — unescaped, this grant would also cover sibling databases
		// (my_db would match my1db). But with partial_revokes=ON the server
		// takes names literally, escape character included, so each mode needs
		// its own spelling of the same database.
		grantSection = fmt.Sprintf(`-- 2. Read access to the database being backed up. In database-level
--    grants MySQL treats _ and %% as wildcards unless partial_revokes is ON,
--    so the right spelling depends on your server. Check it first:
--
--      SELECT @@partial_revokes;
--
--    If it returns 0 (the default), the wildcard characters must be escaped
--    so the grant covers exactly this database and no look-alikes:
GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON %[2]s.* TO '%[1]s'@'%%';
--    If it returns 1, names are literal — run this instead:
--      GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON %[3]s.* TO '%[1]s'@'%%';
`, u, d, myIdent(database))
	}
	return fmt.Sprintf(`-- Run as an administrative user.
-- 1. The backup user. The server GENERATES the password and prints it in
--    the result — copy it into dbferry now, it is shown only once:
CREATE USER '%[1]s'@'%%' IDENTIFIED BY RANDOM PASSWORD;

%[2]s
-- 3. Stored procedures/functions in the dump (MySQL 8.0.20+):
GRANT SHOW_ROUTINE ON *.* TO '%[1]s'@'%%';
-- (on older servers use instead: GRANT SELECT ON mysql.proc TO '%[1]s'@'%%';)

FLUSH PRIVILEGES;
`, u, grantSection), nil
}

// ValidatePrefix rejects prefixes that would break or broaden the generated
// policy: `*` and `?` are IAM wildcards (a user-supplied wildcard silently
// grants beyond the literal prefix), path navigation (`..`, `//`) diverges
// from the pipeline's normalized object keys, and `%` is banned outright —
// the pipeline URL-parses the destination, so a percent-encoded prefix
// (`team%2Fprod`, `%2e%2e`) would decode into exactly the characters the
// literal checks reject, leaving the policy scoped to a path the pipeline
// never writes. The returned prefix is trimmed of surrounding slashes;
// empty is valid (whole bucket).
func ValidatePrefix(prefix string) (string, error) {
	p := strings.Trim(prefix, "/")
	if p == "" {
		return "", nil
	}
	if strings.ContainsAny(p, "*?") {
		return "", fmt.Errorf("prefix must not contain wildcard characters (* or ?)")
	}
	if strings.Contains(p, "%") {
		return "", fmt.Errorf("prefix must not contain percent signs (URL-encoding is ambiguous across S3 tools)")
	}
	if strings.Contains(p, "${") {
		return "", fmt.Errorf("prefix must not contain IAM policy variables (${...})")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("prefix must not contain control characters")
		}
	}
	if strings.Contains(p, "//") {
		return "", fmt.Errorf("prefix must not contain empty path segments (//)")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("prefix must not contain path navigation segments (. or ..)")
		}
	}
	return p, nil
}

// S3Policy returns a minimal AWS-style policy for the backup prefix. The
// object statement covers the streaming multipart upload (PutObject also
// authorizes CreateMultipartUpload/UploadPart/Complete), abort-on-failure
// with its ListParts verification, and read-back; ListBucket is
// prefix-limited (retention listing; s3:prefix is only valid on ListBucket,
// so it is the sole bucket-level action). withDelete adds DeleteObject —
// required for GFS retention; without it backups are append-only and
// pruning cannot work. The prefix is validated via ValidatePrefix.
func S3Policy(bucket, prefix string, withDelete bool) (string, error) {
	if bucket == "" || strings.ContainsAny(bucket, "*?/%") {
		return "", fmt.Errorf("bucket must be a plain bucket name (no wildcards, slashes or percent signs)")
	}
	p, err := ValidatePrefix(prefix)
	if err != nil {
		return "", err
	}

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
	listPrefix := "*"
	if p != "" {
		objARN = "arn:aws:s3:::" + bucket + "/" + p + "/*"
		listPrefix = p + "/*"
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
					"StringLike": map[string]any{"s3:prefix": listPrefix},
				},
			},
		},
	}
	out, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		panic(err) // static structure; cannot fail
	}
	return string(out) + "\n", nil
}
