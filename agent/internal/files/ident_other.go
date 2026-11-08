//go:build !unix

package files

import "io/fs"

// Without file ownership and identity, files can't be shared safely.
func identity(fs.FileInfo) (dev, ino uint64, uid int, ok bool) { return 0, 0, 0, false }
