//go:build !linux

package httpserver

import (
	"net"
	"os"
)

func peerIdentity(_ net.Conn) (int, int, error) { return os.Getuid(), 0, nil }
