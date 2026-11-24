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

func TestSummarizeRecoveryPointsCountsEligible(t *testing.T) {
	summary := summarizeRecoveryPoints(3, []bool{true, false, false})
	if summary["count"] != 3 || summary["eligible"] != 1 || summary["next_action"] != "agent-vm-deploy prune-recovery-points" {
		t.Fatalf("summary = %+v", summary)
	}
	empty := summarizeRecoveryPoints(0, nil)
	if empty["count"] != 0 || empty["budget"] != recoveryPointBudgetHint || empty["next_action"] != nil {
		t.Fatalf("empty summary = %+v", empty)
	}
	if _, ok := empty["next_action"]; ok {
		t.Fatal("empty summary advertises a prune action")
	}
}
