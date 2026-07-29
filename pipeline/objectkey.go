package pipeline

import (
	"crypto/rand"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// dest is a parsed s3://bucket/prefix destination.
type dest struct {
	bucket string
	prefix string
}

// parseDest parses an s3://bucket/prefix URL. The prefix may be empty.
func parseDest(s string) (dest, error) {
	u, err := url.Parse(s)
	if err != nil {
		return dest{}, fmt.Errorf("pipeline: parse --dest: invalid URL")
	}
	if u.Scheme != "s3" {
		return dest{}, fmt.Errorf("pipeline: --dest must be an s3:// URL, got %q", u.Scheme)
	}
	if u.Host == "" {
		return dest{}, fmt.Errorf("pipeline: --dest is missing a bucket (s3://BUCKET/prefix)")
	}
	return dest{bucket: u.Host, prefix: strings.Trim(u.Path, "/")}, nil
}

// objectKey builds the versioned object key (key_schema 1, ADR-0005):
//
//	<prefix>/<engine>/<cluster>/<database>/<YYYY>/<MM>/<backup_id>.dump.zst.age
//
// The date segments drive lifecycle rules and bucket navigation. This is a
// future public contract with the customer — change it only via a new schema
// version.
func (d dest) objectKey(engine, cluster, database string, t time.Time, backupID string) string {
	segs := make([]string, 0, 7)
	if d.prefix != "" {
		segs = append(segs, d.prefix)
	}
	segs = append(segs,
		engine,
		cluster,
		sanitizeKeySegment(database),
		t.Format("2006"),
		t.Format("01"),
		backupID+".dump.zst.age",
	)
	return path.Join(segs...)
}

// newBackupID returns a backup id that is unique even for parallel backups of
// one database: a sortable UTC timestamp plus a ULID (crypto/rand entropy).
func newBackupID(t time.Time) (string, error) {
	id, err := ulid.New(ulid.Timestamp(t), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		return "", fmt.Errorf("pipeline: generate backup id: %w", err)
	}
	return t.Format("20060102T150405Z") + "-" + id.String(), nil
}

// sanitizeKeySegment keeps a DSN-derived value safe inside an object key.
// Two invariants, both load-bearing for retention safety:
//
//   - "" / "." / ".." must never survive as a segment — path.Join would
//     collapse them and widen the per-database scope onto sibling databases'
//     backups (a database named ".." would otherwise scope the retention pass
//     to the whole engine root and let Prune delete other databases' backups).
//   - The mapping must be collision-free: two distinct database names on one
//     cluster must never share a scope, or pruning one could delete the
//     other's backups. Percent-encoding ('%' included, so decoding is
//     unambiguous) guarantees that; a lossy replacement-char mapping cannot.
func sanitizeKeySegment(s string) string {
	switch s {
	case ".", "..":
		return strings.ReplaceAll(s, ".", "%2E")
	case "":
		// No input ever encodes to a bare "%" ('%' itself becomes "%25"),
		// so this cannot collide with any real name.
		return "%"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '/' || c == '\\' || c == ' ' || c == '%' || c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
