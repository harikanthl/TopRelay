//go:build !darwin && !linux

package upstream

import (
	"errors"
	"net"
)

func peerOf(*net.UnixConn) (Peer, error) {
	return Peer{}, errors.New("Unix socket upstreams are supported on macOS and Linux only, where the peer's user can be checked")
}
