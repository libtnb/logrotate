//go:build unix

package logrotate

import "syscall"

func setUmask(mask int) int { return syscall.Umask(mask) }
