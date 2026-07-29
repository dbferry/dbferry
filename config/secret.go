// Package config stores dbferry's named connections and destinations, keeping
// secrets out of the config file (ADR-0004): the file holds only references,
// resolved at run time from the OS keychain or environment. The package is
// top-level so the cloud service can reuse the same model (ADR-0001).
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

// keyringService groups all dbferry secrets in the OS keychain.
const keyringService = "dbferry"

// SecretRef points at a secret without holding its value. Exactly one of
// Keyring or Env is set. In TOML it is a single string, `"keyring:NAME"` or
// `"env:NAME"` — readable and easy to edit by hand.
type SecretRef struct {
	Keyring string
	Env     string
}

func (r SecretRef) empty() bool { return r.Keyring == "" && r.Env == "" }

// MarshalText / UnmarshalText render SecretRef as "keyring:NAME" or "env:NAME".
func (r SecretRef) MarshalText() ([]byte, error) {
	if r.empty() {
		return nil, errors.New("empty secret reference")
	}
	return []byte(r.String()), nil
}

func (r *SecretRef) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case strings.HasPrefix(s, "keyring:"):
		r.Keyring = strings.TrimPrefix(s, "keyring:")
	case strings.HasPrefix(s, "env:"):
		r.Env = strings.TrimPrefix(s, "env:")
	default:
		return fmt.Errorf(`secret reference must be "keyring:NAME" or "env:NAME", got %q`, s)
	}
	if r.empty() {
		return fmt.Errorf("empty secret reference name in %q", s)
	}
	return nil
}

// String is a non-secret description safe to print (the reference, never the
// value).
func (r SecretRef) String() string {
	switch {
	case r.Keyring != "":
		return "keyring:" + r.Keyring
	case r.Env != "":
		return "env:" + r.Env
	default:
		return "(unset)"
	}
}

// Resolve returns the secret value from its provider. A keyring reference on a
// machine without a usable keychain is a clear, actionable error, never a
// silent empty.
func (r SecretRef) Resolve() (string, error) {
	switch {
	case r.Env != "":
		v := os.Getenv(r.Env)
		if v == "" {
			return "", fmt.Errorf("secret env var $%s is not set", r.Env)
		}
		return v, nil
	case r.Keyring != "":
		v, err := keyring.Get(keyringService, r.Keyring)
		if err != nil {
			if errors.Is(err, keyring.ErrNotFound) {
				return "", fmt.Errorf("secret %q not found in the OS keychain (re-run `dbferry init` or use an env reference)", r.Keyring)
			}
			return "", fmt.Errorf("read secret %q from the OS keychain: %w (on a headless machine use an env reference instead)", r.Keyring, err)
		}
		return v, nil
	default:
		return "", errors.New("empty secret reference")
	}
}

// Store puts a value in the OS keychain for a keyring reference. Env references
// are owned by the user, so this errors for them.
func (r SecretRef) Store(value string) error {
	if r.Keyring == "" {
		return errors.New("can only store keyring-backed secrets (env references are user-managed)")
	}
	if err := keyring.Set(keyringService, r.Keyring, value); err != nil {
		return fmt.Errorf("store secret %q in the OS keychain: %w", r.Keyring, err)
	}
	return nil
}

// StoreWithRollback stores a value like Store and returns a rollback that
// puts the keychain back the way it was: restore the previous value if the
// entry existed, delete the entry if it did not. A plain Delete-on-failure
// would be wrong here — re-running add/init over an existing connection and
// hitting a config-write error must not destroy the only copy of that
// connection's stored password. If the prior state cannot be determined, the
// store is refused up front: overwriting a value we could not read risks
// destroying it with no way back. The rollback returns an error so callers
// can tell the user when the keychain was left holding the new value.
func (r SecretRef) StoreWithRollback(value string) (func() error, error) {
	prev, getErr := keyring.Get(keyringService, r.Keyring)
	if getErr != nil && !errors.Is(getErr, keyring.ErrNotFound) {
		return nil, fmt.Errorf("read current secret %q from the OS keychain: %w (not overwriting a value that cannot be restored)", r.Keyring, getErr)
	}
	if err := r.Store(value); err != nil {
		return nil, err
	}
	if getErr == nil {
		return func() error {
			if err := keyring.Set(keyringService, r.Keyring, prev); err != nil {
				return fmt.Errorf("restore previous secret %q in the OS keychain: %w", r.Keyring, err)
			}
			return nil
		}, nil
	}
	return func() error { return r.Delete() }, nil
}

// Delete removes a keyring-backed secret. Env references are left alone.
func (r SecretRef) Delete() error {
	if r.Keyring == "" {
		return nil
	}
	err := keyring.Delete(keyringService, r.Keyring)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete secret %q from the OS keychain: %w", r.Keyring, err)
	}
	return nil
}
