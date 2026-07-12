package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/zalando/go-keyring"
)

func TestSecretRefTextRoundTrip(t *testing.T) {
	for _, s := range []string{"keyring:dbferry/prod", "env:DB_PASS"} {
		var r SecretRef
		if err := r.UnmarshalText([]byte(s)); err != nil {
			t.Fatalf("unmarshal %q: %v", s, err)
		}
		b, err := r.MarshalText()
		if err != nil || string(b) != s {
			t.Errorf("round-trip %q → %q, %v", s, b, err)
		}
	}
	var r SecretRef
	if err := r.UnmarshalText([]byte("plaintext")); err == nil {
		t.Error("bare string should be rejected (no keyring:/env: prefix)")
	}
}

func TestSecretResolveEnvAndKeyring(t *testing.T) {
	t.Setenv("DBFERRY_TEST_SECRET", "s3kr3t")
	if v, err := (SecretRef{Env: "DBFERRY_TEST_SECRET"}).Resolve(); err != nil || v != "s3kr3t" {
		t.Errorf("env resolve = %q,%v", v, err)
	}
	if _, err := (SecretRef{Env: "DBFERRY_TEST_MISSING"}).Resolve(); err == nil {
		t.Error("missing env should error")
	}

	keyring.MockInit()
	ref := SecretRef{Keyring: "dbferry/test"}
	if err := ref.Store("kc-value"); err != nil {
		t.Fatal(err)
	}
	if v, err := ref.Resolve(); err != nil || v != "kc-value" {
		t.Errorf("keyring resolve = %q,%v", v, err)
	}
	if err := ref.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Resolve(); err == nil {
		t.Error("resolve after delete should error")
	}
}

func TestConnectionValidate(t *testing.T) {
	good := &Connection{Engine: "postgres", DSN: "postgres://u@h:5432/db?sslmode=require", Password: SecretRef{Env: "X"}}
	if err := good.Validate(); err != nil {
		t.Errorf("valid connection rejected: %v", err)
	}
	withPw := &Connection{Engine: "postgres", DSN: "postgres://u:secret@h/db", Password: SecretRef{Env: "X"}}
	if err := withPw.Validate(); err == nil || !strings.Contains(err.Error(), "must NOT contain a password") {
		t.Errorf("dsn with password should be rejected, got %v", err)
	}
	badEngine := &Connection{Engine: "oracle", DSN: "oracle://x", Password: SecretRef{Env: "X"}}
	if err := badEngine.Validate(); err == nil {
		t.Error("bad engine should be rejected")
	}
}

func TestBackupDSNSelectsDatabaseAndInjectsPassword(t *testing.T) {
	t.Setenv("PW", "p@ss:w/rd") // special chars → must be percent-encoded
	conn := &Connection{
		Engine:   "postgres",
		DSN:      "postgres://fnd_user@host:25060/defaultdb?sslmode=verify-full&sslrootcert=/x/ca.crt",
		Password: SecretRef{Env: "PW"},
	}
	// --database wins.
	dsn, secrets, err := conn.BackupDSN("fnd_central")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "/fnd_central?") {
		t.Errorf("database not swapped: %s", dsn)
	}
	if !strings.Contains(dsn, "sslmode=verify-full") || !strings.Contains(dsn, "sslrootcert=") {
		t.Errorf("TLS options not preserved: %s", dsn)
	}
	if strings.Contains(dsn, "p@ss:w/rd") {
		t.Errorf("password should be percent-encoded, not raw: %s", dsn)
	}
	if len(secrets) != 1 || secrets[0] != "p@ss:w/rd" {
		t.Errorf("secrets = %v", secrets)
	}
	// No database anywhere → error.
	if _, _, err := conn.BackupDSN(""); err == nil {
		t.Error("no database and no default_database should error")
	}
	// default_database used when --database empty.
	conn.DefaultDatabase = "fnd_central"
	if dsn, _, err := conn.BackupDSN(""); err != nil || !strings.Contains(dsn, "/fnd_central?") {
		t.Errorf("default_database not used: %s, %v", dsn, err)
	}
}

func TestDestinationResolve(t *testing.T) {
	d := &Destination{Bucket: "b", Prefix: "p"}
	if d.DestURL() != "s3://b/p" {
		t.Errorf("DestURL = %s", d.DestURL())
	}
	if s, secrets, err := d.Resolve(); err != nil || s.HasStatic || len(secrets) != 0 {
		t.Errorf("no-cred destination → static=%v secrets=%v err=%v", s.HasStatic, secrets, err)
	}
	t.Setenv("AK", "akid")
	t.Setenv("SK", "secret-key")
	d.AccessKey = &SecretRef{Env: "AK"}
	d.SecretKey = &SecretRef{Env: "SK"}
	s, secrets, err := d.Resolve()
	if err != nil || !s.HasStatic || s.AccessKey != "akid" || s.SecretKey != "secret-key" {
		t.Fatalf("static creds resolve: %+v %v", s, err)
	}
	if len(secrets) != 2 {
		t.Errorf("expected 2 secrets to redact, got %v", secrets)
	}
}

func TestRedactor(t *testing.T) {
	var r Redactor
	r.Add("topsecret", "", "akid")
	out := r.Redact("connect with topsecret and key akid ok")
	if strings.Contains(out, "topsecret") || strings.Contains(out, "akid") {
		t.Errorf("secrets leaked: %s", out)
	}
}

func TestStoreRoundTripAtomicAndPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	// Missing file → empty config.
	if c, err := Load(path); err != nil || len(c.Connections) != 0 {
		t.Fatalf("missing file load: %v", err)
	}

	c := emptyConfig()
	c.Connections["prod"] = &Connection{
		Engine: "postgres", DSN: "postgres://u@h/db?sslmode=require",
		Password: SecretRef{Keyring: "dbferry/prod"}, DefaultDatabase: "app",
		Destination: "do", AgeRecipient: "age1xxx",
	}
	c.Destinations["do"] = &Destination{Bucket: "sp", Prefix: "fnd", Endpoint: "https://x", Region: "fra1"}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}

	// 0600 written.
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("saved perms = %#o, want 0600", perm)
	}
	// TOML uses the string secret form.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `password = 'keyring:dbferry/prod'`) &&
		!strings.Contains(string(raw), `password = "keyring:dbferry/prod"`) {
		t.Errorf("secret not in string form:\n%s", raw)
	}

	// Round-trips.
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Connections["prod"].Password.Keyring != "dbferry/prod" ||
		got.Connections["prod"].DefaultDatabase != "app" ||
		got.Destinations["do"].Region != "fra1" {
		t.Errorf("round-trip mismatch: %+v", got.Connections["prod"])
	}

	// Insecure perms rejected.
	os.Chmod(path, 0o644)
	if _, err := Load(path); err == nil {
		t.Error("0644 config should be rejected")
	}
	os.Chmod(path, 0o600)

	// Symlink rejected.
	link := filepath.Join(dir, "link.toml")
	os.Symlink(path, link)
	if _, err := Load(link); err == nil {
		t.Error("symlinked config should be rejected")
	}
}

// Sanity: the config marshals with go-toml without error for an empty config.
func TestEmptyConfigMarshals(t *testing.T) {
	if _, err := toml.Marshal(emptyConfig()); err != nil {
		t.Fatal(err)
	}
}

func TestConfigLookupsAndNames(t *testing.T) {
	c := emptyConfig()
	c.Connections["b"] = &Connection{Engine: "postgres", DSN: "postgres://u@h/db", Password: SecretRef{Env: "X"}}
	c.Connections["a"] = &Connection{Engine: "mysql", DSN: "mysql://u@h/db", Password: SecretRef{Env: "Y"}}
	c.Destinations["z"] = &Destination{Bucket: "zb"}

	if got := c.ConnectionNames(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("ConnectionNames = %v (want sorted [a b])", got)
	}
	if got := c.DestinationNames(); len(got) != 1 || got[0] != "z" {
		t.Errorf("DestinationNames = %v", got)
	}
	if _, err := c.connection("a"); err != nil {
		t.Errorf("connection(a): %v", err)
	}
	if _, err := c.connection("missing"); err == nil {
		t.Error("connection(missing) should error")
	}
	if _, err := c.destination("z"); err != nil {
		t.Errorf("destination(z): %v", err)
	}
	if _, err := c.destination("missing"); err == nil {
		t.Error("destination(missing) should error")
	}
}

func TestConnectDSN(t *testing.T) {
	t.Setenv("PW", "secret")
	conn := &Connection{
		Engine: "postgres", DSN: "postgres://u@h:5432/maintdb?sslmode=require",
		Password: SecretRef{Env: "PW"}, DefaultDatabase: "app",
	}
	// ConnectDSN keeps default_database (for discovery) and injects the password.
	dsn, secrets, err := conn.ConnectDSN()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "/app?") || !strings.Contains(dsn, ":secret@") {
		t.Errorf("ConnectDSN = %s", dsn)
	}
	if len(secrets) != 1 || secrets[0] != "secret" {
		t.Errorf("secrets = %v", secrets)
	}
}

func TestDestinationValidate(t *testing.T) {
	if err := (&Destination{}).Validate(); err == nil {
		t.Error("empty bucket should be rejected")
	}
	if err := (&Destination{Bucket: "b", AccessKey: &SecretRef{Env: "AK"}}).Validate(); err == nil {
		t.Error("access_key without secret_key should be rejected")
	}
	if err := (&Destination{Bucket: "b"}).Validate(); err != nil {
		t.Errorf("bucket-only destination should be valid: %v", err)
	}
}

func TestDestinationResolveSessionToken(t *testing.T) {
	t.Setenv("AK", "ak")
	t.Setenv("SK", "sk")
	t.Setenv("ST", "token")
	d := &Destination{Bucket: "b",
		AccessKey: &SecretRef{Env: "AK"}, SecretKey: &SecretRef{Env: "SK"}, SessionToken: &SecretRef{Env: "ST"}}
	s, secrets, err := d.Resolve()
	if err != nil || s.SessionToken != "token" || len(secrets) != 3 {
		t.Errorf("session token resolve: %+v secrets=%v err=%v", s, secrets, err)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("DBFERRY_CONFIG", "/tmp/explicit.toml")
	if DefaultPath() != "/tmp/explicit.toml" {
		t.Errorf("DBFERRY_CONFIG not honoured: %s", DefaultPath())
	}
	t.Setenv("DBFERRY_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if DefaultPath() != "/xdg/dbferry/config.toml" {
		t.Errorf("XDG path = %s", DefaultPath())
	}
}

func TestSecretRefStringAndEmptyMarshal(t *testing.T) {
	if (SecretRef{}).String() != "(unset)" {
		t.Error("empty ref String")
	}
	if _, err := (SecretRef{}).MarshalText(); err == nil {
		t.Error("empty ref should not marshal")
	}
	if err := (SecretRef{Env: "X"}).Store("v"); err == nil {
		t.Error("Store on env ref should error")
	}
	if err := (SecretRef{Env: "X"}).Delete(); err != nil {
		t.Error("Delete on env ref should be a no-op")
	}
}
