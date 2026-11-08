//go:build !unix

package upstream

import (
	"errors"
	"io/fs"
)

// On systems without Unix file ownership the checks can't be made, so secret files and sockets are refused.

func checkOwner(fs.FileInfo) error {
	return errors.New("file ownership can't be checked on this system; use --header-stdin instead")
}

func checkDirOwner(fs.FileInfo) error {
	return errors.New("directory ownership can't be checked on this system")
}
