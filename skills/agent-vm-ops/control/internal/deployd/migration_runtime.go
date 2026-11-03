package deployd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const (
	maxMigrationSourceBytes  = int64(1 << 30)
	migrationBackupRetention = 7 * 24 * time.Hour
	legacyMigrationSession   = "release-agent-vm-migration-"
)

const migrationCompletionMarker = ".rqlite-cutover-complete"

func (runtime *HostRuntime) migrationMarkerPath() string {
	return filepath.Join(runtime.Config.Migration.RecoveryDirectory, migrationCompletionMarker)
}

func (runtime *HostRuntime) MigrationComplete(ctx context.Context) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	path := runtime.migrationMarkerPath()
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect migration completion marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return false, errors.New("migration completion marker is not a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read migration completion marker: %w", err)
	}
	var marker struct {
		Format            int    `json:"format"`
		TransactionID     string `json:"transaction_id"`
		BusinessAuthority string `json:"business_authority"`
	}
	if err := json.Unmarshal(data, &marker); err != nil || marker.Format != 1 || marker.TransactionID == "" || marker.BusinessAuthority != "rqlite" {
		return false, errors.New("migration completion marker is invalid")
	}
	return true, nil
}

func (runtime *HostRuntime) PreflightMigration(_ context.Context) error {
	if runtime.Config.Migration.RQLiteURL == "" {
		return errors.New("initial state migration requires migration.rqlite_url")
	}
	if _, err := runtime.rqliteMigrationURL(); err != nil {
		return err
	}
	caInfo, err := os.Lstat(runtime.Config.Migration.RQLiteCAFile)
	if err != nil {
		return fmt.Errorf("inspect rqlite CA bundle: %w", err)
	}
	if caInfo.Mode()&os.ModeSymlink != 0 || !caInfo.Mode().IsRegular() {
		return errors.New("rqlite CA bundle is not a regular file")
	}
	authInfo, err := os.Lstat(runtime.Config.Migration.RQLiteAuthFile)
	if err != nil {
		return fmt.Errorf("inspect rqlite auth file: %w", err)
	}
	if authInfo.Mode()&os.ModeSymlink != 0 || !authInfo.Mode().IsRegular() || authInfo.Mode().Perm() != 0o600 {
		return errors.New("rqlite auth file is not a private regular file")
	}
	info, err := os.Stat(runtime.Config.Migration.LegacyStateDB)
	if err != nil {
		return fmt.Errorf("inspect legacy SQLite state: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("legacy SQLite state is not a regular file")
	}
	if info.Size() > maxMigrationSourceBytes {
		return fmt.Errorf("legacy SQLite state is %d bytes and exceeds the 1 GiB migration limit", info.Size())
	}
	return nil
}

func (runtime *HostRuntime) StartStateAuthority(ctx context.Context) error {
	if runtime.Config.Migration.RQLiteURL == "" {
		return errors.New("migration.rqlite_url is required")
	}
	if err := runtime.systemctlFixed(ctx, "start", runtime.Config.Migration.RQLiteUnit); err != nil {
		return err
	}
	parsed, err := url.Parse(runtime.Config.Migration.RQLiteURL)
	if err != nil || parsed.Host == "" {
		return errors.New("migration.rqlite_url is invalid")
	}
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		dialer := net.Dialer{Timeout: 2 * time.Second}
		connection, dialErr := dialer.DialContext(ctx, "tcp", parsed.Host)
		if dialErr == nil {
			_ = connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("rqlite did not become reachable: %w", dialErr)
		case <-ticker.C:
		}
	}
}

func (runtime *HostRuntime) EnterLegacyFence(ctx context.Context, owner string) error {
	if err := runtime.PreflightMigration(ctx); err != nil {
		return err
	}
	request := map[string]any{
		"protocol":     4,
		"action":       "drain-enter",
		"session_id":   legacyMigrationSession + owner,
		"operation_id": "migration-drain-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"generation":   int64(0),
	}
	body, status, err := runtime.control(ctx, runtime.Config.Migration.LegacySocket, request)
	if err != nil {
		return err
	}
	if status != http.StatusOK || !responseOK(body) {
		return fmt.Errorf("legacy service rejected migration fence with HTTP %d", status)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		return errors.New("legacy migration fence response is missing a result")
	}
	if draining, ok := result["draining"].(bool); !ok || !draining {
		return errors.New("legacy service did not confirm migration fence")
	}
	return nil
}

