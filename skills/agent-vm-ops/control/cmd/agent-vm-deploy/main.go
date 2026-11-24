package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/install"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
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
	if args[0] == "serve" {
		return runServe(args[1:])
	}
	if args[0] == "host-action" {
		if len(args) != 1 {
			return errors.New("host-action is a systemd socket handler")
		}
		return deployd.RunHostAction(os.Stdin)
	}
	if args[0] == "host-observe" {
		if len(args) != 1 {
			return errors.New("host-observe is a systemd socket handler")
		}
		return deployd.RunHostObservation(os.Stdin)
	}
	if args[0] == "legacy-incus" {
		return runLegacyIncus(args[1:])
	}
	if args[0] == "prepare-legacy-slot-access" {
		if len(args) != 2 {
			return errors.New("prepare-legacy-slot-access requires one fixed slot")
		}
		return install.PrepareLegacySlotAccess(args[1])
	}
	if args[0] == "slot-relay" {
		if len(args) != 1 {
			return errors.New("slot-relay is a systemd socket handler")
		}
		return deployd.RunSlotRelay(os.Stdin)
	}
	if args[0] == "request-relay" {
		if len(args) != 1 {
			return errors.New("request-relay is a systemd socket handler")
		}
		return deployd.RunRequestRelay(os.Stdin)
	}
	if args[0] == "request-relay-child" {
		if len(args) != 2 {
			return errors.New("request-relay-child requires one fixed slot")
		}
		return deployd.RunRequestRelayChild(args[1], os.Stdin, os.Stdout)
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
	if args[0] == "maintenance-binary" {
		return runMaintenanceBinary(args[1:])
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socket := flags.String("socket", "/run/agent-vm-native/deployd.sock", "deployd Unix socket")
	id := flags.String("transaction", "", "deployment transaction ID")
	slot := flags.String("slot", "", "target slot")
	bundle := flags.String("bundle", "", "verified release bundle")
	action := flags.String("action", "", "recovery action")
	timeoutSeconds := flags.Int("timeout", 0, "request timeout in seconds (defaults to the command's operation budget)")
	jsonOutput := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	_ = jsonOutput
	explicitTimeout := false
	flags.Visit(func(option *flag.Flag) { explicitTimeout = explicitTimeout || option.Name == "timeout" })
	if !explicitTimeout {
		switch {
		case command == "migrate":
			*timeoutSeconds = 9000
		case command == "recover" && *action == "retry-stage":
			*timeoutSeconds = 900
		case command == "recover" && *action == "complete-forward":
			*timeoutSeconds = 900
		case command == "rollback" || (command == "recover" && *action == "rollback"):
			*timeoutSeconds = 480
		default:
			*timeoutSeconds = 60
		}
	}
	if *timeoutSeconds <= 0 {
		return errors.New("timeout must be positive")
	}
	client := deployd.Client{Socket: *socket, Timeout: time.Duration(*timeoutSeconds) * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSeconds)*time.Second)
	defer cancel()
	if commandMutatesDeployment(command) {
		updateLock, lockErr := native.AcquireLock("/run/agent-vm-native/release-update.lock", true, true)
		if lockErr != nil {
			return errors.New("another release update is already in progress")
		}
		defer updateLock.Close()
	}
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
		request := map[string]any{"transaction_id": *id, "action": *action}
		if *action == "complete-forward" {
			if *bundle == "" {
				return errors.New("recover --action complete-forward requires --bundle")
			}
			request["bundle"] = *bundle
		} else if *bundle != "" {
			return errors.New("only recover --action complete-forward accepts --bundle")
		}
		method, path, body = "POST", "/v1/recover", request
	case "migrate":
		method, path, body = "POST", "/v1/migrate", map[string]any{}
	case "recovery-points":
		method, path = "GET", "/v1/recovery-points"
	case "prune-recovery-points":
		method, path, body = "POST", "/v1/recovery-points/prune", map[string]any{}
	default:
		return fmt.Errorf("unknown deployd command %q", command)
	}
	payload, _, err := client.Call(ctx, method, path, body)
	if err != nil {
		if len(payload) > 0 {
			writePayload(os.Stderr, payload)
		}
		return err
	}
	_, _ = os.Stdout.Write(payload)
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		_, _ = os.Stdout.Write([]byte("\n"))
	}
	return nil
}

func commandMutatesDeployment(command string) bool {
	switch command {
	case "plan", "stage", "verify", "activate", "rollback", "recover", "migrate":
		return true
	default:
		return false
	}
}

