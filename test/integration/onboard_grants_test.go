//go:build integration

package integration

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/dbferry/dbferry/onboard"
	"github.com/dbferry/dbferry/pipeline"
)

// stripToStatements removes comment lines (which may contain semicolons)
// and psql meta-commands, returning executable statements.
func stripToStatements(script string) []string {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "\\") {
			continue
		}
		kept = append(kept, line)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		if body := strings.TrimSpace(stmt); body != "" {
			out = append(out, body)
		}
	}
	return out
}

// TestGeneratedPostgresGrantsRunARealDump pins the onboard contract on the
// engine invocation itself: a role created from the generated snippet (and
// nothing else) must dump a database that has a NON-public schema — the
// case naive public-only grants would fail.
func TestGeneratedPostgresGrantsRunARealDump(t *testing.T) {
	suffix := uniqueSuffix()
	srcDB := "it_grants_" + suffix
	user := "it_grants_role_" + suffix
	password := "it-grants-pass"

	admin := openPG(t, pg17DSN)
	loadPGFixture(t, admin, pg17DSN, srcDB)
	t.Cleanup(func() {
		admin.Exec(`DROP DATABASE IF EXISTS "` + srcDB + `" WITH (FORCE)`)
		admin.Exec(`DROP ROLE IF EXISTS "` + user + `"`)
	})

	// A second schema with data — pg_read_all_data must cover it too.
	srcAdmin := openPG(t, dsnWithDB(t, pg17DSN, srcDB))
	pgExec(t, srcAdmin,
		`CREATE SCHEMA app2`,
		`CREATE TABLE app2.notes (id int PRIMARY KEY, body text)`,
		`INSERT INTO app2.notes VALUES (1, 'second-schema-data')`,
	)

	script, err := onboard.PostgresGrants(user, srcDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range stripToStatements(script) {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("apply %q: %v", stmt, err)
		}
	}
	// The script sets the password via psql's interactive \password; tests
	// set it explicitly.
	if _, err := admin.Exec(fmt.Sprintf(`ALTER ROLE "%s" PASSWORD '%s'`, user, password)); err != nil {
		t.Fatal(err)
	}

	prefix := "it/grants-pg/" + suffix
	dsn := strings.Replace(dsnWithDB(t, pg17DSN, srcDB), "dbferry:dbferry@", user+":"+password+"@", 1)
	res, err := pipeline.Run(context.Background(), pipeline.Config{
		DSN:          dsn,
		Dest:         "s3://" + s3Bucket + "/" + prefix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   s3Endpoint,
	})
	if err != nil {
		t.Fatalf("backup with generated grants failed: %v", err)
	}
	if res.Bytes == 0 {
		t.Fatal("empty backup")
	}

	// The dump must actually CONTAIN the non-public schema's data.
	ciphertext := getObject(t, s3Client(t), res.Key)
	plain := decryptDecompress(t, ciphertext, ageIdentityPath(t))
	if !strings.Contains(string(plain), "second-schema-data") {
		t.Fatal("dump misses the non-public schema's rows")
	}
}

// TestDoctorFlagsUnreadableLargeObjects pins the residual gap the grants
// cannot close: large objects carry per-object ACLs no role-level grant
// covers, so the doctor must surface an admin-owned large object as a
// failing check for the generated role instead of staying green.
func TestDoctorFlagsUnreadableLargeObjects(t *testing.T) {
	suffix := uniqueSuffix()
	srcDB := "it_lo_" + suffix
	user := "it_lo_role_" + suffix
	password := "it-lo-pass"

	admin := openPG(t, pg17DSN)
	loadPGFixture(t, admin, pg17DSN, srcDB)
	t.Cleanup(func() {
		admin.Exec(`DROP DATABASE IF EXISTS "` + srcDB + `" WITH (FORCE)`)
		admin.Exec(`DROP ROLE IF EXISTS "` + user + `"`)
	})

	srcAdmin := openPG(t, dsnWithDB(t, pg17DSN, srcDB))
	pgExec(t, srcAdmin, `SELECT lo_from_bytea(0, 'admin-owned blob')`)

	script, err := onboard.PostgresGrants(user, srcDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range stripToStatements(script) {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("apply %q: %v", stmt, err)
		}
	}
	if _, err := admin.Exec(fmt.Sprintf(`ALTER ROLE "%s" PASSWORD '%s'`, user, password)); err != nil {
		t.Fatal(err)
	}

	dsn := strings.Replace(dsnWithDB(t, pg17DSN, srcDB), "dbferry:dbferry@", user+":"+password+"@", 1)
	var loCheck *pipeline.Check
	for _, c := range pipeline.DiagnoseSource(context.Background(), dsn) {
		if c.Name == "large object read access" {
			cc := c
			loCheck = &cc
		}
	}
	if loCheck == nil {
		t.Fatal("doctor has no large object check for a database with an unreadable large object")
	}
	if loCheck.Status != pipeline.StatusFail {
		t.Fatalf("large object check = %s (%s), want fail", loCheck.Status, loCheck.Detail)
	}
}