func (runtime *HostRuntime) WaitLegacyQuiescence(ctx context.Context) (MigrationQuiescence, error) {
	deadline := time.NewTimer(time.Duration(runtime.Config.Migration.DrainTimeoutSeconds) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		quiescence, err := runtime.legacyQuiescenceSample(ctx)
		if err == nil && quiescence.PendingOperations == 0 && quiescence.InFlightOperations == 0 && quiescence.TransitionTasks == 0 {
			return quiescence, nil
		}
		select {
		case <-ctx.Done():
			return MigrationQuiescence{}, ctx.Err()
		case <-deadline.C:
			if err != nil {
				return MigrationQuiescence{}, fmt.Errorf("legacy service did not quiesce: %w", err)
			}
			return MigrationQuiescence{}, errors.New("legacy service did not quiesce before the bounded migration deadline")
		case <-ticker.C:
		}
	}
}

func (runtime *HostRuntime) legacyQuiescenceSample(ctx context.Context) (MigrationQuiescence, error) {
	status, code, err := runtime.control(ctx, runtime.Config.Migration.LegacySocket, map[string]any{"protocol": 4, "action": "drain-status"})
	if err != nil {
		return MigrationQuiescence{}, err
	}
	if code != http.StatusOK {
		return MigrationQuiescence{}, fmt.Errorf("legacy drain status returned HTTP %d", code)
	}
	statusResult, ok := status["result"].(map[string]any)
	if !ok {
		return MigrationQuiescence{}, errors.New("legacy drain status result is missing")
	}
	if draining, ok := statusResult["draining"].(bool); !ok || !draining {
		return MigrationQuiescence{}, errors.New("legacy service is not fenced for migration")
	}
	metrics, code, err := runtime.control(ctx, runtime.Config.Migration.LegacySocket, map[string]any{"protocol": 4, "action": "admin-metrics"})
	if err != nil {
		return MigrationQuiescence{}, err
	}
	if code != http.StatusOK {
		return MigrationQuiescence{}, fmt.Errorf("legacy metrics returned HTTP %d", code)
	}
	value, ok := metrics["result"].(map[string]any)
	if !ok {
		return MigrationQuiescence{}, errors.New("legacy metrics result is missing")
	}
	pending, pendingOK := arrayLengthValue(value["pending_operations"])
	unknown, unknownOK := arrayLengthValue(value["historical_unknown_operations"])
	inFlight, inFlightOK := integerValue(value["in_flight_operations"])
	epoch, epochOK := value["observation_epoch"].(string)
	states, statesOK := value["task_states"].(map[string]any)
	if !pendingOK || !unknownOK || !inFlightOK || !epochOK || epoch == "" || !statesOK {
		return MigrationQuiescence{}, errors.New("legacy migration metrics are incomplete")
	}
	if unknown != 0 {
		return MigrationQuiescence{}, errors.New("legacy service has historical unknown operations")
	}
	transitionTasks := int64(0)
	for _, stateName := range []string{"preparing", "closing", "deleting", "failed"} {
		count, ok := integerValue(states[stateName])
		if !ok || count < 0 {
			return MigrationQuiescence{}, fmt.Errorf("legacy task state %s is unverified", stateName)
		}
		transitionTasks += count
	}
	return MigrationQuiescence{StateEpoch: epoch, PendingOperations: pending, InFlightOperations: inFlight, TransitionTasks: transitionTasks}, nil
}

func (runtime *HostRuntime) StopLegacy(ctx context.Context) error {
	return runtime.systemctlFixed(ctx, "stop", runtime.Config.Migration.LegacyUnit)
}