func runUpdate(args []string) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	version := flags.String("version", "", "published release SemVer without v")
	source := flags.String("source-revision", "", "expected 40-character source revision")
	localBundle := flags.String("bundle", "", "protected local copy of the published bundle")
	serverFile := flags.String("server-file", "", "protected local file identical to the published server")
	socket := flags.String("socket", "/run/agent-vm-native/deployd.sock", "deployd Unix socket")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *version == "" || *source == "" {
		return errors.New("update requires --version and --source-revision")
	}
	if *serverFile != "" && *localBundle == "" {
		return errors.New("--server-file requires --bundle")
	}
	baseContext, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	ctx, cancel := context.WithTimeout(baseContext, 15*time.Minute)
	defer cancel()
	updateLock, err := native.AcquireLock("/run/agent-vm-native/release-update.lock", true, true)
	if err != nil {
		return errors.New("another release update is already in progress")
	}
	defer updateLock.Close()
	client := deployd.Client{Socket: *socket, Timeout: 15 * time.Minute}
	if *socket == deployd.Defaults().Socket {
		payload, _, err := client.Call(ctx, "GET", "/v1/status", nil)
		if err != nil {
			writePayload(os.Stderr, payload)
			return err
		}
		if !dedicatedUpdateReady(payload) {
			return errors.New("host permission migration required before updating; see deploy/docs/blue-green.md")
		}
	}
	const stagingRoot = "/var/lib/agent-vm-native/release-staging"
	if err := os.MkdirAll(stagingRoot, 0o750); err != nil {
		return fmt.Errorf("create release staging root: %w", err)
	}
	cleanupOrphanStaging(ctx, client, stagingRoot)
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
	bundle, err := releasefetch.Prepare(ctx, *version, *source, directory, *localBundle, *serverFile)
	if err != nil {
		return err
	}
	if err := exposeVerifiedBundle(directory, bundle); err != nil {
		return err
	}
	statusPayload, _, err := client.Call(ctx, "GET", "/v1/status", nil)
	if err != nil {
		// statusPayload is intentionally bounded by deployd.Client and preserves
		// the structured blocker for operators.
		writePayload(os.Stderr, statusPayload)
		return err
	}
	var status struct {
		OK    bool `json:"ok"`
		Value struct {
			Observation struct {
				ActiveSlot string `json:"active_slot"`
			} `json:"observation"`
		} `json:"value"`
	}
	if err := json.Unmarshal(statusPayload, &status); err != nil || !status.OK || (status.Value.Observation.ActiveSlot != "blue" && status.Value.Observation.ActiveSlot != "green") {
		writeUpdateRecoveryHint(ctx, client, "")
		return errors.New("status response missing active slot")
	}
	targetSlot := "blue"
	if status.Value.Observation.ActiveSlot == "blue" {
		targetSlot = "green"
	}
	// Keep candidate bytes after any request that may have crossed the effect barrier.
	preserveStaging = true
	receipt, err := client.RunBlueGreen(ctx, targetSlot, bundle)
	payload := receipt.Payload
	if err != nil {
		writePayload(os.Stderr, payload)
		if !hasStructuredError(payload) {
			writeUpdateRecoveryHint(ctx, client, receipt.ID)
		}
		return err
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
	writeMaintenanceStatus(payload)
	return nil
}

// writeMaintenanceStatus names the host actions that the blue-green
// transaction does not perform. Without it a successful update silently leaves
// the stable deployd binary on the previous release.
func writeMaintenanceStatus(transactionPayload []byte) {
	var envelope struct {
		Value struct {
			TargetSlot string `json:"target_slot"`
		} `json:"value"`
	}
	if json.Unmarshal(transactionPayload, &envelope) != nil || envelope.Value.TargetSlot == "" {
		return
	}
	maintenance := "current"
	if served, err := install.ServedDeployBinaryVersion(); err == nil && served != "" && served != version {
		maintenance = "pending"
	}
	maintenanceAction := "none"
	if maintenance != "current" {
		maintenanceAction = install.MaintenanceBinaryPath + ": install the release binary and restart agent-vm-deployd.service"
	}
	status := map[string]any{
		"maintenance_binary":        maintenance,
		"next_action":               maintenanceAction,
		"deployed_slot":             envelope.Value.TargetSlot,
		"slot_binary_version":       version,
		"recovery_points":           recoveryPointSummary(transactionPayload),
		"service_accounts_modified": false,
	}
	if served, err := install.ServedDeployBinaryVersion(); err == nil && served != "" {
		status["maintenance_binary_version"] = served
	}
	encoded, _ := json.Marshal(status)
	_, _ = os.Stderr.Write(append(encoded, '\n'))
}

