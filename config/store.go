package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	toml "github.com/pelletier/go-toml/v2"
)

// DefaultPath is the config location: $DBFERRY_CONFIG, else
// $XDG_CONFIG_HOME/dbferry/config.toml (default ~/.config/dbferry/config.toml).
func DefaultPath() string {
	if p := os.Getenv("DBFERRY_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "dbferry", "config.toml")
}

func emptyConfig() *Config {
	return &Config{
		Connections:  map[string]*Connection{},
		Destinations: map[string]*Destination{},
	}
}

// Load reads and validates the config at path. A missing file yields an empty
// config (first run). The file must be a regular file, mode 0600, owned by the
// current user — a leaked or shared config is refused rather than trusted.
func Load(path string) (*Config, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyConfig(), nil
		}
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("config %s is a symlink; refusing to follow it", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("config %s has insecure permissions %#o; want 0600 (chmod 600 %s)", path, perm, path)
	}
	if err := checkOwner(path, info); err != nil {
		return nil, err
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := emptyConfig()
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.Connections == nil {
		c.Connections = map[string]*Connection{}
	}
	if c.Destinations == nil {
		c.Destinations = map[string]*Destination{}
	}
	return c, nil
}

// Save writes the config atomically: serialize first (so a marshal error leaves
// the old file untouched), then write to a temp file in the same directory with
// fsync and rename into place, all under a file lock against concurrent writers.
func (c *Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	if !locked {
		return fmt.Errorf("config %s is locked by another dbferry process; retry shortly", path)
	}
	defer lock.Unlock()

	b, err := toml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename below succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
