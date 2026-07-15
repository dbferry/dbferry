package config

import (
	"fmt"
	"net/url"
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
	return dsn, []string{pw}, nil
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

// ValidateDSNTemplate checks that a DSN template is a parseable URL that
// holds no inline password — the template is stored, and stored DSNs must
// never contain a secret (ADR-0004).
func ValidateDSNTemplate(dsn string) error {
	if dsn == "" {
		return fmt.Errorf("dsn is required")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("dsn is not a valid URL")
	}
	if _, hasPw := u.User.Password(); hasPw {
		return fmt.Errorf("the dsn template must NOT contain a password; store the password separately")
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
