package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Account handoff keeps /etc/tmpfiles.d/agent-vm-fresh-accounts.conf so the
// dedicated accounts get their runtime directories back at every boot. Without
// the deployd singleton lock rule the controller fails its mount namespace
// setup after a restart, so the rule is part of the handoff contract.
func TestFreshAccountRulesRecreateDeploydRuntime(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("../../../../..", "deploy/assets/tmpfiles.d/agent-vm-fresh-accounts.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "f /run/agent-vm-native/deployd.lock 0600 agent-vm-deploy agent-vm-deploy -") {
		t.Fatalf("fresh account rules lost the deployd singleton lock:\n%s", source)
	}
}