func (runtime *HostRuntime) VerifyLegacyStopped(ctx context.Context) error {
	unit := runtime.Config.Migration.LegacyUnit
	command := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=MainPID", "--property=FragmentPath")
	output, err := command.Output()
	if err != nil {
		return errors.New("legacy systemd unit status is unavailable")
	}
	properties := parseSystemdProperties(string(output))
	if properties["LoadState"] != "loaded" || properties["ActiveState"] != "inactive" || properties["SubState"] != "dead" || properties["MainPID"] != "0" {
		return errors.New("legacy systemd unit is not verified stopped")
	}
	return nil
}

func (runtime *HostRuntime) VerifySQLiteWritersClosed(ctx context.Context) error {
	deadline := time.NewTimer(time.Duration(runtime.Config.Migration.WriterCheckSeconds) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := runtime.findSQLiteWriter(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return lastErr
		case <-ticker.C:
		}
	}
}

func (runtime *HostRuntime) findSQLiteWriter() error {
	base := filepath.Clean(runtime.Config.Migration.LegacyStateDB)
	for _, entry := range procEntries() {
		pid := entry
		if pid == os.Getpid() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd.Name()))
			if err != nil {
				continue
			}
			target = strings.TrimSuffix(target, " (deleted)")
			if target == base || target == base+"-wal" || target == base+"-shm" {
				return fmt.Errorf("SQLite writer check found process %d holding %s", pid, filepath.Base(target))
			}
		}
	}
	return nil
}

func validateMigrationSnapshotSize(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect migration snapshot: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("migration snapshot is not a regular file")
	}
	if info.Size() > maxMigrationSourceBytes {
		return fmt.Errorf("migration snapshot is %d bytes and exceeds the 1 GiB migration limit", info.Size())
	}
	return nil
}

func procEntries() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	result := make([]int, 0, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid > 0 {
			result = append(result, pid)
		}
	}
	return result
}

func (runtime *HostRuntime) PrepareMigrationSource(ctx context.Context, owner string) (MigrationSource, error) {
	if err := runtime.PreflightMigration(ctx); err != nil {
		return MigrationSource{}, err
	}
	if err := os.MkdirAll(runtime.Config.Migration.RecoveryDirectory, 0o700); err != nil {
		return MigrationSource{}, fmt.Errorf("prepare migration recovery directory: %w", err)
	}
	backup := filepath.Join(runtime.Config.Migration.RecoveryDirectory, "migration-"+owner+".sqlite3")
	if err := state.SnapshotSQLite(ctx, runtime.Config.Migration.LegacyStateDB, backup); err != nil {
		return MigrationSource{}, err
	}
	if err := validateMigrationSnapshotSize(backup); err != nil {
		return MigrationSource{}, err
	}
	evidence, err := state.PrepareSQLiteMigration(ctx, backup, owner)
	if err != nil {
		return MigrationSource{}, err
	}
	return MigrationSource{Evidence: evidence, BackupPath: backup}, nil
}

func (runtime *HostRuntime) ImportMigration(ctx context.Context, source MigrationSource) (state.SQLiteMigrationResult, error) {
	url, err := runtime.rqliteMigrationURL()
	if err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	return state.MigrateSQLiteToRQLiteWithCA(ctx, source.BackupPath, url, runtime.Config.Migration.RQLiteCAFile, source.Evidence)
}

func (runtime *HostRuntime) RetryMigration(ctx context.Context, owner string) (state.SQLiteMigrationResult, error) {
	if err := runtime.VerifyLegacyStopped(ctx); err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	if err := runtime.VerifySQLiteWritersClosed(ctx); err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	if err := os.MkdirAll(runtime.Config.Migration.RecoveryDirectory, 0o700); err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	backup := filepath.Join(runtime.Config.Migration.RecoveryDirectory, "migration-"+owner+".sqlite3")
	if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
		if err := state.SnapshotSQLite(ctx, runtime.Config.Migration.LegacyStateDB, backup); err != nil {
			return state.SQLiteMigrationResult{}, err
		}
	} else if err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	if err := validateMigrationSnapshotSize(backup); err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	evidence, err := state.PrepareSQLiteMigration(ctx, backup, owner)
	if err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	return runtime.ImportMigration(ctx, MigrationSource{Evidence: evidence, BackupPath: backup})
}

