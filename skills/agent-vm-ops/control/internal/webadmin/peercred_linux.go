//go:build linux

package webadmin

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func peerUID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("Unix peer credentials require a Unix connection")
	}
	var uid int
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
	if controlErr != nil {
		return 0, controlErr
	}
	return uid, nil
}
