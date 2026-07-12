package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dbferry/dbferry/pipeline"
)

func TestRedactorScrubsDSNAndPassword(t *testing.T) {
	dsn := "postgres://user:S3kr3tP@ss@db.example:5432/shop"
	redact := newRedactor(dsn)

	in := "connect failed for " + dsn + " (password S3kr3tP@ss)"
	got := redact(in)
	if strings.Contains(got, "S3kr3tP@ss") {
		t.Errorf("password leaked: %q", got)
	}
	if strings.Contains(got, dsn) {
		t.Errorf("DSN leaked: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("expected redaction marker: %q", got)
	}
}

func TestRedactorNoDSNIsNoop(t *testing.T) {
	if newRedactor("")("nothing to hide") != "nothing to hide" {
		t.Error("empty DSN should be a no-op redactor")
	}
}

// TestFailRedactsSecretFromOutput is the acceptance guard: a secret embedded in
// an error must not reach stderr (poc-plan 3.3).
func TestFailRedactsSecretFromOutput(t *testing.T) {
	dsn := "postgres://user:TOPSECRET@h:5432/db"
	var stderr bytes.Buffer
	u := &ui{stdout: &bytes.Buffer{}, stderr: &stderr}
	err := fmt.Errorf("boom talking to %s", dsn)

	code := u.fail(err, newRedactor(dsn))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if strings.Contains(stderr.String(), "TOPSECRET") {
		t.Fatalf("secret leaked to stderr: %q", stderr.String())
	}
}

func TestFailJSONShape(t *testing.T) {
	var stdout bytes.Buffer
	u := &ui{stdout: &stdout, stderr: &bytes.Buffer{}, json: true}
	code := u.fail(&pipeline.Error{Kind: pipeline.KindUpload, Err: fmt.Errorf("bucket denied")}, redactNothing)
	if code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}
	var r jsonResult
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if r.OK || r.Kind != "upload" || r.Error == "" {
		t.Errorf("unexpected json: %+v", r)
	}
}

func TestSuccessJSONShape(t *testing.T) {
	var stdout bytes.Buffer
	u := &ui{stdout: &stdout, stderr: &bytes.Buffer{}, json: true}
	u.success(pipeline.Result{BackupID: "id1", Bucket: "b", Key: "k", ManifestKey: "m", Bytes: 42, SHA256: "abc"})
	var r jsonResult
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !r.OK || r.BackupID != "id1" || r.CiphertextBytes != 42 {
		t.Errorf("unexpected json: %+v", r)
	}
}

func TestSuccessQuietIsSilent(t *testing.T) {
	var stdout bytes.Buffer
	u := &ui{stdout: &stdout, stderr: &bytes.Buffer{}, quiet: true}
	u.success(pipeline.Result{BackupID: "id1"})
	if stdout.Len() != 0 {
		t.Errorf("quiet success should print nothing, got %q", stdout.String())
	}
}

func TestHint(t *testing.T) {
	if !strings.Contains(hint(pipeline.KindDump, "exec: pg_dump: executable file not found in $PATH"), "PATH") {
		t.Error("missing pg_dump hint should mention PATH")
	}
	if !strings.Contains(hint(pipeline.KindDump, "permission denied for table secret"), "GRANT") {
		t.Error("permission-denied hint should suggest a grant")
	}
	if hint(pipeline.KindUpload, "x") == "" || hint(pipeline.KindConnect, "x") == "" {
		t.Error("upload/connect hints should be non-empty")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 1048576: "1.0 MiB"}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestParsePartSize(t *testing.T) {
	ok := map[string]int64{
		"32MiB": 32 << 20, "5mib": 5 << 20, "1GiB": 1 << 30,
		"10MB": 10_000_000, "8": 8 << 20, // bare number = MiB
	}
	for in, want := range ok {
		got, err := parsePartSize(in)
		if err != nil || got != want {
			t.Errorf("parsePartSize(%q) = %d,%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"1MiB", "4MiB", "0", "abc", "-5MiB"} {
		if _, err := parsePartSize(bad); err == nil {
			t.Errorf("parsePartSize(%q) should error (below 5MiB min or invalid)", bad)
		}
	}
}

func TestExitCode(t *testing.T) {
	cases := map[pipeline.Kind]int{
		pipeline.KindConnect: 3, pipeline.KindDump: 4, pipeline.KindUpload: 5,
		pipeline.KindCanceled: 130, pipeline.KindUnknown: 1,
	}
	for k, want := range cases {
		if got := exitCode(&pipeline.Error{Kind: k, Err: fmt.Errorf("x")}); got != want {
			t.Errorf("exitCode(%v) = %d, want %d", k, got, want)
		}
	}
}

func TestVersionAndUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb, false, false); code != 0 {
		t.Errorf("version exit = %d", code)
	}
	if strings.TrimSpace(out.String()) != version {
		t.Errorf("version output = %q, want %q", out.String(), version)
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"frobnicate"}, &out, &errb, false, false); code != 1 {
		t.Errorf("unknown command exit = %d, want 1", code)
	}
}
