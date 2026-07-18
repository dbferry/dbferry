package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// keySchemaVersion is the version of the object-key + manifest contract
// (ADR-0005). It is written into every manifest; the layout may only change
// by bumping this, never by editing the meaning of an existing version.
const keySchemaVersion = 1

// backupFormat describes the pipeline that produced the ciphertext, so a
// restorer knows the exact inverse chain to run.
const backupFormat = "pg_dump -Fc -Z0 | zstd | age"

// Manifest is the JSON sidecar written next to a completed backup object. A
// backup is valid only with its manifest, and the manifest is written only
// after the object's multipart upload completes (ADR-0005). Field names are
// part of the versioned public contract — change them only via keySchemaVersion.
type Manifest struct {
	KeySchema        int    `json:"key_schema"`
	BackupID         string `json:"backup_id"`
	CreatedAt        string `json:"created_at"` // RFC3339, UTC
	Engine           string `json:"engine"`
	Cluster          string `json:"cluster"`
	Database         string `json:"database"`
	Object           string `json:"object"` // key of the ciphertext object
	Format           string `json:"format"`
	DumpClient       string `json:"dump_client"`               // e.g. "pg_dump (PostgreSQL) 18.3"
	DbferryVersion   string `json:"dbferry_version,omitempty"` // CLI build version
	CiphertextBytes  int64  `json:"ciphertext_bytes"`
	CiphertextSHA256 string `json:"ciphertext_sha256"` // hex
	PartSize         int64  `json:"part_size"`
	Concurrency      int    `json:"concurrency"`
}

// manifestKey is the manifest's object key: the sibling of the ciphertext key
// with the .dump.zst.age suffix replaced by .manifest.json (ADR-0005).
func manifestKey(objectKey string) string {
	return strings.TrimSuffix(objectKey, ciphertextSuffix) + manifestSuffix
}

func (m Manifest) marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("pipeline: marshal manifest: %w", err)
	}
	return append(b, '\n'), nil
}

// manifestCreatedAt formats the backup timestamp for the manifest.
func manifestCreatedAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