func recoveryPointSummary(transactionPayload []byte) map[string]any {
	summary := map[string]any{"count": 0, "budget": recoveryPointBudgetHint}
	status, _, err := deployd.Client{Socket: deployd.Defaults().Socket, Timeout: 10 * time.Second}.Call(context.Background(), "GET", "/v1/recovery-points", nil)
	if err != nil {
		return summary
	}
	var envelope struct {
		Value struct {
			Total    int `json:"total"`
			Budget   int `json:"budget_units"`
			Eligible int
			Points   []struct {
				Eligible bool `json:"eligible"`
			} `json:"points"`
		} `json:"value"`
	}
	if json.Unmarshal(status, &envelope) != nil {
		return summary
	}
	summary["count"] = envelope.Value.Total
	if envelope.Value.Budget > 0 {
		summary["budget"] = envelope.Value.Budget
	}
	for _, point := range envelope.Value.Points {
		if point.Eligible {
			summary["eligible"] = summary["eligible"].(int) + 1
		}
	}
	if envelope.Value.Total >= envelope.Value.Budget {
		summary["next_action"] = "agent-vm-deploy prune-recovery-points"
	}
	return summary
}

// runMaintenanceBinary separates the two steps a blue-green update never
// performs: replacing the stable deployd executable and restarting its unit.
// Splitting them keeps the transaction finished before deployd restarts.
func runMaintenanceBinary(args []string) error {
	flags := flag.NewFlagSet("maintenance-binary", flag.ContinueOnError)
	product := flags.String("product", "", "extracted release product directory holding bin/agent-vm-deploy")
	installOnly := flags.Bool("install", false, "replace the stable binary only")
	restartOnly := flags.Bool("restart", false, "restart agent-vm-deployd.service only")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *installOnly == *restartOnly || (*installOnly && *product == "") || (*restartOnly && *product != "") {
		return errors.New("maintenance-binary requires exactly one of --install/--product or --restart")
	}
	if *installOnly {
		release, err := install.DeployBinaryVersion(filepath.Join(*product, "bin", "agent-vm-deploy"))
		if err != nil {
			return err
		}
		// Self-reported versions of parsed strings are soft evidence; the byte
		// comparison that matters happened before staging.
		if installed, err := install.DeployBinaryVersion(install.MaintenanceBinaryPath); err == nil && installed != version {
			return fmt.Errorf("this binary is %s and the installed stable binary is %s; run the matching release binary", version, installed)
		}
		backup, err := install.InstallMaintenanceBinary(*product)
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(map[string]any{"maintenance_binary_version": release, "backup": backup})
		_, _ = os.Stdout.Write(append(encoded, '\n'))
		return nil
	}
	// The restart is issued by whichever binary is running, so the expected
	// version is the installed file, not this process.
	served, err := install.ServedDeployBinaryVersion()
	if err != nil {
		return err
	}
	if err := exec.Command("/usr/bin/systemctl", "restart", "agent-vm-deployd.service").Run(); err != nil {
		return err
	}
	if err := waitDeploydReady(30 * time.Second); err != nil {
		return err
	}
	encoded, _ := json.Marshal(map[string]any{"served_version": served, "deployd_ready": true, "next_action": "re-run status, doctor and admin status before claiming the update"})
	_, _ = os.Stdout.Write(append(encoded, '\n'))
	return nil
}

// waitDeploydReady polls the socket so a crash-looping unit is not reported as
// a successful restart.
func waitDeploydReady(deadline time.Duration) error {
	client := deployd.Client{Socket: deployd.Defaults().Socket, Timeout: 5 * time.Second}
	until := time.Now().Add(deadline)
	for {
		payload, _, err := client.Call(context.Background(), "GET", "/v1/status", nil)
		if err == nil && hasOKStatus(payload) {
			return nil
		}
		if time.Now().After(until) {
			return errors.New("deployd did not become ready after restart; inspect agent-vm-deployd.service")
		}
		time.Sleep(time.Second)
	}
}

func hasOKStatus(payload []byte) bool {
	var envelope struct {
		OK bool `json:"ok"`
	}
	return json.Unmarshal(payload, &envelope) == nil && envelope.OK
}

const recoveryPointBudgetHint = 3

