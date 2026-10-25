//go:build linux

package httpserver

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func peerUID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return os.Getuid(), nil
	}
	var uid int = os.Getuid()
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			controlErr = err
			return
		}
		uid = int(credential.Uid)
	}); err != nil {
		return 0, err
	}
	return uid, controlErr
}
