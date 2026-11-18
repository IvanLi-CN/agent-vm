//go:build linux

package httpserver

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func peerIdentity(conn net.Conn) (int, int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return os.Getuid(), 0, nil
	}
	var uid int = os.Getuid()
	var pid int
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			controlErr = err
			return
		}
		uid = int(credential.Uid)
		pid = int(credential.Pid)
	}); err != nil {
		return 0, 0, err
	}
	return uid, pid, controlErr
}
