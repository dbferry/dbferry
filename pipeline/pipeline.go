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
	"sync"
	"sync/atomic"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

// Defaults for the S3 multipart upload, fixed in ADR-0005:
// part size 32 MiB × concurrency 4 keeps in-flight buffers around 128 MiB, half
// of the 256 MiB RSS budget. The 10k-part S3 limit then caps one object at
// ~312 GiB; hitting it is a controlled error before Complete, never a corrupt
// backup.
const (
	DefaultPartSize    = 32 << 20 // 32 MiB
	DefaultConcurrency = 4
	maxUploadParts     = 10000 // hard S3 limit

	// DefaultMaxRetries bounds SDK retries of a transient S3 operation
	// (poc-plan 2.3). DefaultPartTimeout caps a single UploadPart.
	DefaultMaxRetries  = 5
	DefaultPartTimeout = 5 * time.Minute
)

// Config describes one backup run. DSN is a resolved secret: it must never be
// logged, printed, or placed on a child process argv.
type Config struct {
	// DSN is the fully-resolved database connection string.
	DSN string
	// Dest is the destination URL, e.g. s3://bucket/prefix.
	Dest string
	// AgeRecipient is the age public recipient the backup is encrypted to
	// (BYOK: we never hold the private key — see ADR-0005).
	AgeRecipient string
	// S3Endpoint, when non-empty, points at an S3-compatible endpoint (e.g.
	// MinIO) and switches the client to path-style addressing. Empty means
	// real AWS S3.
	S3Endpoint string
	// S3Region, S3Profile and S3Credentials override how the S3 client
	// authenticates. All are optional: with none set, credentials and region
	// come from the standard AWS chain (env vars, shared config). No secret is
	// taken on argv.
	S3Region      string
	S3Profile     string
	S3Credentials *S3Credentials
	// PartSize and Concurrency override the fixed defaults above; zero uses
	// the default.
	PartSize    int64
	Concurrency int
	// MaxRetries and PartTimeout tune S3 resilience; zero uses the defaults.
	MaxRetries  int
	PartTimeout time.Duration
	// AppVersion is the dbferry build version, recorded in the manifest. It is
	// informational and may be empty.
	AppVersion string
	// AllowNonTransactional permits backing up non-transactional tables (MySQL
	// MyISAM etc.) that can't be snapshotted consistently; without it such a
	// database is refused rather than backed up with a silent inconsistency.
	AllowNonTransactional bool
	// Warn, if set, receives non-fatal warnings (e.g. a non-InnoDB dump
	// proceeding under AllowNonTransactional).
	Warn func(string)
	// Progress, if set, is called periodically with a snapshot of the run. It
	// must be cheap and non-blocking; the pipeline invokes it from one goroutine.
	// The CLI renders it as live progress; the cloud service can persist it.
	Progress func(Progress)
}

// S3Credentials is a static credential set (e.g. from a named destination or
// STS). SessionToken is empty for long-lived keys.
type S3Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Result summarises a completed backup. It carries no secrets and is safe to
// print.
type Result struct {
	// BackupID is the unique id (UTC timestamp + ULID) embedded in the key.
	BackupID string
	// Bucket and Key locate the ciphertext object per the versioned key
	// schema (ADR-0005); ManifestKey is its .manifest.json sibling.
	Bucket      string
	Key         string
	ManifestKey string
	// Bytes and SHA256 are the size and hex SHA-256 of the ciphertext, as
	// recorded in the manifest.
	Bytes  int64
	SHA256 string
}

