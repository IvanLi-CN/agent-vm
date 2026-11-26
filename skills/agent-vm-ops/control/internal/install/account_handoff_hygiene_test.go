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

// The helper writes new socket unit files. systemd drops the listening
// descriptor when a socket unit file changes under it, so a plain start leaves
// a unit that reports active while every connection is refused. The handoff
// must restart it.
func TestHandoffRestartsDeploydSocket(t *testing.T) {
	source, err := os.ReadFile("account_deploy_switch.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `"restart", deploydSocketUnit`) {
		t.Fatal("the handoff starts the deployd socket without restarting it")
	}
}

// The request relay derives the slot UID and setuids into it. NO_NEW_PRIVS
// forbids that transition, so the adapter service must not enable it or every
// SSH-request operation fails with "fork/exec /proc/self/fd/3: operation not
// permitted" once the slot runs under its own account.
func TestRequestRelayMayDropToTheSlotAccount(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("../../../../..", "deploy/assets/systemd/agent-vm-request@.service"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if strings.Contains(text, "NoNewPrivileges=yes") {
		t.Fatal("the request relay cannot setuid into the slot account under NO_NEW_PRIVS")
	}
	if !strings.Contains(text, "NoNewPrivileges=no") {
		t.Fatal("the request relay should state its setuid requirement explicitly")
	}
}