const orphanStagingAge = 24 * time.Hour

func cleanupOrphanStaging(ctx context.Context, client deployd.Client, root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	statusPayload, _, err := client.Call(ctx, "GET", "/v1/status", nil)
	if err != nil {
		return
	}
	var status struct {
		OK    bool `json:"ok"`
		Value struct {
			Transactions []struct {
				StagingID string `json:"staging_id"`
			} `json:"transactions"`
		} `json:"value"`
	}
	if json.Unmarshal(statusPayload, &status) != nil || !status.OK {
		return
	}
	protected := make(map[string]struct{}, len(status.Value.Transactions))
	for _, transaction := range status.Value.Transactions {
		if transaction.StagingID == "" {
			// A legacy active transaction cannot be safely matched to a local
			// directory, so skip the entire reap pass.
			return
		}
		protected[transaction.StagingID] = struct{}{}
	}
	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".update-") ||
			strings.HasPrefix(entry.Name(), install.AccountMigrationStagingPrefix) {
			continue
		}
		if _, ok := protected[entry.Name()]; ok {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < orphanStagingAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}

func errorMayNeedRecovery(payload []byte) bool {
	if len(payload) == 0 {
		return true
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &envelope) != nil || envelope.Error.Code == "" {
		return true
	}
	return envelope.Error.Code == "state_unverified"
}

func hasStructuredError(payload []byte) bool {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(payload, &envelope) == nil && envelope.Error.Code != ""
}

func writeUpdateRecoveryHint(ctx context.Context, client deployd.Client, knownID string) {
	id, phase := knownID, "unknown"
	if id == "" {
		statusPayload, _, err := client.Call(ctx, "GET", "/v1/status", nil)
		if err == nil {
			var status struct {
				Value struct {
					Transactions []struct {
						ID    string `json:"transaction_id"`
						Phase string `json:"phase"`
					} `json:"transactions"`
				} `json:"value"`
			}
			if json.Unmarshal(statusPayload, &status) == nil && len(status.Value.Transactions) == 1 {
				id, phase = status.Value.Transactions[0].ID, status.Value.Transactions[0].Phase
			}
		}
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, `{"code":"deployment_outcome_uncertain","phase":"unknown","recovery_required":true,"next_action":"query_deployd_status"}`)
		return
	}
	encoded, _ := json.Marshal(map[string]any{"code": "deployment_outcome_uncertain", "transaction_id": id, "phase": phase, "recovery_required": true, "next_action": "query_deployd_transaction"})
	_, _ = os.Stderr.Write(append(encoded, '\n'))
}

func publicError(err error) string {
	value := err.Error()
	for _, path := range []string{"/var/lib/agent-vm-native", "/run/agent-vm-native", "/etc/agent-vm-native"} {
		value = strings.ReplaceAll(value, path, "<private-path>")
	}
	return value
}

func writePayload(out *os.File, payload []byte) {
	if len(payload) == 0 {
		return
	}
	const maxDiagnosticBytes = 64 << 10
	if len(payload) > maxDiagnosticBytes {
		payload = append(append([]byte(nil), payload[:maxDiagnosticBytes]...), []byte("\n[diagnostic truncated]\n")...)
	}
	for _, path := range []string{"/var/lib/agent-vm-native", "/run/agent-vm-native", "/etc/agent-vm-native"} {
		payload = []byte(strings.ReplaceAll(string(payload), path, "<private-path>"))
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
	checkAccounts := flags.Bool("check-account-migration", false, "check the existing host before a service account migration")
	accountProof := flags.String("account-migration-proof", "", "protected acceptance record authorizing service account migration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *config == "" || *bundle == "" || *materials == "" {
		return errors.New("install requires --config, --bundle and --materials; --apply enables changes")
	}
	if *apply && *checkAccounts {
		return errors.New("--check-account-migration is read-only and cannot be combined with --apply")
	}
	if *accountProof != "" && !*apply {
		return errors.New("--account-migration-proof requires --apply")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	input := install.Inputs{Config: *config, Bundle: *bundle, Materials: *materials}
	var report install.Report
	var err error
	if *checkAccounts {
		report, err = install.PreflightAccountMigration(ctx, input)
	} else if *accountProof != "" {
		report, err = install.ApplyAccountMigration(ctx, input, *accountProof)
	} else if *apply {
		report, err = install.Apply(ctx, input)
	} else {
		report, err = install.Preflight(ctx, input)
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(report); encodeErr != nil {
		return encodeErr
	}
	return err
}