// Run executes a single backup end to end: it streams pg_dump output through
// zstd and age into an S3 multipart upload, composed as readers with no
// intermediate disk. The upload only completes once the dump has exited
// successfully; any failure upstream aborts the multipart upload and leaves no
// final object (poc-plan 1.2).
//
// It honours ctx cancellation: cancelling kills the dump child process, stops
// the goroutines, and lets the upload abort (hardened further in poc-plan 2.1).
func Run(ctx context.Context, cfg Config) (res Result, err error) {
	if cfg.PartSize == 0 {
		cfg.PartSize = DefaultPartSize
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.PartTimeout == 0 {
		cfg.PartTimeout = DefaultPartTimeout
	}

	// Derive a cancellable context so that returning from Run — on success or on
	// a failure mid-stream — tears down pg_dump (started with exec.CommandContext)
	// instead of leaving it blocked writing to a stdout pipe nobody drains.
	// Cancelling the parent (Ctrl+C) kills pg_dump, unblocks the feed goroutine,
	// and makes the upload abort (poc-plan 2.1).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// If the parent context was cancelled (e.g. Ctrl+C), classify whatever error
	// surfaced first as KindCanceled — regardless of whether it appeared in the
	// preflight, the dump, or the upload. This defer runs before `cancel` above
	// (LIFO), so ctx.Err() reflects only the parent's cancellation here.
	defer func() {
		if err != nil && ctx.Err() != nil && KindOf(err) != KindCanceled {
			err = &Error{Kind: KindCanceled, Err: fmt.Errorf("pipeline: backup canceled: %w", err)}
		}
	}()

	// Live progress: counters updated by the feed goroutine (bytes dumped) and
	// the uploader (ciphertext bytes), sampled by a ticker into cfg.Progress.
	var dumpedBytes, uploadedBytes atomic.Int64
	var phase atomic.Int32
	phase.Store(int32(PhaseConnecting))
	started := time.Now()
	snapshot := func() Progress {
		return Progress{
			Phase:         Phase(phase.Load()),
			DumpedBytes:   dumpedBytes.Load(),
			UploadedBytes: uploadedBytes.Load(),
			Elapsed:       time.Since(started),
		}
	}
	if cfg.Progress != nil {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(150 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					cfg.Progress(snapshot()) // final snapshot, single-goroutine
					return
				case <-t.C:
					cfg.Progress(snapshot())
				}
			}
		}()
		// Runs before `cancel` (LIFO) and blocks until the reporter goroutine
		// has emitted its last snapshot, so no Progress call races the caller
		// after Run returns.
		defer func() { close(stop); wg.Wait() }()
	}

	// Select the engine driver from the DSN scheme (postgres:// or mysql://).
	drv, err := newDriver(cfg.DSN)
	if err != nil {
		return Result{}, err
	}

	// Preflight: verify the connection (KindConnect if unreachable/unauthorized,
	// distinct from a mid-dump failure), then run engine-specific safety checks
	// (e.g. MySQL non-InnoDB detection) which may warn or refuse.
	if err := drv.TestConnection(ctx); err != nil {
		return Result{}, err
	}
	if err := drv.Preflight(ctx, DriverOptions{
		AllowNonTransactional: cfg.AllowNonTransactional,
		Warn:                  cfg.Warn,
	}); err != nil {
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
	key := dst.objectKey(drv.Engine(), drv.Cluster(), drv.Database(), now, backupID)

	uploader, err := newUploader(ctx, cfg)
	if err != nil {
		return Result{}, err
	}

	// pr/pw bridge the push side (dump writing) to the pull side (the uploader
	// reading Body). The feed goroutine writes dump→zstd→age into pw; the
	// uploader reads ciphertext from pr.
	pr, pw := io.Pipe()

	cmd := drv.BuildDumpCommand(ctx)
	stderr := newCappedBuffer(8 << 10) // keep the tail of the dump tool's stderr
	cmd.Stderr = stderr
	dumpOut, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: dump stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		// e.g. the dump tool not on PATH — a dump-stage failure with a fix.
		return Result{}, classify(KindDump, "pipeline: start %s: %w", cmd.Args[0], err)
	}

	go feed(pw, countReader{dumpOut, &dumpedBytes}, cmd, stderr, recipient)

	phase.Store(int32(PhaseStreaming))
	out, err := uploader.upload(ctx, dst.bucket, key, pr, &uploadedBytes)
	if err != nil {
		// Drain the pipe so the feed goroutine cannot block on a write; the
		// upload has already aborted the multipart upload, so no final object
		// exists. Cancellation is classified by the deferred check above.
		pr.CloseWithError(err)
		return Result{}, err
	}

	phase.Store(int32(PhaseFinalizing))

	// The object is now complete. Write the manifest that makes it a valid
	// backup — only now, never before (ADR-0005 invariant). If this fails
	// the object is an orphan without a manifest: Run must NOT report success,
	// leaving it for reconciliation/cleanup (poc-plan 2.4).
	mkey := manifestKey(key)
	m := Manifest{
		KeySchema:        keySchemaVersion,
		BackupID:         backupID,
		CreatedAt:        manifestCreatedAt(now),
		Engine:           drv.Engine(),
		Cluster:          drv.Cluster(),
		Database:         drv.Database(),
		Object:           key,
		Format:           drv.DumpFormat(),
		DumpClient:       drv.DumpClientVersion(ctx),
		DbferryVersion:   cfg.AppVersion,
		CiphertextBytes:  out.bytes,
		CiphertextSHA256: out.sha256,
		PartSize:         cfg.PartSize,
		Concurrency:      cfg.Concurrency,
	}
	body, err := m.marshal()
	if err != nil {
		return Result{}, err
	}
	if err := uploader.putObject(ctx, dst.bucket, mkey, body, "application/json"); err != nil {
		return Result{}, classify(KindUpload, "pipeline: backup object uploaded but manifest write failed; "+
			"the backup is not valid and needs reconciliation (%s): %w", key, err)
	}

	phase.Store(int32(PhaseDone))

	return Result{
		BackupID:    backupID,
		Bucket:      dst.bucket,
		Key:         key,
		ManifestKey: mkey,
		Bytes:       out.bytes,
		SHA256:      out.sha256,
	}, nil
}

