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
