//go:build !unix

package config

import "os"

// checkOwner is a no-op where file ownership isn't a POSIX uid (e.g. Windows).
func checkOwner(string, os.FileInfo) error { return nil }

// RequirePrivate is a no-op where permission bits are synthesized (Windows
// reports 0666/0444 regardless of ACLs, so the POSIX check would reject every
// file — including ones this program just wrote with 0600).
func RequirePrivate(string, string, os.FileInfo) error { return nil }
