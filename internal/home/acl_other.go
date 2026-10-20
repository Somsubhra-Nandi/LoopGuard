//go:build !windows

package home

import "os"

// restrictToUser makes the file readable and writable by its owner only.
func restrictToUser(path string) error { return os.Chmod(path, 0o600) }
