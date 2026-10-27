package management

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

type fakeImageBackend struct {
	artifact ImageArtifact
	imports  int
	deletes  int
}

func (backend *fakeImageBackend) ImportTrusted(context.Context, string, string) (ImageArtifact, error) {
	backend.imports++
	return backend.artifact, nil
}

func (backend *fakeImageBackend) ImportLocal(context.Context, string, []byte, []byte) (ImageArtifact, error) {
	backend.imports++
	return backend.artifact, nil
}

func (backend *fakeImageBackend) DeleteImage(context.Context, string) error {
	backend.deletes++
	return nil
}

type fakeActionBackend struct {
	vmCalls int
	err     error
}

func (backend *fakeActionBackend) ExecuteVMAction(context.Context, string, string, string, int64, map[string]any) (map[string]any, error) {
	backend.vmCalls++
	return map[string]any{"state": "changed"}, backend.err
}

func (backend *fakeActionBackend) ExecuteMaintenance(context.Context, string, string, map[string]any) (map[string]any, error) {
	return map[string]any{"state": "changed"}, backend.err
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
	service := New(store, Options{TrustedImageSourceIDs: []string{"approved"}, Backend: backend})
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
	service := New(store, Options{})
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

type uncertainActionError struct{}

func (uncertainActionError) Error() string   { return "native result is uncertain" }
func (uncertainActionError) Uncertain() bool { return true }
