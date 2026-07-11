//go:build integration

package integration

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// MySQL CLI/driver connection, overridable by env.
var (
	myHost = env("DBFERRY_TEST_MYSQL_HOST", "127.0.0.1")
	myPort = env("DBFERRY_TEST_MYSQL_PORT", "3308")
	myUser = env("DBFERRY_TEST_MYSQL_USER", "root")
	myPass = env("DBFERRY_TEST_MYSQL_PASS", "dbferry")
)

// TestMySQLFixtureRoundtrip validates the MySQL fixture and the comparison
// tooling ahead of the pipeline's MySQL support (Етап 4): load the fixture,
// dump it with the same options the driver will use (--routines --events
// --single-transaction --set-gtid-purged=OFF), restore into a fresh database,
// and assert tables, routines, events and triggers all survive.
func TestMySQLFixtureRoundtrip(t *testing.T) {
	const src, dst = "app", "app_restored"

	// Load the fixture (creates database `app`).
	runMySQLStdin(t, "load fixture", "", filepath.Join("fixtures", "mysql", "fixture.sql"))
	t.Cleanup(func() {
		runMySQL(t, "-e", "DROP DATABASE IF EXISTS "+src+"; DROP DATABASE IF EXISTS "+dst+";")
	})

	// mysqldump app -> restore into a fresh database.
	dumpPath := filepath.Join(t.TempDir(), "app.sql")
	dump := runMySQLDump(t, src)
	if err := os.WriteFile(dumpPath, dump, 0o600); err != nil {
		t.Fatal(err)
	}
	runMySQL(t, "-e", "CREATE DATABASE "+dst)
	runMySQLStdin(t, "restore", dst, dumpPath)

	// Compare fingerprints.
	srcDB := openMySQL(t, src)
	dstDB := openMySQL(t, dst)
	diffDigests(t, digestMySQL(t, srcDB, src), digestMySQL(t, dstDB, dst))
}

func mysqlBaseArgs() []string {
	return []string{"-h", myHost, "-P", myPort, "-u", myUser, "--protocol=TCP"}
}

// runMySQL runs the mysql client with the password supplied via MYSQL_PWD (never
// on argv).
func runMySQL(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("mysql", append(mysqlBaseArgs(), args...)...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+myPass)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mysql %v: %v\n%s", args, err, out)
	}
}

// runMySQLStdin runs the mysql client with a file piped to stdin, optionally
// against a specific database.
func runMySQLStdin(t *testing.T, what, db, sqlFile string) {
	t.Helper()
	args := mysqlBaseArgs()
	if db != "" {
		args = append(args, db)
	}
	f, err := os.Open(sqlFile)
	if err != nil {
		t.Fatalf("%s: open %s: %v", what, sqlFile, err)
	}
	defer f.Close()
	cmd := exec.Command("mysql", args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+myPass)
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s (%s): %v\n%s", what, sqlFile, err, out)
	}
}

func runMySQLDump(t *testing.T, db string) []byte {
	t.Helper()
	args := append(mysqlBaseArgs(),
		"--single-transaction", "--routines", "--events", "--set-gtid-purged=OFF", db)
	cmd := exec.Command("mysqldump", args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+myPass)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mysqldump %s: %v", db, err)
	}
	return out
}

func openMySQL(t *testing.T, db string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s", myUser, myPass, myHost, myPort, db)
	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("ping mysql (run `make stand-up`?): %v", err)
	}
	return sqlDB
}

// digestMySQL fingerprints a database: per-table row count and CHECKSUM TABLE
// (content-based, independent of the database name), plus the names of stored
// routines, events, and triggers.
func digestMySQL(t *testing.T, db *sql.DB, schema string) map[string]string {
	t.Helper()
	d := map[string]string{}

	tables := queryStrings(t, db,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema=? AND table_type='BASE TABLE' ORDER BY 1`, schema)
	for _, tbl := range tables {
		var count int
		if err := db.QueryRow(fmt.Sprintf("SELECT count(*) FROM `%s`.`%s`", schema, tbl)).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		var name string
		var checksum sql.NullInt64
		if err := db.QueryRow(fmt.Sprintf("CHECKSUM TABLE `%s`.`%s`", schema, tbl)).Scan(&name, &checksum); err != nil {
			t.Fatalf("checksum %s: %v", tbl, err)
		}
		d["table:"+tbl+":count"] = strconv.Itoa(count)
		d["table:"+tbl+":checksum"] = strconv.FormatInt(checksum.Int64, 10)
	}
	for _, r := range queryStrings(t, db,
		`SELECT concat(routine_type,':',routine_name) FROM information_schema.routines
		 WHERE routine_schema=? ORDER BY 1`, schema) {
		d["routine:"+r] = "1"
	}
	for _, e := range queryStrings(t, db,
		`SELECT event_name FROM information_schema.events WHERE event_schema=? ORDER BY 1`, schema) {
		d["event:"+e] = "1"
	}
	for _, tr := range queryStrings(t, db,
		`SELECT concat(event_object_table,':',trigger_name,':',action_timing,':',event_manipulation)
		 FROM information_schema.triggers WHERE trigger_schema=? ORDER BY 1`, schema) {
		d["trigger:"+tr] = "1"
	}
	return d
}
