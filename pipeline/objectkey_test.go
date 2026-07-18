package pipeline

import (
	"regexp"
	"testing"
	"time"
)

func TestParseDest(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantBucket string
		wantPrefix string
		wantErr    bool
	}{
		{name: "bucket and prefix", in: "s3://my-bucket/team/pg", wantBucket: "my-bucket", wantPrefix: "team/pg"},
		{name: "bucket only", in: "s3://my-bucket", wantBucket: "my-bucket"},
		{name: "trailing slash trimmed", in: "s3://my-bucket/pre/", wantBucket: "my-bucket", wantPrefix: "pre"},
		{name: "wrong scheme", in: "https://my-bucket/x", wantErr: true},
		{name: "missing bucket", in: "s3:///justprefix", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := parseDest(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseDest(%q) = %+v, want error", tc.in, d)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDest(%q) unexpected error: %v", tc.in, err)
			}
			if d.bucket != tc.wantBucket || d.prefix != tc.wantPrefix {
				t.Fatalf("parseDest(%q) = {bucket:%q prefix:%q}, want {bucket:%q prefix:%q}",
					tc.in, d.bucket, d.prefix, tc.wantBucket, tc.wantPrefix)
			}
		})
	}
}

// TestObjectKeySchema pins the versioned key schema (key_schema 1, ADR-0005).
// This is a public contract with the customer; a change here means a new schema
// version, not an edit to this test.
func TestObjectKeySchema(t *testing.T) {
	when := time.Date(2026, 7, 11, 16, 40, 25, 0, time.UTC)
	const id = "20260711T164025Z-01KX90TDX1XVMNBKV3GTF67CE3"

	got := dest{bucket: "b", prefix: "e2e"}.objectKey("postgres", "localhost_5417", "src", when, id)
	want := "e2e/postgres/localhost_5417/src/2026/07/" + id + ".dump.zst.age"
	if got != want {
		t.Errorf("with prefix:\n got  %q\n want %q", got, want)
	}

	gotNoPrefix := dest{bucket: "b"}.objectKey("postgres", "localhost_5417", "src", when, id)
	wantNoPrefix := "postgres/localhost_5417/src/2026/07/" + id + ".dump.zst.age"
	if gotNoPrefix != wantNoPrefix {
		t.Errorf("no prefix:\n got  %q\n want %q", gotNoPrefix, wantNoPrefix)
	}
}

func TestNewBackupIDFormatAndUniqueness(t *testing.T) {
	when := time.Date(2026, 7, 11, 16, 40, 25, 0, time.UTC)
	re := regexp.MustCompile(`^20260711T164025Z-[0-9A-HJKMNP-TV-Z]{26}$`)

	a, err := newBackupID(when)
	if err != nil {
		t.Fatalf("newBackupID: %v", err)
	}
	if !re.MatchString(a) {
		t.Fatalf("backup id %q does not match schema %s", a, re)
	}

	// Two ids for the same instant must differ (parallel backups of one DB).
	b, err := newBackupID(when)
	if err != nil {
		t.Fatalf("newBackupID: %v", err)
	}
	if a == b {
		t.Fatalf("backup ids collided for the same timestamp: %q", a)
	}
}

func TestSanitizeKeySegment(t *testing.T) {
	if got := sanitizeKeySegment("a/b c\td"); got != "a_b_c_d" {
		t.Errorf("sanitizeKeySegment = %q, want %q", got, "a_b_c_d")
	}
}
