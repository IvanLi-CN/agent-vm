package deployd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
	"golang.org/x/sys/unix"
)

const (
	maxMigrationSourceBytes  = int64(1 << 30)
	maxMigrationSnapshots    = 3
	migrationBackupRetention = 7 * 24 * time.Hour
	legacyMigrationSession   = "release-agent-vm-migration-"
)

const migrationCompletionMarker = ".rqlite-cutover-complete"

type migrationCompletionMarkerData struct {
	Format            int       `json:"format"`
	TransactionID     string    `json:"transaction_id"`
	BusinessAuthority string    `json:"business_authority"`
	SourceSHA256      string    `json:"source_sha256"`
	SourceSize        int64     `json:"source_size"`
	StateEpoch        string    `json:"state_epoch"`
	TargetSHA256      string    `json:"target_sha256"`
	RetentionUntil    time.Time `json:"retention_until"`
}

func (runtime *HostRuntime) migrationMarkerPath() string {
	return filepath.Join(runtime.Config.Migration.RecoveryDirectory, migrationCompletionMarker)
}

func (runtime *HostRuntime) MigrationComplete(ctx context.Context) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	if _, err := runtime.readMigrationMarker(); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (runtime *HostRuntime) readMigrationMarker() (migrationCompletionMarkerData, error) {
	path := runtime.migrationMarkerPath()
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return migrationCompletionMarkerData{}, fs.ErrNotExist
		}
		return migrationCompletionMarkerData{}, fmt.Errorf("inspect migration completion marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return migrationCompletionMarkerData{}, errors.New("migration completion marker is not a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return migrationCompletionMarkerData{}, fmt.Errorf("read migration completion marker: %w", err)
	}
	var marker migrationCompletionMarkerData
	if err := json.Unmarshal(data, &marker); err != nil || marker.Format != 2 || marker.TransactionID == "" || marker.BusinessAuthority != "rqlite" || marker.SourceSHA256 == "" || marker.SourceSize < 0 || marker.StateEpoch == "" || marker.RetentionUntil.IsZero() || !validMigrationDigest(marker.TargetSHA256) {
		return migrationCompletionMarkerData{}, errors.New("migration completion marker is invalid")
	}
	if !validMigrationDigest(marker.SourceSHA256) {
		return migrationCompletionMarkerData{}, errors.New("migration completion marker source digest is invalid")
	}
	return marker, nil
}

func validMigrationDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// VerifyMigrationTarget is optional at the interface boundary so fake runtimes
// can focus on the transaction state machine. HostRuntime always implements it
// and verifies the durable rqlite contents before migration is considered done.
func (runtime *HostRuntime) VerifyMigrationTarget(ctx context.Context, owner string) error {
	marker, err := runtime.readMigrationMarker()
	if err != nil {
		return err
	}
	if marker.TransactionID != owner {
		return errors.New("migration completion marker transaction does not match the durable transaction")
	}
	url, err := runtime.rqliteMigrationURL()
	if err != nil {
		return err
	}
	return state.VerifySQLiteMigrationTarget(ctx, url, runtime.Config.Migration.RQLiteCAFile, marker.TargetSHA256)
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
	info, err := os.Lstat(runtime.Config.Migration.LegacyStateDB)
	if err != nil {
		return fmt.Errorf("inspect legacy SQLite state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("legacy SQLite state is not a regular file")
	}
	sourceSize, err := sqliteSourceFootprint(runtime.Config.Migration.LegacyStateDB)
	if err != nil {
		return err
	}
	if sourceSize > maxMigrationSourceBytes {
		return fmt.Errorf("legacy SQLite source footprint is %d bytes and exceeds the 1 GiB migration limit", sourceSize)
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
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
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

func (runtime *HostRuntime) GrantLegacyController(ctx context.Context, lease controllerrole.Lease) error {
	if lease.Slot != "blue" {
		return errors.New("legacy migration controller lease must target the blue slot")
	}
	body, status, err := runtime.slotRequest(ctx, runtime.Config.Migration.LegacySocket, http.MethodPost, "/internal/v1/controller-role", map[string]any{
		"action": "grant",
		"lease":  lease,
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK || !responseOK(body) {
		return fmt.Errorf("legacy service rejected migration controller lease with HTTP %d", status)
	}
	if valid, ok := body["valid"].(bool); !ok || !valid {
		return errors.New("legacy service did not confirm the migration controller lease")
	}
	return nil
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
	if err := runtime.systemctlFixed(ctx, "stop", runtime.Config.Migration.LegacyUnit); err != nil {
		return err
	}
	// The legacy process owns this lock for its lifetime, so it cannot be
	// acquired before systemd stops that process. Acquire it immediately after
	// the stop request and keep it through writer verification, snapshot, and
	// import; a concurrent restart then fails closed at the systemd check.
	return runtime.acquireLegacyAuthority(ctx)
}

func (runtime *HostRuntime) VerifyLegacyStopped(ctx context.Context) error {
	if err := runtime.acquireLegacyAuthority(ctx); err != nil {
		return err
	}
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
	entries, err := procEntries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		pid := entry
		if pid == os.Getpid() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspect SQLite writer process %d: %w", pid, err)
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd.Name()))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return fmt.Errorf("inspect SQLite writer process %d file descriptor: %w", pid, err)
			}
			target = strings.TrimSuffix(target, " (deleted)")
			if target == base || target == base+"-wal" || target == base+"-shm" {
				return fmt.Errorf("SQLite writer check found process %d holding %s", pid, filepath.Base(target))
			}
		}
	}
	return nil
}

