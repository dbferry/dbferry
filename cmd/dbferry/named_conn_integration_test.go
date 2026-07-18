//go:build integration

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbferry/dbferry/config"
)

// testS3Endpoint honors DBFERRY_TEST_S3_ENDPOINT like the integration suite
// (dev machines where localhost:9000 is shadowed need a LAN address).
func testS3Endpoint() string {
	if v := os.Getenv("DBFERRY_TEST_S3_ENDPOINT"); v != "" {
		return v
	}
	return "http://localhost:9000"
}

// TestRunAndDoctorViaConnection drives the whole named-connection path end to
// end against the stand: build a config, then `run --connection` and
// `doctor --connection` (poc-plan 0.5.6). Needs `make stand-up`.
func TestRunAndDoctorViaConnection(t *testing.T) {
	recipient, err := os.ReadFile(filepath.Join("..", "..", "test", "integration", ".stand", "age-recipient.txt"))
	if err != nil {
		t.Skipf("stand not set up (run `make stand-up`): %v", err)
	}

	t.Setenv("STAND_PW", "dbferry")
	t.Setenv("MINIO_KEY", "minioadmin")
	t.Setenv("MINIO_SECRET", "minioadmin")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := &config.Config{
		Connections: map[string]*config.Connection{
			"stand": {
				Engine:          "postgres",
				DSN:             "postgres://dbferry@localhost:5417/postgres",
				Password:        config.SecretRef{Env: "STAND_PW"},
				DefaultDatabase: "postgres",
				Destination:     "minio",
				AgeRecipient:    strings.TrimSpace(string(recipient)),
			},
		},
		Destinations: map[string]*config.Destination{
			"minio": {
				Bucket: "dbferry-backups", Prefix: "cmdit",
				Endpoint: testS3Endpoint(), Region: "us-east-1",
				AccessKey: &config.SecretRef{Env: "MINIO_KEY"},
				SecretKey: &config.SecretRef{Env: "MINIO_SECRET"},
			},
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	// doctor should pass (warns allowed).
	var dOut, dErr bytes.Buffer
	if code := run([]string{"doctor", "--connection", "stand", "--config", cfgPath}, &dOut, &dErr, false, false); code != 0 {
		t.Fatalf("doctor exit = %d\n%s%s", code, dOut.String(), dErr.String())
	}
	if !strings.Contains(dOut.String(), "connect") {
		t.Errorf("doctor output missing checks:\n%s", dOut.String())
	}

	// run via the connection should back up.
	var rOut bytes.Buffer
	if code := run([]string{"run", "--connection", "stand", "--config", cfgPath}, &rOut, io.Discard, false, false); code != 0 {
		t.Fatalf("run --connection exit = %d\n%s", code, rOut.String())
	}
	if !strings.Contains(rOut.String(), "backup complete") {
		t.Errorf("run output missing success:\n%s", rOut.String())
	}
}
