package upstream

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerOf(c *net.UnixConn) (Peer, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var p Peer
	var serr error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e != nil {
			serr = e
			return
		}
		p.UID, p.PID = int(cred.Uid), int(cred.Pid)
	})
	if err != nil {
		return Peer{}, err
	}
	return p, serr
}
