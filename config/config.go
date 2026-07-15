package config

import (
	"fmt"
	"sort"
)

// Config is the whole dbferry config file: named connections and destinations.
type Config struct {
	Connections  map[string]*Connection  `toml:"connections,omitempty"`
	Destinations map[string]*Destination `toml:"destinations,omitempty"`
}

// Connection is a database cluster. It stores a DSN template with the password
// removed (all other options preserved verbatim) plus a password reference; the
// database to back up is chosen per run (ADR-0004).
type Connection struct {
	Engine          string    `toml:"engine"`
	DSN             string    `toml:"dsn"` // password-stripped template
	Password        SecretRef `toml:"password"`
	DefaultDatabase string    `toml:"default_database,omitempty"`
	Destination     string    `toml:"destination,omitempty"`
	AgeRecipient    string    `toml:"age_recipient,omitempty"`
}

// Destination is a named S3(-compatible) target. Credentials are optional: with
// none, the standard AWS credential chain is used.
type Destination struct {
	Bucket       string     `toml:"bucket"`
	Prefix       string     `toml:"prefix,omitempty"`
	Endpoint     string     `toml:"endpoint,omitempty"`
	Region       string     `toml:"region,omitempty"`
	Profile      string     `toml:"profile,omitempty"`
	AccessKey    *SecretRef `toml:"access_key,omitempty"`
	SecretKey    *SecretRef `toml:"secret_key,omitempty"`
	SessionToken *SecretRef `toml:"session_token,omitempty"`
}

func (c *Config) connection(name string) (*Connection, error) {
	conn := c.Connections[name]
	if conn == nil {
		return nil, fmt.Errorf("no connection named %q (see `dbferry connections list`)", name)
	}
	return conn, nil
}

func (c *Config) destination(name string) (*Destination, error) {
	dst := c.Destinations[name]
	if dst == nil {
		return nil, fmt.Errorf("no destination named %q (see `dbferry destinations list`)", name)
	}
	return dst, nil
}

// ConnectionNames returns connection names, sorted.
func (c *Config) ConnectionNames() []string  { return sortedKeys(c.Connections) }
func (c *Config) DestinationNames() []string { return sortedKeys(c.Destinations) }

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Validate checks structural invariants, including that no secret hides in a
// DSN query string.
func (conn *Connection) Validate() error {
	switch conn.Engine {
	case "postgres", "mysql":
	default:
		return fmt.Errorf("engine must be postgres or mysql, got %q", conn.Engine)
	}
	if err := ValidateDSNTemplate(conn.DSN); err != nil {
		return err
	}
	if conn.Password.empty() {
		return fmt.Errorf("password reference is required (keyring or env)")
	}
	return nil
}

func (d *Destination) Validate() error {
	if d.Bucket == "" {
		return fmt.Errorf("bucket is required")
	}
	// Credentials are optional (fall back to the AWS chain); if any static key
	// is given, both access and secret must be present.
	if (d.AccessKey != nil) != (d.SecretKey != nil) {
		return fmt.Errorf("access_key and secret_key must be set together")
	}
	return nil
}
