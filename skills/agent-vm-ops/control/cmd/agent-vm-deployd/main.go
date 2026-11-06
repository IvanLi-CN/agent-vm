package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
)

var version = "agent-vm-deployd/dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("agent-vm-deployd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "/etc/agent-vm-native/deployd.json", "deployd configuration path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 1 && (flags.Arg(0) == "version" || flags.Arg(0) == "--version") {
		fmt.Println(version)
		return nil
	}
	if flags.NArg() != 0 {
		return errors.New("agent-vm-deployd does not accept positional arguments")
	}
	config, err := deployd.Load(*configPath)
	if err != nil {
		return err
	}
	processLock, err := native.AcquireLock(config.LockPath, true, true)
	if err != nil {
		return fmt.Errorf("acquire deployd singleton lock: %w", err)
	}
	defer processLock.Close()
	database, err := store.Open(config.Database, config.Retention(), time.Now)
	if err != nil {
		return err
	}
	defer database.Close()
	runtime := deployd.NewHostRuntime(config)
	manager := deployd.NewManagerWithMigration(database, runtime, runtime, config.LeaseDuration(), time.Now)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := manager.RecoverOnStart(ctx); err != nil {
		slog.Error("deployd recovery observation failed", "error", err)
	}
	if err := manager.MaintainController(ctx); err != nil {
		slog.Warn("initial controller lease was not granted", "error", err)
	}
	metrics := deployd.NewMetrics(manager)
	server := deployd.NewServer(manager, metrics)
	if err := server.RestoreLatest(ctx); err != nil {
		slog.Warn("persisted deployd observation could not be restored", "error", err)
	}
	if err := server.Sample(ctx); err != nil {
		slog.Warn("initial deployd observation failed", "error", err)
	}

	errorsCh := make(chan error, 2)
	go func() { errorsCh <- deployd.ServeUDS(ctx, config.Socket, server.Handler()) }()
	go func() {
		errorsCh <- deployd.ServeMetrics(ctx, config.MetricsListen, server.MetricsHandler(), config.MetricsTLS, config.NetworkACL)
	}()
	go maintenanceLoop(ctx, manager, config.ObserveInterval())
	go publicationLoop(ctx, server, config.ObserveInterval())

	slog.Info("agent vm deployd started", "version", version, "socket", config.Socket, "metrics_listen", config.MetricsListen)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errorsCh:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, os.ErrClosed) {
			return err
		}
		return nil
	}
}

func maintenanceLoop(ctx context.Context, manager *deployd.Manager, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	sampleTicker := time.NewTicker(interval)
	cleanupTicker := time.NewTicker(time.Hour)
	defer sampleTicker.Stop()
	defer cleanupTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sampleTicker.C:
			if err := manager.MaintainController(ctx); err != nil {
				slog.Warn("controller lease maintenance failed", "error", err)
			}
		case <-cleanupTicker.C:
			if err := manager.Cleanup(ctx); err != nil {
				slog.Warn("deployd retention cleanup failed", "error", err)
			}
			if err := manager.CleanupMigrationBackups(ctx); err != nil {
				slog.Warn("migration backup cleanup failed", "error", err)
			}
		}
	}
}

// Publish only completed persisted observations, independently of the serialized
// role/transaction writer. This loop never probes slots or grants authority.
func publicationLoop(ctx context.Context, server *deployd.Server, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-server.Manager.Store.ObservationUpdates():
		}
		if err := server.RestoreLatest(ctx); err != nil {
			slog.Warn("deployd snapshot publication failed", "error", err)
		}
	}
}
