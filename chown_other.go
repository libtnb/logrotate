//go:build !linux

package logrotate

import "os"

// chown is a no-op outside Linux; ownership preservation is only implemented
// there.
func chown(string, os.FileInfo) error { return nil }
