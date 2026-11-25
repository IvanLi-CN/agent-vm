package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyStoppedDeploymentJournal requires the incumbent's /run/agent-vm-native/
// deployd.lock to be root-owned before it can copy the journal. Re-applying the
// fresh-accounts rule against /run during the handoff would chown that lock to
// agent-vm-deploy first, so the handoff must only install the rule file and let
// it apply at the next boot.
func TestHandoffNeverTakesOverTheLiveDeploydLock(t *testing.T) {
	rule, err := os.ReadFile(filepath.Join("../../../../..", "deploy/assets/tmpfiles.d/agent-vm-fresh-accounts.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rule), "f /run/agent-vm-native/deployd.lock 0600 agent-vm-deploy agent-vm-deploy -") {
		t.Fatalf("the rule no longer owns the runtime lock, so this guard is stale:\n%s", rule)
	}
	source, err := os.ReadFile("account_deploy_switch.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), `"agent-vm-fresh-accounts.conf", freshAccounts, "/run/agent-vm-native"`) {
		t.Fatal("the handoff applies the fresh-account rule to /run and would steal the live lock")
	}
}
