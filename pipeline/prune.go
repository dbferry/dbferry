package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// deleteBatchSize is the DeleteObjects API limit.
const deleteBatchSize = 1000

// PruneOptions tunes a retention pass.
type PruneOptions struct {
	// DryRun selects what would be deleted without deleting anything.
	DryRun bool
	// RequireScope, when non-empty, pins the pass to an expected scope
	// (as returned by BackupScope): if the scope derived from cfg differs —
	// e.g. the stored connection or destination was edited between enqueue
	// and execution — the pass refuses with ErrScopeMismatch instead of
	// pruning a scope no fresh backup has landed in.
	RequireScope string
}

// ErrScopeMismatch is returned by Prune when RequireScope does not match the
// scope derived from the configuration. Nothing has been listed or deleted.
var ErrScopeMismatch = errors.New("pipeline: prune scope mismatch")

// PruneResult reports what a retention pass saw and did.
type PruneResult struct {
	Listing Listing
	// Kept and Deleted partition the valid backups by the policy. With
	// DryRun, Deleted is the selection that would have been removed.
	Kept    []BackupInfo
	Deleted []BackupInfo
	// DanglingDeleted are manifest keys whose ciphertext no longer existed
	// (interrupted earlier deletes) that were cleaned up.
	DanglingDeleted []string
	// Orphans are ciphertexts without a manifest. Reported, never touched:
	// an upload finishing right now looks exactly like one.
	Orphans []BackupInfo
}

// Prune applies a GFS retention policy to one database's backups under
// cfg.Dest: list, select with SelectRetention, delete what the policy drops,
// and clean up dangling manifests. It is the single entry point for both the
// cloud retention job and `dbferry prune`.
//
// Deletion order is ciphertext first, then manifest: a crash in between
// leaves a dangling manifest — cleaned by the next pass — never an
// unmanageable orphan. Deleting requires s3:DeleteObject on the destination.
func Prune(ctx context.Context, cfg Config, policy RetentionPolicy, opts PruneOptions) (PruneResult, error) {
	if err := policy.Validate(); err != nil {
		return PruneResult{}, err
	}
	scope, dst, owner, err := backupScope(cfg)
	if err != nil {
		return PruneResult{}, err
	}
	if opts.RequireScope != "" && opts.RequireScope != scope {
		return PruneResult{}, fmt.Errorf("%w: config resolves to %q, caller expects %q", ErrScopeMismatch, scope, opts.RequireScope)
	}
	api, err := newS3Client(ctx, cfg)
	if err != nil {
		return PruneResult{}, err
	}
	return pruneWith(ctx, api, dst.bucket, scope, owner, policy, opts)
}

// pruneWith is the client-agnostic core of Prune, injectable for tests.
func pruneWith(ctx context.Context, api s3ObjectAPI, bucket, scope string, owner *scopeOwner, policy RetentionPolicy, opts PruneOptions) (PruneResult, error) {
	listing, err := listBackups(ctx, api, bucket, scope, owner)
	if err != nil {
		return PruneResult{}, err
	}

	res := PruneResult{Listing: listing}
	var danglingKeys []string
	for _, b := range listing.Backups {
		switch b.State {
		case BackupOrphan:
			res.Orphans = append(res.Orphans, b)
		case BackupDanglingManifest:
			danglingKeys = append(danglingKeys, b.ManifestKey)
		}
	}
	// Dangling classification is by key suffix alone — before deleting, read
	// each manifest and keep hands off anything that isn't verifiably one of
	// ours at the current key_schema. A newer schema's manifest (whose
	// ciphertext key this build simply doesn't recognize) must survive an old
	// binary's prune pass.
	dangling, err := deletableDangling(ctx, api, bucket, owner, danglingKeys)
	if err != nil {
		return res, err
	}
	res.Kept, res.Deleted = SelectRetention(listing.Backups, policy)

	if opts.DryRun {
		res.DanglingDeleted = dangling
		return res, nil
	}
	if err := deleteBackups(ctx, api, bucket, scope, res.Deleted); err != nil {
		return res, err
	}
	if err := deleteKeys(ctx, api, bucket, scope, manifestSuffix, dangling); err != nil {
		return res, err
	}
	res.DanglingDeleted = dangling
	return res, nil
}

