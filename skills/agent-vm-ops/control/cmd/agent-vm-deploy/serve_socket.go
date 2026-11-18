package main

import (
	"errors"
	"net"
	"os"
	"strconv"
)

func inheritedDeploySocket(path string) (net.Listener, error) {
	pid, fdCount, name := os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS"), os.Getenv("LISTEN_FDNAMES")
	if pid == "" && fdCount == "" && name == "" {
		if os.Geteuid() == 0 {
			return nil, nil
		}
		return nil, errors.New("unprivileged deployment service requires the systemd listener")
	}
	for _, key := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		_ = os.Unsetenv(key)
	}
	if pid != strconv.Itoa(os.Getpid()) || fdCount != "1" || name != "agent-vm-deploy" {
		return nil, errors.New("invalid deployment socket activation descriptors")
	}
	file := os.NewFile(3, "agent-vm-deploy")
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, err
	}
	listenerUnix, ok := listener.(*net.UnixListener)
	if !ok || listenerUnix.Addr().Network() != "unix" || listenerUnix.Addr().String() != path {
		_ = listener.Close()
		return nil, errors.New("systemd listener is not the configured deployment Unix socket")
	}
	listenerUnix.SetUnlinkOnClose(false)
	return listenerUnix, nil
}
