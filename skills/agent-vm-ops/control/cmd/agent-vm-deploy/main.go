package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/install"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/releasefetch"
)

var version = "agent-vm-deploy/dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, publicError(err))
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "version" || args[0] == "--version" {
		fmt.Println(version)
		return nil
	}
	if args[0] == "install" {
		return runInstall(args[1:])
	}
	if args[0] == "wait-process" {
		return runWaitProcess(args[1:])
	}
	if args[0] == "wait-authority" {
		return runWaitAuthority(args[1:])
	}
	if args[0] == "update" {
		return runUpdate(args[1:])
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socket := flags.String("socket", "/run/agent-vm-native/deployd.sock", "deployd Unix socket")
	id := flags.String("transaction", "", "deployment transaction ID")
	slot := flags.String("slot", "", "target slot")
	bundle := flags.String("bundle", "", "verified release bundle")
	action := flags.String("action", "", "recovery action")
	timeoutSeconds := flags.Int("timeout", 60, "request timeout in seconds")
	jsonOutput := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	_ = jsonOutput
	if command == "migrate" && *timeoutSeconds == 60 {
		*timeoutSeconds = 9000
	}
	if *timeoutSeconds <= 0 {
		return errors.New("timeout must be positive")
	}
	client := deployd.Client{Socket: *socket, Timeout: time.Duration(*timeoutSeconds) * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSeconds)*time.Second)
	defer cancel()
	var method, path string
	var body any
	switch command {
	case "status":
		if *id == "" {
			method, path = "GET", "/v1/status"
		} else {
			method, path = "GET", "/v1/transactions/"+url.PathEscape(*id)
		}
	case "transaction":
		if *id == "" {
			return errors.New("transaction requires --transaction")
		}
		method, path = "GET", "/v1/transactions/"+url.PathEscape(*id)
	case "plan":
		if *slot == "" {
			return errors.New("plan requires --slot")
		}
		method, path, body = "POST", "/v1/plan", map[string]any{"target_slot": *slot, "bundle": *bundle}
	case "stage":
		if *slot == "" || *bundle == "" {
			return errors.New("stage requires --slot and --bundle")
		}
		method, path, body = "POST", "/v1/stage", map[string]any{"target_slot": *slot, "bundle": *bundle}
	case "verify", "activate", "rollback":
		if *id == "" {
			return fmt.Errorf("%s requires --transaction", command)
		}
		method, path, body = "POST", "/v1/"+command, map[string]any{"transaction_id": *id}
	case "recover":
		if *id == "" || *action == "" {
			return errors.New("recover requires --transaction and --action")
		}
		method, path, body = "POST", "/v1/recover", map[string]any{"transaction_id": *id, "action": *action}
	case "migrate":
		method, path, body = "POST", "/v1/migrate", map[string]any{}
	default:
		return fmt.Errorf("unknown deployd command %q", command)
	}
	payload, _, err := client.Call(ctx, method, path, body)
	if err != nil {
		if len(payload) > 0 {
			_, _ = os.Stderr.Write(payload)
			_, _ = os.Stderr.Write([]byte("\n"))
		}
		return err
	}
	_, _ = os.Stdout.Write(payload)
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		_, _ = os.Stdout.Write([]byte("\n"))
	}
	return nil
}

