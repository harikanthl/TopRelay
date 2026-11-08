//go:build unix

package upstream

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func ownerUID(st fs.FileInfo) (uint32, bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return s.Uid, true
}

func checkOwner(st fs.FileInfo) error {
	uid, ok := ownerUID(st)
	if !ok {
		return fmt.Errorf("can't read the file's owner")
	}
	if int(uid) != os.Getuid() {
		return fmt.Errorf("owned by uid %d, not by you (uid %d)", uid, os.Getuid())
	}
	return nil
}

// checkDirOwner accepts a directory owned by this user or root that group and others can't write to.
func checkDirOwner(st fs.FileInfo) error {
	uid, ok := ownerUID(st)
	if !ok {
		return fmt.Errorf("can't read the directory's owner")
	}
	if int(uid) != os.Getuid() && uid != 0 {
		return fmt.Errorf("owned by uid %d, not by you or root", uid)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("other users can write to it (mode %04o), so they could replace the socket", st.Mode().Perm())
	}
	return nil
}
