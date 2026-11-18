package main

import (
	"strings"
	"testing"
)

func TestInstallRequiresAllExplicitInputs(t *testing.T) {
	for _, args := range [][]string{{"install"}, {"install", "--apply"}, {"install", "--config", "/tmp/config", "--bundle", "/tmp/product"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "requires --config") {
			t.Fatalf("incomplete install accepted: %v", err)
		}
	}
}

func TestUpdateRequiresReleaseIdentity(t *testing.T) {
	for _, args := range [][]string{{"update"}, {"update", "--version", "1.4.22"}, {"update", "--source-revision", strings.Repeat("a", 40)}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "update requires") {
			t.Fatalf("incomplete update accepted: %v", err)
		}
	}
}

func TestServeDispatchAndLazyCLI(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"serve", "--config", t.TempDir() + "/missing.json"}); err == nil || !strings.Contains(err.Error(), "deployd config missing") {
		t.Fatalf("serve dispatch = %v", err)
	}
	if err := run([]string{"status", "--socket", t.TempDir() + "/missing.sock", "--timeout", "1"}); err == nil || strings.Contains(err.Error(), "deployd config missing") {
		t.Fatalf("CLI started daemon resources: %v", err)
	}
}
