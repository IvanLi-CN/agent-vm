package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIncusTrustRequiresExactlyOneProject(t *testing.T) {
	for _, raw := range []string{
		`{"type":"client","restricted":false,"projects":["agents"]}`,
		`{"type":"client","restricted":true,"projects":["agents","another"]}`,
		`{"type":"client","restricted":true,"projects":["another"]}`,
		`{"type":"server","restricted":true,"projects":["agents"]}`,
	} {
		if err := verifyIncusTrust([]byte(raw), "agents"); err == nil {
			t.Errorf("accepted incorrect trust record: %s", raw)
		}
	}
	if err := verifyIncusTrust([]byte(`{"type":"client","restricted":true,"projects":["agents"]}`), "agents"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeIncusInstallerCredentials(t *testing.T) {
	dir := os.Getenv("AGENT_VM_TEST_INCUS_INSTALLER_CERTS")
	if dir == "" {
		t.Skip("requires an isolated Incus server and protected fixture credentials")
	}
	project := os.Getenv("AGENT_VM_TEST_INCUS_INSTALLER_PROJECT")
	cfg := Config{Files: map[string]string{
		"incus_client_certificate": filepath.Join(dir, "client.crt"),
		"incus_client_private_key": filepath.Join(dir, "client.key"),
		"incus_server_certificate": filepath.Join(dir, "server.crt"),
	}}
	if err := checkIncusCredentials(t.Context(), cfg, project); err != nil {
		t.Fatal(err)
	}
	if err := checkIncusCredentials(t.Context(), cfg, project+"-other"); err == nil {
		t.Fatal("accepted same certificate for a different project")
	}
	cfg.Files["incus_server_certificate"] = cfg.Files["incus_client_certificate"]
	if err := checkIncusCredentials(t.Context(), cfg, project); err == nil {
		t.Fatal("accepted a mismatched server certificate pin")
	}
}
