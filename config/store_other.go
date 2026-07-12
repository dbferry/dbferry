//go:build !unix

package config

import "os"

// checkOwner is a no-op where file ownership isn't a POSIX uid (e.g. Windows).
func checkOwner(string, os.FileInfo) error { return nil }
