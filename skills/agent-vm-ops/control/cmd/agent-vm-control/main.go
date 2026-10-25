package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/httpserver"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/manifest"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/scheduler"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/state"
)

var version = "agent-vm-control/dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if control, ok := err.(*protocol.Error); ok {
			_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"ok": false, "protocol": protocol.Version, "error": control})
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(os.Stdout, version)
		return nil
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "request":
		return request(args[1:])
	case "native-worker":
		return nativeWorker(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "/etc/agent-vm-native/config.json", "configuration path")
	manifestPath := flags.String("manifest", "", "validated release manifest path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := manifest.Load(*manifestPath); err != nil {
		return err
	}
	// Establish exclusive control ownership before opening SQLite. The legacy
	// bridge takes the shared side of this same lock before touching state.
	lock, err := native.AcquireLock(filepath.Join(filepath.Dir(cfg.StateDB), "authority.lock"), true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	store, err := state.Open(cfg.StateDB, cfg.AllowCreateDB)
	if err != nil {
		return err
	}
	defer store.Close()
	runner := native.NewRunner()
	guest := native.NewIncusClient(runner, cfg.Project, native.ClientOptions{Profile: cfg.Profile, Network: cfg.Network, Pool: cfg.Pool})
	var snapshots *observe.SnapshotStore
	if cfg.MetricsDir != "" {
		snapshots = observe.NewSnapshotStore(cfg.MetricsDir)
	}
	service := lifecycle.New(store, cfg, lifecycle.Options{Guest: guest, Resident: true, Snapshots: snapshots})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	schedulerInstance := scheduler.New(service)
	go func() {
		if err := httpserver.Serve(ctx, cfg.Socket, service, snapshots); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("control server stopped", "error", err)
			stop()
		}
	}()
	slog.Info("agent vm control started", "socket", cfg.Socket, "database", cfg.StateDB)
	return schedulerInstance.Run(ctx)
}

func request(args []string) error {
	flags := flag.NewFlagSet("request", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socket := flags.String("socket", "/run/agent-vm-native/control.sock", "control socket")
	timeout := flags.Duration("timeout", 240*time.Second, "transport timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, 2*1024*1024+1))
	if err != nil {
		return err
	}
	if len(body) > 2*1024*1024 {
		return protocol.NewError("request_too_large", "request body exceeds the limit", nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	response, status, err := httpserver.Request(ctx, *socket, body)
	if err != nil {
		return err
	}
	_, _ = os.Stdout.Write(response)
	if len(response) == 0 || response[len(response)-1] != '\n' {
		_, _ = os.Stdout.Write([]byte("\n"))
	}
	if status >= 400 {
		return fmt.Errorf("control request returned HTTP %d", status)
	}
	return nil
}

func nativeWorker(args []string) error {
	flags := flag.NewFlagSet("native-worker", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	timeout := flags.Duration("timeout", 300*time.Second, "worker timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	command := flags.Args()
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		return errors.New("native-worker requires an argv after --")
	}
	runner := native.NewRunner()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	if err != nil {
		return err
	}
	result, runErr := runner.Run(ctx, command, input)
	_, _ = os.Stdout.Write(result.Stdout)
	_, _ = os.Stderr.Write(result.Stderr)
	if runErr != nil {
		return runErr
	}
	return nil
}
