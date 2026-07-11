package pipeline

import "context"

// DatabaseInfo is one database found by discovery. Accessible reports whether
// the connecting role can actually back it up; an inaccessible database is
// reported (not silently dropped) so the operator knows a grant is missing
// (poc-plan 5.2).
type DatabaseInfo struct {
	Name       string `json:"name"`
	Accessible bool   `json:"accessible"`
}

// TestConnection verifies a database is reachable and authenticated, without
// running a backup (poc-plan 5.1). The engine is chosen from the DSN scheme.
func TestConnection(ctx context.Context, dsn string) error {
	drv, err := newDriver(dsn)
	if err != nil {
		return err
	}
	return drv.TestConnection(ctx)
}

// ListDatabases returns the user databases in the cluster the DSN points at,
// each flagged accessible or not — the foundation of dbferry's per-database
// value: one DSN, back up each database separately (poc-plan 5.2).
func ListDatabases(ctx context.Context, dsn string) ([]DatabaseInfo, error) {
	drv, err := newDriver(dsn)
	if err != nil {
		return nil, err
	}
	return drv.ListDatabases(ctx)
}
