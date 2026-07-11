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
	for _, f := range []string{"--single-transaction", "--set-gtid-purged=OFF", "--routines", "--events"} {
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
