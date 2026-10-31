package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/httpserver"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/management"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/manifest"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/scheduler"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/webadmin"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/webassets"
	"golang.org/x/sys/unix"
)

var version = "agent-vm-control/dev"

const installedManifestPath = "/opt/agent-vm-native/release-manifest.json"

type exitError struct {
	code    int
	cause   error
	emitted bool
}

func (err *exitError) Error() string { return err.cause.Error() }
func (err *exitError) Unwrap() error { return err.cause }

func main() {
	if err := run(os.Args[1:]); err != nil {
		var processExit *exitError
		if errors.As(err, &processExit) {
			if !processExit.emitted {
				fmt.Fprintln(os.Stderr, err)
			}
			os.Exit(processExit.code)
		}
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
	case "web":
		return web(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "/etc/agent-vm-native/config.json", "configuration path")
	manifestPath := flags.String("manifest", installedManifestPath, "validated release manifest path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *manifestPath != installedManifestPath {
		return errors.New("manifest_invalid: serve requires the fixed installed release manifest path")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	release, err := manifest.LoadMetadata(*manifestPath)
	if err != nil {
		return err
	}
	// Local SQLite remains single-writer with the legacy bridge. Remote rqlite
	// is the shared blue-green authority and must not be guarded by one process
	// exclusive lock; lifecycle resources use named locks below instead.
	var authorityLock *native.AuthorityLock
	if !remoteStateDSN(cfg.StateDB) {
		authorityLock, err = native.AcquireLock(filepath.Join(filepath.Dir(cfg.StateDB), "authority.lock"), true, false)
		if err != nil {
			return err
		}
		defer authorityLock.Close()
	}
	store, err := state.Open(cfg.StateDB, cfg.AllowCreateDB)
	if err != nil {
		return err
	}
	defer store.Close()
	managementStateSupported := len(release.StateReads) > 0 && len(release.StateWrites) > 0
	if cfg.Management.Enabled && !managementStateSupported {
		return errors.New("management_disabled: installed release does not declare management state compatibility")
	}
	if managementStateSupported {
		if err := store.EnsureManagementSchema(context.Background()); err != nil {
			return fmt.Errorf("initialize management schema: %w", err)
		}
		// SQLite is single-writer, but the staging directory is also shared by
		// the two resident services. Hold the same resource lock used by the
		// upload path before removing files left by a crashed peer.
		stagingLock, err := native.AcquireNamedLock(context.Background(), filepath.Join(cfg.StoragePath, "locks"), "management-resource:image-staging")
		if err != nil {
			return fmt.Errorf("lock upload staging: %w", err)
		}
		cleanupErr := webadmin.CleanupUploadStaging(cfg.Management.Upload.StagingPath)
		closeErr := stagingLock.Close()
		if cleanupErr != nil {
			return fmt.Errorf("clean upload staging: %w", cleanupErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close upload staging lock: %w", closeErr)
		}
		if err := store.SeedLegacyImage(context.Background(), cfg.ImageFingerprint); err != nil {
			return fmt.Errorf("initialize managed image catalog: %w", err)
		}
		capabilities, err := store.Capabilities(context.Background())
		if err != nil {
			return fmt.Errorf("read state capabilities: %w", err)
		}
		capabilityNames := make([]string, 0, len(capabilities))
		for _, capability := range capabilities {
			capabilityNames = append(capabilityNames, capability.Name+"@"+strconv.FormatInt(capability.Version, 10))
		}
		if err := manifest.ValidateStateCapabilities(release, capabilityNames); err != nil {
			return err
		}
	}
	runner := native.NewRunner()
	guest := native.NewIncusClient(runner, cfg.Project, native.ClientOptions{Profile: cfg.Profile, Network: cfg.Network, Pool: cfg.Pool, StoragePath: cfg.StoragePath})
	var snapshots *observe.SnapshotStore
	if cfg.MetricsDir != "" {
		snapshots = observe.NewSnapshotStore(cfg.MetricsDir)
	}
	var releaseMetadata *protocol.ReleaseMetadata
	if release.ReleaseID != "" {
		actions := make(map[string]protocol.ActionCompatibility, len(release.Actions))
		for action, item := range release.Actions {
			actions[action] = protocol.ActionCompatibility{
				Request:  protocol.Schema{Schema: item.Request.Schema, SemanticRevision: item.Request.SemanticRevision},
				Response: protocol.Schema{Schema: item.Response.Schema, SemanticRevision: item.Response.SemanticRevision},
				RequestWindow: protocol.CompatibilityWindow{
					SchemaMin: item.RequestWindow.SchemaMin, SchemaMax: item.RequestWindow.SchemaMax,
					SemanticMin: item.RequestWindow.SemanticMin, SemanticMax: item.RequestWindow.SemanticMax,
				},
				ResponseWindow: protocol.CompatibilityWindow{
					SchemaMin: item.ResponseWindow.SchemaMin, SchemaMax: item.ResponseWindow.SchemaMax,
					SemanticMin: item.ResponseWindow.SemanticMin, SemanticMax: item.ResponseWindow.SemanticMax,
				},
			}
		}
		releaseMetadata = &protocol.ReleaseMetadata{
			Version: release.Version, ReleaseID: release.ReleaseID, SourceRevision: release.SourceRevision, Protocol: release.Protocol,
			Transport: release.Transport, ManifestPresent: release.ManifestPresent, ArtifactsVerified: release.ArtifactsVerified,
			Actions: actions,
		}
	}
	service := lifecycle.New(store, cfg, lifecycle.Options{Guest: guest, Resident: true, Snapshots: snapshots, CPU: observe.NewCPUObserver(), Memory: observe.NewMemoryObserver(), Release: releaseMetadata, LockDir: filepath.Join(cfg.StoragePath, "locks")})
	imageBackend := management.NewIncusImageBackend(guest, cfg.Management.Upload.StagingPath, cfg.RequiredGuestTools)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	managementOptions := management.Options{
		TrustedImageSourceIDs:  cfg.Management.TrustedImageSourceIDs,
		UploadMaxBytes:         cfg.Management.Upload.MaxBytes,
		UploadMinimumFreeBytes: cfg.Management.Upload.MinimumFreeBytes,
		AvailableImageStorageBytes: func() (int64, error) {
			return guest.StoragePoolAvailableBytes(context.Background())
		},
		ReserveImageStorage: func(size int64) (func(), error) {
			return service.ReservePhysicalStorage(context.Background(), size)
		},
		CurrentSystemVersion: func(ctx context.Context) (string, error) {
			epoch, err := store.StateEpoch(ctx)
			if err != nil {
				return "", err
			}
			return "system:" + epoch, nil
		},
		IsDraining: func(ctx context.Context) (bool, error) {
			return service.IsDraining(ctx)
		},
		Clock:            time.Now,
		Backend:          imageBackend,
		Actions:          &webadmin.LifecycleActions{Service: service, Store: store},
		OperationLockDir: filepath.Join(cfg.StoragePath, "locks"),
	}
	if cfg.Management.Upload.Enabled {
		managementOptions.AvailableStagingBytes = func() (int64, error) {
			return availableStagingBytes(cfg.Management.Upload.StagingPath)
		}
	}
	var managementService *management.Service
	if cfg.Management.Enabled {
		managementService = management.New(store, managementOptions)
		if err := managementService.RecoverPendingOperations(ctx); err != nil {
			return fmt.Errorf("recover management operations: %w", err)
		}
	}
	if cfg.Management.Enabled {
		authority, authorityErr := webadmin.NewAuthority(webadmin.AuthorityOptions{
			Config: cfg.Management, Management: managementService, Lifecycle: service, Store: store, Snapshots: snapshots, ProductVersion: release.Version,
		})
		if authorityErr != nil {
			return authorityErr
		}
		if err := webassets.Verify(); err != nil {
			return err
		}
		unified, unifiedErr := webadmin.NewUnifiedServer(webadmin.UnifiedOptions{
			Config: cfg.Management, Authority: authority, Lifecycle: service, Store: store,
			Snapshots: snapshots, ProductVersion: release.Version, Assets: webassets.Files,
		})
		if unifiedErr != nil {
			return unifiedErr
		}
		var app http.Handler = unified.Handler()
		go func() {
			if err := httpserver.ServeApp(ctx, cfg.Socket, service, snapshots, app); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("unified control server stopped", "error", err)
				stop()
			}
		}()
	} else {
		go func() {
			if err := httpserver.Serve(ctx, cfg.Socket, service, snapshots); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("control server stopped", "error", err)
				stop()
			}
		}()
	}
	schedulerInstance := scheduler.New(service)
	slog.Info("agent vm control started", "socket", cfg.Socket, "database_backend", stateBackend(cfg.StateDB))
	return schedulerInstance.Run(ctx)
}

func stateBackend(value string) string {
	if remoteStateDSN(value) {
		return "rqlite"
	}
	return "sqlite"
}

func remoteStateDSN(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func web(args []string) error {
	_ = args
	return errors.New("management web command is retired; use agent-vm-control serve")
}

func availableStagingBytes(path string) (int64, error) {
	if path == "" {
		return 0, errors.New("staging path is empty")
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bavail < 0 || stat.Bsize < 0 || uint64(stat.Bavail) > uint64(^uint64(0))/uint64(stat.Bsize) {
		return 0, errors.New("staging capacity is out of range")
	}
	bytes := uint64(stat.Bavail) * uint64(stat.Bsize)
	if bytes > uint64(^uint64(0)>>1) {
		return 0, errors.New("staging capacity exceeds int64")
	}
	return int64(bytes), nil
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
		control := protocol.NewError("request_too_large", "request body exceeds the limit", nil)
		payload, _ := json.Marshal(map[string]any{"ok": false, "protocol": protocol.Version, "error": control})
		_, _ = os.Stderr.Write(append(payload, '\n'))
		return &exitError{code: 2, cause: control, emitted: true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	response, status, exitCode, err := httpserver.RequestWithExitCode(ctx, *socket, body)
	if err != nil {
		return &exitError{code: 3, cause: err}
	}
	if exitCode != 0 {
		_, _ = os.Stderr.Write(response)
		if len(response) == 0 || response[len(response)-1] != '\n' {
			_, _ = os.Stderr.Write([]byte("\n"))
		}
		return &exitError{code: exitCode, cause: fmt.Errorf("control request returned HTTP %d", status), emitted: true}
	}
	_, _ = os.Stdout.Write(response)
	if len(response) == 0 || response[len(response)-1] != '\n' {
		_, _ = os.Stdout.Write([]byte("\n"))
	}
	if status >= 400 {
		return &exitError{code: 3, cause: fmt.Errorf("successful control response returned HTTP %d", status)}
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