func procEntries() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("inspect /proc for SQLite writers: %w", err)
	}
	result := make([]int, 0, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid > 0 {
			result = append(result, pid)
		}
	}
	return result, nil
}

func (runtime *HostRuntime) acquireLegacyAuthority(ctx context.Context) error {
	path := filepath.Join(filepath.Dir(runtime.Config.Migration.LegacyStateDB), "authority.lock")
	for {
		runtime.mu.Lock()
		if runtime.legacyAuthorityLock != nil {
			runtime.mu.Unlock()
			return nil
		}
		runtime.mu.Unlock()
		lock, err := native.AcquireLock(path, true, true)
		if err == nil {
			runtime.mu.Lock()
			if runtime.legacyAuthorityLock == nil {
				runtime.legacyAuthorityLock = lock
				lock = nil
			}
			runtime.mu.Unlock()
			if lock != nil {
				_ = lock.Close()
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func validateMigrationSnapshotSize(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect migration snapshot: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("migration snapshot is not a regular file")
	}
	if info.Size() > maxMigrationSourceBytes {
		return fmt.Errorf("migration snapshot is %d bytes and exceeds the 1 GiB migration limit", info.Size())
	}
	return nil
}

func sqliteSourceFootprint(statePath string) (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := statePath + suffix
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("inspect SQLite source %s: %w", filepath.Base(path), err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return 0, fmt.Errorf("SQLite source %s is not a regular file", filepath.Base(path))
		}
		if info.Size() < 0 || total > maxMigrationSourceBytes-info.Size() {
			return 0, errors.New("legacy SQLite source footprint exceeds the 1 GiB migration limit")
		}
		total += info.Size()
	}
	return total, nil
}

func (runtime *HostRuntime) ensureRecoveryDirectory() error {
	path := runtime.Config.Migration.RecoveryDirectory
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("prepare migration recovery directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect migration recovery directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("migration recovery directory is not a private regular directory")
	}
	return nil
}

func syncMigrationDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func cleanupMigrationSnapshotTemps(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "migration-") || !strings.HasSuffix(entry.Name(), ".sqlite3.tmp") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("migration temporary entry is not a regular file: %s", entry.Name())
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove incomplete migration snapshot %s: %w", entry.Name(), err)
		}
		removed = true
	}
	if removed {
		if err := syncMigrationDirectory(directory); err != nil {
			return fmt.Errorf("sync migration recovery directory after temporary cleanup: %w", err)
		}
	}
	return nil
}

func removeMigrationSnapshot(path string) error {
	removed := false
	for _, candidate := range []string{path, path + ".tmp"} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("migration snapshot is not a regular file: %s", filepath.Base(candidate))
		}
		if err := os.Remove(candidate); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		if err := syncMigrationDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *HostRuntime) checkMigrationBudget(sourceSize int64) error {
	if sourceSize < 0 || sourceSize > maxMigrationSourceBytes {
		return errors.New("backup_storage_exhausted: legacy SQLite source footprint exceeds the migration budget")
	}
	if err := cleanupMigrationSnapshotTemps(runtime.Config.Migration.RecoveryDirectory); err != nil {
		return fmt.Errorf("clean incomplete migration snapshots: %w", err)
	}
	entries, err := os.ReadDir(runtime.Config.Migration.RecoveryDirectory)
	if err != nil {
		return fmt.Errorf("inspect migration recovery budget: %w", err)
	}
	var total int64
	count := 0
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
		count++
		if info.Size() > maxMigrationSourceBytes-total {
			return errors.New("backup_storage_exhausted: migration recovery budget is exhausted")
		}
		total += info.Size()
	}
	if count >= maxMigrationSnapshots || sourceSize > maxMigrationSourceBytes-total {
		return errors.New("backup_storage_exhausted: migration recovery budget is exhausted")
	}
	available, err := availableMigrationStorageBytes(runtime.Config.Migration.RecoveryDirectory)
	if err != nil {
		return fmt.Errorf("inspect migration recovery free space: %w", err)
	}
	if available < sourceSize {
		return errors.New("backup_storage_exhausted: migration recovery filesystem has insufficient free space")
	}
	return nil
}

func availableMigrationStorageBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bavail < 0 || stat.Bsize < 0 || uint64(stat.Bavail) > uint64(^uint64(0))/uint64(stat.Bsize) {
		return 0, errors.New("migration recovery filesystem capacity is out of range")
	}
	bytes := uint64(stat.Bavail) * uint64(stat.Bsize)
	if bytes > uint64(^uint64(0)>>1) {
		return 0, errors.New("migration recovery filesystem capacity exceeds int64")
	}
	return int64(bytes), nil
}

func (runtime *HostRuntime) PrepareMigrationSource(ctx context.Context, owner string) (source MigrationSource, returnErr error) {
	if err := runtime.PreflightMigration(ctx); err != nil {
		return MigrationSource{}, err
	}
	if err := runtime.ensureRecoveryDirectory(); err != nil {
		return MigrationSource{}, err
	}
	sourceSize, err := sqliteSourceFootprint(runtime.Config.Migration.LegacyStateDB)
	if err != nil {
		return MigrationSource{}, err
	}
	if err := runtime.checkMigrationBudget(sourceSize); err != nil {
		return MigrationSource{}, err
	}
	backup := filepath.Join(runtime.Config.Migration.RecoveryDirectory, "migration-"+owner+".sqlite3")
	if _, err := os.Lstat(backup); err == nil {
		return MigrationSource{}, errors.New("verified migration snapshot already exists")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return MigrationSource{}, fmt.Errorf("inspect migration snapshot destination: %w", err)
	}
	snapshotStarted := true
	defer func() {
		if snapshotStarted {
			if cleanupErr := removeMigrationSnapshot(backup); cleanupErr != nil {
				if returnErr == nil {
					returnErr = fmt.Errorf("migration snapshot cleanup failed: %w", cleanupErr)
				} else {
					returnErr = fmt.Errorf("%w; migration snapshot cleanup failed: %v", returnErr, cleanupErr)
				}
			}
		}
	}()
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
	snapshotStarted = false
	return MigrationSource{Evidence: evidence, BackupPath: backup}, nil
}

func (runtime *HostRuntime) ImportMigration(ctx context.Context, source MigrationSource) (state.SQLiteMigrationResult, error) {
	url, err := runtime.rqliteMigrationURL()
	if err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	return state.MigrateSQLiteToRQLiteWithCA(ctx, source.BackupPath, url, runtime.Config.Migration.RQLiteCAFile, source.Evidence)
}

func (runtime *HostRuntime) RetryMigration(ctx context.Context, owner string, expected state.SQLiteMigrationEvidence, allowResnapshot bool) (state.SQLiteMigrationResult, error) {
	if err := runtime.VerifyLegacyStopped(ctx); err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	if err := runtime.VerifySQLiteWritersClosed(ctx); err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	if err := runtime.ensureRecoveryDirectory(); err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	if marker, markerErr := runtime.readMigrationMarker(); markerErr == nil {
		if marker.TransactionID != owner {
			return state.SQLiteMigrationResult{}, errors.New("migration completion marker belongs to another transaction")
		}
		markerEvidence := state.SQLiteMigrationEvidence{
			SourceSHA256: marker.SourceSHA256,
			SourceSize:   marker.SourceSize,
			StateEpoch:   marker.StateEpoch,
		}
		if expected.SourceSHA256 != "" {
			if err := verifyMigrationSourceEvidence(expected, markerEvidence); err != nil {
				return state.SQLiteMigrationResult{}, err
			}
		}
		if err := runtime.VerifyMigrationTarget(ctx, owner); err != nil {
			return state.SQLiteMigrationResult{}, fmt.Errorf("verify persisted migration completion: %w", err)
		}
		return state.SQLiteMigrationResult{
			SourceSHA256: marker.SourceSHA256,
			SourceSize:   marker.SourceSize,
			StateEpoch:   marker.StateEpoch,
			TargetSHA256: marker.TargetSHA256,
		}, nil
	} else if !errors.Is(markerErr, fs.ErrNotExist) {
		return state.SQLiteMigrationResult{}, markerErr
	}
	backup := filepath.Join(runtime.Config.Migration.RecoveryDirectory, "migration-"+owner+".sqlite3")
	info, err := os.Lstat(backup)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if !allowResnapshot || expected.SourceSHA256 != "" {
				return state.SQLiteMigrationResult{}, errors.New("verified migration snapshot is missing; retry cannot resample the legacy database")
			}
			source, prepareErr := runtime.PrepareMigrationSource(ctx, owner)
			if prepareErr != nil {
				return state.SQLiteMigrationResult{}, prepareErr
			}
			return runtime.ImportMigration(ctx, source)
		}
		return state.SQLiteMigrationResult{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return state.SQLiteMigrationResult{}, errors.New("verified migration snapshot is not a private regular file")
	}
	if err := validateMigrationSnapshotSize(backup); err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	evidence, err := state.PrepareSQLiteMigration(ctx, backup, owner)
	if err != nil {
		return state.SQLiteMigrationResult{}, err
	}
	if expected.SourceSHA256 != "" {
		if err := verifyMigrationSourceEvidence(expected, evidence); err != nil {
			return state.SQLiteMigrationResult{}, err
		}
	}
	return runtime.ImportMigration(ctx, MigrationSource{Evidence: evidence, BackupPath: backup})
}

