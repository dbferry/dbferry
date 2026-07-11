package pipeline

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// pgSource is a PostgreSQL connection resolved from the DSN. It is passed to
// pg_dump through libpq environment variables, never on argv, so the password
// never appears in `ps` output.
type pgSource struct {
	host     string
	port     string
	user     string
	password string
	database string
	sslmode  string
}

// parsePostgresDSN parses a postgres:// or postgresql:// URL into a pgSource.
// The database name is required: dbferry backs up one database per run.
func parsePostgresDSN(dsn string) (pgSource, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return pgSource{}, fmt.Errorf("pipeline: parse DSN: invalid URL")
	}
	switch u.Scheme {
	case "postgres", "postgresql":
	default:
		return pgSource{}, fmt.Errorf("pipeline: DSN scheme %q is not a PostgreSQL URL", u.Scheme)
	}

	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		return pgSource{}, fmt.Errorf("pipeline: DSN must include a database name (postgres://host/DBNAME)")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	password, _ := u.User.Password()

	return pgSource{
		host:     u.Hostname(),
		port:     port,
		user:     u.User.Username(),
		password: password,
		database: db,
		sslmode:  u.Query().Get("sslmode"),
	}, nil
}

// cluster is the source-cluster label for the object key. Host alone can't tell
// two clusters on the same host apart, so a non-default port is folded in.
func (s pgSource) cluster() string {
	c := s.host
	if s.port != "5432" {
		c += "_" + s.port
	}
	return sanitizeKeySegment(c)
}

// dumpCommand builds `pg_dump -Fc -Z0` with the connection supplied entirely
// through libpq env vars. -Fc is the custom format (selective restore, parallel
// pg_restore) and -Z0 disables pg_dump's own compression so our zstd stage owns
// it (DECISIONS.md 2026-07-11). No DSN or password reaches argv.
func (s pgSource) dumpCommand(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "pg_dump", "-Fc", "-Z0")
	env := append(os.Environ(),
		"PGHOST="+s.host,
		"PGPORT="+s.port,
		"PGUSER="+s.user,
		"PGPASSWORD="+s.password,
		"PGDATABASE="+s.database,
	)
	if s.sslmode != "" {
		env = append(env, "PGSSLMODE="+s.sslmode)
	}
	cmd.Env = env
	return cmd
}

// dumpFailure turns a non-zero pg_dump exit into an actionable error carrying
// the tail of its stderr. The DSN is not part of this message; full credential
// redaction across all output arrives in poc-plan 3.3.
func dumpFailure(waitErr error, stderrTail string) error {
	stderrTail = strings.TrimSpace(stderrTail)
	if stderrTail == "" {
		return fmt.Errorf("pipeline: pg_dump failed: %w", waitErr)
	}
	return fmt.Errorf("pipeline: pg_dump failed: %w\n%s", waitErr, stderrTail)
}
