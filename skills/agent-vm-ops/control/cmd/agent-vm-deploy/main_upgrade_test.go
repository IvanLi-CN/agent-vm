package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// bundleWithUnits builds a candidate archive carrying release-owned unit files,
// the shape `update` extracts before a blue-green transaction starts.
func bundleWithUnits(t *testing.T, names ...string) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, name := range names {
		content := []byte("[Service]\nExecStart=/new-" + name + "\n")
		if err := writer.WriteHeader(&tar.Header{Name: "systemd/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle.tar")
	if err := os.WriteFile(bundle, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return bundle
}

// The unit extraction must not land in the verified staging directory: by the
// time it runs, exposeVerifiedBundle has handed that directory to the
// deployment account, so a root-owned Mkdir there fails and every update stops
// before the transaction. It also must tolerate an existing temp root, because
// install.Extract creates its own destination and refuses a non-empty one.
func TestCandidateUnitExtractionStaysOutsideTheStagingTree(t *testing.T) {
	staging := t.TempDir()
	bundle := bundleWithUnits(t, "agent-vm-native@.service")
	handedOver := filepath.Join(staging, "bundle.tar")
	content, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handedOver, content, 0o644); err != nil {
		t.Fatal(err)
	}

	extracted := ""
	published, err := publishCandidateHostUnitsWith(handedOver, func(product string) ([]string, error) {
		extracted = product
		if _, statErr := os.Stat(product); statErr != nil {
			t.Fatalf("extraction target was not created: %v", statErr)
		}
		return []string{"agent-vm-native@.service"}, nil
	})
	if err != nil {
		t.Fatalf("candidate unit extraction failed: %v", err)
	}
	if len(published) != 1 || published[0] != "agent-vm-native@.service" {
		t.Fatalf("published = %v", published)
	}
	if extracted == "" || filepath.Dir(filepath.Dir(extracted)) == staging || !filepath.IsAbs(extracted) {
		t.Fatalf("units were extracted into the staging tree: %q", extracted)
	}
	if _, statErr := os.Stat(filepath.Dir(extracted)); !os.IsNotExist(statErr) {
		t.Fatalf("extraction root was not cleaned up: %v", statErr)
	}
}

// A repeated update runs the same extraction against a temp root that may
// already hold siblings; that must stay a success rather than a create error.
func TestCandidateUnitExtractionIsRepeatable(t *testing.T) {
	bundle := bundleWithUnits(t, "agent-vm-deployd.service", "agent-vm-deployd.socket")
	for attempt := range 2 {
		published, err := publishCandidateHostUnitsWith(bundle, func(string) ([]string, error) {
			return []string{"agent-vm-deployd.socket"}, nil
		})
		if err != nil {
			t.Fatalf("attempt %d failed: %v", attempt, err)
		}
		if len(published) != 1 {
			t.Fatalf("attempt %d published = %v", attempt, published)
		}
	}
}

// Only socket units are restarted. Restarting a plain service would interrupt
// deployd or the slot in the middle of the update.
func TestActivatePublishedUnitsIgnoresNonSocketUnits(t *testing.T) {
	if err := activatePublishedUnitsWith(nil, func(...string) error {
		t.Fatal("empty publish list ran systemd")
		return nil
	}); err != nil {
		t.Fatalf("empty publish list failed: %v", err)
	}
	var calls [][]string
	if err := activatePublishedUnitsWith([]string{"agent-vm-native@.service", "agent-vm-slot-relay.socket", "agent-vm-deployd.socket"}, func(args ...string) error {
		calls = append(calls, args)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"daemon-reload"}, {"restart", "agent-vm-slot-relay.socket"}, {"restart", "agent-vm-deployd.socket"}}
	if len(calls) != len(want) {
		t.Fatalf("systemd calls = %v", calls)
	}
	for index := range want {
		if len(calls[index]) != len(want[index]) || calls[index][0] != want[index][0] || (len(want[index]) == 2 && calls[index][1] != want[index][1]) {
			t.Fatalf("systemd call %d = %v, want %v", index, calls[index], want[index])
		}
	}

	// A reload failure stops the update before any socket is restarted, so a
	// half-published host is never reported as successful.
	rolled := 0
	if err := activatePublishedUnitsWith([]string{"agent-vm-deployd.socket"}, func(...string) error {
		rolled++
		return os.ErrPermission
	}); err == nil || rolled != 1 {
		t.Fatalf("reload failure = %v after %d call(s)", err, rolled)
	}
}
