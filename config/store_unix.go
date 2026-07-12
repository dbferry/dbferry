//go:build unix

package config

import (
	"fmt"
	"os"
	"syscall"
)

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
