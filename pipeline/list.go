package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Object-key suffixes of the two artifacts a backup consists of (key_schema 1).
const (
	ciphertextSuffix = ".dump.zst.age"
	manifestSuffix   = ".manifest.json"
)

// maxManifestBytes caps how much of a manifest object is read. Real manifests
// are well under a kilobyte; anything larger is not one of ours.
const maxManifestBytes = 1 << 20

// s3ObjectAPI is the subset of S3 operations listing and retention need. The
// real *s3.Client satisfies it; tests inject fakes.
type s3ObjectAPI interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObjects(context.Context, *s3.DeleteObjectsInput, ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// BackupState classifies one listed backup. Only BackupValid entries are ever
// eligible for retention decisions; every other state is reported and left
// untouched (except dangling manifests, which Prune cleans up — they describe
// data that no longer exists).
type BackupState int

const (
	// BackupValid: ciphertext with a parseable manifest of a known key_schema.
	BackupValid BackupState = iota
	// BackupOrphan: ciphertext without a manifest — an interrupted upload.
	// Never deleted by retention (a just-finished upload whose manifest has
	// not landed yet looks exactly like this).
	BackupOrphan
	// BackupDanglingManifest: manifest without its ciphertext — an
	// interrupted delete. Safe to remove; Prune does.
	BackupDanglingManifest
	// BackupCorruptManifest: the manifest exists but cannot be read as one of
	// ours (bad JSON, wrong object reference, oversized). Never touched.
	BackupCorruptManifest
	// BackupUnsupportedSchema: the manifest's key_schema is newer than this
	// build understands. Never touched.
	BackupUnsupportedSchema
)

func (s BackupState) String() string {
	switch s {
	case BackupValid:
		return "valid"
	case BackupOrphan:
		return "orphan"
	case BackupDanglingManifest:
		return "dangling-manifest"
	case BackupCorruptManifest:
		return "corrupt-manifest"
	case BackupUnsupportedSchema:
		return "unsupported-schema"
	default:
		return "unknown"
	}
}

// BackupInfo is one backup (or backup artifact) found in the bucket.
type BackupInfo struct {
	// Key is the ciphertext object key; empty for a dangling manifest.
	Key string
	// ManifestKey is the manifest object key; empty for an orphan.
	ManifestKey string
	State       BackupState
	// CreatedAt is the manifest's created_at; for entries without a usable
	// manifest it is recovered from the timestamp in the backup id key
	// segment. Zero if neither is available.
	CreatedAt time.Time
	// Bytes is the ciphertext size from the listing (0 for a dangling manifest).
	Bytes int64
	// Manifest is the parsed manifest; nil unless State is BackupValid.
	Manifest *Manifest
}

// Listing is the result of ListBackups: every backup artifact of one database
// under one destination, newest first.
type Listing struct {
	Bucket string
	// Scope is the key prefix that was listed:
	// <prefix>/<engine>/<cluster>/<database>/.
	Scope   string
	Backups []BackupInfo
}

// Valid returns only the backups eligible for retention decisions.
func (l Listing) Valid() []BackupInfo {
	var out []BackupInfo
	for _, b := range l.Backups {
		if b.State == BackupValid {
			out = append(out, b)
		}
	}
	return out
}

// ListBackups lists one database's backups under cfg.Dest. The listing scope
// (engine, cluster, database) is derived from cfg.DSN exactly as Run derives
// the object key — the DSN is only parsed, no connection is made to the
// database. Objects under the scope that are not backup artifacts are ignored.
func ListBackups(ctx context.Context, cfg Config) (Listing, error) {
	api, err := newS3Client(ctx, cfg)
	if err != nil {
		return Listing{}, err
	}
	scope, dst, owner, err := backupScope(cfg)
	if err != nil {
		return Listing{}, err
	}
	return listBackups(ctx, api, dst.bucket, scope, owner)
}

// scopeOwner is the identity a scope's manifests must claim. A manifest under
// the right key prefix whose engine/cluster/database say otherwise is treated
// as foreign — reported, never deleted. This is defense in depth against any
// key-encoding drift ever pooling two databases into one retention pass: keys
// can collide only if the manifests, written from the raw names, agree too.
type scopeOwner struct {
	engine, cluster, database string
}

func (o *scopeOwner) matches(m *Manifest) bool {
	return o == nil || (m.Engine == o.engine && m.Cluster == o.cluster && m.Database == o.database)
}

// BackupScope returns the per-database scope prefix a backup object key
// belongs to: everything above the <YYYY>/<MM>/<file> tail of the key_schema-1
// layout, with a trailing slash. It lets a caller pin a retention pass to the
// exact scope a specific backup was written to.
func BackupScope(objectKey string) string {
	return path.Dir(path.Dir(path.Dir(objectKey))) + "/"
}

// backupScope derives the per-database key prefix from the run configuration,
// sharing the exact derivation Run uses for the object key, plus the identity
// that scope's manifests must claim.
func backupScope(cfg Config) (string, dest, *scopeOwner, error) {
	drv, err := newDriver(cfg.DSN)
	if err != nil {
		return "", dest{}, nil, err
	}
	dst, err := parseDest(cfg.Dest)
	if err != nil {
		return "", dest{}, nil, err
	}
	segs := make([]string, 0, 4)
	if dst.prefix != "" {
		segs = append(segs, dst.prefix)
	}
	segs = append(segs, drv.Engine(), drv.Cluster(), sanitizeKeySegment(drv.Database()))
	owner := &scopeOwner{engine: drv.Engine(), cluster: drv.Cluster(), database: drv.Database()}
	return path.Join(segs...) + "/", dst, owner, nil
}

func listBackups(ctx context.Context, api s3ObjectAPI, bucket, scope string, owner *scopeOwner) (Listing, error) {
	// stem (key without artifact suffix) → the pair of artifacts seen there.
	type artifacts struct {
		cipherKey   string
		cipherBytes int64
		manifestKey string
		manifestLen int64
	}
	pairs := map[string]*artifacts{}
	var order []string

	paginator := s3.NewListObjectsV2Paginator(api, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(scope),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return Listing{}, classify(KindUpload, "pipeline: list backups under s3://%s/%s: %w", bucket, scope, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			var stem string
			switch {
			case strings.HasSuffix(key, ciphertextSuffix):
				stem = strings.TrimSuffix(key, ciphertextSuffix)
			case strings.HasSuffix(key, manifestSuffix):
				stem = strings.TrimSuffix(key, manifestSuffix)
			default:
				continue // not a backup artifact — never ours to touch
			}
			p := pairs[stem]
			if p == nil {
				p = &artifacts{}
				pairs[stem] = p
				order = append(order, stem)
			}
			if strings.HasSuffix(key, ciphertextSuffix) {
				p.cipherKey = key
				p.cipherBytes = aws.ToInt64(obj.Size)
			} else {
				p.manifestKey = key
				p.manifestLen = aws.ToInt64(obj.Size)
			}
		}
	}

	backups := make([]BackupInfo, 0, len(order))
	for _, stem := range order {
		p := pairs[stem]
		switch {
		case p.cipherKey == "":
			backups = append(backups, BackupInfo{
				ManifestKey: p.manifestKey,
				State:       BackupDanglingManifest,
				CreatedAt:   createdAtFromStem(stem),
			})
		case p.manifestKey == "":
			backups = append(backups, BackupInfo{
				Key:       p.cipherKey,
				State:     BackupOrphan,
				CreatedAt: createdAtFromStem(stem),
				Bytes:     p.cipherBytes,
			})
		default:
			info, err := readManifest(ctx, api, bucket, p.cipherKey, p.manifestKey, p.manifestLen, owner)
			if err != nil {
				return Listing{}, err
			}
			info.Bytes = p.cipherBytes
			backups = append(backups, info)
		}
	}

	// Newest first; key as a deterministic tie-break for equal timestamps.
	sort.Slice(backups, func(i, j int) bool {
		if !backups[i].CreatedAt.Equal(backups[j].CreatedAt) {
			return backups[i].CreatedAt.After(backups[j].CreatedAt)
		}
		return backups[i].Key > backups[j].Key
	})
	return Listing{Bucket: bucket, Scope: scope, Backups: backups}, nil
}

// readManifest fetches and validates one manifest. A manifest that cannot be
// read as ours degrades the backup's state (corrupt/unsupported) rather than
// failing the listing; a transport error fails the listing so the caller can
// retry.
func readManifest(ctx context.Context, api s3ObjectAPI, bucket, cipherKey, manifestKey string, size int64, owner *scopeOwner) (BackupInfo, error) {
	info := BackupInfo{Key: cipherKey, ManifestKey: manifestKey, CreatedAt: createdAtFromStem(strings.TrimSuffix(cipherKey, ciphertextSuffix))}
	if size > maxManifestBytes {
		info.State = BackupCorruptManifest
		return info, nil
	}
	out, err := api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(manifestKey)})
	if err != nil {
		return BackupInfo{}, classify(KindUpload, "pipeline: read manifest s3://%s/%s: %w", bucket, manifestKey, err)
	}
	body, err := io.ReadAll(io.LimitReader(out.Body, maxManifestBytes))
	closeErr := out.Body.Close()
	if err != nil || closeErr != nil {
		return BackupInfo{}, classify(KindUpload, "pipeline: read manifest s3://%s/%s: %w", bucket, manifestKey, firstErr(err, closeErr))
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		info.State = BackupCorruptManifest
		return info, nil
	}
	if m.KeySchema > keySchemaVersion {
		info.State = BackupUnsupportedSchema
		return info, nil
	}
	// Only an exact, fully-populated schema-1 manifest that describes its
	// sibling AND claims this scope's own engine/cluster/database is trusted;
	// anything less is corrupt and never touched. JSON zero values must not
	// slip through as "valid" — a manifest is the thing that makes a
	// ciphertext deletable, and the identity check makes key-encoding
	// collisions insufficient to pool two databases into one retention pass.
	created, timeErr := time.Parse(time.RFC3339, m.CreatedAt)
	if m.KeySchema != keySchemaVersion || m.Object != cipherKey || m.BackupID == "" || timeErr != nil || !owner.matches(&m) {
		info.State = BackupCorruptManifest
		return info, nil
	}
	info.CreatedAt = created.UTC()
	info.State = BackupValid
	info.Manifest = &m
	return info, nil
}

// createdAtFromStem recovers the backup timestamp from the key's backup id
// segment (20060102T150405Z-<ULID>). Zero time if the segment doesn't parse.
func createdAtFromStem(stem string) time.Time {
	base := path.Base(stem)
	ts, _, ok := strings.Cut(base, "-")
	if !ok {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102T150405Z", ts, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("unknown error")
}
