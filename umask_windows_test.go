//go:build windows

package logrotate

// Windows has no umask; TestFileModeOption exercises the chmod path anyway.
func setUmask(int) int { return 0 }
