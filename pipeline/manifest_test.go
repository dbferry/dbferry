package pipeline

import (
	"encoding/json"
	"testing"
)

// TestManifestKey pins the sidecar naming: same directory and backup_id as the
// ciphertext object, with the suffix swapped (DECISIONS.md key schema).
func TestManifestKey(t *testing.T) {
	obj := "e2e/postgres/localhost_5417/src/2026/07/20260711T164025Z-01KX90TDX1XVMNBKV3GTF67CE3.dump.zst.age"
	want := "e2e/postgres/localhost_5417/src/2026/07/20260711T164025Z-01KX90TDX1XVMNBKV3GTF67CE3.manifest.json"
	if got := manifestKey(obj); got != want {
		t.Errorf("manifestKey:\n got  %q\n want %q", got, want)
	}
}

func TestManifestMarshalRoundTrip(t *testing.T) {
	m := Manifest{
		KeySchema:        keySchemaVersion,
		BackupID:         "20260711T164025Z-01KX90TDX1XVMNBKV3GTF67CE3",
		CreatedAt:        "2026-07-11T16:40:25Z",
		Engine:           "postgres",
		Cluster:          "localhost_5417",
		Database:         "src",
		Object:           "e2e/postgres/localhost_5417/src/2026/07/x.dump.zst.age",
		Format:           backupFormat,
		DumpClient:       "pg_dump (PostgreSQL) 18.3",
		CiphertextBytes:  132202,
		CiphertextSHA256: "abc123",
		PartSize:         DefaultPartSize,
		Concurrency:      DefaultConcurrency,
	}
	b, err := m.marshal()
	if err != nil {
		t.Fatal(err)
	}
	var got Manifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if got != m {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, m)
	}
	if got.KeySchema != 1 {
		t.Errorf("key_schema = %d, want 1", got.KeySchema)
	}
}
