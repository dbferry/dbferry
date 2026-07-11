// Package pipeline streams a single database backup to object storage as
// dump | zstd | age | S3 multipart, with no intermediate disk. It is the core
// of the dbferry CLI and is designed to be invoked unchanged from River jobs in
// the cloud service (ADR-0001), so it must stay decoupled from any control
// plane: no globals, no logging of secrets, everything through Config and ctx.
package pipeline

import (
	"context"
	"errors"
)

// Config describes one backup run. DSN is a resolved secret: it must never be
// logged, printed, or placed on a child process argv.
type Config struct {
	// DSN is the fully-resolved database connection string.
	DSN string
	// Dest is the destination URL, e.g. s3://bucket/prefix.
	Dest string
	// AgeRecipient is the age public recipient the backup is encrypted to
	// (BYOK: we never hold the private key — see DECISIONS.md).
	AgeRecipient string
}

// ErrNotImplemented is returned by the skeleton pipeline until the streaming
// stages land (poc-plan 1.2).
var ErrNotImplemented = errors.New("pipeline: not implemented")

// Run executes a single backup end to end. It must honour ctx cancellation:
// cancelling kills the dump child process, stops all goroutines, and triggers a
// best-effort abort of the multipart upload (poc-plan 2.1).
func Run(ctx context.Context, cfg Config) error {
	return ErrNotImplemented
}
