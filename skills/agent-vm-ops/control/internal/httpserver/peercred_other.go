//go:build !linux

package httpserver

import (
	"net"
	"os"
)

func peerUID(_ net.Conn) (int, error) { return os.Getuid(), nil }