func verifyMigrationSourceEvidence(expected, actual state.SQLiteMigrationEvidence) error {
	if expected.SourceSHA256 != actual.SourceSHA256 || expected.SourceSize != actual.SourceSize || expected.StateEpoch != actual.StateEpoch {
		return errors.New("verified migration snapshot does not match the original migration evidence")
	}
	return nil
}

func (runtime *HostRuntime) rqliteMigrationURL() (string, error) {
	parsed, err := url.Parse(runtime.Config.Migration.RQLiteURL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
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

func (runtime *HostRuntime) FinalizeMigration(ctx context.Context, owner string, success bool, result state.SQLiteMigrationResult) error {
	if !success {
		return nil
	}
	if !validMigrationDigest(result.SourceSHA256) || result.SourceSize < 0 || result.StateEpoch == "" || !validMigrationDigest(result.TargetSHA256) {
		return errors.New("migration completion result is incomplete")
	}
	if err := runtime.ensureRecoveryDirectory(); err != nil {
		return err
	}
	if err := runtime.VerifySQLiteWritersClosed(ctx); err != nil {
		return err
	}
	completedAt := time.Now().UTC()
	marker := migrationCompletionMarkerData{
		Format:            2,
		TransactionID:     owner,
		BusinessAuthority: "rqlite",
		SourceSHA256:      result.SourceSHA256,
		SourceSize:        result.SourceSize,
		StateEpoch:        result.StateEpoch,
		TargetSHA256:      result.TargetSHA256,
		RetentionUntil:    completedAt.Add(migrationBackupRetention),
	}
	if existing, err := runtime.readMigrationMarker(); err == nil {
		if existing.TransactionID != marker.TransactionID || existing.SourceSHA256 != marker.SourceSHA256 || existing.SourceSize != marker.SourceSize || existing.StateEpoch != marker.StateEpoch || existing.TargetSHA256 != marker.TargetSHA256 {
			return errors.New("migration completion marker does not match the finalized migration result")
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		if err := runtime.writeMigrationMarker(marker); err != nil {
			return err
		}
	} else {
		return err
	}
	return removeLegacySQLiteFiles(runtime.Config.Migration.LegacyStateDB)
}

func removeLegacySQLiteFiles(statePath string) error {
	removed := false
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := statePath + suffix
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove migrated SQLite source %s: %w", filepath.Base(path), err)
			}
		} else {
			removed = true
		}
	}
	if removed {
		if err := syncMigrationDirectory(filepath.Dir(statePath)); err != nil {
			return fmt.Errorf("sync legacy SQLite directory after cleanup: %w", err)
		}
	}
	return nil
}

func (runtime *HostRuntime) writeMigrationMarker(marker migrationCompletionMarkerData) error {
	encoded, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode migration completion marker: %w", err)
	}
	path := runtime.migrationMarkerPath()
	temporary := path + ".tmp"
	if info, inspectErr := os.Lstat(temporary); inspectErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("migration completion temporary marker is not a regular file")
		}
		if err := os.Remove(temporary); err != nil {
			return fmt.Errorf("remove stale migration completion marker: %w", err)
		}
	} else if !errors.Is(inspectErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect temporary migration completion marker: %w", inspectErr)
	}
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

func (runtime *HostRuntime) CleanupMigrationBackups(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := runtime.ensureRecoveryDirectory(); err != nil {
		return err
	}
	if err := cleanupMigrationSnapshotTemps(runtime.Config.Migration.RecoveryDirectory); err != nil {
		return err
	}
	marker, err := runtime.readMigrationMarker()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(runtime.Config.Migration.RecoveryDirectory)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-migrationBackupRetention)
	removed := false
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
		if entry.Name() == "migration-"+marker.TransactionID+".sqlite3" && now.Before(marker.RetentionUntil) {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				return err
			}
			removed = true
		}
	}
	if removed {
		if err := syncMigrationDirectory(runtime.Config.Migration.RecoveryDirectory); err != nil {
			return err
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
