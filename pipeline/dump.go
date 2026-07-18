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

// dumpCommandWith builds `<binary> -Fc -Z0` with the connection supplied
// entirely through libpq env vars. -Fc is the custom format (selective restore,
// parallel pg_restore) and -Z0 disables pg_dump's own compression so our zstd
// stage owns it (ADR-0005). No DSN or password reaches argv.
// binary is the (version-selected) pg_dump path (poc-plan 5.3).
func (s pgSource) dumpCommandWith(ctx context.Context, binary string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, "-Fc", "-Z0")
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

// pgDumpVersionOf returns the version string of a pg_dump binary for the
// manifest, e.g. "pg_dump (PostgreSQL) 17.2". Best-effort: "unknown" on error.
func pgDumpVersionOf(ctx context.Context, binary string) string {
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// dumpFailure turns a non-zero dump-tool exit into an actionable error carrying
// the tail of its stderr (which names the tool). The DSN is not part of this
// message; full credential redaction across all output is poc-plan 3.3.
func dumpFailure(waitErr error, stderrTail string) error {
	stderrTail = strings.TrimSpace(stderrTail)
	if stderrTail == "" {
		return classify(KindDump, "pipeline: dump failed: %w", waitErr)
	}
	return classify(KindDump, "pipeline: dump failed: %w\n%s", waitErr, stderrTail)
}