// TestDoctorFlagsUnreadableSequences pins the sequence half of the read
// check: pg_dump reads sequence state, so a role with SELECT on every table
// but no privilege on a sequence must fail the doctor, not surprise the
// first backup.
func TestDoctorFlagsUnreadableSequences(t *testing.T) {
	suffix := uniqueSuffix()
	srcDB := "it_seq_" + suffix
	user := "it_seq_role_" + suffix
	password := "it-seq-pass"

	admin := openPG(t, pg17DSN)
	loadPGFixture(t, admin, pg17DSN, srcDB)
	t.Cleanup(func() {
		admin.Exec(`DROP DATABASE IF EXISTS "` + srcDB + `" WITH (FORCE)`)
		admin.Exec(`DROP ROLE IF EXISTS "` + user + `"`)
	})

	// Hand-rolled naive grants (NOT the generated snippet): tables covered,
	// the sequence forgotten.
	srcAdmin := openPG(t, dsnWithDB(t, pg17DSN, srcDB))
	pgExec(t, srcAdmin, `CREATE SEQUENCE public.orders_seq`)
	pgExec(t, admin, fmt.Sprintf(`CREATE ROLE "%s" LOGIN PASSWORD '%s'`, user, password))
	pgExec(t, srcAdmin,
		fmt.Sprintf(`GRANT CONNECT ON DATABASE "%s" TO "%s"`, srcDB, user),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO "%s"`, user),
		fmt.Sprintf(`GRANT SELECT ON ALL TABLES IN SCHEMA public TO "%s"`, user),
	)

	dsn := strings.Replace(dsnWithDB(t, pg17DSN, srcDB), "dbferry:dbferry@", user+":"+password+"@", 1)
	for _, c := range pipeline.DiagnoseSource(context.Background(), dsn) {
		if c.Name == "table read access" {
			// Every table IS readable — only sequences can be in the
			// unreadable set, so a failure here is the sequence gap.
			if c.Status != pipeline.StatusFail || !strings.Contains(c.Detail, "sequence") {
				t.Fatalf("sequence gap not flagged: %s — %s", c.Status, c.Detail)
			}
			return
		}
	}
	t.Fatal("doctor has no table read access check")
}

// TestGeneratedMySQLGrantsRunARealDump pins the MySQL side: a user created
// from the generated snippet dumps a database with a view, trigger, routine
// and event — exactly the object kinds our flags request.
func TestGeneratedMySQLGrantsRunARealDump(t *testing.T) {
	suffix := uniqueSuffix()
	srcDB := "it_grants_" + suffix
	user := "it_grants_" + suffix // MySQL usernames are limited to 32 chars

	admin, err := sql.Open("mysql", mysqlDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	mustExec := func(q string) {
		t.Helper()
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec("CREATE DATABASE `" + srcDB + "`")
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS `" + srcDB + "`")
		admin.Exec("DROP USER IF EXISTS '" + user + "'@'%'")
	})
	mustExec("CREATE TABLE `" + srcDB + "`.t (id INT PRIMARY KEY, v TEXT) ENGINE=InnoDB")
	mustExec("INSERT INTO `" + srcDB + "`.t VALUES (1, 'mysql-grants-data')")
	mustExec("CREATE VIEW `" + srcDB + "`.v AS SELECT id FROM `" + srcDB + "`.t")
	mustExec("CREATE PROCEDURE `" + srcDB + "`.p() SELECT 1")

	// Apply the generated grants, replacing RANDOM PASSWORD with a known one
	// (the server-side generation is interactive output a test cannot read).
	password := "it-grants-pass"
	generated, err := onboard.MySQLGrants(user, srcDB)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(generated,
		"IDENTIFIED BY RANDOM PASSWORD", "IDENTIFIED BY '"+password+"'")
	for _, stmt := range stripToStatements(script) {
		mustExec(stmt)
	}

	prefix := "it/grants-my/" + suffix
	host := strings.TrimPrefix(mysqlDSN[strings.Index(mysqlDSN, "tcp(")+4:], "")
	host = host[:strings.Index(host, ")")]
	res, err := pipeline.Run(context.Background(), pipeline.Config{
		DSN:          fmt.Sprintf("mysql://%s:%s@%s/%s", user, password, host, srcDB),
		Dest:         "s3://" + s3Bucket + "/" + prefix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   s3Endpoint,
	})
	if err != nil {
		t.Fatalf("backup with generated mysql grants failed: %v", err)
	}
	ciphertext := getObject(t, s3Client(t), res.Key)
	plain := decryptDecompress(t, ciphertext, ageIdentityPath(t))
	for _, want := range []string{"mysql-grants-data", "PROCEDURE"} {
		if !strings.Contains(string(plain), want) {
			t.Fatalf("dump misses %q", want)
		}
	}
}
