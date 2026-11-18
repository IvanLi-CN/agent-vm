//go:build linux

package deployd

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeRequestRelayRejectsNonRootPeer(t *testing.T) {
	if os.Getenv("AGENT_VM_NATIVE_REQUEST_RELAY") != "1" || os.Geteuid() != 0 {
		t.Skip("requires the session-owned root Linux fixture")
	}
	dir, err := os.MkdirTemp("/tmp", "agent-vm-request-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "request.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("/usr/bin/python3", "-c", "import socket,sys;s=socket.socket(socket.AF_UNIX);s.connect(sys.argv[1]);s.close()", path)
	child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Wait()
	_ = listener.SetDeadline(time.Now().Add(5 * time.Second))
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	file, err := conn.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := RunRequestRelay(file); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("non-root peer was not rejected: %v", err)
	}
}
