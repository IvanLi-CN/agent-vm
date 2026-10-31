package management

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

type fakeImageBackend struct {
	artifact ImageArtifact
	imports  int
	deletes  int
}

func TestReasonValidationUsesUnicodeCodePointsAndAllowsOmission(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("a", 512), strings.Repeat("\U0001f431", 512)} {
		if err := validateReason(value); err != nil {
			t.Fatalf("reason %q rejected: %v", value[:minTestStringLen(len(value), 8)], err)
		}
	}
	for _, value := range []string{" \t\n", strings.Repeat("a", 513), string([]byte{0xff})} {
		if err := validateReason(value); err == nil {
			t.Fatalf("invalid reason %q was accepted", value[:minTestStringLen(len(value), 8)])
		}
	}
}

func minTestStringLen(value, maximum int) int {
	if value < maximum {
		return value
	}
	return maximum
}

type blockingImageBackend struct {
	fakeImageBackend
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type sharedCapacityImageBackend struct {
	artifact  ImageArtifact
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	mu        sync.Mutex
	imports   int
	completed bool
}

func (backend *sharedCapacityImageBackend) ImportTrusted(ctx context.Context, sourceID, fingerprint string) (ImageArtifact, error) {
	backend.mu.Lock()
	backend.imports++
	count := backend.imports
	backend.mu.Unlock()
	if count == 1 {
		backend.once.Do(func() { close(backend.entered) })
		select {
		case <-backend.release:
		case <-ctx.Done():
			return ImageArtifact{}, ctx.Err()
		}
		backend.mu.Lock()
		backend.completed = true
		backend.mu.Unlock()
	}
	return backend.artifact, nil
}

func (backend *sharedCapacityImageBackend) TrustedImportSize(context.Context, string, string) (int64, error) {
	return 1, nil
}

func (backend *sharedCapacityImageBackend) ImportLocal(context.Context, string, []byte, []byte) (ImageArtifact, error) {
	return backend.artifact, nil
}

func (backend *sharedCapacityImageBackend) DeleteImage(context.Context, string) error {
	return nil
}

func (backend *sharedCapacityImageBackend) available() (int64, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.completed {
		return 512, nil
	}
	return 513, nil
}

type partialImageBackend struct {
	fakeImageBackend
	err error
}

func (backend *blockingImageBackend) ImportTrusted(ctx context.Context, sourceID, fingerprint string) (ImageArtifact, error) {
	backend.once.Do(func() { close(backend.entered) })
	select {
	case <-backend.release:
		return backend.artifact, nil
	case <-ctx.Done():
		return ImageArtifact{}, ctx.Err()
	}
}

func (backend *fakeImageBackend) ImportTrusted(context.Context, string, string) (ImageArtifact, error) {
	backend.imports++
	return backend.artifact, nil
}

func (backend *fakeImageBackend) TrustedImportSize(context.Context, string, string) (int64, error) {
	return 1, nil
}

func (backend *fakeImageBackend) ImportLocal(context.Context, string, []byte, []byte) (ImageArtifact, error) {
	backend.imports++
	return backend.artifact, nil
}

func (backend *fakeImageBackend) DeleteImage(context.Context, string) error {
	backend.deletes++
	return nil
}

func (backend *partialImageBackend) ImportLocal(context.Context, string, []byte, []byte) (ImageArtifact, error) {
	backend.imports++
	return backend.artifact, backend.err
}

type fakeActionBackend struct {
	vmCalls          int
	maintenanceCalls int
	err              error
}

type blockingActionBackend struct {
	fakeActionBackend
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (backend *fakeActionBackend) ExecuteVMAction(context.Context, string, string, string, int64, map[string]any) (map[string]any, error) {
	backend.vmCalls++
	return map[string]any{"state": "changed"}, backend.err
}

func (backend *fakeActionBackend) ExecuteMaintenance(context.Context, string, string, map[string]any) (map[string]any, error) {
	backend.maintenanceCalls++
	return map[string]any{"state": "changed"}, backend.err
}

func (backend *blockingActionBackend) ExecuteMaintenance(ctx context.Context, operationID, action string, parameters map[string]any) (map[string]any, error) {
	backend.once.Do(func() { close(backend.entered) })
	select {
	case <-backend.release:
		return map[string]any{"state": "changed"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (backend *fakeActionBackend) ValidateVMAction(context.Context, state.Task, string, map[string]any) error {
	return nil
}

func TestImageImportRejectsBadDigestAndRecordsFailedOperation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend := &fakeImageBackend{artifact: ImageArtifact{Fingerprint: strings.Repeat("a", 64), Architecture: "x86_64", InstanceType: "container", Runtime: true, SyncTool: true, GuestReady: true}}
	service := New(store, Options{UploadMaxBytes: 1024, Backend: backend})
	request := ImportRequest{
		OperationID: "import-bad-digest", Actor: "admin", Reason: "test import", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.02",
		Source: ImportSource{Kind: "local_upload", Format: "unified", ExpectedSHA256: strings.Repeat("b", 64), ExpectedSize: 3},
	}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	operation, err = service.UploadImageImport(context.Background(), operation.OperationID, []byte("abc"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != "failed" || operation.Phase != "integrity_check" {
		t.Fatalf("operation = %#v", operation)
	}
	if backend.imports != 0 {
		t.Fatalf("backend imports = %d", backend.imports)
	}
}

func TestImageImportRejectsInsufficientStagingCapacityBeforeClaimingOperation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend := &fakeImageBackend{artifact: ImageArtifact{Fingerprint: strings.Repeat("a", 64), Architecture: "x86_64", InstanceType: "container", Runtime: true, SyncTool: true, GuestReady: true}}
	service := New(store, Options{
		UploadMaxBytes:         1024,
		UploadMinimumFreeBytes: 512,
		AvailableStagingBytes: func() (int64, error) {
			return 100, nil
		},
		Backend: backend,
	})
	request := ImportRequest{
		OperationID: "import-low-space", Actor: "admin", Reason: "test staging capacity", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.02",
		Source: ImportSource{Kind: "local_upload", Format: "unified", ExpectedSHA256: strings.Repeat("b", 64), ExpectedSize: 128},
	}
	if _, err := service.StartImageImport(context.Background(), request); err == nil || !strings.HasPrefix(err.Error(), "staging_capacity_low:") {
		t.Fatalf("staging capacity error = %v", err)
	}
	operation, err := store.GetManagementOperation(context.Background(), request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if operation != nil {
		t.Fatalf("operation was claimed despite low capacity: %#v", operation)
	}
	if backend.imports != 0 {
		t.Fatalf("backend imports = %d", backend.imports)
	}
}

func TestImageImportUploadCapacityFailureLeavesOperationRetryable(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	content := []byte("abc")
	available := int64(4096)
	backend := &fakeImageBackend{artifact: ImageArtifact{Fingerprint: strings.Repeat("a", 64), Architecture: "x86_64", InstanceType: "container", Runtime: true, SyncTool: true, GuestReady: true}}
	service := New(store, Options{
		UploadMaxBytes:         1024,
		UploadMinimumFreeBytes: 512,
		AvailableStagingBytes:  func() (int64, error) { return available, nil },
		Backend:                backend,
	})
	request := ImportRequest{
		OperationID: "import-retryable-capacity", Actor: "admin", Reason: "retry upload after capacity recovery", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.02",
		Source: ImportSource{Kind: "local_upload", Format: "unified", ExpectedSHA256: digestBytes(content), ExpectedSize: int64(len(content))},
	}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	available = 3
	blocked, err := service.UploadImageImport(context.Background(), operation.OperationID, content, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "staging_capacity_low:") {
		t.Fatalf("capacity error = %v", err)
	}
	if blocked.Status != "awaiting_upload" {
		t.Fatalf("operation became terminal after capacity failure: %#v", blocked)
	}
	available = 4096
	completed, err := service.UploadImageImport(context.Background(), operation.OperationID, content, nil)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "succeeded" || backend.imports != 1 {
		t.Fatalf("completed operation = %#v, backend imports = %d", completed, backend.imports)
	}
}

func TestTrustedImportCapacityFailureDoesNotClaimOrImport(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fingerprint := strings.Repeat("a", 64)
	available := int64(4096)
	backend := &fakeImageBackend{artifact: ImageArtifact{Fingerprint: fingerprint, Architecture: "x86_64", InstanceType: "container", Runtime: true, SyncTool: true, GuestReady: true}}
	service := New(store, Options{
		TrustedImageSourceIDs:      []string{"approved"},
		UploadMinimumFreeBytes:     512,
		AvailableImageStorageBytes: func() (int64, error) { return available, nil },
		Backend:                    backend,
	})
	request := ImportRequest{
		OperationID: "trusted-retryable-capacity", Actor: "admin", Reason: "retry trusted import after capacity recovery", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.02",
		Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: fingerprint},
	}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	available = 1
	queued, err := service.ProcessTrustedImport(context.Background(), operation.OperationID)
	if err == nil || !strings.HasPrefix(err.Error(), "staging_capacity_low:") {
		t.Fatalf("capacity error = %v", err)
	}
	if queued.Status != "queued" || backend.imports != 0 {
		t.Fatalf("operation was claimed or imported after capacity failure: %#v, imports=%d", queued, backend.imports)
	}
	available = 4096
	completed, err := service.ProcessTrustedImport(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "succeeded" || backend.imports != 1 {
		t.Fatalf("completed operation = %#v, backend imports = %d", completed, backend.imports)
	}
}

func TestTrustedImportDoesNotCrossAdmissionDrain(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fingerprint := strings.Repeat("a", 64)
	backend := &fakeImageBackend{artifact: ImageArtifact{Fingerprint: fingerprint, Architecture: "x86_64", InstanceType: "container", Runtime: true, SyncTool: true, GuestReady: true}}
	service := New(store, Options{
		TrustedImageSourceIDs:      []string{"approved"},
		UploadMinimumFreeBytes:     512,
		AvailableImageStorageBytes: func() (int64, error) { return 4096, nil },
		Backend:                    backend,
	})
	request := ImportRequest{
		OperationID: "trusted-drain-fence", Actor: "admin", Reason: "do not import during release", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.02",
		Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: fingerprint},
	}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(context.Background(), "draining", "1"); err != nil {
		t.Fatal(err)
	}
	blocked, err := service.ProcessTrustedImport(context.Background(), operation.OperationID)
	if err == nil || !strings.Contains(err.Error(), "draining:") {
		t.Fatalf("drain error = %v", err)
	}
	stored, getErr := store.GetManagementOperation(context.Background(), operation.OperationID)
	if getErr != nil || blocked != nil || stored == nil || stored.Status != "queued" || backend.imports != 0 {
		t.Fatalf("operation crossed drain = returned=%#v stored=%#v get_err=%v imports=%d", blocked, stored, getErr, backend.imports)
	}
}

func TestConcurrentTrustedImportsReserveImageStorage(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fingerprint := strings.Repeat("a", 64)
	backend := &blockingImageBackend{fakeImageBackend: fakeImageBackend{artifact: ImageArtifact{Fingerprint: fingerprint, Architecture: "x86_64", InstanceType: "container", Runtime: true, SyncTool: true, GuestReady: true}}, entered: make(chan struct{}), release: make(chan struct{})}
	service := New(store, Options{TrustedImageSourceIDs: []string{"approved"}, UploadMinimumFreeBytes: 512, AvailableImageStorageBytes: func() (int64, error) { return 513, nil }, Backend: backend})
	requests := []ImportRequest{
		{OperationID: "trusted-concurrent-a", Actor: "admin", Reason: "concurrent import", TargetVersion: "image-catalog:revision-1", VersionLabel: "a", Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: fingerprint}},
		{OperationID: "trusted-concurrent-b", Actor: "admin", Reason: "concurrent import", TargetVersion: "image-catalog:revision-1", VersionLabel: "b", Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: fingerprint}},
	}
	for _, request := range requests {
		if _, err := service.StartImageImport(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	firstDone := make(chan error, 1)
	go func() {
		operation, err := service.ProcessTrustedImport(context.Background(), requests[0].OperationID)
		if err == nil && operation.Status != "succeeded" {
			err = fmt.Errorf("first operation = %#v", operation)
		}
		firstDone <- err
	}()
	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("first import did not reach native backend")
	}
	second, err := service.ProcessTrustedImport(context.Background(), requests[1].OperationID)
	if err == nil || !strings.HasPrefix(err.Error(), "staging_capacity_low:") || second.Status != "queued" {
		t.Fatalf("second import result=%#v err=%v", second, err)
	}
	close(backend.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentTrustedImportsUseSharedStorageLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.sqlite3")
	firstStore, err := state.OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer firstStore.Close()
	secondStore, err := state.OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	fingerprint := strings.Repeat("d", 64)
	backend := &sharedCapacityImageBackend{
		artifact: ImageArtifact{Fingerprint: fingerprint, Architecture: "x86_64", InstanceType: "container", Runtime: true, SyncTool: true, GuestReady: true},
		entered:  make(chan struct{}), release: make(chan struct{}),
	}
	options := Options{
		TrustedImageSourceIDs: []string{"approved"}, UploadMinimumFreeBytes: 512,
		AvailableImageStorageBytes: backend.available, Backend: backend, OperationLockDir: filepath.Join(root, "locks"),
	}
	first := New(firstStore, options)
	second := New(secondStore, options)
	requests := []ImportRequest{
		{OperationID: "shared-storage-a", Actor: "admin", Reason: "shared lock", TargetVersion: "image-catalog:revision-1", VersionLabel: "a", Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: fingerprint}},
		{OperationID: "shared-storage-b", Actor: "admin", Reason: "shared lock", TargetVersion: "image-catalog:revision-1", VersionLabel: "b", Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: fingerprint}},
	}
	for _, request := range requests {
		if _, err := first.StartImageImport(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	firstDone := make(chan error, 1)
	go func() {
		operation, processErr := first.ProcessTrustedImport(context.Background(), requests[0].OperationID)
		if processErr == nil && operation.Status != "succeeded" {
			processErr = fmt.Errorf("first operation = %#v", operation)
		}
		firstDone <- processErr
	}()
	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("first shared-storage import did not reach native backend")
	}
	secondDone := make(chan struct {
		operation *state.ManagementOperation
		err       error
	}, 1)
	go func() {
		operation, processErr := second.ProcessTrustedImport(context.Background(), requests[1].OperationID)
		secondDone <- struct {
			operation *state.ManagementOperation
			err       error
		}{operation: operation, err: processErr}
	}()
	select {
	case result := <-secondDone:
		t.Fatalf("second import crossed shared storage lock: operation=%#v err=%v", result.operation, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	close(backend.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	result := <-secondDone
	if result.err == nil || !strings.HasPrefix(result.err.Error(), "staging_capacity_low:") || result.operation == nil || result.operation.Status != "queued" {
		t.Fatalf("second shared-storage result = %#v err=%v", result.operation, result.err)
	}
}

func TestSharedManagementResourceLockSerializesAcrossServices(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "locks")
	first := New(nil, Options{OperationLockDir: lockDir})
	second := New(nil, Options{OperationLockDir: lockDir})
	firstUnlock, err := first.AcquireResourceLock(context.Background(), "image-catalog")
	if err != nil {
		t.Fatal(err)
	}
	secondDone := make(chan struct {
		unlock func()
		err    error
	}, 1)
	go func() {
		unlock, lockErr := second.AcquireResourceLock(context.Background(), "image-catalog")
		secondDone <- struct {
			unlock func()
			err    error
		}{unlock: unlock, err: lockErr}
	}()
	select {
	case result := <-secondDone:
		if result.unlock != nil {
			result.unlock()
		}
		t.Fatalf("shared management lock was not exclusive: err=%v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	firstUnlock()
	result := <-secondDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	result.unlock()
}

func TestImageImportUploadStreamFailureIsDurable(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, Options{UploadMaxBytes: 1024, Backend: &fakeImageBackend{}})
	request := ImportRequest{
		OperationID: "import-stream-failure", Actor: "admin", Reason: "test interrupted upload", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.02",
		Source: ImportSource{Kind: "local_upload", Format: "unified", ExpectedSHA256: strings.Repeat("b", 64), ExpectedSize: 3},
	}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	operation, err = service.FailImageImportUpload(context.Background(), operation.OperationID, context.Canceled)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != "failed" || operation.Phase != "upload" || operation.Error["code"] != "internal_error" {
		t.Fatalf("operation = %#v", operation)
	}
	audit, err := service.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) < 2 {
		t.Fatalf("audit = %#v", audit)
	}
}

func TestLocalImportReadFailureRecordsFingerprintAndCleanup(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fingerprint := strings.Repeat("d", 64)
	backend := &partialImageBackend{fakeImageBackend: fakeImageBackend{artifact: ImageArtifact{Fingerprint: fingerprint}}, err: uncertainActionError{}}
	service := New(store, Options{UploadMaxBytes: 1024, Backend: backend})
	content := []byte("abc")
	request := ImportRequest{OperationID: "local-read-failure", Actor: "admin", Reason: "record cleanup evidence", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.05", Source: ImportSource{Kind: "local_upload", Format: "unified", ExpectedSHA256: digestBytes(content), ExpectedSize: int64(len(content))}}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := service.UploadImageImport(context.Background(), operation.OperationID, content, nil)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != "failed" || backend.imports != 1 || backend.deletes != 1 {
		t.Fatalf("finished=%#v imports=%d deletes=%d", finished, backend.imports, backend.deletes)
	}
	if finished.Error["imported_fingerprint"] != fingerprint || finished.Error["cleanup"] != "verified" {
		t.Fatalf("cleanup evidence = %#v", finished.Error)
	}
}

func TestImportFailureDoesNotDeleteCatalogedFingerprint(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	fingerprint := strings.Repeat("e", 64)
	if err := store.SeedLegacyImage(ctx, fingerprint); err != nil {
		t.Fatal(err)
	}
	backend := &fakeImageBackend{}
	service := New(store, Options{TrustedImageSourceIDs: []string{"approved"}, Backend: backend})
	operation, err := service.StartImageImport(ctx, ImportRequest{
		OperationID: "trusted-import-cataloged", Actor: "admin", Reason: "preserve cataloged image", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.06",
		Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: fingerprint},
	})
	if err != nil {
		t.Fatal(err)
	}
	running, claimed, err := store.TransitionManagementOperation(ctx, operation.OperationID, "queued", "image_import", "running")
	if err != nil || !claimed {
		t.Fatalf("claim trusted import = %#v claimed=%v err=%v", running, claimed, err)
	}
	operation = running
	finished, err := service.finishBackendImportFailure(ctx, operation, ImageArtifact{Fingerprint: fingerprint}, uncertainActionError{})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != "outcome_unknown" || finished.Error["cleanup"] != "blocked_catalog_unverified" || backend.deletes != 0 {
		t.Fatalf("finished=%#v deletes=%d", finished, backend.deletes)
	}
	image, err := store.GetManagedImage(ctx, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if image == nil || !image.IsDefault {
		t.Fatalf("catalog image = %#v", image)
	}
}

func TestExistingImportOperationWinsBeforeAStaleCatalogCheck(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, Options{UploadMaxBytes: 1024, Backend: &fakeImageBackend{}})
	request := ImportRequest{
		OperationID: "import-replay", Actor: "admin", Reason: "replay import", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.02",
		Source: ImportSource{Kind: "local_upload", Format: "unified", ExpectedSHA256: strings.Repeat("b", 64), ExpectedSize: 3},
	}
	first, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertManagedImage(context.Background(), state.ManagedImage{Fingerprint: strings.Repeat("c", 64), Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed"}); err != nil {
		t.Fatal(err)
	}
	replay, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.OperationID != first.OperationID || replay.Status != "awaiting_upload" {
		t.Fatalf("replayed operation = %#v", replay)
	}
}

func TestIntegrityFailureRemainsIndependentFromSuitabilityFailure(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend := &fakeImageBackend{artifact: ImageArtifact{Fingerprint: strings.Repeat("b", 64), Architecture: "x86_64", InstanceType: "vm", Runtime: true, SyncTool: true, GuestReady: true}}
	service := New(store, Options{TrustedImageSourceIDs: []string{"approved"}, UploadMinimumFreeBytes: 512, AvailableImageStorageBytes: func() (int64, error) { return 4096, nil }, Backend: backend})
	request := ImportRequest{OperationID: "trusted-mismatch", Actor: "admin", Reason: "check image", TargetVersion: "image-catalog:revision-1", VersionLabel: "bad", Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: strings.Repeat("a", 64)}}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	operation, err = service.ProcessTrustedImport(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	image, err := store.GetManagedImage(context.Background(), strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if image == nil || image.Integrity != "failed" || image.Suitability != "failed" {
		t.Fatalf("verification = %#v", image)
	}
}

func TestUnsuitableImageIsPersistedAsFailedAndCannotBecomeDefault(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	legacy := strings.Repeat("a", 64)
	if err := store.SeedLegacyImage(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	failedFingerprint := strings.Repeat("c", 64)
	service := New(store, Options{UploadMaxBytes: 1024, Backend: &fakeImageBackend{artifact: ImageArtifact{Fingerprint: failedFingerprint, Architecture: "x86_64", InstanceType: "vm", Runtime: true, SyncTool: true, GuestReady: true}}})
	content := []byte("abc")
	request := ImportRequest{OperationID: "import-unsuitable", Actor: "admin", Reason: "test suitability", TargetVersion: "image-catalog:revision-1", VersionLabel: "bad", Source: ImportSource{Kind: "local_upload", Format: "unified", ExpectedSHA256: digestBytes(content), ExpectedSize: int64(len(content))}}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	operation, err = service.UploadImageImport(context.Background(), operation.OperationID, content, nil)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != "failed" {
		t.Fatalf("operation = %#v", operation)
	}
	image, err := store.GetManagedImage(context.Background(), failedFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if image == nil || image.State != "failed" || len(image.Findings) != 1 || image.Findings[0] != "unsupported_instance_type" {
		t.Fatalf("failed image = %#v", image)
	}
	selection, err := service.SelectDefault(context.Background(), "select-failed", "admin", "try bad image", failedFingerprint, "image-selection:revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if selection.Status != "failed" {
		t.Fatalf("failed image selection operation = %#v", selection)
	}
	selected, found, err := store.CurrentDefaultImage(context.Background())
	if err != nil || !found || selected != legacy {
		t.Fatalf("default changed after failed selection: %q found=%v err=%v", selected, found, err)
	}
}

func TestDeleteImageFailsClosedWithoutNativeBackend(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	legacy := strings.Repeat("a", 64)
	imageFingerprint := strings.Repeat("b", 64)
	if err := store.SeedLegacyImage(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertManagedImage(ctx, state.ManagedImage{Fingerprint: imageFingerprint, Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed"}); err != nil {
		t.Fatal(err)
	}
	service := New(store, Options{TrustedImageSourceIDs: []string{"approved"}})
	operation, err := service.DeleteImage(ctx, "delete-without-backend", "admin", "remove image", imageFingerprint, "image:"+imageFingerprint+":revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != "failed" || operation.Error["code"] != "authority_unavailable" {
		t.Fatalf("operation = %#v", operation)
	}
	image, err := store.GetManagedImage(ctx, imageFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if image == nil {
		t.Fatal("catalog image was removed without native deletion")
	}
}

func TestVMActionOperationIsIdempotentAndRejectsStaleGeneration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	updatedAt := "2026-10-27T00:00:00Z"
	lease := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	fingerprint := strings.Repeat("a", 64)
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "task-1", SessionID: "session-1", Generation: 4, State: "active", InstanceName: "agent-vm-task-1", LeaseExpiresAt: &lease, CreatedAt: updatedAt, UpdatedAt: updatedAt, ImageFingerprint: fingerprint, ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	backend := &fakeActionBackend{}
	service := New(store, Options{Actions: backend})
	request := VMActionRequest{OperationID: "restart-1", Actor: "admin", Reason: "repair", TaskID: "task-1", TargetVersion: "vm:task-1:" + updatedAt, Action: "restart", ExpectedGeneration: 4}
	first, err := service.ExecuteVMAction(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ExecuteVMAction(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "succeeded" || second.Status != "succeeded" || backend.vmCalls != 1 {
		t.Fatalf("first=%#v second=%#v calls=%d", first, second, backend.vmCalls)
	}
	request.OperationID = "restart-stale"
	request.ExpectedGeneration = 3
	if _, err := service.ExecuteVMAction(context.Background(), request); err == nil || !strings.HasPrefix(err.Error(), "stale_target:") {
		t.Fatalf("stale action error = %v", err)
	}
}

func TestUncertainVMActionRemainsOutcomeUnknown(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	updatedAt := "2026-10-27T00:00:00Z"
	lease := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "task-2", SessionID: "session-2", Generation: 1, State: "active", InstanceName: "agent-vm-task-2", LeaseExpiresAt: &lease, CreatedAt: updatedAt, UpdatedAt: updatedAt, ImageFingerprint: strings.Repeat("a", 64), ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	backend := &fakeActionBackend{err: uncertainActionError{}}
	service := New(store, Options{Actions: backend})
	operation, err := service.ExecuteVMAction(context.Background(), VMActionRequest{OperationID: "restart-unknown", Actor: "admin", Reason: "repair", TaskID: "task-2", TargetVersion: "vm:task-2:" + updatedAt, Action: "restart", ExpectedGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != "outcome_unknown" {
		t.Fatalf("operation = %#v", operation)
	}
}

func TestProtocolUncertainVMActionRemainsOutcomeUnknown(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	updatedAt := "2026-10-27T00:00:00Z"
	lease := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "task-protocol-unknown", SessionID: "session-protocol-unknown", Generation: 1, State: "active", InstanceName: "agent-vm-task-protocol-unknown", LeaseExpiresAt: &lease, CreatedAt: updatedAt, UpdatedAt: updatedAt, ImageFingerprint: strings.Repeat("a", 64), ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	backend := &fakeActionBackend{err: protocol.NewError("restart_partial", "guest stop failed", map[string]any{"outcome_verified": false})}
	service := New(store, Options{Actions: backend})
	operation, err := service.ExecuteVMAction(context.Background(), VMActionRequest{OperationID: "restart-protocol-unknown", Actor: "admin", Reason: "repair", TaskID: "task-protocol-unknown", TargetVersion: "vm:task-protocol-unknown:" + updatedAt, Action: "restart", ExpectedGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != "outcome_unknown" {
		t.Fatalf("operation = %#v", operation)
	}
}

func TestMaintenanceChecksCurrentSystemVersionBeforeClaim(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, Options{Actions: &fakeActionBackend{}, CurrentSystemVersion: func(context.Context) (string, error) { return "system:revision-2", nil }})
	request := MaintenanceRequest{OperationID: "maintenance-stale", Actor: "admin", Reason: "reconcile", TargetVersion: "system:revision-1", Action: "reconcile"}
	if _, err := service.ExecuteMaintenance(context.Background(), request); err == nil || !strings.HasPrefix(err.Error(), "stale_target:") {
		t.Fatalf("maintenance error = %v", err)
	}
	operation, err := store.GetManagementOperation(context.Background(), request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if operation != nil {
		t.Fatalf("stale maintenance was claimed: %#v", operation)
	}
}

func TestMaintenanceDoesNotBecomeStaleFromItsOwnJournalWrites(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend := &fakeActionBackend{}
	service := New(store, Options{Actions: backend, CurrentSystemVersion: func(context.Context) (string, error) { return "system:initial", nil }})
	operation, err := service.ExecuteMaintenance(context.Background(), MaintenanceRequest{OperationID: "maintenance-valid", Actor: "admin", Reason: "reconcile", TargetVersion: "system:initial", Action: "reconcile"})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != "succeeded" {
		t.Fatalf("operation = %#v", operation)
	}
}

func TestRecoverPendingOperationsWaitsForPeerOperationLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.sqlite3")
	firstStore, err := state.OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer firstStore.Close()
	secondStore, err := state.OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	backend := &blockingActionBackend{entered: make(chan struct{}), release: make(chan struct{})}
	lockDir := filepath.Join(root, "locks")
	first := New(firstStore, Options{Actions: backend, CurrentSystemVersion: func(context.Context) (string, error) { return "system:initial", nil }, OperationLockDir: lockDir})
	second := New(secondStore, Options{OperationLockDir: lockDir})
	request := MaintenanceRequest{OperationID: "maintenance-peer-fence", Actor: "admin", Reason: "peer fence", TargetVersion: "system:initial", Action: "reconcile"}
	executionDone := make(chan struct{})
	var execution *state.ManagementOperation
	var executionErr error
	go func() {
		execution, executionErr = first.ExecuteMaintenance(context.Background(), request)
		close(executionDone)
	}()
	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("peer maintenance action did not start")
	}
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- second.RecoverPendingOperations(context.Background()) }()
	select {
	case err := <-recoveryDone:
		t.Fatalf("recovery finalized a live peer operation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.release)
	select {
	case <-executionDone:
	case <-time.After(time.Second):
		t.Fatal("peer maintenance action did not finish")
	}
	if executionErr != nil || execution == nil || execution.Status != "succeeded" {
		t.Fatalf("execution=%#v err=%v", execution, executionErr)
	}
	select {
	case err := <-recoveryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("recovery did not finish after peer released operation lock")
	}
	recovered, err := second.GetOperation(context.Background(), request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.Status != "succeeded" {
		t.Fatalf("recovered operation = %#v, want peer result", recovered)
	}
}

func TestMaintenanceCannotCrossReleaseActivationDrain(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetMeta(context.Background(), "draining", "1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(context.Background(), "drain_owner", "release-tx-123"); err != nil {
		t.Fatal(err)
	}
	backend := &fakeActionBackend{}
	service := New(store, Options{Actions: backend, CurrentSystemVersion: func(context.Context) (string, error) { return "system:initial", nil }})
	_, err = service.ExecuteMaintenance(context.Background(), MaintenanceRequest{OperationID: "maintenance-release-fence", Actor: "admin", Reason: "release fence", TargetVersion: "system:initial", Action: "reconcile"})
	if err == nil || !strings.Contains(err.Error(), "draining: release activation fence") {
		t.Fatalf("maintenance fence error = %v", err)
	}
	if backend.maintenanceCalls != 0 {
		t.Fatalf("maintenance backend was invoked: %#v", backend)
	}
	queued, err := store.GetManagementOperation(context.Background(), "maintenance-release-fence")
	if err != nil {
		t.Fatal(err)
	}
	if queued == nil || queued.Status != "queued" {
		t.Fatalf("fenced operation = %#v, want queued", queued)
	}
}

func TestVMActionCannotCrossReleaseActivationDrain(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.SetMeta(ctx, "draining", "1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(ctx, "drain_owner", "release-tx-456"); err != nil {
		t.Fatal(err)
	}
	updatedAt := "2026-10-28T00:00:00Z"
	lease := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if err := store.InsertTask(ctx, state.Task{TaskID: "release-fence-task", SessionID: "release-fence-session", Generation: 3, State: "active", InstanceName: "agent-vm-release-fence", LeaseExpiresAt: &lease, CreatedAt: updatedAt, UpdatedAt: updatedAt, ImageFingerprint: strings.Repeat("a", 64), ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	backend := &fakeActionBackend{}
	service := New(store, Options{Actions: backend})
	_, err = service.ExecuteVMAction(ctx, VMActionRequest{OperationID: "release-fence-vm", Actor: "admin", Reason: "release fence", TaskID: "release-fence-task", TargetVersion: "vm:release-fence-task:" + updatedAt, Action: "release", ExpectedGeneration: 3})
	if err == nil || !strings.Contains(err.Error(), "draining: release activation fence") {
		t.Fatalf("VM action fence error = %v", err)
	}
	if backend.vmCalls != 0 {
		t.Fatalf("VM backend was invoked: %#v", backend)
	}
	queued, err := store.GetManagementOperation(ctx, "release-fence-vm")
	if err != nil {
		t.Fatal(err)
	}
	if queued == nil || queued.Status != "queued" {
		t.Fatalf("fenced VM operation = %#v, want queued", queued)
	}
}

func TestRecoverPendingOperationsDoesNotReplayTrustedImports(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend := &fakeImageBackend{artifact: ImageArtifact{Fingerprint: strings.Repeat("a", 64), Architecture: "x86_64", InstanceType: "container", Runtime: true, SyncTool: true, GuestReady: true}}
	service := New(store, Options{TrustedImageSourceIDs: []string{"approved"}, Backend: backend})
	request := ImportRequest{OperationID: "trusted-recovery", Actor: "admin", Reason: "recover image", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.03", Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: strings.Repeat("a", 64)}}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != "queued" {
		t.Fatalf("queued operation = %#v", operation)
	}
	if _, claimed, err := store.TransitionManagementOperation(context.Background(), operation.OperationID, "queued", "image_import", "running"); err != nil || !claimed {
		t.Fatalf("simulate interrupted import: claimed=%v err=%v", claimed, err)
	}
	if err := service.RecoverPendingOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.GetOperation(context.Background(), operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.Status != "outcome_unknown" || backend.imports != 0 {
		t.Fatalf("recovered=%#v backend imports=%d", recovered, backend.imports)
	}
	if recovered.Error["expected_fingerprint"] != request.Source.ExpectedFingerprint || recovered.Error["cleanup"] != "unverified" {
		t.Fatalf("recovery evidence = %#v", recovered.Error)
	}
}

func TestFinishOperationPersistsAfterExecutionContextExpires(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, Options{TrustedImageSourceIDs: []string{"approved"}})
	request := ImportRequest{
		OperationID: "finalize-after-timeout", Actor: "admin", Reason: "record timeout", TargetVersion: "image-catalog:revision-1", VersionLabel: "2026.04",
		Source: ImportSource{Kind: "trusted_remote", SourceID: "approved", ExpectedFingerprint: strings.Repeat("a", 64)},
	}
	operation, err := service.StartImageImport(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	running, claimed, err := store.TransitionManagementOperation(context.Background(), operation.OperationID, "queued", "image_import", "running")
	if err != nil || !claimed {
		t.Fatalf("claim operation: claimed=%v err=%v", claimed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished, err := service.finishOperation(ctx, running, "reconciliation", "outcome_unknown", nil, map[string]any{"code": "internal_error", "retryable": true})
	if err != nil {
		t.Fatal(err)
	}
	if finished == nil || finished.Status != "outcome_unknown" {
		t.Fatalf("finished operation = %#v", finished)
	}
	audits, err := service.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) == 0 || audits[0].OperationID == nil || *audits[0].OperationID != request.OperationID {
		t.Fatalf("audits = %#v", audits)
	}
}

type uncertainActionError struct{}

func (uncertainActionError) Error() string   { return "native result is uncertain" }
func (uncertainActionError) Uncertain() bool { return true }