// feed streams pg_dump's stdout through zstd and age into pw. It only closes pw
// cleanly (signalling EOF, which lets the upload complete) once the dump has
// exited successfully; every failure path closes pw with an error so the
// uploader aborts instead of completing a truncated backup.
func feed(pw *io.PipeWriter, dumpOut io.Reader, cmd interface{ Wait() error }, stderr *cappedBuffer, recipient age.Recipient) {
	ageW, err := age.Encrypt(pw, recipient)
	if err != nil {
		pw.CloseWithError(classify(KindDump, "pipeline: start age: %w", err))
		return
	}
	// Single-goroutine encoder: one compression window instead of GOMAXPROCS,
	// keeping the memory footprint predictable (poc-plan 6.3).
	zw, err := zstd.NewWriter(ageW, zstd.WithEncoderConcurrency(1))
	if err != nil {
		pw.CloseWithError(classify(KindDump, "pipeline: start zstd: %w", err))
		return
	}

	_, copyErr := io.Copy(zw, dumpOut)
	// Reap the child only after stdout is fully drained (StdoutPipe requires
	// Wait to follow the last read).
	waitErr := cmd.Wait()

	switch {
	case copyErr != nil:
		pw.CloseWithError(classify(KindDump, "pipeline: streaming pg_dump output: %w", copyErr))
	case waitErr != nil:
		pw.CloseWithError(dumpFailure(waitErr, stderr.String()))
	default:
		// Finalize compression then encryption; only then signal clean EOF.
		if err := zw.Close(); err != nil {
			pw.CloseWithError(classify(KindDump, "pipeline: finalize zstd: %w", err))
			return
		}
		if err := ageW.Close(); err != nil {
			pw.CloseWithError(classify(KindDump, "pipeline: finalize age: %w", err))
			return
		}
		pw.Close()
		return
	}
	// On any error path, also release compression resources.
	zw.Close()
}
