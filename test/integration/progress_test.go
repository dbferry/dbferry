//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/dbferry/dbferry/pipeline"
)

// TestProgressReported verifies the pipeline emits honest progress: at least one
// snapshot, ending in PhaseDone with non-zero dumped and uploaded byte counters
// (poc-plan 3.1).
func TestProgressReported(t *testing.T) {
	suffix := uniqueSuffix()
	srcDB := "it_prog_" + suffix

	admin := openPG(t, pg17DSN)
	loadPGFixture(t, admin, pg17DSN, srcDB)
	t.Cleanup(func() { admin.Exec(`DROP DATABASE IF EXISTS "` + srcDB + `" WITH (FORCE)`) })

	var mu sync.Mutex
	var snaps []pipeline.Progress
	_, err := pipeline.Run(context.Background(), pipeline.Config{
		DSN:          dsnWithDB(t, pg17DSN, srcDB),
		Dest:         "s3://" + s3Bucket + "/prog/" + suffix,
		AgeRecipient: ageRecipient(t),
		S3Endpoint:   s3Endpoint,
		Progress: func(p pipeline.Progress) {
			mu.Lock()
			snaps = append(snaps, p)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(snaps) == 0 {
		t.Fatal("no progress snapshots reported")
	}
	last := snaps[len(snaps)-1]
	if last.Phase != pipeline.PhaseDone {
		t.Errorf("final phase = %v, want done", last.Phase)
	}
	if last.DumpedBytes == 0 || last.UploadedBytes == 0 {
		t.Errorf("expected non-zero dumped/uploaded bytes, got %+v", last)
	}
}