func runUpdate(args []string) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	version := flags.String("version", "", "published release SemVer without v")
	source := flags.String("source-revision", "", "expected 40-character source revision")
	socket := flags.String("socket", "/run/agent-vm-native/deployd.sock", "deployd Unix socket")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *version == "" || *source == "" {
		return errors.New("update requires --version and --source-revision")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	const stagingRoot = "/var/lib/agent-vm-native/release-staging"
	if err := os.MkdirAll(stagingRoot, 0o750); err != nil {
		return fmt.Errorf("create release staging root: %w", err)
	}
	directory, err := os.MkdirTemp(stagingRoot, ".update-")
	if err != nil {
		return fmt.Errorf("create release staging directory: %w", err)
	}
	preserveStaging := false
	defer func() {
		if !preserveStaging {
			_ = os.RemoveAll(directory)
		}
	}()
	bundle, err := releasefetch.Fetch(ctx, *version, *source, directory)
	if err != nil {
		return err
	}
	client := deployd.Client{Socket: *socket, Timeout: 15 * time.Minute}
	statusPayload, _, err := client.Call(ctx, "GET", "/v1/status", nil)
	if err != nil {
		// statusPayload is intentionally bounded by deployd.Client and preserves
		// the structured blocker for operators.
		writePayload(os.Stderr, statusPayload)
		return err
	}
	var status struct {
		Value struct {
			Observation struct {
				ActiveSlot string `json:"active_slot"`
			} `json:"observation"`
		} `json:"value"`
	}
	if err := json.Unmarshal(statusPayload, &status); err != nil || (status.Value.Observation.ActiveSlot != "blue" && status.Value.Observation.ActiveSlot != "green") {
		return errors.New("status response missing active slot")
	}
	targetSlot := "blue"
	if status.Value.Observation.ActiveSlot == "blue" {
		targetSlot = "green"
	}
	var result struct {
		Value struct {
			ID string `json:"transaction_id"`
		} `json:"value"`
	}
	payload, _, err := client.Call(ctx, "POST", "/v1/stage", map[string]any{"target_slot": targetSlot, "bundle": filepath.Clean(bundle)})
	if err != nil {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(payload, &envelope)
		preserveStaging = len(payload) == 0 || envelope.Error.Code == "state_unverified"
		writePayload(os.Stderr, payload)
		return err
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		preserveStaging = true
		return fmt.Errorf("stage response invalid: %w", err)
	}
	id := result.Value.ID
	if id == "" {
		preserveStaging = true
		return errors.New("stage response missing transaction ID")
	}
	preserveStaging = true
	for _, step := range []struct{ action, phase, state string }{
		{action: "verify", phase: "controller_handoff", state: "active"},
		{action: "activate", phase: "finalize", state: "finalized"},
	} {
		payload, _, err = client.Call(ctx, "POST", "/v1/"+step.action, map[string]any{"transaction_id": id})
		if err != nil {
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(payload, &envelope)
			preserveStaging = len(payload) == 0 || envelope.Error.Code == "state_unverified"
			writePayload(os.Stderr, payload)
			return err
		}
		if !validTransactionAcknowledgement(payload, id, step.phase, step.state) {
			preserveStaging = true
			return errors.New("deployd acknowledgement is incomplete")
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		// The Deployd transaction is already finalized. Keep the successful
		// acknowledgement as the command result and surface cleanup separately.
		fmt.Fprintln(os.Stderr, `{"code":"release_staging_cleanup_failed","phase":"staging_cleanup","retryable":true,"reason":"staging_cleanup_failed","next_action":"remove_staging_after_verification"}`)
	}
	if _, statErr := os.Stat(directory); os.IsNotExist(statErr) {
		preserveStaging = false
	}
	_, _ = os.Stdout.Write(payload)
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		_, _ = os.Stdout.Write([]byte("\n"))
	}
	return nil
}

func publicError(err error) string {
	value := err.Error()
	for _, path := range []string{"/var/lib/agent-vm-native", "/run/agent-vm-native", "/etc/agent-vm-native"} {
		value = strings.ReplaceAll(value, path, "<private-path>")
	}
	return value
}

func validTransactionAcknowledgement(payload []byte, expectedID, expectedPhase, expectedState string) bool {
	var response struct {
		OK    bool `json:"ok"`
		Value struct {
			ID    string `json:"transaction_id"`
			Phase string `json:"phase"`
			State string `json:"state"`
		} `json:"value"`
	}
	if json.Unmarshal(payload, &response) != nil {
		return false
	}
	return response.OK && response.Value.ID == expectedID && response.Value.Phase == expectedPhase && response.Value.State == expectedState
}

func writePayload(out *os.File, payload []byte) {
	if len(payload) == 0 {
		return
	}
	const maxDiagnosticBytes = 64 << 10
	if len(payload) > maxDiagnosticBytes {
		payload = append(append([]byte(nil), payload[:maxDiagnosticBytes]...), []byte("\n[diagnostic truncated]\n")...)
	}
	_, _ = out.Write(payload)
	if payload[len(payload)-1] != '\n' {
		_, _ = out.Write([]byte("\n"))
	}
}

func runInstall(args []string) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	config := flags.String("config", "", "protected installation input JSON")
	bundle := flags.String("bundle", "", "verified product tar archive")
	materials := flags.String("materials", "", "matching verified deployment materials directory")
	apply := flags.Bool("apply", false, "apply installation after preflight")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *config == "" || *bundle == "" || *materials == "" {
		return errors.New("install requires --config, --bundle and --materials; --apply enables changes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	input := install.Inputs{Config: *config, Bundle: *bundle, Materials: *materials}
	var report install.Report
	var err error
	if *apply {
		report, err = install.Apply(ctx, input)
	} else {
		report, err = install.Preflight(ctx, input)
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(report); encodeErr != nil {
		return encodeErr
	}
	return err
}
