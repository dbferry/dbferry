//go:build unix

package config

import (
	"fmt"
	"os"
	"syscall"
)

// RequirePrivate refuses a secret-bearing file readable by group or other.
// POSIX-only: Windows reports synthesized permission bits (0666/0444) that
// would make every file "insecure", including ones this program just wrote
// with 0600 — access control there is ACLs, which these bits don't reflect.
func RequirePrivate(what, path string, info os.FileInfo) error {
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s %s has insecure permissions %#o; want 0600 (chmod 600 %s)", what, path, perm, path)
	}
	return nil
}

// checkOwner refuses a config file owned by another user (defence beyond the
// 0600 permission bits).
func checkOwner(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("config %s is owned by uid %d, not you (uid %d); refusing to use it", path, st.Uid, os.Getuid())
	}
	return nil
}
