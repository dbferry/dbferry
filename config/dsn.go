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

// withPassword parses the template, optionally overrides the database, and
// injects the resolved password via url.UserPassword (correct encoding). All
// other DSN options survive verbatim.
func (conn *Connection) withPassword(database string) (string, []string, error) {
	pw, err := conn.Password.Resolve()
	if err != nil {
		return "", nil, err
	}
	u, err := url.Parse(conn.DSN)
	if err != nil {
		return "", nil, fmt.Errorf("connection dsn: invalid URL")
	}
	if database != "" {
		u.Path = "/" + database
	}
	u.User = url.UserPassword(u.User.Username(), pw)
	return u.String(), []string{pw}, nil
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
