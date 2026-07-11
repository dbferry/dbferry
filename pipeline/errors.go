package pipeline

import (
	"errors"
	"fmt"
)

// Kind classifies where a backup failed, so the CLI can pick a distinct exit
// code (poc-plan 2.5) and callers can react programmatically.
type Kind int

const (
	// KindUnknown is an unclassified failure.
	KindUnknown Kind = iota
	// KindConnect: could not reach or authenticate to the source database.
	KindConnect
	// KindDump: the dump tool (pg_dump) failed.
	KindDump
	// KindUpload: object storage / the multipart upload failed.
	KindUpload
	// KindCanceled: the run was cancelled (e.g. Ctrl+C).
	KindCanceled
)

func (k Kind) String() string {
	switch k {
	case KindConnect:
		return "connect"
	case KindDump:
		return "dump"
	case KindUpload:
		return "upload"
	case KindCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Error is a classified pipeline error.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// classify wraps an error with a Kind. If err already carries a Kind it is kept,
// so the most specific classification (set closest to the cause) wins.
func classify(kind Kind, format string, args ...any) *Error {
	err := fmt.Errorf(format, args...)
	var existing *Error
	if errors.As(err, &existing) {
		return existing
	}
	return &Error{Kind: kind, Err: err}
}

// KindOf returns the Kind of an error, or KindUnknown.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindUnknown
}
