package pipeline

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/jackc/pgx/v5"
)

// postgresDriver backs up a PostgreSQL database with pg_dump. It reuses the
// libpq-env dump command (secrets off argv) from dump.go. Preflight selects the
// pg_dump client that matches the server major (poc-plan 5.3); pgDumpPath is
// empty until then and BuildDumpCommand falls back to "pg_dump" on PATH.
type postgresDriver struct {
	dsn        string
	src        pgSource
	pgDumpPath string
}

func newPostgresDriver(dsn string) (*postgresDriver, error) {
	src, err := parsePostgresDSN(dsn)
	if err != nil {
		return nil, err
	}
	return &postgresDriver{dsn: dsn, src: src}, nil
}

func (d *postgresDriver) Engine() string   { return "postgres" }
func (d *postgresDriver) Cluster() string  { return d.src.cluster() }
func (d *postgresDriver) Database() string { return d.src.database }

func (d *postgresDriver) TestConnection(ctx context.Context) error {
	return preflightPostgres(ctx, d.dsn)
}

// Preflight refuses a server outside the supported major range and selects a
// compatible pg_dump client, all before any multipart upload is created.
func (d *postgresDriver) Preflight(ctx context.Context, opts DriverOptions) error {
	major, err := postgresServerMajor(ctx, d.dsn)
	if err != nil {
		return classify(KindConnect, "pipeline: read server version: %w", err)
	}
	if err := checkSupportedPGMajor(major); err != nil {
		return classify(KindDump, "pipeline: %w", err)
	}
	client, err := selectPgDump(discoverPgDumpClients(ctx), major)
	if err != nil {
		return classify(KindDump, "pipeline: %w", err)
	}
	d.pgDumpPath = client.path
	return nil
}

func (d *postgresDriver) pgDump() string {
	if d.pgDumpPath != "" {
		return d.pgDumpPath
	}
	return "pg_dump"
}

func (d *postgresDriver) ListDatabases(ctx context.Context) ([]DatabaseInfo, error) {
	conn, err := pgx.Connect(ctx, d.dsn)
	if err != nil {
		return nil, classify(KindConnect, "pipeline: connect for discovery: %w", err)
	}
	defer conn.Close(ctx)
	// Templates and the default 'postgres' maintenance DB are system databases
	// and filtered out. Accessibility folds datallowconn with the role's CONNECT
	// privilege so a listed-but-unreachable database is flagged, not dropped.
	rows, err := conn.Query(ctx,
		`SELECT datname, datallowconn AND has_database_privilege(datname, 'CONNECT')
		 FROM pg_database
		 WHERE NOT datistemplate AND datname <> 'postgres'
		 ORDER BY datname`)
	if err != nil {
		return nil, classify(KindConnect, "pipeline: list databases: %w", err)
	}
	defer rows.Close()
	var dbs []DatabaseInfo
	for rows.Next() {
		var info DatabaseInfo
		if err := rows.Scan(&info.Name, &info.Accessible); err != nil {
			return nil, err
		}
		dbs = append(dbs, info)
	}
	return dbs, rows.Err()
}

func (d *postgresDriver) BuildDumpCommand(ctx context.Context) *exec.Cmd {
	return d.src.dumpCommandWith(ctx, d.pgDump())
}

func (d *postgresDriver) BuildRestoreCommand(targetDB string) []string {
	return []string{"pg_restore", "-d", targetDB, "--no-owner"}
}

func (d *postgresDriver) DumpFormat() string { return "pg_dump -Fc -Z0 | zstd | age" }

func (d *postgresDriver) DumpClientVersion(ctx context.Context) string {
	return pgDumpVersionOf(ctx, d.pgDump())
}

