// Package pipeline streams a single database backup to object storage as
// dump | zstd | age | S3 multipart, with no intermediate disk. It is the core
// of the dbferry CLI and is designed to be invoked unchanged from River jobs in
// the cloud service (ADR-0001), so it must stay decoupled from any control
// plane: no globals, no logging of secrets, everything through Config and ctx.
package pipeline

import (
	"context"
	"fmt"
	"io"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

// Defaults for the S3 multipart upload, fixed in DECISIONS.md (2026-07-11):
// part size 32 MiB × concurrency 4 keeps in-flight buffers around 128 MiB, half
// of the 256 MiB RSS budget. The 10k-part S3 limit then caps one object at
// ~312 GiB; hitting it is a controlled error before Complete, never a corrupt
// backup.
const (
	DefaultPartSize    = 32 << 20 // 32 MiB
	DefaultConcurrency = 4
	maxUploadParts     = 10000 // hard S3 limit
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
	// S3Endpoint, when non-empty, points at an S3-compatible endpoint (e.g.
	// MinIO) and switches the client to path-style addressing. Empty means
	// real AWS S3. Credentials and region come from the standard AWS config
	// chain (env vars, shared config); no secret is taken on argv.
	S3Endpoint string
	// PartSize and Concurrency override the fixed defaults above; zero uses
	// the default.
	PartSize    int64
	Concurrency int
}

// Result summarises a completed backup. It carries no secrets and is safe to
// print.
type Result struct {
	// BackupID is the unique id (UTC timestamp + ULID) embedded in the key.
	BackupID string
	// Bucket and Key locate the ciphertext object per the versioned key
	// schema (DECISIONS.md); the manifest sibling lands in poc-plan 1.5.
	Bucket string
	Key    string
	// Bytes is the number of ciphertext bytes uploaded.
	Bytes int64
}

// Run executes a single backup end to end: it streams pg_dump output through
// zstd and age into an S3 multipart upload, composed as readers with no
// intermediate disk. The upload only completes once the dump has exited
// successfully; any failure upstream aborts the multipart upload and leaves no
// final object (poc-plan 1.2).
//
// It honours ctx cancellation: cancelling kills the dump child process, stops
// the goroutines, and lets the upload abort (hardened further in poc-plan 2.1).
func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.PartSize == 0 {
		cfg.PartSize = DefaultPartSize
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = DefaultConcurrency
	}

	// Derive a cancellable context so that returning from Run — on success or,
	// crucially, on an upload error mid-stream — tears down pg_dump (started
	// with exec.CommandContext) instead of leaving it blocked writing to a
	// stdout pipe nobody is draining. Full cancellation handling is poc-plan
	// 2.1; this just prevents a leak.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	src, err := parsePostgresDSN(cfg.DSN)
	if err != nil {
		return Result{}, err
	}
	dst, err := parseDest(cfg.Dest)
	if err != nil {
		return Result{}, err
	}
	recipient, err := age.ParseX25519Recipient(cfg.AgeRecipient)
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: parse age recipient: %w", err)
	}

	now := time.Now().UTC()
	backupID, err := newBackupID(now)
	if err != nil {
		return Result{}, err
	}
	key := dst.objectKey("postgres", src.cluster(), src.database, now, backupID)

	uploader, err := newUploader(ctx, cfg)
	if err != nil {
		return Result{}, err
	}

	// pr/pw bridge the push side (pg_dump writing) to the pull side (the
	// uploader reading Body). The feed goroutine writes dump→zstd→age into pw;
	// the uploader reads ciphertext from pr.
	pr, pw := io.Pipe()

	cmd := src.dumpCommand(ctx)
	stderr := newCappedBuffer(8 << 10) // keep the tail of pg_dump's stderr
	cmd.Stderr = stderr
	dumpOut, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: pg_dump stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("pipeline: start pg_dump: %w", err)
	}

	go feed(pw, dumpOut, cmd, stderr, recipient)

	out, err := uploader.upload(ctx, dst.bucket, key, pr)
	if err != nil {
		// Drain the pipe so the feed goroutine cannot block on a write; the
		// upload has already aborted the multipart upload, so no final object
		// exists.
		pr.CloseWithError(err)
		return Result{}, err
	}

	return Result{BackupID: backupID, Bucket: dst.bucket, Key: key, Bytes: out.bytes}, nil
}

// feed streams pg_dump's stdout through zstd and age into pw. It only closes pw
// cleanly (signalling EOF, which lets the upload complete) once the dump has
// exited successfully; every failure path closes pw with an error so the
// uploader aborts instead of completing a truncated backup.
func feed(pw *io.PipeWriter, dumpOut io.Reader, cmd interface{ Wait() error }, stderr *cappedBuffer, recipient age.Recipient) {
	ageW, err := age.Encrypt(pw, recipient)
	if err != nil {
		pw.CloseWithError(fmt.Errorf("pipeline: start age: %w", err))
		return
	}
	zw, err := zstd.NewWriter(ageW)
	if err != nil {
		pw.CloseWithError(fmt.Errorf("pipeline: start zstd: %w", err))
		return
	}

	_, copyErr := io.Copy(zw, dumpOut)
	// Reap the child only after stdout is fully drained (StdoutPipe requires
	// Wait to follow the last read).
	waitErr := cmd.Wait()

	switch {
	case copyErr != nil:
		pw.CloseWithError(fmt.Errorf("pipeline: streaming pg_dump output: %w", copyErr))
	case waitErr != nil:
		pw.CloseWithError(dumpFailure(waitErr, stderr.String()))
	default:
		// Finalize compression then encryption; only then signal clean EOF.
		if err := zw.Close(); err != nil {
			pw.CloseWithError(fmt.Errorf("pipeline: finalize zstd: %w", err))
			return
		}
		if err := ageW.Close(); err != nil {
			pw.CloseWithError(fmt.Errorf("pipeline: finalize age: %w", err))
			return
		}
		pw.Close()
		return
	}
	// On any error path, also release compression resources.
	zw.Close()
}
