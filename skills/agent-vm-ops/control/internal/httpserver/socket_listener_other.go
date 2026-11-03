//go:build !linux

package httpserver

import (
	"fmt"
	"net"
	"os"
)

func listenUnixSocket(path string, mode os.FileMode) (net.Listener, error) {
	if err := prepareSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}
