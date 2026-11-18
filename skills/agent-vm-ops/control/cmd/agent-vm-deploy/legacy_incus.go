package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
)

func runLegacyIncus(args []string) error {
	if !deployd.IsServerAccount(os.Geteuid()) || len(args) < 2 || (args[0] != "blue" && args[0] != "green") {
		return errors.New("legacy Incus adapter requires a fixed slot and the server account")
	}
	slot := args[0]
	cfg, err := config.Load(filepath.Join("/etc/agent-vm-native/slots", slot, "config.json"))
	if err != nil || cfg.Slot != slot {
		return errors.New("cannot load fixed slot configuration for legacy Incus adapter")
	}
	ctx := context.Background()
	args = args[1:]
	handled, err := legacyIncusObservation(ctx, os.Stdout, cfg, args, native.ReadHostObservation)
	if handled || err != nil {
		return err
	}
	return syscall.Exec("/usr/bin/incus", append([]string{"incus"}, args...), os.Environ())
}

// The historical in-process client consumes CLI JSON. Translate only its
// exact observations and keep every other call on the restricted Incus CLI.
func legacyIncusObservation(ctx context.Context, output io.Writer, cfg config.Config, args []string, read func(context.Context, native.HostObservationRequest) (native.HostObservationResult, error)) (bool, error) {
	pool := "/1.0/storage-pools/" + url.PathEscape(cfg.Pool)
	var operation string
	switch {
	case slices.Equal(args, []string{"query", "/1.0/networks/" + url.PathEscape(cfg.Network) + "/leases?project=default"}):
		operation = "network-leases"
	case slices.Equal(args, []string{"query", pool + "?project=default"}):
		operation = "pool-capacity"
	case slices.Equal(args, []string{"query", pool + "/resources?project=" + url.QueryEscape(cfg.Project)}):
		operation = "image-storage"
	case slices.Equal(args, []string{"operation", "list", "--all-projects", "--format=json"}):
		operation = "incus-quiet"
	default:
		return false, nil
	}
	result, err := read(ctx, native.HostObservationRequest{Slot: cfg.Slot, Operation: operation})
	if err != nil {
		return true, err
	}
	var response any
	switch operation {
	case "network-leases":
		rows := make([]map[string]string, 0, len(result.Addresses))
		for _, address := range result.Addresses {
			rows = append(rows, map[string]string{"address": address})
		}
		response = rows
	case "pool-capacity":
		// The helper already checked the actual pool name, driver and source.
		response = map[string]any{"name": cfg.Pool, "driver": "dir", "config": map[string]string{"source": cfg.StoragePath}}
	case "image-storage":
		response = map[string]any{"space_available": result.Available}
	case "incus-quiet":
		response = []any{}
	}
	return true, json.NewEncoder(output).Encode(response)
}
