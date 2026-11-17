package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func nameStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenForTest(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func insertNamedTestImage(t *testing.T, s *Store, letter, label string) string {
	t.Helper()
	fp := strings.Repeat(letter, 64)
	err := s.InsertManagedImage(context.Background(), ManagedImage{Fingerprint: fp, VersionLabel: &label, Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed", DefaultCompatibility: "none", CreatedAt: letter})
	if err != nil {
		t.Fatal(err)
	}
	return fp
}
func expectImageCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *ImageSelectionError
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("expected %s: %v", code, err)
	}
}

func TestImageNameMigrationReservesOriginalSuffixesAndReenters(t *testing.T) {
	ctx := context.Background()
	s := nameStore(t)
	labels := []string{"demo", "demo", "demo-2", "", "bad/name", strings.Repeat("长", 140), "e\u0301", "é"}
	for n, label := range labels {
		insertNamedTestImage(t, s, string(rune('1'+n)), label)
	}
	// Recreate the exact pre-migration condition without relying on import allocation.
	if err := s.withWrite(ctx, func(tx *Tx) error {
		if _, err := tx.exec("DELETE FROM image_names"); err != nil {
			return err
		}
		_, err := tx.exec("DELETE FROM meta WHERE key='image_names_migrated'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureManagementSchema(ctx); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for n := range labels {
		value, err := s.ImageName(ctx, strings.Repeat(string(rune('1'+n)), 64))
		if err != nil || value == nil {
			t.Fatalf("name %d: %v", n, err)
		}
		names = append(names, *value)
		if _, err := NormalizeImageName(*value); err != nil {
			t.Fatal(err)
		}
	}
	if names[0] != "demo" || names[1] != "demo-3" || names[2] != "demo-2" || names[3] != "image-"+strings.Repeat("4", 64) || names[4] != "bad-name" || names[6] != "é" || names[7] != "é-2" {
		t.Fatalf("names: %#v", names)
	}
	before, _ := s.StateEpoch(ctx)
	if err := s.EnsureManagementSchema(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := s.StateEpoch(ctx)
	if before != after {
		t.Fatal("reentry mutated state")
	}
}

func TestNameRebindCASAuditAndPinnedOperation(t *testing.T) {
	ctx := context.Background()
	s := nameStore(t)
	a := insertNamedTestImage(t, s, "a", "base")
	b := insertNamedTestImage(t, s, "b", "next")
	version, _ := s.ImageCatalogVersion(ctx)
	_, err := s.SetImageName(ctx, b, nil, version, "clear-b", "admin", "test")
	if err != nil {
		t.Fatal(err)
	}
	session := "new-session"
	request := map[string]any{"image": "base"}
	pending := map[string]any{"outcome": "pending", "_request": request}
	if _, err := s.ClaimOperation(ctx, "create", "acquire", &session, nil, pending, AcquireImageSelection{}); err != nil {
		t.Fatal(err)
	}
	if pending["_image_fingerprint"] != a {
		t.Fatalf("pin: %#v", pending)
	}
	version, _ = s.ImageCatalogVersion(ctx)
	name := "base"
	_, err = s.SetImageName(ctx, b, &name, version, "move", "admin", "roll forward")
	if err != nil {
		t.Fatal(err)
	}
	prior, err := s.ClaimOperation(ctx, "create", "acquire", &session, nil, map[string]any{"outcome": "pending", "_request": request}, AcquireImageSelection{})
	if err != nil || prior.Response["_image_fingerprint"] != a {
		t.Fatalf("replay changed pin: %#v %v", prior, err)
	}
	if err := s.DeleteManagedImage(ctx, a); err == nil {
		t.Fatal("pending creation did not protect image")
	}
	replay, err := s.SetImageName(ctx, b, &name, version, "move", "admin", "roll forward")
	if err != nil || replay["fingerprint"] != b {
		t.Fatalf("name replay: %v", err)
	}
	_, err = s.SetImageName(ctx, a, &name, version, "stale", "admin", "")
	expectImageCode(t, err, "stale_target")
	audits, err := s.ListManagementAudit(ctx, 20)
	if err != nil || len(audits) != 2 {
		t.Fatalf("audit %d: %v", len(audits), err)
	}
	images, err := s.CreatableImages(ctx)
	if err != nil || len(images) != 2 {
		t.Fatalf("list %v %v", images, err)
	}
}

func TestExplicitImageConflictDoesNotClaimOperation(t *testing.T) {
	ctx := context.Background()
	s := nameStore(t)
	a := insertNamedTestImage(t, s, "a", "one")
	b := insertNamedTestImage(t, s, "b", "two")
	if err := s.InsertTask(ctx, Task{TaskID: "task", SessionID: "session", ImageFingerprint: a, State: "cached", InstanceName: "guest", CreatedAt: "now", UpdatedAt: "now", ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	session := "session"
	_, err := s.ClaimOperation(ctx, "conflict", "acquire", &session, nil, map[string]any{"outcome": "pending", "_request": map[string]any{"image": b}}, AcquireImageSelection{})
	expectImageCode(t, err, "image_conflict")
	operation, _ := s.GetOperation(ctx, "conflict")
	if operation != nil {
		t.Fatal("conflict wrote journal")
	}
	pending := map[string]any{"outcome": "pending", "_request": map[string]any{}}
	if _, err := s.ClaimOperation(ctx, "reuse", "acquire", &session, nil, pending, AcquireImageSelection{DefaultFingerprint: b}); err != nil || pending["_image_fingerprint"] != a {
		t.Fatalf("implicit reuse changed image: %#v %v", pending, err)
	}
}

func TestAcquireRejectsVirtualMachineImageBeforeJournalClaim(t *testing.T) {
	s := nameStore(t)
	ctx := context.Background()
	fp := strings.Repeat("9", 64)
	if err := s.InsertManagedImage(ctx, ManagedImage{Fingerprint: fp, InstanceType: "virtual-machine", Architecture: "x86_64", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed"}); err != nil {
		t.Fatal(err)
	}
	session := "container-only"
	generation := int64(0)
	_, err := s.ClaimOperation(ctx, "vm-image-refusal", "acquire", &session, &generation, map[string]any{"_request": map[string]any{"image": fp}, "outcome": "pending"}, AcquireImageSelection{})
	expectImageCode(t, err, "image_unavailable")
	operation, err := s.GetOperation(ctx, "vm-image-refusal")
	if err != nil || operation != nil {
		t.Fatalf("refused selector created journal: %#v %v", operation, err)
	}
}
