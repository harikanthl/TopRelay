//go:build unix

package files

import (
	"io/fs"
	"syscall"
)

func identity(st fs.FileInfo) (dev, ino uint64, uid int, ok bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return uint64(s.Dev), uint64(s.Ino), int(s.Uid), true
}
