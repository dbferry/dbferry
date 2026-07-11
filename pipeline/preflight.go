package pipeline

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// preflightPostgres verifies the database is reachable and authenticates before
// the dump starts, so a connection problem is reported as KindConnect, distinct
// from a mid-dump failure (poc-plan 2.5). The DSN is used in process only and
// never reaches a child argv. This is the seed of the driver's testConnection
// (poc-plan 5.1).
func preflightPostgres(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return classify(KindConnect, "connect to database: %w", err)
	}
	defer conn.Close(ctx)
	if err := conn.Ping(ctx); err != nil {
		return classify(KindConnect, "ping database: %w", err)
	}
	return nil
}
