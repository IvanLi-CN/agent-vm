package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestManagementMigrationIsAdditiveAndSeedsLegacyDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	db, err := openLegacyFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fingerprint := strings.Repeat("a", 64)
	if err := store.SeedLegacyImage(context.Background(), fingerprint); err != nil {
		t.Fatal(err)
	}
	capabilities, err := store.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 4 {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	image, err := store.GetManagedImage(context.Background(), fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if image == nil || !image.IsDefault || image.Integrity != "legacy_unverified" || image.Suitability != "not_assessed" {
		t.Fatalf("legacy image = %#v", image)
	}
	task := Task{TaskID: "legacy-task", SessionID: "legacy-session", Generation: 9, State: "absent", InstanceName: "agent-vm-legacy", CreatedAt: "now", UpdatedAt: "now", ImageFingerprint: fingerprint, ResourcesJSON: "{}", ResourceReservationJSON: "{}"}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	image, err = store.GetManagedImage(context.Background(), fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if image.References.Default != 1 {
		t.Fatalf("legacy references = %#v", image.References)
	}
}

func TestLegacySeedBackfillsHistoricalTaskImages(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	historical, configured := strings.Repeat("f", 64), strings.Repeat("a", 64)
	if err := store.InsertTask(ctx, Task{TaskID: "historical-task", SessionID: "historical-session", Generation: 2, State: "cached", InstanceName: "agent-vm-historical", CreatedAt: "now", UpdatedAt: "now", ImageFingerprint: historical, ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedLegacyImage(ctx, configured); err != nil {
		t.Fatal(err)
	}
	image, err := store.GetManagedImage(ctx, historical)
	if err != nil {
		t.Fatal(err)
	}
	if image == nil || image.SourceKind != "legacy_config" || image.Integrity != "legacy_unverified" || image.References.Cached != 1 {
		t.Fatalf("historical image = %#v", image)
	}
}

func TestDefaultSwitchLeavesExistingTaskImageBindingUntouched(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	oldFingerprint, newFingerprint := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := store.SeedLegacyImage(ctx, oldFingerprint); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertManagedImage(ctx, ManagedImage{Fingerprint: newFingerprint, VersionLabel: testStringPointer("2026.02"), Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed", DefaultCompatibility: "none"}); err != nil {
		t.Fatal(err)
	}
	task := Task{TaskID: "old-session", SessionID: "old-session", Generation: 1, State: "cached", InstanceName: "agent-vm-old-session", CreatedAt: "now", UpdatedAt: "now", ImageFingerprint: oldFingerprint, ResourcesJSON: "{}", ResourceReservationJSON: "{}"}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	version, err := store.ImageSelectionVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SelectManagedImage(ctx, newFingerprint, version); err != nil {
		t.Fatal(err)
	}
	selected, found, err := store.CurrentDefaultImage(ctx)
	if err != nil || !found || selected != newFingerprint {
		t.Fatalf("selected=%q found=%v err=%v", selected, found, err)
	}
	oldTask, err := store.GetTask(ctx, map[string]any{"task_id": task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if oldTask.ImageFingerprint != oldFingerprint {
		t.Fatalf("old task fingerprint changed: %q", oldTask.ImageFingerprint)
	}
	oldImage, err := store.GetManagedImage(ctx, oldFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if oldImage.References.Cached != 1 || oldImage.IsDefault || oldImage.Revision < 2 {
		t.Fatalf("old image references = %#v default=%v revision=%d", oldImage.References, oldImage.IsDefault, oldImage.Revision)
	}
}

func TestManagementAuditListAcceptsNullDetail(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.RecordManagementAudit(ctx, ManagementAudit{EventID: "audit-null-detail", Action: "test", Actor: "admin", Reason: "verify", Target: map[string]any{"kind": "test", "id": "one"}, Outcome: "accepted", Phase: "test"}); err != nil {
		t.Fatal(err)
	}
	audits, err := store.ListManagementAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].Detail != nil {
		t.Fatalf("audits = %#v", audits)
	}
}

func TestImageCatalogVersionChangesOnlyForCatalogMutations(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first, err := store.ImageCatalogVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("d", 64)
	if err := store.InsertManagedImage(ctx, ManagedImage{Fingerprint: fingerprint, Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed"}); err != nil {
		t.Fatal(err)
	}
	second, err := store.ImageCatalogVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second != "image-catalog:revision-2" {
		t.Fatalf("catalog versions = %q -> %q", first, second)
	}
	selectionBefore, err := store.ImageSelectionVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SelectManagedImage(ctx, fingerprint, selectionBefore); err != nil {
		t.Fatal(err)
	}
	third, err := store.ImageCatalogVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third != second {
		t.Fatalf("selection changed catalog version: %q -> %q", second, third)
	}
}

func TestPreparingTaskReferenceIsCountedOnceAndClearedOnActivation(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	fingerprint := strings.Repeat("e", 64)
	if err := store.InsertManagedImage(ctx, ManagedImage{Fingerprint: fingerprint, Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTask(ctx, Task{TaskID: "preparing-task", SessionID: "preparing-session", Generation: 1, State: "preparing", InstanceName: "agent-vm-preparing", CreatedAt: "now", UpdatedAt: "now", ImageFingerprint: fingerprint, ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	image, err := store.GetManagedImage(ctx, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if image.References.PendingCreation != 1 {
		t.Fatalf("preparing references = %#v", image.References)
	}
	if err := store.UpdateTask(ctx, "preparing-task", map[string]any{"state": "active", "updated_at": "active"}); err != nil {
		t.Fatal(err)
	}
	image, err = store.GetManagedImage(ctx, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if image.References.PendingCreation != 0 || image.References.Active != 1 {
		t.Fatalf("active references = %#v", image.References)
	}
}

func TestDeleteManagedImageRechecksEveryReferenceClass(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	fingerprint := strings.Repeat("c", 64)
	if err := store.InsertManagedImage(ctx, ManagedImage{Fingerprint: fingerprint, Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed", DefaultCompatibility: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddImageReference(ctx, fingerprint, "pending_creation", "create-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddImageReference(ctx, fingerprint, "verification", "verify-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteManagedImage(ctx, fingerprint); err == nil {
		t.Fatal("verification reference was not rejected")
	} else {
		var referenceErr *ImageReferenceError
		if !errors.As(err, &referenceErr) || referenceErr.Counts.PendingCreation != 1 || referenceErr.Counts.Verification != 1 {
			t.Fatalf("delete error = %v", err)
		}
	}
	if err := store.RemoveImageReference(ctx, fingerprint, "pending_creation", "create-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveImageReference(ctx, fingerprint, "verification", "verify-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteManagedImage(ctx, fingerprint); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.GetManagedImage(ctx, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != nil {
		t.Fatalf("deleted image still exists: %#v", deleted)
	}
}

func TestDeletedLegacyImageIsNotReseededOnRestart(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	legacy, replacement := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := store.SeedLegacyImage(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertManagedImage(ctx, ManagedImage{Fingerprint: replacement, Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed"}); err != nil {
		t.Fatal(err)
	}
	selection, err := store.ImageSelectionVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SelectManagedImage(ctx, replacement, selection); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteManagedImage(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedLegacyImage(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	image, err := store.GetManagedImage(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if image != nil {
		t.Fatalf("deleted legacy image was reseeded: %#v", image)
	}
}

func testStringPointer(value string) *string { return &value }

func openLegacyFixture(path string) (*sql.DB, error) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixtures", "control-baseline.sql"))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(string(fixture)); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
