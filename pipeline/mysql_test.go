package pipeline

import (
	"context"
	"strings"
	"testing"
)

func TestParseMySQLDSN(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		want    mysqlSource
		wantErr string
	}{
		{
			name: "full url",
			dsn:  "mysql://user:pass@db.example:3307/shop",
			want: mysqlSource{host: "db.example", port: "3307", user: "user", password: "pass", database: "shop"},
		},
		{
			name: "default port",
			dsn:  "mysql://u@localhost/app",
			want: mysqlSource{host: "localhost", port: "3306", user: "u", database: "app"},
		},
		{
			name: "ssl-mode required (managed provider connection string)",
			dsn:  "mysql://doadmin@db.example:25060/defaultdb?ssl-mode=REQUIRED",
			want: mysqlSource{host: "db.example", port: "25060", user: "doadmin", database: "defaultdb", sslMode: "REQUIRED"},
		},
		{
			name: "ssl-mode is case-insensitive",
			dsn:  "mysql://u@h/app?ssl-mode=verify_ca",
			want: mysqlSource{host: "h", port: "3306", user: "u", database: "app", sslMode: "VERIFY_CA"},
		},
		{name: "unknown ssl-mode", dsn: "mysql://u@h/app?ssl-mode=maybe", wantErr: "unsupported ssl-mode"},
		{name: "missing database", dsn: "mysql://u:p@localhost:3306/", wantErr: "database name"},
		{name: "wrong scheme", dsn: "postgres://u@localhost/app", wantErr: "not a MySQL URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMySQLDSN(tc.dsn)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseMySQLDSN(%q) error = %v, want containing %q", tc.dsn, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMySQLDSN(%q) unexpected error: %v", tc.dsn, err)
			}
			if got != tc.want {
				t.Fatalf("parseMySQLDSN(%q) = %+v, want %+v", tc.dsn, got, tc.want)
			}
		})
	}
}

func TestMySQLDriverMetadata(t *testing.T) {
	d, err := newMySQLDriver("mysql://u:p@h:3307/shop")
	if err != nil {
		t.Fatal(err)
	}
	if d.Engine() != "mysql" || d.Database() != "shop" || d.Cluster() != "h_3307" {
		t.Errorf("metadata: engine=%q db=%q cluster=%q", d.Engine(), d.Database(), d.Cluster())
	}
	if !strings.Contains(d.DumpFormat(), "mysqldump") {
		t.Errorf("dump format = %q", d.DumpFormat())
	}
}

// TestMySQLDumpCommandKeepsSecretsOffArgv is the security invariant: the
// password travels via MYSQL_PWD, never on argv; the required flags are present.
func TestMySQLDumpCommandKeepsSecretsOffArgv(t *testing.T) {
	d, err := newMySQLDriver("mysql://root:s3cr3t@h:3306/app")
	if err != nil {
		t.Fatal(err)
	}
	cmd := d.BuildDumpCommand(context.Background())

	for _, a := range cmd.Args {
		if strings.Contains(a, "s3cr3t") {
			t.Fatalf("password leaked onto argv: %q", cmd.Args)
		}
	}
	joined := strings.Join(cmd.Args, " ")
	for _, f := range []string{"--single-transaction", "--set-gtid-purged=OFF", "--no-tablespaces", "--routines", "--events"} {
		if !strings.Contains(joined, f) {
			t.Errorf("dump command missing %s: %q", f, cmd.Args)
		}
	}
	var sawPwd bool
	for _, e := range cmd.Env {
		if e == "MYSQL_PWD=s3cr3t" {
			sawPwd = true
		}
	}
	if !sawPwd {
		t.Error("expected MYSQL_PWD in the command environment")
	}
}

// TestMySQLDatabaseNeverParsedAsOption pins the end-of-options guard: a
// database legally named like a mysqldump option must arrive after "--", or it
// becomes a file-write primitive (--tab=/dir writes plaintext dumps to disk).
func TestMySQLDatabaseNeverParsedAsOption(t *testing.T) {
	d, err := newMySQLDriver("mysql://root:pw@h:3306/--tab=%2Ftmp%2Fevil")
	if err != nil {
		t.Fatal(err)
	}
	cmd := d.BuildDumpCommand(context.Background())
	args := cmd.Args
	last := args[len(args)-1]
	if last != "--tab=/tmp/evil" || args[len(args)-2] != "--" {
		t.Fatalf("database must be the sole argument after --: %q", args)
	}

	// The mysql client ignores "--" (unlike mysqldump), so the restore target
	// must travel as --database=NAME to stay out of its option parser.
	restore := d.BuildRestoreCommand("--init-command=DROP DATABASE x")
	if restore[len(restore)-1] != "--database=--init-command=DROP DATABASE x" {
		t.Fatalf("restore target must be passed via --database=: %q", restore)
	}
}

// TestMySQLSSLModeReachesDriverAndTools pins the TLS path end to end: the
// DSN's ssl-mode must land in the go-sql-driver config AND on the
// mysqldump/mysql argv — a user with REQUIRE SSL fails auth (1045) if it is
// dropped anywhere.
func TestMySQLSSLModeReachesDriverAndTools(t *testing.T) {
	d, err := newMySQLDriver("mysql://doadmin:pw@h:25060/defaultdb?ssl-mode=REQUIRED")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.src.goDSN(); !strings.Contains(got, "tls=skip-verify") {
		t.Errorf("driver DSN lost ssl-mode=REQUIRED (want tls=skip-verify): %q", got)
	}
	dump := strings.Join(d.BuildDumpCommand(context.Background()).Args, " ")
	if !strings.Contains(dump, "--ssl-mode=REQUIRED") {
		t.Errorf("mysqldump argv lost --ssl-mode: %q", dump)
	}
	restore := strings.Join(d.BuildRestoreCommand("target"), " ")
	if !strings.Contains(restore, "--ssl-mode=REQUIRED") {
		t.Errorf("restore argv lost --ssl-mode: %q", restore)
	}

	// VERIFY_* escalate to full verification in the driver.
	d2, err := newMySQLDriver("mysql://u:p@h/app?ssl-mode=VERIFY_IDENTITY")
	if err != nil {
		t.Fatal(err)
	}
	if got := d2.src.goDSN(); !strings.Contains(got, "tls=true") {
		t.Errorf("VERIFY_IDENTITY should map to tls=true: %q", got)
	}

	// No ssl-mode = today's behavior, untouched.
	d3, err := newMySQLDriver("mysql://u:p@h/app")
	if err != nil {
		t.Fatal(err)
	}
	if got := d3.src.goDSN(); strings.Contains(got, "tls=") {
		t.Errorf("DSN without ssl-mode must not set tls: %q", got)
	}
	if plain := strings.Join(d3.BuildDumpCommand(context.Background()).Args, " "); strings.Contains(plain, "--ssl-mode") {
		t.Errorf("dump argv without ssl-mode must not carry the flag: %q", plain)
	}
}
