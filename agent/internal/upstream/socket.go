package upstream

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// CheckSocket checks a Unix socket path before every connection: it must be a socket owned by this user with no
// group or other permission bits (0600 or stricter), in a directory owned by this user or root that nobody else
// can write to (so the socket can't be swapped). Symlinks in the directory part are resolved first.
func CheckSocket(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("socket %s: the path must be absolute", path)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("socket %s: %v", path, err)
	}
	dst, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("socket %s: %v", path, err)
	}
	if err := checkDirOwner(dst); err != nil {
		return fmt.Errorf("socket directory %s: %v", dir, err)
	}
	st, err := os.Lstat(filepath.Join(dir, filepath.Base(path)))
	if err != nil {
		return fmt.Errorf("socket %s: %v (is the app running?)", path, err)
	}
	if st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a socket", path)
	}
	if err := checkOwner(st); err != nil {
		return fmt.Errorf("socket %s: %v", path, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("socket %s: other users can connect to it (mode %04o); the app should chmod it 600", path, st.Mode().Perm())
	}
	return nil
}

// Peer is the process on the other end of a Unix socket.
type Peer struct {
	UID int
	// PID is 0 where the system doesn't report it.
	PID int
}

// PeerOf returns the process on the other end of a Unix socket connection (macOS and Linux).
func PeerOf(c *net.UnixConn) (Peer, error) { return peerOf(c) }

// dialUnix checks the socket, connects, and checks that the peer runs as this user.
func dialUnix(ctx context.Context, path string, pin *Pin) (net.Conn, error) {
	if err := CheckSocket(path); err != nil {
		return nil, err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, fmt.Errorf("socket %s: unexpected connection type", path)
	}
	peer, err := peerOf(uc)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("socket %s: can't read the peer's credentials: %v", path, err)
	}
	if peer.UID != os.Getuid() {
		c.Close()
		return nil, fmt.Errorf("socket %s: the app on the other end runs as uid %d, not as you", path, peer.UID)
	}
	if pin != nil {
		if peer.PID <= 0 {
			c.Close()
			return nil, fmt.Errorf("socket %s: this system doesn't report the peer process, so the code-signature pin can't be checked", path)
		}
		if err := pin.check(ctx, peer.PID); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}
