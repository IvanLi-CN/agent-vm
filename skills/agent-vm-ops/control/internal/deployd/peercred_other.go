//go:build !linux

package deployd

import (
	"net"
	"os"
)

func peerUID(_ net.Conn) (int, error) { return os.Getuid(), nil }
