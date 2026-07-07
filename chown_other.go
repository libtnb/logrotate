//go:build !linux

package logrotate

import "os"

func chown(string, os.FileInfo) error { return nil }
