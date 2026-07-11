package pipeline

import "time"

// Phase is the lifecycle stage of a backup run. The streaming pipeline runs
// dump, compress, encrypt and upload concurrently, so these are honest
// lifecycle transitions — not a fake sequential "50% compressing".
type Phase int32

const (
	PhaseConnecting Phase = iota // preflighting the database connection
	PhaseStreaming               // dump → zstd → age → S3 multipart, all flowing
	PhaseFinalizing              // completing the upload and writing the manifest
	PhaseDone                    // finished successfully
)

func (p Phase) String() string {
	switch p {
	case PhaseConnecting:
		return "connecting"
	case PhaseStreaming:
		return "streaming"
	case PhaseFinalizing:
		return "finalizing"
	case PhaseDone:
		return "done"
	default:
		return "unknown"
	}
}

// Progress is a snapshot of a running backup. DumpedBytes is what pg_dump has
// produced (pre-compression); UploadedBytes is the ciphertext confirmed in S3.
// Their ratio is the live compression+encryption factor — no dishonest percent
// is reported because the final dump size is unknown until pg_dump finishes.
type Progress struct {
	Phase         Phase
	DumpedBytes   int64
	UploadedBytes int64
	Elapsed       time.Duration
}
