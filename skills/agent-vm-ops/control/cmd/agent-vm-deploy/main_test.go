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
