package pipeline

import (
	"context"
	"net/url"
	"os/exec"
)

// DatabaseDriver abstracts a backup source so the pipeline is engine-agnostic
// (poc-plan 4.1). PostgreSQL and MySQL implement it; the same streaming pipeline
// and object-key/manifest contract apply to both.
type DatabaseDriver interface {
	// Engine is the object-key engine segment ("postgres" | "mysql").
	Engine() string
	// Cluster is the source-cluster label for the object key.
	Cluster() string
	// Database is the single database this run backs up.
	Database() string

	// TestConnection verifies reachability and authentication before dumping,
	// so a connection problem is a KindConnect error distinct from a dump one.
	TestConnection(ctx context.Context) error
	// Preflight runs engine-specific safety checks after the connection is
	// known good (e.g. MySQL non-InnoDB detection). It may emit warnings via
	// opts.Warn and returns a classified error to refuse an unsafe run.
	Preflight(ctx context.Context, opts DriverOptions) error
	// ListDatabases returns the user (non-system) databases in the cluster,
	// each flagged accessible or not, for discovery (poc-plan 5.2).
	ListDatabases(ctx context.Context) ([]DatabaseInfo, error)

	// BuildDumpCommand builds the streaming dump command. The connection
	// secret travels through the environment, never on argv.
	BuildDumpCommand(ctx context.Context) *exec.Cmd
	// BuildRestoreCommand returns the documented manual restore command for a
	// target database (poc-plan 1.4/4.3).
	BuildRestoreCommand(targetDB string) []string

	// DumpFormat and DumpClientVersion describe the dump for the manifest.
	DumpFormat() string
	DumpClientVersion(ctx context.Context) string

	// Diagnose returns engine-specific health checks (dump-client availability
	// and version compatibility) for `dbferry doctor` (poc-plan 0.5.5).
	Diagnose(ctx context.Context) []Check
}

// DriverOptions carries run-time knobs into engine-specific preflight.
type DriverOptions struct {
	// AllowNonTransactional permits backing up non-transactional tables
	// (e.g. MyISAM) that --single-transaction can't snapshot consistently.
	AllowNonTransactional bool
	// Warn, if set, receives non-fatal warnings (e.g. a possibly-inconsistent
	// non-InnoDB dump proceeding under AllowNonTransactional).
	Warn func(string)
}

// newDriver selects a driver from the DSN scheme.
func newDriver(dsn string) (DatabaseDriver, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, classify(KindConnect, "pipeline: parse DSN: invalid URL")
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		return newPostgresDriver(dsn)
	case "mysql":
		return newMySQLDriver(dsn)
	default:
		return nil, classify(KindConnect, "pipeline: unsupported DSN scheme %q (want postgres:// or mysql://)", u.Scheme)
	}
}
