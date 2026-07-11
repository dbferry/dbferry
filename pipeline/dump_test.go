package pipeline

import (
	"context"
	"strings"
	"testing"
)

func TestParsePostgresDSN(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		want    pgSource
		wantErr string
	}{
		{
			name: "full url",
			dsn:  "postgres://user:pass@db.example:6543/shop?sslmode=require",
			want: pgSource{host: "db.example", port: "6543", user: "user", password: "pass", database: "shop", sslmode: "require"},
		},
		{
			name: "default port and postgresql scheme",
			dsn:  "postgresql://u@localhost/app",
			want: pgSource{host: "localhost", port: "5432", user: "u", database: "app"},
		},
		{name: "missing database", dsn: "postgres://u:p@localhost:5432/", wantErr: "database name"},
		{name: "wrong scheme", dsn: "mysql://u@localhost/app", wantErr: "not a PostgreSQL URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePostgresDSN(tc.dsn)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parsePostgresDSN(%q) error = %v, want containing %q", tc.dsn, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePostgresDSN(%q) unexpected error: %v", tc.dsn, err)
			}
			if got != tc.want {
				t.Fatalf("parsePostgresDSN(%q) = %+v, want %+v", tc.dsn, got, tc.want)
			}
		})
	}
}

func TestClusterLabel(t *testing.T) {
	if got := (pgSource{host: "localhost", port: "5432"}).cluster(); got != "localhost" {
		t.Errorf("default port cluster = %q, want %q", got, "localhost")
	}
	if got := (pgSource{host: "localhost", port: "5417"}).cluster(); got != "localhost_5417" {
		t.Errorf("non-default port cluster = %q, want %q", got, "localhost_5417")
	}
}

// TestDumpCommandKeepsSecretsOffArgv is the security invariant: the password
// and DSN travel through libpq env vars, never on the child's argv.
func TestDumpCommandKeepsSecretsOffArgv(t *testing.T) {
	s := pgSource{host: "h", port: "5432", user: "u", password: "s3cr3t", database: "app"}
	cmd := s.dumpCommand(context.Background())

	for _, arg := range cmd.Args {
		if strings.Contains(arg, "s3cr3t") || strings.Contains(arg, "app") {
			t.Fatalf("secret/db leaked onto argv: %q", cmd.Args)
		}
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "-Fc") || !strings.Contains(joined, "-Z0") {
		t.Errorf("dump command missing -Fc/-Z0: %q", cmd.Args)
	}

	var sawPassword, sawDB bool
	for _, e := range cmd.Env {
		if e == "PGPASSWORD=s3cr3t" {
			sawPassword = true
		}
		if e == "PGDATABASE=app" {
			sawDB = true
		}
	}
	if !sawPassword || !sawDB {
		t.Errorf("expected PGPASSWORD/PGDATABASE in env; sawPassword=%v sawDB=%v", sawPassword, sawDB)
	}
}

func TestCappedBufferKeepsTail(t *testing.T) {
	b := newCappedBuffer(5)
	if _, err := b.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "world" {
		t.Errorf("cappedBuffer tail = %q, want %q", got, "world")
	}
}
