package deployd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
)

// RunHostObservation serves one systemd-accepted connection from the server account.
func RunHostObservation(input *os.File) error {
	if os.Geteuid() != 0 {
		return errors.New("host observations require the systemd root service")
	}
	conn, err := net.FileConn(input)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, ok := conn.(*net.UnixConn); !ok {
		return errors.New("host observations require a Unix connection")
	}
	uid, err := peerUID(conn)
	if err != nil || !isServiceAccount(uid, slotUser) {
		return errors.New("unauthorized host observation peer")
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	data, err := io.ReadAll(io.LimitReader(conn, 257))
	if err != nil || len(data) > 256 {
		return errors.New("host observation request exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request native.HostObservationRequest
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid host observation request")
	}
	if err := request.Validate(); err != nil {
		return err
	}
	dir := filepath.Join("/etc/agent-vm-native/slots", request.Slot)
	if err := checkHostUnitDirectory(dir); err != nil {
		return errors.New("untrusted slot configuration directory")
	}
	path := filepath.Join(dir, "config.json")
	file, err := os.Lstat(path)
	if err != nil || !file.Mode().IsRegular() || file.Mode().Perm()&0022 != 0 {
		return errors.New("untrusted slot configuration")
	}
	if owner, ok := file.Sys().(*syscall.Stat_t); !ok || owner.Uid != 0 {
		return errors.New("untrusted slot configuration owner")
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Slot != request.Slot {
		return errors.New("cannot load protected slot configuration")
	}
	client := native.NewIncusClient(nil, cfg.Project, native.ClientOptions{Network: cfg.Network, Pool: cfg.Pool, StoragePath: cfg.StoragePath})
	value, err := client.HostObservation(ctx, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(conn).Encode(value)
}
