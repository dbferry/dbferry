package pipeline

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Supported PostgreSQL server major range. Outside it, dbferry refuses before
// starting a dump (poc-plan 5.3), because the shipped pg_dump clients (14–17)
// can't be guaranteed to produce a restorable dump.
const (
	minSupportedPGMajor = 14
	maxSupportedPGMajor = 17
)

// pgClient is a discovered pg_dump binary and its major version.
type pgClient struct {
	path  string
	major int
}

var pgVersionRe = regexp.MustCompile(`(\d+)\.\d+`)

// parsePgDumpMajor extracts the major version from `pg_dump --version` output,
// e.g. "pg_dump (PostgreSQL) 17.2" → 17.
func parsePgDumpMajor(version string) (int, bool) {
	m := pgVersionRe.FindStringSubmatch(version)
	if m == nil {
		return 0, false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return major, true
}

// discoverPgDumpClients finds available pg_dump binaries: the PGDG per-version
// locations (/usr/lib/postgresql/<major>/bin/pg_dump — what the dbferry image
// ships) and whatever is on PATH.
func discoverPgDumpClients(ctx context.Context) []pgClient {
	var clients []pgClient
	seen := map[string]bool{}
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		out, err := exec.CommandContext(ctx, path, "--version").Output()
		if err != nil {
			return
		}
		if major, ok := parsePgDumpMajor(string(out)); ok {
			seen[path] = true
			clients = append(clients, pgClient{path: path, major: major})
		}
	}
	matches, _ := filepath.Glob("/usr/lib/postgresql/*/bin/pg_dump")
	for _, m := range matches {
		add(m)
	}
	if p, err := exec.LookPath("pg_dump"); err == nil {
		add(p)
	}
	return clients
}

// selectPgDump picks the client to dump a server of serverMajor: an exact major
// match is preferred (cleanest restore); otherwise the smallest available major
// newer than the server (a newer client can dump an older server). If no client
// is at least serverMajor, dumping is impossible — an error, not a silent wrong
// choice.
func selectPgDump(clients []pgClient, serverMajor int) (pgClient, error) {
	var newer *pgClient
	for i := range clients {
		c := clients[i]
		if c.major == serverMajor {
			return c, nil
		}
		if c.major > serverMajor && (newer == nil || c.major < newer.major) {
			newer = &clients[i]
		}
	}
	if newer != nil {
		return *newer, nil
	}
	return pgClient{}, fmt.Errorf("no pg_dump client >= server major %d is available "+
		"(a newer server needs a newer client)", serverMajor)
}

// checkSupportedPGMajor refuses a server major outside the supported range.
func checkSupportedPGMajor(major int) error {
	if major < minSupportedPGMajor || major > maxSupportedPGMajor {
		return fmt.Errorf("PostgreSQL server major %d is outside the supported range %d–%d",
			major, minSupportedPGMajor, maxSupportedPGMajor)
	}
	return nil
}

// postgresServerMajor queries the connected server's major version.
func postgresServerMajor(ctx context.Context, dsn string) (int, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	var num int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		return 0, err
	}
	return num / 10000, nil // e.g. 170002 → 17
}