func (runtime *HostRuntime) rqliteMigrationURL() (string, error) {
	parsed, err := url.Parse(runtime.Config.Migration.RQLiteURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("migration.rqlite_url is invalid")
	}
	authData, err := os.ReadFile(runtime.Config.Migration.RQLiteAuthFile)
	if err != nil {
		return "", fmt.Errorf("read rqlite auth file: %w", err)
	}
	var users []struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(authData, &users); err != nil {
		return "", fmt.Errorf("decode rqlite auth file: %w", err)
	}
	for _, user := range users {
		if user.Username == "agent-vm-control" && user.Password != "" && !strings.HasPrefix(user.Password, "<") {
			parsed.User = url.UserPassword(user.Username, user.Password)
			return parsed.String(), nil
		}
	}
	return "", errors.New("rqlite auth file has no configured agent-vm-control credential")
}

func (runtime *HostRuntime) FinalizeMigration(ctx context.Context, owner string, success bool) error {
	if !success {
		return nil
	}
	if err := runtime.VerifySQLiteWritersClosed(ctx); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := runtime.Config.Migration.LegacyStateDB + suffix
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove migrated SQLite source %s: %w", filepath.Base(path), err)
		}
	}
	marker := map[string]any{
		"format":             1,
		"transaction_id":     owner,
		"business_authority": "rqlite",
		"completed_at":       time.Now().UTC(),
	}
	encoded, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode migration completion marker: %w", err)
	}
	path := runtime.migrationMarkerPath()
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create migration completion marker: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("write migration completion marker: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync migration completion marker: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close migration completion marker: %w", err)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		return fmt.Errorf("protect migration completion marker: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("commit migration completion marker: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open migration completion directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync migration completion directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close migration completion directory: %w", err)
	}
	return nil
}

func (runtime *HostRuntime) FenceLegacy(ctx context.Context) error {
	fenceErr := runtime.EnterLegacyFence(ctx, "recovery")
	if fenceErr == nil || !runtime.systemdUnitStopped(ctx, runtime.Config.Migration.LegacyUnit) {
		if stopErr := runtime.StopLegacy(ctx); stopErr != nil {
			if fenceErr != nil {
				return fmt.Errorf("legacy fence failed: %v; stop failed: %w", fenceErr, stopErr)
			}
			return stopErr
		}
	}
	if err := runtime.VerifyLegacyStopped(ctx); err != nil {
		return err
	}
	if err := runtime.VerifySQLiteWritersClosed(ctx); err != nil {
		return err
	}
	return nil
}

func (runtime *HostRuntime) CleanupMigrationBackups(_ context.Context) error {
	entries, err := os.ReadDir(runtime.Config.Migration.RecoveryDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-migrationBackupRetention)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "migration-") || !strings.HasSuffix(entry.Name(), ".sqlite3") {
			continue
		}
		path := filepath.Join(runtime.Config.Migration.RecoveryDirectory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("migration recovery entry is not a regular file: %s", entry.Name())
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (runtime *HostRuntime) systemctlFixed(ctx context.Context, action, unit string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return errors.New("unsupported systemd action")
	}
	if unit != "agent-vm-native.service" && unit != "agent-vm-rqlite.service" {
		return errors.New("unsupported migration systemd unit")
	}
	command := exec.CommandContext(ctx, "systemctl", action, unit)
	if _, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s %s failed: %w", action, unit, err)
	}
	return nil
}

func (runtime *HostRuntime) systemdUnitStopped(ctx context.Context, unit string) bool {
	command := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=MainPID")
	output, err := command.Output()
	if err != nil {
		return false
	}
	properties := parseSystemdProperties(string(output))
	return properties["LoadState"] == "loaded" && properties["ActiveState"] == "inactive" && properties["SubState"] == "dead" && properties["MainPID"] == "0"
}
