package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/dbferry/dbferry/pipeline"
)

func TestSplitDSN(t *testing.T) {
	engine, tmpl, pw, err := splitDSN("postgres://u:s3cr3t@h:5432/db?sslmode=require&sslrootcert=/x")
	if err != nil {
		t.Fatal(err)
	}
	if engine != "postgres" || pw != "s3cr3t" {
		t.Errorf("engine=%q pw=%q", engine, pw)
	}
	if strings.Contains(tmpl, "s3cr3t") {
		t.Errorf("password must be stripped from template: %s", tmpl)
	}
	if !strings.Contains(tmpl, "sslmode=require") || !strings.Contains(tmpl, "sslrootcert=") {
		t.Errorf("options not preserved: %s", tmpl)
	}

	if e, _, pw, _ := splitDSN("mysql://u@h/db"); e != "mysql" || pw != "" {
		t.Errorf("mysql no-password: engine=%q pw=%q", e, pw)
	}
	if _, _, _, err := splitDSN("redis://x/y"); err == nil {
		t.Error("bad scheme should error")
	}
}

func TestRenderChecks(t *testing.T) {
	// All OK → exit 0.
	var buf bytes.Buffer
	u := &ui{stdout: &buf, stderr: io.Discard}
	if code := u.renderChecks([]pipeline.Check{{Name: "connect", Status: pipeline.StatusOK, Detail: "ok"}}, redactNothing); code != 0 {
		t.Errorf("all-ok exit = %d", code)
	}

	// A fail → exit 1, fix printed.
	buf.Reset()
	code := u.renderChecks([]pipeline.Check{
		{Name: "connect", Status: pipeline.StatusOK},
		{Name: "write", Status: pipeline.StatusFail, Detail: "denied", Fix: "grant s3:PutObject"},
	}, redactNothing)
	if code != 1 {
		t.Errorf("with fail exit = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "grant s3:PutObject") {
		t.Errorf("fix not printed: %s", buf.String())
	}

	// Warn alone does not fail.
	if code := u.renderChecks([]pipeline.Check{{Name: "delete", Status: pipeline.StatusWarn}}, redactNothing); code != 0 {
		t.Errorf("warn-only exit = %d, want 0", code)
	}

	// JSON mode.
	buf.Reset()
	uj := &ui{stdout: &buf, stderr: io.Discard, json: true}
	uj.renderChecks([]pipeline.Check{{Name: "connect", Status: pipeline.StatusOK}}, redactNothing)
	var got struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name, Status string
		} `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !got.OK || len(got.Checks) != 1 || got.Checks[0].Status != "ok" {
		t.Errorf("unexpected json: %+v", got)
	}
}

func TestRenderChecksRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	u := &ui{stdout: &buf, stderr: io.Discard}
	redact := func(s string) string { return strings.ReplaceAll(s, "TOPSECRET", "[redacted]") }
	u.renderChecks([]pipeline.Check{{Name: "connect", Status: pipeline.StatusFail, Detail: "auth failed for TOPSECRET"}}, redact)
	if strings.Contains(buf.String(), "TOPSECRET") {
		t.Errorf("secret leaked in doctor output: %s", buf.String())
	}
}
