package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBackupStateString(t *testing.T) {
	want := map[BackupState]string{
		BackupValid:             "valid",
		BackupOrphan:            "orphan",
		BackupDanglingManifest:  "dangling-manifest",
		BackupCorruptManifest:   "corrupt-manifest",
		BackupUnsupportedSchema: "unsupported-schema",
		BackupState(99):         "unknown",
	}
	for s, str := range want {
		if got := s.String(); got != str {
			t.Errorf("BackupState(%d).String() = %q, want %q", s, got, str)
		}
	}
}

func TestListingValid(t *testing.T) {
	l := Listing{Backups: []BackupInfo{
		{Key: "a", State: BackupValid},
		{Key: "b", State: BackupOrphan},
		{Key: "c", State: BackupValid},
		{Key: "d", State: BackupCorruptManifest},
	}}
	got := l.Valid()
	if len(got) != 2 || got[0].Key != "a" || got[1].Key != "c" {
		t.Fatalf("Valid() = %+v", got)
	}
}

func TestFirstErr(t *testing.T) {
	e := errors.New("x")
	if firstErr(nil, e) != e || firstErr(e, nil) != e {
		t.Error("firstErr must return the first non-nil error")
	}
	if firstErr(nil, nil) == nil {
		t.Error("firstErr with no errors must still return a non-nil placeholder")
	}
}

func TestScopeOwnerNilMatchesEverything(t *testing.T) {
	var o *scopeOwner
	if !o.matches(&Manifest{Database: "anything"}) {
		t.Error("nil owner must not constrain (direct listBackups callers opt out)")
	}
}

func TestErrorUnwrap(t *testing.T) {
	inner := errors.New("cause")
	wrapped := classify(KindDump, "context: %w", inner)
	if !errors.Is(wrapped, inner) {
		t.Error("classified errors must unwrap to their cause")
	}
}

func TestDiscoveryRejectsBadDSN(t *testing.T) {
	ctx := context.Background()
	if err := TestConnection(ctx, "ftp://nope/db"); err == nil {
		t.Error("TestConnection with a non-database scheme must fail")
	}
	if _, err := ListDatabases(ctx, "ftp://nope/db"); err == nil {
		t.Error("ListDatabases with a non-database scheme must fail")
	}
}

func TestParseDestInvalidURL(t *testing.T) {
	if _, err := parseDest("s3://bucket/%zz\x7f"); err == nil {
		// A control byte makes url.Parse fail outright.
		t.Error("unparseable dest URL must be rejected")
	}
}

// Config-level entry points must fail fast on unusable configuration before
// touching the network.
func TestEntryPointsRejectBadConfig(t *testing.T) {
	ctx := context.Background()
	bad := Config{DSN: "ftp://nope/db", Dest: "s3://bucket"}

	if _, err := ListBackups(ctx, bad); err == nil {
		t.Error("ListBackups with a non-database DSN must fail")
	}
	if _, err := Prune(ctx, bad, RetentionPolicy{KeepDaily: 1}, PruneOptions{}); err == nil {
		t.Error("Prune with a non-database DSN must fail")
	}
	if err := DeleteBackups(ctx, bad, nil); err == nil {
		t.Error("DeleteBackups with a non-database DSN must fail")
	}
	if _, err := Run(ctx, bad); err == nil {
		t.Error("Run with a non-database DSN must fail")
	}

	badDest := Config{DSN: "postgres://u@h:5432/db", Dest: "http://not-s3/x"}
	if _, err := ListBackups(ctx, badDest); err == nil {
		t.Error("ListBackups with a non-s3 dest must fail")
	}

	badPolicy := Config{DSN: "postgres://u@h:5432/db", Dest: "s3://bucket"}
	if _, err := Prune(ctx, badPolicy, RetentionPolicy{KeepDaily: -1}, PruneOptions{}); err == nil {
		t.Error("Prune with an invalid retention policy must fail before listing")
	}
}

// A transport error while validating a dangling manifest must fail the pass
// (so the caller retries) rather than silently skipping or deleting.
func TestPruneDanglingTransportErrorFailsPass(t *testing.T) {
	f := newFakeObjectStore()
	// Only a dangling manifest — no paired backup, so the sole GetObject in
	// the pass is the dangling validation read.
	f.objects[testScope+"2026/07/20260710T020000Z-GONE"+manifestSuffix] = []byte(`{"key_schema":1}`)
	f.onGet = errors.New("connection reset")

	_, err := pruneWith(context.Background(), f, "bucket", testScope, nil, RetentionPolicy{KeepDaily: 1}, PruneOptions{})
	if err == nil || !strings.Contains(err.Error(), "dangling manifest") {
		t.Fatalf("transport error must surface, got: %v", err)
	}
}

// An oversized object with a manifest key suffix is not one of ours and must
// never become deletable via the dangling path.
func TestPruneDanglingOversizedManifestSkipped(t *testing.T) {
	f := newFakeObjectStore()
	f.putBackup(testScope, mustTime(t, "2026-07-15T02:00:00Z"), "a")
	huge := testScope + "2026/07/20260710T020000Z-HUGE" + manifestSuffix
	body := append([]byte(`{"key_schema":1}`), make([]byte, maxManifestBytes)...)
	f.objects[huge] = body

	res, err := pruneWith(context.Background(), f, "bucket", testScope, nil, RetentionPolicy{KeepDaily: 1}, PruneOptions{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(res.DanglingDeleted) != 0 {
		t.Fatalf("oversized manifest was deleted: %v", res.DanglingDeleted)
	}
	if _, ok := f.objects[huge]; !ok {
		t.Error("oversized manifest must survive the pass")
	}
}
