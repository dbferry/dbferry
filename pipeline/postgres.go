package pipeline

import (
	"context"
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
