// Command testslot is an isolated runtime used by the Agent VM Deployd
// systemd acceptance test. It is never included in a release bundle.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const protocol = 4

type configuration struct {
	Slot   string `json:"slot"`
	Socket string `json:"socket"`
}

type releaseManifest struct {
	ReleaseID      string `json:"release_id"`
	SourceRevision string `json:"source_revision"`
}

type runtimeState struct {
	sync.Mutex
	controller bool
	traffic    bool
	draining   bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "serve" {
		return errors.New("testslot requires the serve command")
	}
	flags := flag.NewFlagSet("testslot serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "slot configuration")
	manifestPath := flags.String("manifest", "", "slot release manifest")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *configPath == "" || *manifestPath == "" {
		return errors.New("testslot requires --config and --manifest")
	}
	var config configuration
	if err := readJSON(*configPath, &config); err != nil {
		return fmt.Errorf("read slot config: %w", err)
	}
	if config.Slot != "blue" && config.Slot != "green" {
		return errors.New("slot must be blue or green")
	}
	if config.Socket == "" || !filepath.IsAbs(config.Socket) {
		return errors.New("slot socket must be absolute")
	}
	var manifest releaseManifest
	if err := readJSON(*manifestPath, &manifest); err != nil {
		return fmt.Errorf("read release manifest: %w", err)
	}
	if manifest.ReleaseID == "" || manifest.SourceRevision == "" {
		return errors.New("release manifest identity is incomplete")
	}
	return serve(config, manifest)
}

func serve(config configuration, manifest releaseManifest) error {
	if err := os.MkdirAll(filepath.Dir(config.Socket), 0o750); err != nil {
		return err
	}
	if info, err := os.Lstat(config.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("slot socket is not a socket")
		}
		_ = os.Remove(config.Socket)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", config.Socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(config.Socket, 0o660); err != nil {
		_ = listener.Close()
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(config.Socket)
	}()

	state := &runtimeState{}
	startToken := time.Now().UTC().Format(time.RFC3339Nano)
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/v1/health", func(writer http.ResponseWriter, _ *http.Request) {
		state.Lock()
		controller, traffic, draining := state.controller, state.traffic, state.draining
		state.Unlock()
		writeJSON(writer, map[string]any{
			"ok":                 true,
			"protocol":           protocol,
			"process_ready":      true,
			"state_authority":    "authority-1",
			"ready":              true,
			"controller_owned":   controller,
			"traffic_accepting":  traffic && !draining,
			"release_id":         manifest.ReleaseID,
			"source_revision":    manifest.SourceRevision,
			"process_id":         os.Getpid(),
			"process_start_time": startToken,
		})
	})
	mux.HandleFunc("/internal/v1/control", func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			writeJSONStatus(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
			return
		}
		action, _ := payload["action"].(string)
		result := map[string]any{}
		state.Lock()
		switch action {
		case "admin-metrics":
			result = map[string]any{
				"up": true, "observed_at": time.Now().UTC().Format(time.RFC3339Nano),
				"observation_consistent": true,
				"observation_epoch":      "epoch-1", "pending_operations": []any{},
				"historical_unknown_operations": []any{}, "in_flight_operations": 0,
				"doctor": map[string]any{"ready": true, "issues": []any{}},
			}
		case "admin-status":
			result = map[string]any{"up": true, "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "tasks": []any{}, "limit": 25, "next_cursor": nil, "partial": false, "unknown": 0, "observation_consistent": true, "probed": 0, "verified": 0, "timed_out": 0}
		case "doctor":
			result = map[string]any{"up": true, "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "state_epoch": "epoch-1", "issues": []any{}}
		case "drain-status":
			result = map[string]any{"draining": state.draining, "ready": true, "state_capabilities": []any{"legacy-agent-vm-core@1"}}
		case "drain-enter":
			state.draining = true
			result = map[string]any{"draining": true}
		case "drain-exit":
			state.draining = false
			result = map[string]any{"draining": false}
		default:
			state.Unlock()
			writeJSONStatus(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported action"})
			return
		}
		state.Unlock()
		writeJSON(writer, map[string]any{"ok": true, "protocol": protocol, "action": action, "result": result})
	})
	mux.HandleFunc("/internal/v1/controller-role", func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			writeJSONStatus(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
			return
		}
		state.Lock()
		switch payload.Action {
		case "grant":
			state.controller = true
		case "revoke", "fence":
			state.controller = false
		default:
			state.Unlock()
			writeJSONStatus(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported action"})
			return
		}
		valid := state.controller
		state.Unlock()
		writeJSON(writer, map[string]any{"ok": true, "protocol": protocol, "valid": valid})
	})
	mux.HandleFunc("/internal/v1/traffic-role", func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			writeJSONStatus(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
			return
		}
		state.Lock()
		switch payload.Action {
		case "enable":
			state.traffic = true
		case "disable":
			state.traffic = false
		default:
			state.Unlock()
			writeJSONStatus(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported action"})
			return
		}
		accepting := state.traffic && !state.draining
		state.Unlock()
		writeJSON(writer, map[string]any{"ok": true, "protocol": protocol, "traffic_accepting": accepting})
	})
	mux.HandleFunc("/internal/v1/snapshots/", func(writer http.ResponseWriter, request *http.Request) {
		name := filepath.Base(request.URL.Path)
		var result map[string]any
		switch name {
		case "admin-metrics":
			result = map[string]any{
				"up": true, "observed_at": time.Now().UTC().Format(time.RFC3339Nano),
				"observation_consistent": true,
				"observation_epoch":      "epoch-1", "pending_operations": []any{},
				"historical_unknown_operations": []any{}, "in_flight_operations": 0,
				"doctor": map[string]any{"ready": true, "issues": []any{}},
			}
		case "admin-status":
			result = map[string]any{"up": true, "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "tasks": []any{}, "limit": 25, "next_cursor": nil, "partial": false, "unknown": 0, "observation_consistent": true, "probed": 0, "verified": 0, "timed_out": 0}
		case "doctor":
			result = map[string]any{"up": true, "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "state_epoch": "epoch-1", "issues": []any{}}
		default:
			writeJSONStatus(writer, http.StatusNotFound, map[string]any{"ok": false, "error": "snapshot not found"})
			return
		}
		writeJSON(writer, map[string]any{"ok": true, "protocol": protocol, "result": result})
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func readJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	return decoder.Decode(target)
}

func writeJSON(writer http.ResponseWriter, value map[string]any) {
	writeJSONStatus(writer, http.StatusOK, value)
}

func writeJSONStatus(writer http.ResponseWriter, status int, value map[string]any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
