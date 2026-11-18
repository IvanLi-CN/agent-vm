//go:build linux

package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
)

func TestNativeAccountMigrationCandidate(t *testing.T) {
	root := os.Getenv("AGENT_VM_NATIVE_ACCOUNT_FIXTURE")
	config := os.Getenv("AGENT_VM_NATIVE_ACCOUNT_CONFIG")
	if root == "" || config == "" {
		t.Skip("requires a session-owned Linux fixture with a running historical blue-green installation")
	}
	if os.Geteuid() != 0 {
		t.Fatal("fixture requires local root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	input := Inputs{Config: config, Bundle: filepath.Join(root, "product.tar"), Materials: filepath.Join(root, "materials")}
	report, err := PreflightAccountMigration(ctx, input)
	if err != nil {
		t.Fatalf("preflight: %v (%v)", err, report.Blockers)
	}
	product := filepath.Join(t.TempDir(), "product")
	file, err := os.Open(input.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := Extract(file, product, 256<<20); err != nil {
		t.Fatal(err)
	}
	client := deployd.Client{Socket: deployd.Defaults().Socket, Timeout: 8 * time.Minute}
	receipt, err := prepareAccountCandidate(ctx, input, product, report, client)
	t.Logf("transaction=%s phase=%s", receipt.ID, receipt.Action)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := activateAccountCandidate(ctx, client, receipt, report)
	if err != nil {
		t.Fatal(err)
	}
	if err := handoffDeploymentUnit(ctx, handoff, input); err != nil {
		t.Fatal(err)
	}
	t.Logf("active=%s deployment account adopted the controller", handoff.Lease.Slot)
}
