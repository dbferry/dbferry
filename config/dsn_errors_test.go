package config

import (
	"strings"
	"testing"
)

func TestConnectDSNErrorPaths(t *testing.T) {
	// Password reference cannot resolve: the error must not invent a DSN.
	t.Setenv("DBFERRY_TEST_UNSET_PW", "")
	conn := &Connection{Engine: "postgres", DSN: "postgres://u@h:5432/db",
		Password: SecretRef{Env: "DBFERRY_TEST_UNSET_PW"}}
	if _, _, err := conn.ConnectDSN(); err == nil ||
		!strings.Contains(err.Error(), "DBFERRY_TEST_UNSET_PW") {
		t.Errorf("unresolvable password: %v", err)
	}

	// Password resolves but the template is not a URL.
	t.Setenv("DBFERRY_TEST_PW", "pw")
	broken := &Connection{Engine: "postgres", DSN: "://not-a-url",
		Password: SecretRef{Env: "DBFERRY_TEST_PW"}}
	if _, _, err := broken.ConnectDSN(); err == nil ||
		!strings.Contains(err.Error(), "invalid URL") {
		t.Errorf("broken template: %v", err)
	}
}

func TestValidateDSNTemplateErrorPaths(t *testing.T) {
	cases := map[string]string{
		"":                                "dsn is required",
		"://bad":                          "not a valid URL",
		"postgres:relative":               "absolute URL",
		"postgres://u:leak@h:5432/db":     "must NOT contain a password",
		"postgres://u@h:5432/db?a=%zz":    "query string is not valid",
		"postgres://u@h:5432/db?passwd=x": "must NOT contain a password",
	}
	for dsn, want := range cases {
		err := ValidateDSNTemplate(dsn)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateDSNTemplate(%q) = %v, want %q", dsn, err, want)
		}
	}
}

func TestDestURLWithoutPrefix(t *testing.T) {
	d := &Destination{Bucket: "b"}
	if got := d.DestURL(); got != "s3://b" {
		t.Errorf("DestURL = %q", got)
	}
}

func TestDestinationResolveErrorPaths(t *testing.T) {
	t.Setenv("DBFERRY_TEST_AK", "AK")
	t.Setenv("DBFERRY_TEST_SK", "SK")
	t.Setenv("DBFERRY_TEST_MISSING", "")

	// access_key unresolvable.
	d := &Destination{Bucket: "b",
		AccessKey: &SecretRef{Env: "DBFERRY_TEST_MISSING"},
		SecretKey: &SecretRef{Env: "DBFERRY_TEST_SK"}}
	if _, _, err := d.Resolve(); err == nil || !strings.Contains(err.Error(), "access_key") {
		t.Errorf("missing access key: %v", err)
	}

	// secret_key unresolvable.
	d = &Destination{Bucket: "b",
		AccessKey: &SecretRef{Env: "DBFERRY_TEST_AK"},
		SecretKey: &SecretRef{Env: "DBFERRY_TEST_MISSING"}}
	if _, _, err := d.Resolve(); err == nil || !strings.Contains(err.Error(), "secret_key") {
		t.Errorf("missing secret key: %v", err)
	}

	// session_token unresolvable.
	d = &Destination{Bucket: "b",
		AccessKey:    &SecretRef{Env: "DBFERRY_TEST_AK"},
		SecretKey:    &SecretRef{Env: "DBFERRY_TEST_SK"},
		SessionToken: &SecretRef{Env: "DBFERRY_TEST_MISSING"}}
	if _, _, err := d.Resolve(); err == nil || !strings.Contains(err.Error(), "session_token") {
		t.Errorf("missing session token: %v", err)
	}

	// Full static-credentials path, secrets registered for redaction.
	d = &Destination{Bucket: "b",
		AccessKey:    &SecretRef{Env: "DBFERRY_TEST_AK"},
		SecretKey:    &SecretRef{Env: "DBFERRY_TEST_SK"},
		SessionToken: &SecretRef{Env: "DBFERRY_TEST_AK"}}
	s, secrets, err := d.Resolve()
	if err != nil || !s.HasStatic || len(secrets) != 3 {
		t.Errorf("static resolve: settings=%+v secrets=%d err=%v", s, len(secrets), err)
	}
}