// checkReadAccess reports what the current role cannot read and pg_dump
// would therefore fail on: tables, and large objects — whose per-object
// ACLs no role-level grant (pg_read_all_data included) covers.
func (d *postgresDriver) checkReadAccess(ctx context.Context) []Check {
	conn, err := pgx.Connect(ctx, d.dsn)
	if err != nil {
		return []Check{{Name: "table read access", Status: StatusFail, Detail: err.Error()}}
	}
	defer conn.Close(ctx)

	checks := []Check{}

	// Tables need SELECT; sequences need SELECT or USAGE (pg_dump reads
	// their state with a plain SELECT, which either privilege allows).
	var unreadable int
	var sample string
	err = conn.QueryRow(ctx, `
		SELECT count(*),
		       coalesce(min(n.nspname || '.' || c.relname), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND NOT n.nspname LIKE 'pg_toast%'
		  AND ((c.relkind IN ('r', 'p', 'm') AND NOT has_table_privilege(c.oid, 'SELECT'))
		    OR (c.relkind = 'S' AND NOT has_sequence_privilege(c.oid, 'SELECT')
		                        AND NOT has_sequence_privilege(c.oid, 'USAGE')))`).Scan(&unreadable, &sample)
	switch {
	case err != nil:
		checks = append(checks, Check{Name: "table read access", Status: StatusWarn,
			Detail: "could not verify per-table privileges: " + err.Error()})
	case unreadable > 0:
		checks = append(checks, Check{Name: "table read access", Status: StatusFail,
			Detail: fmt.Sprintf("%d table(s)/sequence(s) not readable by this role (e.g. %s) — pg_dump will fail on them", unreadable, sample),
			Fix:    "grant SELECT on those tables/sequences (or grant the pg_read_all_data role)"})
	default:
		checks = append(checks, Check{Name: "table read access", Status: StatusOK, Detail: "every table and sequence is readable"})
	}

	// Large objects are dumped by default and read through their own ACLs:
	// unreadable means owned by another role with no SELECT granted to this
	// one (directly or via PUBLIC). NULL lomacl is the default owner-only ACL.
	var loCount int
	var loSample string
	err = conn.QueryRow(ctx, `
		SELECT count(*), coalesce(min(m.oid)::text, '')
		FROM pg_largeobject_metadata m
		WHERE NOT pg_has_role(m.lomowner, 'USAGE')
		  AND (m.lomacl IS NULL OR NOT EXISTS (
		        SELECT 1 FROM aclexplode(m.lomacl) a
		        WHERE a.privilege_type = 'SELECT'
		          AND (a.grantee = 0 OR pg_has_role(a.grantee, 'USAGE'))))`).Scan(&loCount, &loSample)
	switch {
	case err != nil:
		checks = append(checks, Check{Name: "large object read access", Status: StatusWarn,
			Detail: "could not verify large object privileges: " + err.Error()})
	case loCount > 0:
		checks = append(checks, Check{Name: "large object read access", Status: StatusFail,
			Detail: fmt.Sprintf("%d large object(s) not readable by this role (e.g. oid %s) — pg_dump includes large objects and will fail on them", loCount, loSample),
			Fix:    "GRANT SELECT ON LARGE OBJECT <oid> TO the role per object (PostgreSQL has no bulk large-object grant), or change their owner"})
	}
	return checks
}

func (d *postgresDriver) Diagnose(ctx context.Context) []Check {
	major, err := postgresServerMajor(ctx, d.dsn)
	if err != nil {
		return []Check{{Name: "server version", Status: StatusFail, Detail: err.Error()}}
	}
	if err := checkSupportedPGMajor(major); err != nil {
		return []Check{{Name: "server version", Status: StatusFail,
			Detail: fmt.Sprintf("PostgreSQL %d", major),
			Fix:    fmt.Sprintf("supported range is %d–%d; upgrade dbferry or use a supported server", minSupportedPGMajor, maxSupportedPGMajor)}}
	}
	checks := []Check{{Name: "server version", Status: StatusOK, Detail: fmt.Sprintf("PostgreSQL %d (supported)", major)}}

	client, err := selectPgDump(discoverPgDumpClients(ctx), major)
	switch {
	case err != nil:
		checks = append(checks, Check{Name: "pg_dump client", Status: StatusFail, Detail: err.Error(),
			Fix: fmt.Sprintf("install postgresql-client-%d (or newer)", major)})
	case client.major != major:
		checks = append(checks, Check{Name: "pg_dump client", Status: StatusWarn,
			Detail: fmt.Sprintf("pg_dump %d selected for server %d (works, but not an exact match)", client.major, major),
			Fix:    fmt.Sprintf("install postgresql-client-%d for a matched, cleaner dump", major)})
	default:
		checks = append(checks, Check{Name: "pg_dump client", Status: StatusOK,
			Detail: fmt.Sprintf("pg_dump %d (matches server)", client.major)})
	}

	// Dump-level permissions: pg_dump reads EVERY table and large object of
	// the connected database — a connect-only role passes the checks above
	// and then fails mid-dump. Count what the role cannot read so the
	// doctor says it now.
	checks = append(checks, d.checkReadAccess(ctx)...)
	return checks
}
