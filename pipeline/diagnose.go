package pipeline

import (
	"context"
	"fmt"
)

// CheckStatus is a diagnostic outcome.
type CheckStatus int

const (
	StatusOK CheckStatus = iota
	StatusWarn
	StatusFail
)

func (s CheckStatus) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	default:
		return "unknown"
	}
}

// Check is one typed diagnostic result. Fix is a concrete next step (empty when
// OK). This is the stable surface `dbferry doctor` renders — it does not reach
// into pipeline internals (poc-plan 0.5.5).
type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
	Fix    string      `json:"fix,omitempty"`
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

// DiagnoseSource checks a database source: reachability, engine-specific dump
// client compatibility, and read/discovery access.
func DiagnoseSource(ctx context.Context, dsn string) []Check {
	drv, err := newDriver(dsn)
	if err != nil {
		return []Check{{Name: "dsn", Status: StatusFail, Detail: err.Error(),
			Fix: "use a postgres:// or mysql:// DSN"}}
	}
	if err := drv.TestConnection(ctx); err != nil {
		return []Check{{Name: "connect", Status: StatusFail, Detail: err.Error(),
			Fix: "check host/port/database/credentials and that the role may CONNECT"}}
	}
	checks := []Check{{Name: "connect", Status: StatusOK, Detail: "reachable and authenticated"}}
	checks = append(checks, drv.Diagnose(ctx)...)
	if dbs, err := drv.ListDatabases(ctx); err != nil {
		checks = append(checks, Check{Name: "read access", Status: StatusWarn, Detail: err.Error(),
			Fix: "grant catalog read access to the role"})
	} else {
		checks = append(checks, Check{Name: "discovery", Status: StatusOK,
			Detail: fmt.Sprintf("%d user database(s) visible", len(dbs))})
	}
	return checks
}

// DiagnoseDestination checks a destination by probing write/read/delete.
func DiagnoseDestination(ctx context.Context, cfg Config) []Check {
	p := ProbeDestination(ctx, cfg)
	if !p.Write {
		return []Check{{Name: "write", Status: StatusFail, Detail: errStr(p.WriteErr),
			Fix: "grant s3:CreateMultipartUpload/UploadPart/PutObject and check the bucket, credentials and endpoint"}}
	}
	checks := []Check{{Name: "write", Status: StatusOK, Detail: "PutObject ok"}}
	if p.Read {
		checks = append(checks, Check{Name: "read", Status: StatusOK, Detail: "HeadObject ok"})
	} else {
		checks = append(checks, Check{Name: "read", Status: StatusWarn, Detail: errStr(p.ReadErr),
			Fix: "grant s3:GetObject/HeadObject (recommended for verification)"})
	}
	if p.Delete {
		checks = append(checks, Check{Name: "delete", Status: StatusOK, Detail: "DeleteObject ok"})
	} else {
		checks = append(checks, Check{Name: "delete", Status: StatusWarn,
			Detail: "append-only (no DeleteObject) — fine for backups",
			Fix:    "retention will need s3:DeleteObject; probe object left at " + p.Leftover})
	}
	return checks
}
