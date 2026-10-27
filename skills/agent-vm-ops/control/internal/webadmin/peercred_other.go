//go:build !linux

package webadmin

import (
	"errors"
	"net"
)

func peerUID(net.Conn) (int, error) {
	return 0, errors.New("Unix peer credentials are unavailable on this platform")
}
