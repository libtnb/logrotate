package logrotate

import (
	"os"
	"syscall"
)

// chown transfers ownership of the file it replaces to name, so rotation
// under a privileged process keeps logs readable by their original owner.
func chown(name string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Chown(name, int(stat.Uid), int(stat.Gid))
}
