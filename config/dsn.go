package config

import (
	"fmt"
	"net/url"
	"strings"
)

// ConnectDSN returns a DSN for connecting/discovery: the stored template with
// the password injected, keeping the template's own database (or
// default_database if set). Also returns the secrets to redact.
func (conn *Connection) ConnectDSN() (dsn string, secrets []string, err error) {
	return conn.withPassword(conn.DefaultDatabase)
}

// BackupDSN returns a DSN for backing up one database. Per ADR-0004 the database
// is chosen per run: `database` (from --database) wins, else default_database;
// with neither it is an error rather than a silent whole-cluster surprise.
func (conn *Connection) BackupDSN(database string) (dsn string, secrets []string, err error) {
	db := database
	if db == "" {
		db = conn.DefaultDatabase
	}
	if db == "" {
		return "", nil, fmt.Errorf("no database selected: pass --database or set default_database on the connection")
	}
	return conn.withPassword(db)
}

// withPassword resolves the password reference and renders the template via
// BuildDSN.
func (conn *Connection) withPassword(database string) (string, []string, error) {
	pw, err := conn.Password.Resolve()
	if err != nil {
		return "", nil, err
	}
	dsn, err := BuildDSN(conn.DSN, pw, database)
	if err != nil {
		return "", nil, err
	}
	// Redact both the raw password and its URL-encoded form: BuildDSN injects
	// it via url.UserPassword, so in the connectable DSN a password like `p@ss`
	// appears percent-encoded (`p%40ss`). Registering only the raw value would
	// let the encoded form slip through if the full DSN ever reached an error
	// or log. (Redacting the encoded password, not the whole DSN, keeps the
	// host/db visible for diagnostics.)
	secrets := []string{pw}
	if enc := strings.TrimPrefix(url.UserPassword("", pw).String(), ":"); enc != "" && enc != pw {
		secrets = append(secrets, enc)
	}
	return dsn, secrets, nil
}

// BuildDSN renders a password-stripped DSN template into a connectable DSN:
// it optionally overrides the database and injects the password via
// url.UserPassword (correct encoding). All other DSN options survive
// verbatim. It is deliberately free of secret-resolution concerns so the
// cloud service can reuse it with its own secret backend (ADR-0001).
func BuildDSN(template, password, database string) (string, error) {
	u, err := url.Parse(template)
	if err != nil {
		return "", fmt.Errorf("connection dsn: invalid URL")
	}
	if database != "" {
		u.Path = "/" + database
	}
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String(), nil
}

// ValidateDSNTemplate checks that a DSN template is a parseable absolute URL
// that holds no inline password — the template is stored, and stored DSNs
// must never contain a secret (ADR-0004). Secrets hide in two places:
// the userinfo (user:pw@) and driver query parameters (libpq accepts
// ?password=... and ?sslpassword=...), so both are rejected.
func ValidateDSNTemplate(dsn string) error {
	if dsn == "" {
		return fmt.Errorf("dsn is required")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("dsn is not a valid URL")
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("dsn must be an absolute URL (scheme://user@host[:port]/db)")
	}
	if _, hasPw := u.User.Password(); hasPw {
		return fmt.Errorf("the dsn template must NOT contain a password; store the password separately")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("dsn query string is not valid")
	}
	for key := range q {
		if k := strings.ToLower(key); strings.Contains(k, "password") || k == "passwd" || k == "pwd" {
			return fmt.Errorf("the dsn template must NOT contain a password (query parameter %q); store the password separately", key)
		}
	}
	return nil
}

// S3Settings is a destination resolved for the pipeline. Static creds are empty
// unless the destination provides them; otherwise the standard AWS chain
// applies.
type S3Settings struct {
	Bucket       string
	Prefix       string
	Endpoint     string
	Region       string
	Profile      string
	AccessKey    string
	SecretKey    string
	SessionToken string
	HasStatic    bool
}

// DestURL is the s3://bucket/prefix form the pipeline expects.
func (d *Destination) DestURL() string {
	if d.Prefix != "" {
		return "s3://" + d.Bucket + "/" + d.Prefix
	}
	return "s3://" + d.Bucket
}

// Resolve turns a destination into S3Settings, returning secrets to redact.
func (d *Destination) Resolve() (S3Settings, []string, error) {
	s := S3Settings{
		Bucket: d.Bucket, Prefix: d.Prefix, Endpoint: d.Endpoint,
		Region: d.Region, Profile: d.Profile,
	}
	var secrets []string
	if d.AccessKey != nil && d.SecretKey != nil {
		ak, err := d.AccessKey.Resolve()
		if err != nil {
			return S3Settings{}, nil, fmt.Errorf("destination access_key: %w", err)
		}
		sk, err := d.SecretKey.Resolve()
		if err != nil {
			return S3Settings{}, nil, fmt.Errorf("destination secret_key: %w", err)
		}
		s.AccessKey, s.SecretKey, s.HasStatic = ak, sk, true
		secrets = append(secrets, ak, sk)
		if d.SessionToken != nil {
			st, err := d.SessionToken.Resolve()
			if err != nil {
				return S3Settings{}, nil, fmt.Errorf("destination session_token: %w", err)
			}
			s.SessionToken = st
			secrets = append(secrets, st)
		}
	}
	return s, secrets, nil
}
