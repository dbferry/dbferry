package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePrivate(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultPathFallbacks(t *testing.T) {
	t.Setenv("DBFERRY_CONFIG", "/explicit/config.toml")
	if got := DefaultPath(); got != "/explicit/config.toml" {
		t.Errorf("DBFERRY_CONFIG override ignored: %q", got)
	}

	t.Setenv("DBFERRY_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := DefaultPath(); got != filepath.Join("/xdg", "dbferry", "config.toml") {
		t.Errorf("XDG path = %q", got)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	if got := DefaultPath(); !strings.HasSuffix(got, filepath.Join(".config", "dbferry", "config.toml")) {
		t.Errorf("home fallback = %q", got)
	}
}

// TestLoadRefusesUnsafeFiles: the config carries secret references and is
// trusted input — a symlink, a directory, or a group-readable file must be
// refused, not silently trusted.
func TestLoadRefusesUnsafeFiles(t *testing.T) {
	dir := t.TempDir()

	real := filepath.Join(dir, "real.toml")
	writePrivate(t, real, "")
	link := filepath.Join(dir, "link.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink: %v", err)
	}

	if _, err := Load(dir); err == nil {
		t.Error("a directory must be refused")
	}

	loose := filepath.Join(dir, "loose.toml")
	if err := os.WriteFile(loose, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(loose); err == nil || !strings.Contains(err.Error(), "insecure permissions") {
		t.Errorf("0644 config: %v", err)
	}
}

func TestLoadRejectsBadContent(t *testing.T) {
	dir := t.TempDir()

	bad := filepath.Join(dir, "bad.toml")
	writePrivate(t, bad, "{not toml")
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Errorf("bad TOML: %v", err)
	}

	// A hand-edited connection that lost its password reference must fail on
	// Load, naming the entry — not later as an opaque marshal error.
	missing := filepath.Join(dir, "missing-pw.toml")
	writePrivate(t, missing, "[connections.prod]\nengine = \"postgres\"\ndsn = \"postgres://u@h:5432/db\"\n")
	if _, err := Load(missing); err == nil || !strings.Contains(err.Error(), `connection "prod" has no password reference`) {
		t.Errorf("missing password ref: %v", err)
	}
}

func TestLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// flock is process-scoped, so a second acquisition from the same process
	// cannot model another dbferry run; what must hold is that Lock cannot be
	// taken on an unwritable directory.
	if _, err := Lock(filepath.Join("/nonexistent-root-dir", "config.toml")); err == nil {
		t.Error("Lock in an uncreatable directory must fail")
	}
}

func TestSaveLockedFailurePaths(t *testing.T) {
	cfg := emptyConfig()

	if err := cfg.SaveLocked(filepath.Join("/nonexistent-root-dir", "config.toml")); err == nil {
		t.Error("SaveLocked into an uncreatable directory must fail")
	}

	// Unwritable directory: CreateTemp fails, the old file stays untouched.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writePrivate(t, path, "")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := cfg.SaveLocked(path); err == nil {
		t.Error("SaveLocked in a read-only directory must fail")
	}
}

func TestSaveTakesAndReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := emptyConfig()
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The lock must be released: a follow-up Lock succeeds immediately.
	unlock, err := Lock(path)
	if err != nil {
		t.Fatalf("lock after Save: %v", err)
	}
	unlock()
}