// deletableDangling filters dangling-manifest keys down to the ones this build
// may delete: manifests that fetch, parse, carry exactly the current
// key_schema AND claim this scope's own identity — the same owner line of
// defense readManifest applies to paired manifests. Anything unreadable, from
// another schema, or claiming another database is left in place (reported as
// dangling again on the next pass); a transport error fails the pass so the
// caller retries.
func deletableDangling(ctx context.Context, api s3ObjectAPI, bucket string, owner *scopeOwner, keys []string) ([]string, error) {
	var out []string
	for _, key := range keys {
		obj, err := api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			return nil, classify(KindUpload, "pipeline: read dangling manifest s3://%s/%s: %w", bucket, key, err)
		}
		// Read one byte past the cap: an oversized object is not one of our
		// manifests (they are well under a kilobyte), and a truncated read
		// that still parses must not make it deletable.
		body, err := io.ReadAll(io.LimitReader(obj.Body, maxManifestBytes+1))
		closeErr := obj.Body.Close()
		if err != nil || closeErr != nil {
			return nil, classify(KindUpload, "pipeline: read dangling manifest s3://%s/%s: %w", bucket, key, firstErr(err, closeErr))
		}
		if len(body) > maxManifestBytes {
			continue
		}
		var m Manifest
		if json.Unmarshal(body, &m) != nil || m.KeySchema != keySchemaVersion || !owner.matches(&m) {
			continue
		}
		out = append(out, key)
	}
	return out, nil
}

// DeleteBackups deletes the given backups (ciphertexts first, then manifests)
// under cfg.Dest. Every entry must be a BackupValid listing result whose
// manifest claims this configuration's own engine/cluster/database, lie
// inside the scope derived from cfg, and carry the expected artifact suffix —
// anything else fails before a single deletion starts. Callers therefore
// cannot smuggle orphans, dangling manifests, or another database's backups
// through this API, whatever list they assembled.
func DeleteBackups(ctx context.Context, cfg Config, backups []BackupInfo) error {
	api, err := newS3Client(ctx, cfg)
	if err != nil {
		return err
	}
	scope, dst, owner, err := backupScope(cfg)
	if err != nil {
		return err
	}
	for _, b := range backups {
		if b.State != BackupValid || b.Manifest == nil || !owner.matches(b.Manifest) {
			return classify(KindUpload,
				"pipeline: refusing to delete %q: not a validated backup of this database (state %s)", b.Key, b.State)
		}
	}
	return deleteBackups(ctx, api, dst.bucket, scope, backups)
}

func deleteBackups(ctx context.Context, api s3ObjectAPI, bucket, scope string, backups []BackupInfo) error {
	ciphers := make([]string, 0, len(backups))
	manifests := make([]string, 0, len(backups))
	for _, b := range backups {
		ciphers = append(ciphers, b.Key)
		manifests = append(manifests, b.ManifestKey)
	}
	// Guard everything up front: a single foreign key aborts the whole pass.
	if err := guardKeys(scope, ciphertextSuffix, ciphers); err != nil {
		return err
	}
	if err := guardKeys(scope, manifestSuffix, manifests); err != nil {
		return err
	}
	// Ciphertext before manifest: an interruption leaves a self-healing
	// dangling manifest, never an orphan nothing will ever clean.
	if err := deleteBatches(ctx, api, bucket, ciphers); err != nil {
		return err
	}
	return deleteBatches(ctx, api, bucket, manifests)
}

// deleteKeys guards and deletes a plain key list (dangling manifests).
func deleteKeys(ctx context.Context, api s3ObjectAPI, bucket, scope, suffix string, keys []string) error {
	if err := guardKeys(scope, suffix, keys); err != nil {
		return err
	}
	return deleteBatches(ctx, api, bucket, keys)
}

// guardKeys enforces that retention only ever deletes backup artifacts inside
// its own scope. This is the tenant-safety line: no matter what the caller
// assembled, a key outside <scope> or without the artifact suffix is refused.
func guardKeys(scope, suffix string, keys []string) error {
	for _, k := range keys {
		if !strings.HasPrefix(k, scope) || !strings.HasSuffix(k, suffix) || len(k) <= len(scope)+len(suffix) {
			return classify(KindUpload, "pipeline: refusing to delete %q: outside the retention scope %q", k, scope)
		}
	}
	return nil
}

func deleteBatches(ctx context.Context, api s3ObjectAPI, bucket string, keys []string) error {
	for start := 0; start < len(keys); start += deleteBatchSize {
		batch := keys[start:min(start+deleteBatchSize, len(keys))]
		ids := make([]types.ObjectIdentifier, len(batch))
		for i, k := range batch {
			ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		out, err := api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return classify(KindUpload, "pipeline: delete backups in s3://%s: %w", bucket, err)
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			msg := fmt.Sprintf("%s: %s", aws.ToString(e.Code), aws.ToString(e.Message))
			if aws.ToString(e.Code) == "AccessDenied" {
				msg += " — retention needs s3:DeleteObject on the destination prefix"
			}
			return classify(KindUpload, "pipeline: delete backups in s3://%s: %d of %d objects failed, first %s (%s)",
				bucket, len(out.Errors), len(batch), aws.ToString(e.Key), msg)
		}
	}
	return nil
}
