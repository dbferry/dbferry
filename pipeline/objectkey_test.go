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
	cases := []struct{ in, want string }{
		{"a/b c\td", "a%2Fb%20c%09d"},
		{"shop", "shop"},
		{"my_db", "my_db"},
		// "." and ".." must not survive: path.Join would collapse them and
		// widen the per-database scope onto other databases' backups.
		{".", "%2E"},
		{"..", "%2E%2E"},
		{"", "%"},
		// Control characters (legal in quoted identifiers) must not reach the
		// key: \r enables log spoofing, DEL and friends break tooling.
		{"a\rb", "a%0Db"},
		{"a\x7fb", "a%7Fb"},
		// '%' is escaped so the encoding stays reversible → collision-free.
		{"100%", "100%25"},
		{"%2E", "%252E"},
	}
	for _, c := range cases {
		if got := sanitizeKeySegment(c.in); got != c.want {
			t.Errorf("sanitizeKeySegment(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Collision-freedom is the retention-safety invariant: distinct names must
	// never map to one scope (pruning one database must not see the other's
	// backups). These pairs collided under naive replacement-char mappings.
	pairs := [][2]string{
		{"..", "__"}, {".", "_"}, {"", "_"},
		{"my db", "my_db"}, {"a\rb", "a_b"}, {"%2E", ".."},
	}
	for _, p := range pairs {
		if sanitizeKeySegment(p[0]) == sanitizeKeySegment(p[1]) {
			t.Errorf("sanitizeKeySegment collision: %q and %q both map to %q", p[0], p[1], sanitizeKeySegment(p[0]))
		}
	}
}

func TestScopeCannotEscapeViaDotSegments(t *testing.T) {
	// A database literally named ".." (legal in PostgreSQL, reachable straight
	// from the DSN path) must scope to its own directory, not collapse the
	// prefix and pool every database's backups into one retention pass.
	scope, _, _, err := backupScope(Config{DSN: "postgres://u:p@host:5432/..", Dest: "s3://bucket/pfx"})
	if err != nil {
		t.Fatalf("backupScope: %v", err)
	}
	want := "pfx/postgres/host/%2E%2E/"
	if scope != want {
		t.Fatalf("scope = %q, want %q", scope, want)
	}
}
