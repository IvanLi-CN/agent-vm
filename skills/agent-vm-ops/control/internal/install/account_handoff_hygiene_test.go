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

// A dedicated deployd must be able to stage a verified release bundle after the
// handoff. Without release-staging in ReadWritePaths every later update fails at
// /v1/stage even though the slot roles look healthy.
func TestDedicatedDeploydCanStageReleaseBundles(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("../../../../..", "deploy/assets/systemd/fresh-deploy-account.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "/var/lib/agent-vm-native/release-staging") {
		t.Fatalf("dedicated deployd cannot write the release staging root:\n%s", source)
	}
}

// A legacy install may already run the helper sockets with older definitions.
// systemd drops their listening descriptor when the unit file changes, so the
// handoff must restart them instead of issuing a no-op start.
func TestHandoffRestartsHelperSockets(t *testing.T) {
	source, err := os.ReadFile("runtime_accounts.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, `"restart", "agent-vm-deploy-host.socket", "agent-vm-slot-relay.socket"`) {
		t.Fatal("the handoff starts the helper sockets without restarting them")
	}
}

// systemd ignores Sockets= outside [Unit]. Older materials lack the line, so
// the handoff synthesizes it; it must land in the unit section.
func TestSteadyDropInDeclaresSocketInUnitSection(t *testing.T) {
	account := []byte("[Unit]\nRequires=agent-vm-deployd.socket\n\n[Service]\nUser=agent-vm-deploy\n")
	steady, _ := deploymentAccountDropIns(account)
	text := string(steady)
	unitStart := strings.Index(text, "[Unit]")
	serviceStart := strings.Index(text, "[Service]")
	declaration := strings.Index(text, "Sockets=agent-vm-deployd.socket")
	if unitStart < 0 || serviceStart < 0 || declaration < 0 || declaration > serviceStart {
		t.Fatalf("Sockets= is not inside [Unit]:\n%s", text)
	}
	if strings.Count(text, "Sockets=agent-vm-deployd.socket") != 1 {
		t.Fatalf("Sockets= was declared more than once:\n%s", text)
	}

	already := []byte("[Unit]\nSockets=agent-vm-deployd.socket\n\n[Service]\nUser=agent-vm-deploy\n")
	steady, _ = deploymentAccountDropIns(already)
	if strings.Count(string(steady), "Sockets=agent-vm-deployd.socket") != 1 {
		t.Fatalf("existing declaration was duplicated:\n%s", steady)
	}
}
