package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestStoreWithRollbackRestoresPreExisting pins the data-loss guard: rolling
// back an overwrite must restore the previous keychain value, not delete it —
// that value may be the only copy of a configured connection's password.
func TestStoreWithRollbackRestoresPreExisting(t *testing.T) {
	keyring.MockInit()
	ref := SecretRef{Keyring: "dbferry/rollback-existing"}
	if err := ref.Store("old-password"); err != nil {
		t.Fatal(err)
	}

	rollback, err := ref.StoreWithRollback("new-password")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := ref.Resolve(); v != "new-password" {
		t.Fatalf("after store, keychain holds %q, want new-password", v)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if v, _ := ref.Resolve(); v != "old-password" {
		t.Fatalf("after rollback, keychain holds %q, want the restored old-password", v)
	}
}

func TestStoreWithRollbackDeletesFreshEntry(t *testing.T) {
	keyring.MockInit()
	ref := SecretRef{Keyring: "dbferry/rollback-fresh"}

	rollback, err := ref.StoreWithRollback("value")
	if err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := ref.Resolve(); err == nil {
		t.Fatal("rollback of a fresh entry must remove it from the keychain")
	}
}

// TestStoreWithRollbackRefusesUnknownPriorState: when the keychain cannot even
// be read, overwriting would risk destroying a value with no way back — the
// store must refuse up front.
func TestStoreWithRollbackRefusesUnknownPriorState(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain locked"))
	t.Cleanup(keyring.MockInit) // restore the working mock for later tests
	ref := SecretRef{Keyring: "dbferry/rollback-broken"}

	if _, err := ref.StoreWithRollback("value"); err == nil ||
		!strings.Contains(err.Error(), "not overwriting") {
		t.Fatalf("expected a refuse-to-overwrite error, got %v", err)
	}
}

func TestSecretRefErrorPaths(t *testing.T) {
	keyring.MockInit()

	// Resolve of a keyring entry that does not exist: actionable not-found.
	if _, err := (SecretRef{Keyring: "dbferry/never-stored"}).Resolve(); err == nil ||
		!strings.Contains(err.Error(), "not found in the OS keychain") {
		t.Errorf("missing keyring entry: %v", err)
	}
	// Store on an env reference is user-managed — refused.
	if err := (SecretRef{Env: "X"}).Store("v"); err == nil {
		t.Error("Store on an env ref must be refused")
	}
	// Delete is a no-op for env refs and for missing entries.
	if err := (SecretRef{Env: "X"}).Delete(); err != nil {
		t.Errorf("Delete on env ref: %v", err)
	}
	if err := (SecretRef{Keyring: "dbferry/never-stored"}).Delete(); err != nil {
		t.Errorf("Delete of a missing entry must be a no-op: %v", err)
	}
	// A reference with an empty name after the prefix is rejected on parse.
	var ref SecretRef
	if err := ref.UnmarshalText([]byte("keyring:")); err == nil {
		t.Error(`"keyring:" with no name must be rejected`)
	}

	// Broken keychain: Resolve/Store/Delete surface the backend error.
	keyring.MockInitWithError(errors.New("keychain locked"))
	t.Cleanup(keyring.MockInit)
	broken := SecretRef{Keyring: "dbferry/x"}
	if _, err := broken.Resolve(); err == nil || !strings.Contains(err.Error(), "keychain") {
		t.Errorf("Resolve on broken keychain: %v", err)
	}
	if err := broken.Store("v"); err == nil {
		t.Error("Store on broken keychain must error")
	}
	if err := broken.Delete(); err == nil {
		t.Error("Delete on broken keychain must error")
	}
}
