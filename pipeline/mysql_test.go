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

func TestHasRoutineGrant(t *testing.T) {
	tests := []struct {
		name   string
		grants []string
		want   bool
	}{
		{"show_routine global", []string{"GRANT SHOW_ROUTINE ON *.* TO 'u'@'%'"}, true},
		{"our snippet", []string{
			"GRANT USAGE ON *.* TO 'dbferry_backup'@'%'",
			"GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON `shop`.* TO 'dbferry_backup'@'%'",
			"GRANT SHOW_ROUTINE ON *.* TO 'dbferry_backup'@'%'",
		}, true},
		{"db-scoped select only", []string{
			"GRANT USAGE ON *.* TO 'u'@'%'",
			"GRANT SELECT, SHOW VIEW, EVENT, TRIGGER ON `shop`.* TO 'u'@'%'",
		}, false},
		{"global select", []string{"GRANT SELECT ON *.* TO 'u'@'%'"}, true},
		{"all privileges", []string{"GRANT ALL PRIVILEGES ON *.* TO 'root'@'%'"}, true},
		{"legacy mysql.proc", []string{"GRANT SELECT ON `mysql`.`proc` TO 'u'@'%'"}, true},
		{"usage only", []string{"GRANT USAGE ON *.* TO 'u'@'%'"}, false},
		// SHOW VIEW must not read as a SELECT/SHOW_ROUTINE lookalike, and a
		// db-scoped SHOW_ROUTINE-looking grant must not count as global.
		{"show view global is not enough", []string{"GRANT SHOW VIEW ON *.* TO 'u'@'%'"}, false},
		{"select on other db", []string{"GRANT SELECT ON `mysqlish`.* TO 'u'@'%'"}, false},
		{"no ON clause", []string{"GRANT PROXY"}, false},
		{"empty", nil, false},
		// Adversarial role names (Codex R1 P1): a role literally named
		// "SHOW_ROUTINE ON *.*" is an assignment row, not a privilege row —
		// the quote inside the would-be privilege list disqualifies it.
		{"hostile role name show_routine", []string{
			"GRANT USAGE ON *.* TO 'u'@'%'",
			"GRANT `SHOW_ROUTINE ON *.*`@`%` TO `u`@`%`",
		}, false},
		{"hostile role name all privileges", []string{
			"GRANT `ALL PRIVILEGES ON *.*`@`%` TO `u`@`%`",
		}, false},
		// Partial revokes (Codex R1 P1): a REVOKE row voids global
		// SELECT/ALL as proof — the revoked schema's routines are invisible
		// while the grant row still looks global.
		{"partial revoke voids global select", []string{
			"GRANT SELECT ON *.* TO 'u'@'%'",
			"REVOKE SELECT ON `shop`.* FROM 'u'@'%'",
		}, false},
		{"partial revoke voids all privileges", []string{
			"GRANT ALL PRIVILEGES ON *.* TO 'u'@'%'",
			"REVOKE ALL PRIVILEGES ON `shop`.* FROM 'u'@'%'",
		}, false},
		// ...but SHOW_ROUTINE is global-only and survives partial revokes.
		{"show_routine survives partial revoke", []string{
			"GRANT SHOW_ROUTINE ON *.* TO 'u'@'%'",
			"REVOKE SELECT ON `shop`.* FROM 'u'@'%'",
		}, true},
		// A revoke row with a quoted pseudo-privilege list must not flip
		// the restriction flag either.
		{"hostile role revoke ignored", []string{
			"GRANT SELECT ON *.* TO 'u'@'%'",
			"REVOKE `SELECT ON x`@`%` FROM `u`@`%`",
		}, true},
		{"grant option suffix still global", []string{
			"GRANT SELECT ON *.* TO 'u'@'%' WITH GRANT OPTION",
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasRoutineGrant(tt.grants); got != tt.want {
				t.Errorf("hasRoutineGrant(%q) = %v, want %v", tt.grants, got, tt.want)
			}
		})
	}
}

// tls= is the go-sql-driver spelling managed providers hand out (DO:
// ?tls=true). It must map onto ssl-mode, not be silently dropped — a DSN
// that asks for TLS must get TLS or an error (Codex R1 P1).
func TestParseMySQLDSNTLSAlias(t *testing.T) {
	tests := []struct {
		dsn     string
		want    string // resulting sslMode
		wantErr bool
	}{
		// tls=true means VERIFIED TLS in go-sql-driver — the alias must
		// keep that strength, never downgrade to unverified REQUIRED
		// (Codex R2).
		{"mysql://u@h:3306/db?tls=true", "VERIFY_IDENTITY", false},
		{"mysql://u@h:3306/db?tls=false", "DISABLED", false},
		{"mysql://u@h:3306/db?tls=preferred", "PREFERRED", false},
		{"mysql://u@h:3306/db?tls=skip-verify", "REQUIRED", false},
		{"mysql://u@h:3306/db?tls=banana", "", true},
		{"mysql://u@h:3306/db?ssl-mode=DISABLED&tls=true", "", true},
		// Different strengths spelled together must be an error, not a
		// silent pick.
		{"mysql://u@h:3306/db?ssl-mode=REQUIRED&tls=true", "", true},
		{"mysql://u@h:3306/db?ssl-mode=VERIFY_IDENTITY&tls=true", "VERIFY_IDENTITY", false},
	}
	for _, tt := range tests {
		src, err := parseMySQLDSN(tt.dsn)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseMySQLDSN(%q): expected error", tt.dsn)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMySQLDSN(%q): %v", tt.dsn, err)
			continue
		}
		if src.sslMode != tt.want {
			t.Errorf("parseMySQLDSN(%q).sslMode = %q, want %q", tt.dsn, src.sslMode, tt.want)
		}
	}

	// The alias must reach BOTH consumers with verification intact: the Go
	// driver DSN gets tls=true (verify), and the dump tools get
	// --ssl-mode=VERIFY_IDENTITY — not the unverified skip-verify/REQUIRED
	// pair.
	src, err := parseMySQLDSN("mysql://u@h:3306/db?tls=true")
	if err != nil {
		t.Fatal(err)
	}
	if dsn := src.goDSN(); !strings.Contains(dsn, "tls=true") {
		t.Errorf("goDSN for tls=true lacks verified tls config: %q", dsn)
	}
	if args := src.sslArgs(); len(args) != 1 || args[0] != "--ssl-mode=VERIFY_IDENTITY" {
		t.Errorf("sslArgs for tls=true = %v, want [--ssl-mode=VERIFY_IDENTITY]", args)
	}
}
